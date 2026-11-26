package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/oauth2"

	"github.com/hoshposh/photo-migrator/internal/photos"
	"github.com/hoshposh/photo-migrator/internal/store"
)

var (
	wdTaken  = time.Date(2019, 6, 14, 18, 2, 0, 0, time.UTC) // unix 1560535320
	wdGoogle = time.Date(2026, 7, 3, 9, 41, 0, 0, time.UTC)
)

func wdSidecar(title string, taken time.Time) string {
	return fmt.Sprintf(`{"title":%q,"photoTakenTime":{"timestamp":"%d"}}`, title, taken.Unix())
}

func wdRow(path, zipName, album, id string, taken, google time.Time) store.DateRow {
	r := store.DateRow{
		SourcePath: path, SourceZip: zipName, ZipEntry: path, AlbumName: album, MediaID: id,
		TakenTime: taken, TakenSource: store.TakenFromSidecar,
		GoogleTime: google, GoogleCheckedAt: time.Now(),
	}
	if taken.IsZero() {
		r.TakenSource = store.TakenNone
	}
	return r
}

// Break caught: a photo flagged (or let through) on the wrong side of the
// 24 hour rule, one copy's date being judged instead of all of them, or an
// unjudgeable photo counted as wrong.
func TestJudgeDates(t *testing.T) {
	h := time.Hour
	tests := []struct {
		name                       string
		rows                       []store.DateRow
		wrong                      int
		noSidecar, unread, unknown int
	}{
		{"+3h is fine", []store.DateRow{wdRow("a.jpg", "z1", "", "m-1", wdTaken, wdTaken.Add(3*h))}, 0, 0, 0, 0},
		{"+25h is wrong", []store.DateRow{wdRow("a.jpg", "z1", "", "m-1", wdTaken, wdTaken.Add(25*h))}, 1, 0, 0, 0},
		{"-25h is wrong", []store.DateRow{wdRow("a.jpg", "z1", "", "m-1", wdTaken, wdTaken.Add(-25*h))}, 1, 0, 0, 0},
		{"no sidecar date", []store.DateRow{wdRow("a.jpg", "z1", "", "m-1", time.Time{}, wdGoogle)}, 0, 1, 0, 0},
		{"missing at Google", []store.DateRow{func() store.DateRow {
			r := wdRow("a.jpg", "z1", "", "m-1", wdTaken, time.Time{})
			r.GoogleMissing = true
			return r
		}()}, 0, 0, 1, 0},
		{"taken date not looked up", []store.DateRow{func() store.DateRow {
			r := wdRow("a.jpg", "z1", "", "m-1", wdTaken, wdGoogle)
			r.TakenSource = ""
			return r
		}()}, 0, 0, 0, 1},
		{"google date not fetched", []store.DateRow{func() store.DateRow {
			r := wdRow("a.jpg", "z1", "", "m-1", wdTaken, time.Time{})
			r.GoogleCheckedAt = time.Time{}
			return r
		}()}, 0, 0, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := judgeDates(tc.rows)
			if len(rep.Wrong) != tc.wrong || rep.NoSidecarDate != tc.noSidecar ||
				rep.NotReadable != tc.unread || rep.NotChecked != tc.unknown {
				t.Errorf("got %d wrong, %d no sidecar, %d unreadable, %d unchecked; want %d, %d, %d, %d",
					len(rep.Wrong), rep.NoSidecarDate, rep.NotReadable, rep.NotChecked,
					tc.wrong, tc.noSidecar, tc.unread, tc.unknown)
			}
		})
	}

	t.Run("shown copy is the one in an album", func(t *testing.T) {
		rep := judgeDates([]store.DateRow{
			wdRow("Photos from 2019/a.jpg", "z1", "", "m-1", wdTaken, wdTaken.Add(25*h)),
			wdRow("Trip/a.jpg", "z2", "Trip", "m-1", wdTaken, wdTaken.Add(25*h)),
		})
		if len(rep.Wrong) != 1 {
			t.Fatalf("got %d wrong, want 1", len(rep.Wrong))
		}
		w := rep.Wrong[0]
		if w.Album != "Trip" || len(w.Copies) != 2 {
			t.Errorf("album = %q, copies = %d; want Trip, 2", w.Album, len(w.Copies))
		}
	})

	t.Run("copies that disagree by days are listed", func(t *testing.T) {
		other := wdTaken.Add(72 * h)
		rep := judgeDates([]store.DateRow{
			wdRow("a/x.jpg", "z1", "", "m-1", wdTaken, wdTaken),
			wdRow("b/x.jpg", "z2", "", "m-1", other, wdTaken),
		})
		if len(rep.Wrong) != 1 {
			t.Fatalf("got %d wrong, want 1", len(rep.Wrong))
		}
		w := rep.Wrong[0]
		if w.ShouldBe != wdTaken || !reflect.DeepEqual(w.OtherExpected, []time.Time{other}) {
			t.Errorf("ShouldBe = %v, OtherExpected = %v", w.ShouldBe, w.OtherExpected)
		}
	})
}

