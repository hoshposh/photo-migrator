package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoshposh/photo-migrator/internal/store"
)

const lvTrip = "Takeout/Google Photos/Trip/"

// lvRun runs the uploaded-videos report against f.
func (f *wdFixture) lvRun(ctx context.Context, opts uploadedVideosOpts) (out, errOut string, err error) {
	f.t.Helper()
	if opts.Format == "" {
		opts.Format = "text"
	}
	var o, e bytes.Buffer
	err = runUploadedVideos(ctx, f.st, f.newClient, opts, &o, &e, f.logger, time.UTC, time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC))
	return o.String(), e.String(), err
}

// Break caught: a standalone video, a still or a held-back video listed as a
// Live Photo video to delete, or the video's link missing.
func TestUploadedVideos_ListsOnlyUploadedLivePhotoVideos(t *testing.T) {
	f := newWDFixture(t)
	f.upload("takeout-001.zip", lvTrip+"IMG_1.HEIC", "Trip", "m-still")
	f.upload("takeout-001.zip", lvTrip+"IMG_1.MP4", "Trip", "m-live")
	f.upload("takeout-001.zip", lvTrip+"VID_1.mp4", "Trip", "m-vid")
	if _, err := f.st.RegisterHeld(store.StatusMotionVideo, "/stage/takeout-001/"+lvTrip+"IMG_2.MOV",
		filepath.Join(f.dir, "takeout-001.zip"), lvTrip+"IMG_2.MOV"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m-still", "m-live", "m-vid"} {
		f.google[id] = wdGoogle
	}

	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 video(s) to remove by hand", "IMG_1.MP4", "Trip", "takeout-001.zip / " + lvTrip + "IMG_1.MP4", wdURL("m-live")} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"VID_1", "IMG_2", "IMG_1.HEIC", "Already removed", "Not checked"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
	// Only the video's ID was asked about.
	if len(f.requests) != 1 || strings.Join(f.requests[0], ",") != "m-live" {
		t.Errorf("batchGet requests = %v, want one for m-live", f.requests)
	}
}

// Break caught: a video deleted in Google Photos still listed, or a deletion
// remembered forever instead of Google being asked on every run.
func TestUploadedVideos_DeletedVideosDropOff(t *testing.T) {
	f := newWDFixture(t)
	for i, n := range []string{"IMG_1", "IMG_2"} {
		f.upload("takeout-001.zip", lvTrip+n+".HEIC", "Trip", fmt.Sprintf("m-s%d", i))
		f.upload("takeout-001.zip", lvTrip+n+".MP4", "Trip", fmt.Sprintf("m-v%d", i))
		f.google[fmt.Sprintf("m-v%d", i)] = wdGoogle
	}
	f.missing["m-v0"] = true

	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 video(s) to remove by hand") || !strings.Contains(out, "IMG_2.MP4") ||
		strings.Contains(out, "IMG_1.MP4") || !strings.Contains(out, "Already removed: 1.") {
		t.Errorf("unexpected output:\n%s", out)
	}

	delete(f.missing, "m-v0")
	out, _, err = f.lvRun(context.Background(), uploadedVideosOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 video(s) to remove by hand") || strings.Contains(out, "Already removed") {
		t.Errorf("second run should list both again:\n%s", out)
	}
}

// Break caught: the list needing the Takeout directory, which is gone once the
// zips are deleted.
func TestUploadedVideos_NoTakeoutDir(t *testing.T) {
	f := newWDFixture(t)
	f.upload("takeout-001.zip", lvTrip+"IMG_1.HEIC", "Trip", "m-still")
	f.upload("takeout-002.zip", lvTrip+"IMG_1.MP4", "Trip", "m-live")
	f.google["m-live"] = wdGoogle

	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{TakeoutDir: ""})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 video(s) to remove by hand") || !strings.Contains(out, "IMG_1.MP4") {
		t.Errorf("video not listed:\n%s", out)
	}
}