// wdFixture is a ledger plus a fake batchGet endpoint.
type wdFixture struct {
	t       *testing.T
	st      *store.Store
	dir     string // the takeout dir
	dbPath  string
	google  map[string]time.Time
	missing map[string]bool

	mu       sync.Mutex
	requests [][]string // IDs of each batchGet, in order
	onReq    func(n int, w http.ResponseWriter, r *http.Request) bool
	client   *photos.Client
	logs     *observer.ObservedLogs
	logger   *zap.Logger
}

func newWDFixture(t *testing.T) *wdFixture {
	t.Helper()
	f := &wdFixture{t: t, dir: t.TempDir(), google: map[string]time.Time{}, missing: map[string]bool{}}
	f.dbPath = filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st

	fake := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(fake.Close)
	target, _ := url.Parse(fake.URL)
	f.client = photos.New(&http.Client{Transport: rewriteTransport{target}})

	core, logs := observer.New(zap.WarnLevel)
	f.logs, f.logger = logs, zap.New(core)
	return f
}

func (f *wdFixture) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/mediaItems:batchGet") {
		http.NotFound(w, r)
		return
	}
	ids := r.URL.Query()["mediaItemIds"]
	f.mu.Lock()
	f.requests = append(f.requests, ids)
	n := len(f.requests)
	hook := f.onReq
	f.mu.Unlock()
	if hook != nil && hook(n, w, r) {
		return
	}
	type result struct {
		MediaItem any `json:"mediaItem,omitempty"`
		Status    any `json:"status,omitempty"`
	}
	var out struct {
		MediaItemResults []result `json:"mediaItemResults"`
	}
	for _, id := range ids {
		if f.missing[id] {
			out.MediaItemResults = append(out.MediaItemResults, result{Status: map[string]any{"code": 5, "message": "not found"}})
			continue
		}
		out.MediaItemResults = append(out.MediaItemResults, result{MediaItem: map[string]any{
			"id":            id,
			"mediaMetadata": map[string]any{"creationTime": f.google[id].Format(time.RFC3339)},
		}})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *wdFixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *wdFixture) newClient() (*photos.Client, error) { return f.client, nil }

func (f *wdFixture) addZip(name string, entries map[string]string) {
	f.t.Helper()
	writeZipEntries(f.t, filepath.Join(f.dir, name), entries)
}

// upload records a file as uploaded from zipName with media ID id.
func (f *wdFixture) upload(zipName, entry, album, id string) {
	f.t.Helper()
	zipPath := filepath.Join(f.dir, zipName)
	staging := "/stage/" + strings.TrimSuffix(zipName, ".zip") + "/" + entry
	for _, err := range []error{
		f.st.RegisterZip(zipPath),
		f.st.RegisterMedia(staging, zipPath, entry, album),
		f.st.SetMediaComplete(staging, id),
	} {
		if err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *wdFixture) run(ctx context.Context, opts wrongDatesOpts, newClient func() (*photos.Client, error)) (out, errOut string, err error) {
	f.t.Helper()
	if opts.TakeoutDir == "" {
		opts.TakeoutDir = f.dir
	}
	if opts.Format == "" {
		opts.Format = "text"
	}
	if newClient == nil {
		newClient = f.newClient
	}
	var o, e bytes.Buffer
	err = runWrongDates(ctx, f.st, newClient, opts, &o, &e, f.logger)
	return o.String(), e.String(), err
}

func (f *wdFixture) rows() map[string]store.DateRow {
	f.t.Helper()
	rows, err := f.st.GetDateRows()
	if err != nil {
		f.t.Fatal(err)
	}
	m := map[string]store.DateRow{}
	for _, r := range rows {
		m[r.ZipEntry] = r
	}
	return m
}

const wdTrip = "Takeout/Google Photos/Trip/"

// Break caught: the report missing a photo Google dated differently, listing
// one it dated within a few hours, or printing times in another zone.
func TestWrongDates_ListsWrongDate(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{
		wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken),
		wdTrip + "b.jpg.supplemental-metadata.json": wdSidecar("b.jpg", wdTaken),
	})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.upload("takeout-001.zip", wdTrip+"b.jpg", "Trip", "m-b")
	f.google["m-a"] = wdGoogle
	f.google["m-b"] = wdTaken.Add(3 * time.Hour)

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.jpg", "2019-06-14 18:02 UTC", "2026-07-03 09:41 UTC", "Trip", "takeout-001.zip"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "b.jpg") {
		t.Errorf("b.jpg is 3 h off and must not be listed:\n%s", out)
	}
}

// Break caught: a photo whose sidecar sits in another zip being treated as
// having no date.
func TestWrongDates_SidecarInOtherZip(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg": "x"})
	f.addZip("takeout-002.zip", map[string]string{wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken)})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.google["m-a"] = wdGoogle

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.jpg") || !strings.Contains(out, "2019-06-14 18:02 UTC") {
		t.Errorf("a.jpg not listed with its date from the other zip:\n%s", out)
	}
}

// Break caught: a zip missing from takeout-dir turning every unmatched photo
// into "no sidecar date", hiding photos whose sidecar was in that zip.
func TestWrongDates_MissingZipLeavesRowsUnchecked(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg": "x"}) // no sidecar for a.jpg
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.upload("takeout-002.zip", wdTrip+"b.jpg", "Trip", "m-b") // takeout-002.zip is not in the dir
	f.google["m-a"], f.google["m-b"] = wdGoogle, wdGoogle

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for entry, r := range f.rows() {
		if r.TakenSource != "" {
			t.Errorf("%s: TakenSource = %q, want unset while a zip is missing", entry, r.TakenSource)
		}
	}
	found := false
	for _, e := range f.logs.All() {
		found = found || strings.Contains(fmt.Sprint(e.Message, e.ContextMap()), "takeout-002.zip")
	}
	if !found {
		t.Errorf("no warning names takeout-002.zip: %v", f.logs.All())
	}
	if !strings.Contains(out, "2 not checked yet") {
		t.Errorf("report does not show the unchecked count:\n%s", out)
	}
}

// Break caught: a token without the read scope being retried, or a "not
// found" being recorded for every photo.
func TestWrongDates_InsufficientScope(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken)})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.onReq = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED"}}`))
		return true
	}

	_, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err == nil || !strings.Contains(err.Error(), "photo-migrator auth") ||
		!strings.Contains(err.Error(), "photoslibrary.readonly.appcreateddata") {
		t.Fatalf("err = %v, want the scope and 'photo-migrator auth' named", err)
	}
	for entry, r := range f.rows() {
		if !r.GoogleCheckedAt.IsZero() {
			t.Errorf("%s has a check time after a refused request", entry)
		}
	}
}

// Break caught: Ctrl+C losing the batches already fetched, or a second run
// asking about IDs it already knows.
func TestWrongDates_InterruptKeepsFetchedBatches(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{"x.txt": "x"})
	for i := 0; i < 60; i++ {
		f.upload("takeout-001.zip", fmt.Sprintf("%sp%02d.jpg", wdTrip, i), "Trip", fmt.Sprintf("m-%02d", i))
		f.google[fmt.Sprintf("m-%02d", i)] = wdTaken
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onReq = func(n int, _ http.ResponseWriter, r *http.Request) bool {
		if n != 2 {
			return false
		}
		cancel()
		<-r.Context().Done() // the client has gone: never answer
		return true
	}

	_, _, err := f.run(ctx, wrongDatesOpts{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	checked := func() int {
		n := 0
		for _, r := range f.rows() {
			if !r.GoogleCheckedAt.IsZero() {
				n++
			}
		}
		return n
	}
	if n := checked(); n != 50 {
		t.Fatalf("%d IDs checked after the interrupt, want 50", n)
	}

	f.onReq = nil
	before := f.requestCount()
	if _, _, err := f.run(context.Background(), wrongDatesOpts{}, nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	rest := f.requests[before:]
	f.mu.Unlock()
	if len(rest) != 1 || len(rest[0]) != 10 {
		t.Errorf("second run made requests %v, want one for the remaining 10 IDs", rest)
	}
	if n := checked(); n != 60 {
		t.Errorf("%d IDs checked after the second run, want 60", n)
	}
}

// Break caught: the report needing the zips and Google again once everything
// is recorded.
func TestWrongDates_RerunMakesNoRequests(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken)})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.google["m-a"] = wdGoogle

	first, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := f.requestCount()

	second, _, err := f.run(context.Background(), wrongDatesOpts{TakeoutDir: filepath.Join(f.dir, "gone")},
		func() (*photos.Client, error) {
			t.Error("newClient called although every date is recorded")
			return nil, errors.New("unexpected")
		})
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("second report differs:\n%s\nvs\n%s", second, first)
	}
	if f.requestCount() != n {
		t.Error("second run made requests")
	}
}