// Break caught: a takeout-dir left in the config after the zips and their
// folder were deleted making the cleanup page fail instead of using the
// database alone.
func TestUploadedVideos_MissingTakeoutDir(t *testing.T) {
	f := newWDFixture(t)
	f.upload("takeout-001.zip", lvTrip+"IMG_1.HEIC", "Trip", "m-still")
	f.upload("takeout-002.zip", lvTrip+"IMG_1.MP4", "Trip", "m-live")
	f.google["m-live"] = wdGoogle

	gone := filepath.Join(f.dir, "deleted-takeout")
	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{TakeoutDir: gone})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 video(s) to remove by hand") || !strings.Contains(out, "IMG_1.MP4") {
		t.Errorf("video not listed:\n%s", out)
	}
}

// Break caught: directory mode (no zips, files compared by path relative to
// the takeout directory) not finding its videos.
func TestUploadedVideos_DirMode(t *testing.T) {
	f := newWDFixture(t)
	for _, n := range []string{"IMG_1.HEIC", "IMG_1.MP4"} {
		p := filepath.Join(f.dir, "Takeout", "Google Photos", "Trip", n)
		if err := f.st.RegisterMedia(p, "", "", "Trip"); err != nil {
			t.Fatal(err)
		}
		if err := f.st.SetMediaComplete(p, "m-"+n); err != nil {
			t.Fatal(err)
		}
	}
	f.google["m-IMG_1.MP4"] = wdGoogle

	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{TakeoutDir: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 video(s) to remove by hand") || !strings.Contains(out, "IMG_1.MP4") {
		t.Errorf("video not listed:\n%s", out)
	}
}

// Break caught: the report touching upload progress: status, media IDs or zip
// statuses.
func TestUploadedVideos_DoesNotChangeUploadState(t *testing.T) {
	f := newWDFixture(t)
	f.upload("takeout-001.zip", lvTrip+"IMG_1.HEIC", "Trip", "m-still")
	f.upload("takeout-001.zip", lvTrip+"IMG_1.MP4", "Trip", "m-live")
	f.google["m-live"] = wdGoogle
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	snapshot := func() string {
		t.Helper()
		var sb strings.Builder
		rows, err := db.Query(`SELECT source_path, status, COALESCE(photos_media_id, '') FROM media_items ORDER BY source_path`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var a, b, c string
			if err := rows.Scan(&a, &b, &c); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&sb, a, b, c)
		}
		_ = rows.Close()
		zr, err := db.Query(`SELECT zip_path, status FROM zip_archives ORDER BY zip_path`)
		if err != nil {
			t.Fatal(err)
		}
		for zr.Next() {
			var a, b string
			if err := zr.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&sb, a, b)
		}
		_ = zr.Close()
		return sb.String()
	}
	before := snapshot()
	if _, _, err := f.lvRun(context.Background(), uploadedVideosOpts{}); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Errorf("upload state changed:\nbefore %s\nafter  %s", before, after)
	}
}