// Break caught: the report touching upload progress, which would make a zip
// or file upload again.
func TestWrongDates_DoesNotChangeUploadState(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken)})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.google["m-a"] = wdGoogle
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	snapshot := func() string {
		t.Helper()
		var sb strings.Builder
		for _, q := range []string{
			`SELECT source_path, status, COALESCE(photos_media_id, '') FROM media_items ORDER BY source_path`,
			`SELECT zip_path, status FROM zip_archives ORDER BY zip_path`,
		} {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var a, b string
				var c sql.NullString
				cols, _ := rows.Columns()
				if len(cols) == 3 {
					err = rows.Scan(&a, &b, &c)
				} else {
					err = rows.Scan(&a, &b)
				}
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintln(&sb, a, b, c.String)
			}
			_ = rows.Close()
		}
		return sb.String()
	}
	before := snapshot()
	if _, _, err := f.run(context.Background(), wrongDatesOpts{}, nil); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Errorf("upload state changed:\nbefore %s\nafter  %s", before, after)
	}
}

// Break caught: progress or log text mixed into JSON on stdout, or the JSON
// shape drifting from the documented one.
func TestWrongDates_JSON(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken)})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.upload("takeout-001.zip", wdTrip+"nodate.jpg", "Trip", "m-n")
	f.google["m-a"], f.google["m-n"] = wdGoogle, wdGoogle

	out, errOut, err := f.run(context.Background(), wrongDatesOpts{Format: "json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Wrong []struct {
			MediaID       string   `json:"media_id"`
			File          string   `json:"file"`
			Album         string   `json:"album"`
			SourceZip     string   `json:"source_zip"`
			InZipPath     string   `json:"in_zip_path"`
			ShouldBe      string   `json:"should_be"`
			GoogleHas     string   `json:"google_has"`
			OtherExpected []string `json:"other_expected"`
			Copies        []struct {
				SourceZip string `json:"source_zip"`
				InZipPath string `json:"in_zip_path"`
				Album     string `json:"album"`
			} `json:"copies"`
		} `json:"wrong"`
		NotJudged struct {
			NoSidecarDate int `json:"no_sidecar_date"`
			NotReadable   int `json:"not_readable"`
			NotChecked    int `json:"not_checked"`
		} `json:"not_judged"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if len(got.Wrong) != 1 {
		t.Fatalf("wrong = %d, want 1", len(got.Wrong))
	}
	w := got.Wrong[0]
	if w.MediaID != "m-a" || w.File != "a.jpg" || w.Album != "Trip" || w.InZipPath != wdTrip+"a.jpg" ||
		filepath.Base(w.SourceZip) != "takeout-001.zip" ||
		w.ShouldBe != "2019-06-14T18:02:00Z" || w.GoogleHas != "2026-07-03T09:41:00Z" ||
		len(w.Copies) != 1 || w.Copies[0].Album != "Trip" {
		t.Errorf("unexpected entry: %+v", w)
	}
	if got.NotJudged.NoSidecarDate != 1 {
		t.Errorf("no_sidecar_date = %d, want 1", got.NotJudged.NoSidecarDate)
	}
	if !strings.Contains(errOut, "checked 2 / 2") {
		t.Errorf("stderr lacks the progress line: %q", errOut)
	}
	if strings.Contains(out, "checked 2 / 2") {
		t.Errorf("progress leaked to stdout: %q", out)
	}
}

// Break caught: one failed batchGet chunk aborting the whole report, or its
// photos being judged as if Google had answered.
func TestWrongDates_FailedChunkIsSkipped(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{"x.txt": "x"})
	for i := 0; i < 150; i++ {
		f.upload("takeout-001.zip", fmt.Sprintf("%sp%03d.jpg", wdTrip, i), "Trip", fmt.Sprintf("m-%03d", i))
		f.google[fmt.Sprintf("m-%03d", i)] = wdTaken
	}
	f.onReq = func(n int, w http.ResponseWriter, _ *http.Request) bool {
		if n != 2 {
			return false
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return true
	}

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatalf("a failed chunk must not fail the command: %v", err)
	}
	checked := 0
	for _, r := range f.rows() {
		if !r.GoogleCheckedAt.IsZero() {
			checked++
		}
	}
	if checked != 100 {
		t.Errorf("%d IDs checked, want 100 (the failed chunk's 50 stay unchecked)", checked)
	}
	if f.logs.Len() == 0 {
		t.Error("the failed chunk was not logged as a warning")
	}
	if !strings.Contains(out, "50 not checked yet") {
		t.Errorf("report lacks the not-checked count:\n%s", out)
	}
}

// Break caught: a persistent failure being retried for every chunk (hours
// for a large library) instead of stopping, or a success not resetting the count.
func TestWrongDates_StopsAfterThreeFailedChunks(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{"x.txt": "x"})
	for i := 0; i < 200; i++ {
		f.upload("takeout-001.zip", fmt.Sprintf("%sp%03d.jpg", wdTrip, i), "Trip", fmt.Sprintf("m-%03d", i))
	}
	f.onReq = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, "bad request", http.StatusBadRequest)
		return true
	}

	_, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err == nil || !strings.Contains(err.Error(), "3 failed batches") {
		t.Fatalf("err = %v, want a stop after 3 failed batches", err)
	}
	if n := f.requestCount(); n != 3 {
		t.Errorf("%d batchGet requests, want 3", n)
	}
	for entry, r := range f.rows() {
		if !r.GoogleCheckedAt.IsZero() {
			t.Errorf("%s was stored", entry)
		}
	}
}

// Break caught: a rejected sign-in treated as a failed chunk, giving hundreds
// of warnings and exit 0 instead of telling the user to sign in again.
func TestWrongDates_RejectedSignInIsFatal(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{"x.txt": "x"})
	for i := 0; i < 120; i++ {
		f.upload("takeout-001.zip", fmt.Sprintf("%sp%03d.jpg", wdTrip, i), "Trip", fmt.Sprintf("m-%03d", i))
	}
	var apiCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Bad Request"}`))
			return
		}
		apiCalls.Add(1)
		http.NotFound(w, r)
	}))
	defer tokenSrv.Close()
	target, _ := url.Parse(tokenSrv.URL)
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: rewriteTransport{target}})
	cfg := &oauth2.Config{ClientID: "id", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: "https://oauth2.example/token"}}
	expired := &oauth2.Token{AccessToken: "old", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Hour)}
	client := photos.New(cfg.Client(ctx, expired))

	_, _, err := f.run(context.Background(), wrongDatesOpts{}, func() (*photos.Client, error) { return client, nil })
	if !errors.Is(err, photos.ErrAuthFailed) {
		t.Fatalf("err = %v, want photos.ErrAuthFailed", err)
	}
	if f.logs.Len() != 0 {
		t.Errorf("a rejected sign-in must not be logged as skipped chunks: %v", f.logs.All())
	}
	for entry, r := range f.rows() {
		if !r.GoogleCheckedAt.IsZero() {
			t.Errorf("%s was stored", entry)
		}
	}
}

// Break caught: Google's "ID does not match any media items" (a photo kept
// from before this tool, which the app cannot read) failing the whole chunk,
// or hiding the other photos in it.
func TestWrongDates_UnreadableItemInChunk(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{
		wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken),
		wdTrip + "b.jpg.supplemental-metadata.json": wdSidecar("b.jpg", wdTaken),
	})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.upload("takeout-001.zip", wdTrip+"b.jpg", "Trip", "m-b")
	f.google["m-a"] = wdGoogle
	f.onReq = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		_, _ = w.Write([]byte(`{"mediaItemResults":[` +
			`{"mediaItem":{"id":"m-a","mediaMetadata":{"creationTime":"2026-07-03T09:41:00Z"}}},` +
			`{"status":{"code":3,"message":"The provided ID does not match any media items."}}]}`))
		return true
	}

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatalf("code 3 must not fail the command: %v", err)
	}
	rows := f.rows()
	if r := rows[wdTrip+"a.jpg"]; r.GoogleCheckedAt.IsZero() || r.GoogleMissing {
		t.Errorf("a.jpg should be stored with Google's date: %+v", r)
	}
	if r := rows[wdTrip+"b.jpg"]; !r.GoogleMissing {
		t.Errorf("b.jpg should be recorded as not readable: %+v", r)
	}
	if !strings.Contains(out, "1 already in your library before this tool, or deleted") {
		t.Errorf("report lacks the not-readable count:\n%s", out)
	}
}