// Break caught: a failed batch hiding its videos instead of listing them as
// "not checked", or JSON drifting from the documented shape.
func TestUploadedVideos_JSONAndNotChecked(t *testing.T) {
	f := newWDFixture(t)
	f.upload("takeout-001.zip", lvTrip+"IMG_1.HEIC", "Trip", "m-still")
	f.upload("takeout-001.zip", lvTrip+"IMG_1.MP4", "Trip", "m-live")
	f.upload("takeout-001.zip", "Takeout/Google Photos/Photos from 2020/IMG_9.HEIC", "", "m-s9")
	f.upload("takeout-001.zip", "Takeout/Google Photos/Photos from 2020/IMG_9.MOV", "", "m-v9")
	f.google["m-live"], f.google["m-v9"] = wdGoogle, wdGoogle
	f.missing["m-v9"] = true

	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Videos []struct {
			MediaID   string `json:"media_id"`
			File      string `json:"file"`
			Album     string `json:"album"`
			SourceZip string `json:"source_zip"`
			InZipPath string `json:"in_zip_path"`
			URL       string `json:"url"`
		} `json:"videos"`
		AlreadyRemoved int `json:"already_removed"`
		NotChecked     int `json:"not_checked"`
	}
	for _, kv := range [][2]string{{"MediaID", "media_id"}, {"File", "file"}, {"Album", "album"}, {"SourceZip", "source_zip"}, {"InZipPath", "in_zip_path"}, {"URL", "url"}} {
		if !strings.Contains(out, `"`+kv[1]+`"`) {
			t.Errorf("JSON lacks key %q:\n%s", kv[1], out)
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(doc.Videos) != 1 || doc.Videos[0].MediaID != "m-live" || doc.Videos[0].File != "IMG_1.MP4" ||
		doc.Videos[0].Album != "Trip" || doc.Videos[0].InZipPath != lvTrip+"IMG_1.MP4" || doc.Videos[0].URL != wdURL("m-live") ||
		doc.AlreadyRemoved != 1 || doc.NotChecked != 0 {
		t.Errorf("unexpected JSON: %+v\n%s", doc, out)
	}

	// A batch Google fails on is listed from stored state and counted.
	f2 := newWDFixture(t)
	f2.upload("takeout-001.zip", lvTrip+"IMG_1.HEIC", "Trip", "m-still")
	f2.upload("takeout-001.zip", lvTrip+"IMG_1.MP4", "Trip", "m-live")
	f2.onReq = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(400)
		return true
	}
	out, _, err = f2.lvRun(context.Background(), uploadedVideosOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 video(s) to remove by hand") || !strings.Contains(out, "IMG_1.MP4") ||
		!strings.Contains(out, "no link") || !strings.Contains(out, "Not checked: 1.") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

// Break caught: an unescaped name in the page, a missing media-ID key for the
// ticks, a missing storage key, a link without rel="noopener", or any other
// network reference.
func TestUploadedVideos_HTML(t *testing.T) {
	f := newWDFixture(t)
	const hostile = `<b>&`
	f.upload("takeout-001.zip", lvTrip+hostile+".HEIC", "Trip", "m-s")
	f.upload("takeout-001.zip", lvTrip+hostile+".MP4", "Trip", "m-live")
	f.google["m-live"] = time.Date(2018, 7, 29, 18, 17, 0, 0, time.UTC)

	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{Format: "html"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`&lt;b&gt;&amp;.MP4`, `data-id="m-live"`, "photo-migrator-live-videos",
		`href="` + wdURL("m-live") + `"`, `rel="noopener"`, "Open in Google Photos",
		"Live Photo videos", "29 Jul 2018, 6:17 PM (GMT&#43;00:00)",
		"Open each video and delete it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("page missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `<b>&.MP4`) {
		t.Errorf("file name not escaped:\n%s", out)
	}
	rest := strings.ReplaceAll(out, wdURL("m-live"), "")
	if i := strings.Index(rest, "http"); i >= 0 {
		t.Errorf("page contains a URL other than the productUrl link: %q", rest[max(0, i-40):min(len(rest), i+60)])
	}
}

// Break caught: an empty list producing a table with no rows instead of a
// "nothing to remove" message.
func TestUploadedVideos_HTMLEmpty(t *testing.T) {
	f := newWDFixture(t)
	out, _, err := f.lvRun(context.Background(), uploadedVideosOpts{Format: "html"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No Live Photo videos to remove.") || strings.Contains(out, "<input") {
		t.Errorf("unexpected page:\n%s", out)
	}
}

// Break caught: --format html silently falling back to text for the plain
// held-back list, which cannot be a checklist.
func TestMotionVideosCmd_HTMLNeedsUploaded(t *testing.T) {
	err := execute(t, "motion-videos", "--format", "html")
	if err == nil || !strings.Contains(err.Error(), "--uploaded") {
		t.Fatalf("err = %v, want an error naming --uploaded", err)
	}
}