// Break caught: a photo Google returned without a date being judged against
// the zero time and listed as 2000 years wrong.
func TestWrongDates_ZeroGoogleTimeStaysUnchecked(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken)})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.onReq = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		_, _ = w.Write([]byte(`{"mediaItemResults":[{"mediaItem":{"id":"m-a","mediaMetadata":{}}}]}`))
		return true
	}

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "a.jpg") || !strings.Contains(out, "1 not checked yet") {
		t.Errorf("a.jpg must be unchecked, not judged:\n%s", out)
	}
}

// Break caught: a directory-mode file's sidecar not being read from disk, or
// a file that no longer exists being recorded as having no date.
func TestWrongDates_DirMode(t *testing.T) {
	f := newWDFixture(t)
	photo := filepath.Join(f.dir, "Trip", "a.jpg")
	gone := filepath.Join(f.dir, "Trip", "gone.jpg") // registered, but not on disk
	if err := os.MkdirAll(filepath.Dir(photo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(photo+".supplemental-metadata.json", []byte(wdSidecar("a.jpg", wdTaken)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{photo, gone} {
		if err := f.st.RegisterMedia(p, "", "", "Trip"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.SetMediaComplete(photo, "m-a"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetMediaComplete(gone, "m-g"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(photo, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.google["m-a"], f.google["m-g"] = wdGoogle, wdGoogle

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.jpg") || !strings.Contains(out, "1 not checked yet") {
		t.Errorf("want a.jpg listed and gone.jpg unchecked:\n%s", out)
	}
}

// Break caught: the command needing credentials, or failing, when there is
// nothing uploaded to check.
func TestWrongDatesCommand_EmptyLedger(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "m.db")
	var runErr error
	out := captureStdout(t, func() { runErr = execute(t, "wrong-dates", "--db-path", dbPath) })
	if runErr != nil {
		t.Fatalf("wrong-dates: %v", runErr)
	}
	if !strings.Contains(out, "✓ No uploaded photos have a wrong date.") {
		t.Errorf("output:\n%s", out)
	}
}

// Break caught: --recheck not asking Google again, so a date fixed by hand
// stays listed; or asking about photos that were not listed.
func TestWrongDates_RecheckSeesFixedDate(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{
		wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken),
		wdTrip + "b.jpg.supplemental-metadata.json": wdSidecar("b.jpg", wdTaken),
	})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.upload("takeout-001.zip", wdTrip+"b.jpg", "Trip", "m-b")
	f.google["m-a"] = wdGoogle
	f.google["m-b"] = wdTaken

	out, _, err := f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.jpg") || !strings.Contains(out, "wrong-dates --recheck") {
		t.Fatalf("first run must list a.jpg and mention --recheck:\n%s", out)
	}
	n := f.requestCount()

	f.google["m-a"] = wdTaken // the user fixed it in Google Photos

	out, _, err = f.run(context.Background(), wrongDatesOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.jpg") || f.requestCount() != n {
		t.Errorf("without Recheck a.jpg must stay listed with no request (requests %d -> %d):\n%s", n, f.requestCount(), out)
	}

	out, _, err = f.run(context.Background(), wrongDatesOpts{Recheck: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.requestCount() != n+1 {
		t.Fatalf("Recheck made %d requests, want 1", f.requestCount()-n)
	}
	if got := f.requests[n]; !reflect.DeepEqual(got, []string{"m-a"}) {
		t.Errorf("Recheck asked for %v, want only m-a", got)
	}
	if strings.Contains(out, "a.jpg") || strings.Contains(out, "--recheck") {
		t.Errorf("nothing should be listed after the fix:\n%s", out)
	}
}

// Break caught: the --recheck hint printed in JSON output.
func TestWrongDates_RecheckHintIsTextOnly(t *testing.T) {
	f := newWDFixture(t)
	f.addZip("takeout-001.zip", map[string]string{wdTrip + "a.jpg.supplemental-metadata.json": wdSidecar("a.jpg", wdTaken)})
	f.upload("takeout-001.zip", wdTrip+"a.jpg", "Trip", "m-a")
	f.google["m-a"] = wdGoogle
	out, _, err := f.run(context.Background(), wrongDatesOpts{Format: "json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "--recheck") {
		t.Errorf("JSON output must not carry the hint:\n%s", out)
	}
}
