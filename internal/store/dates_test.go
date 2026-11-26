package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustExec(t *testing.T, st *Store, q string, args ...any) {
	t.Helper()
	if _, err := st.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func dateRowByPath(t *testing.T, st *Store, path string) DateRow {
	t.Helper()
	rows, err := st.GetDateRows()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.SourcePath == path {
			return r
		}
	}
	t.Fatalf("no date row for %s in %v", path, rows)
	return DateRow{}
}

// Break caught: the migration failing on, or resetting, a ledger that already
// holds uploaded files, so an upgrade loses progress or crashes.
func TestMigration00006_AddsDateColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpTo(db, "migrations", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO media_items (source_path, status, photos_media_id) VALUES ('/s/a.jpg', 'complete', 'm-1')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies later migrations): %v", err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.GetDateRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if !r.TakenTime.IsZero() || r.TakenSource != "" || !r.GoogleTime.IsZero() || !r.GoogleCheckedAt.IsZero() || r.GoogleMissing {
		t.Errorf("new columns not empty: %+v", r)
	}
	var status string
	if err := st.db.QueryRow(`SELECT status FROM media_items WHERE source_path = '/s/a.jpg'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Errorf("status = %q, want complete", status)
	}
}

// Break caught: the date check asking Google about files that were never
// uploaded (dry run, pending, too large, no ID), which would all be reported
// as missing.
func TestGetDateRows_OnlyRealUploads(t *testing.T) {
	st := openTestStore(t)
	mustExec(t, st, `INSERT INTO media_items (source_path, source_zip, zip_entry, album_name, status, photos_media_id) VALUES
		('/s/b.jpg', 'z2', 'b.jpg', 'Trip', 'complete', 'm-2'),
		('/s/a.jpg', 'z2', 'a.jpg', NULL, 'complete', 'm-1'),
		('/s/c.jpg', 'z1', 'c.jpg', NULL, 'complete', 'm-3'),
		('/s/dry.jpg', 'z1', 'dry.jpg', NULL, 'complete', 'dry-run'),
		('/s/empty.jpg', 'z1', 'empty.jpg', NULL, 'complete', ''),
		('/s/pend.jpg', 'z1', 'pend.jpg', NULL, 'pending', NULL),
		('/s/big.jpg', 'z1', 'big.jpg', NULL, 'too_large', NULL)`)

	rows, err := st.GetDateRows()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.SourcePath)
	}
	want := []string{"/s/c.jpg", "/s/a.jpg", "/s/b.jpg"} // by source_zip, then source_path
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if r := rows[2]; r.SourceZip != "z2" || r.ZipEntry != "b.jpg" || r.AlbumName != "Trip" || r.MediaID != "m-2" {
		t.Errorf("fields not filled: %+v", r)
	}
}

// Break caught: a failed sidecar lookup being stored as "not looked up yet",
// so every run would repeat the lookup, or a found date being lost.
func TestSetTakenDates(t *testing.T) {
	st := openTestStore(t)
	mustExec(t, st, `INSERT INTO media_items (source_path, status, photos_media_id) VALUES
		('a', 'complete', 'm-1'), ('b', 'complete', 'm-2')`)
	taken := time.Date(2019, 6, 14, 18, 2, 0, 0, time.UTC)
	if err := st.SetTakenDates([]TakenDate{{SourcePath: "a", Time: taken}, {SourcePath: "b"}}); err != nil {
		t.Fatal(err)
	}
	a := dateRowByPath(t, st, "a")
	if a.TakenSource != TakenFromSidecar || !a.TakenTime.Equal(taken) {
		t.Errorf("a = %q %v, want sidecar %v", a.TakenSource, a.TakenTime, taken)
	}
	b := dateRowByPath(t, st, "b")
	if b.TakenSource != TakenNone || !b.TakenTime.IsZero() {
		t.Errorf("b = %q %v, want none, zero", b.TakenSource, b.TakenTime)
	}
}

// Break caught: Google's date being stored only on one of several rows that
// share a media ID (identical files Google deduplicated), leaving the others
// looking unchecked.
func TestSetGoogleDates_AllRowsWithThatID(t *testing.T) {
	st := openTestStore(t)
	mustExec(t, st, `INSERT INTO media_items (source_path, status, photos_media_id) VALUES
		('a', 'complete', 'm-1'), ('b', 'complete', 'm-1'), ('c', 'complete', 'm-2')`)
	g := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	if err := st.SetGoogleDates([]GoogleDate{{MediaID: "m-1", Time: g}}, now); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "b"} {
		r := dateRowByPath(t, st, p)
		if !r.GoogleTime.Equal(g) || !r.GoogleCheckedAt.Equal(now) || r.GoogleMissing {
			t.Errorf("%s = %+v, want time %v checked %v", p, r, g, now)
		}
	}
	if r := dateRowByPath(t, st, "c"); !r.GoogleCheckedAt.IsZero() {
		t.Errorf("c was touched: %+v", r)
	}

	if err := st.SetGoogleDates([]GoogleDate{{MediaID: "m-1", Missing: true}}, now); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "b"} {
		r := dateRowByPath(t, st, p)
		if !r.GoogleMissing || !r.GoogleTime.IsZero() || !r.GoogleCheckedAt.Equal(now) {
			t.Errorf("%s = %+v, want missing with zero time", p, r)
		}
	}
}

// Break caught: a re-upload that gets a new media ID keeping the old ID's
// Google date (so the report judges the wrong photo), or a repeat completion
// with the same ID throwing away a date that is still valid.
func TestSetMediaComplete_NewIDClearsGoogleDate(t *testing.T) {
	st := openTestStore(t)
	mustExec(t, st, `INSERT INTO media_items (source_path, status, photos_media_id) VALUES ('a', 'complete', 'm-1')`)
	g := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	if err := st.SetGoogleDates([]GoogleDate{{MediaID: "m-1", Time: g}}, now); err != nil {
		t.Fatal(err)
	}

	if err := st.SetMediaComplete("a", "m-2"); err != nil {
		t.Fatal(err)
	}
	r := dateRowByPath(t, st, "a")
	if r.MediaID != "m-2" || !r.GoogleTime.IsZero() || !r.GoogleCheckedAt.IsZero() || r.GoogleMissing {
		t.Errorf("new ID kept the old Google date: %+v", r)
	}

	if err := st.SetGoogleDates([]GoogleDate{{MediaID: "m-2", Time: g}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMediaComplete("a", "m-2"); err != nil {
		t.Fatal(err)
	}
	r = dateRowByPath(t, st, "a")
	if !r.GoogleTime.Equal(g) || !r.GoogleCheckedAt.Equal(now) {
		t.Errorf("same ID lost the Google date: %+v", r)
	}
}

// Break caught: the media-ID index migration failing on a ledger that already
// holds date columns and rows, or leaving the date queries broken.
func TestMigration00007_MediaIDIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpTo(db, "migrations", 6); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO media_items (source_path, status, photos_media_id) VALUES ('/s/a.jpg', 'complete', 'm-1')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies later migrations): %v", err)
	}
	defer func() { _ = st.Close() }()

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_media_photos_media_id'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("index idx_media_photos_media_id not created")
	}
	rows, err := st.GetDateRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MediaID != "m-1" {
		t.Errorf("GetDateRows = %+v, want the one row", rows)
	}
}

// Break caught: a photo's link being stored on only one of the rows that share
// its media ID, or a later date update or "missing" answer (which carries no
// link) erasing a link that is still good.
func TestSetGoogleDates_URL(t *testing.T) {
	st := openTestStore(t)
	mustExec(t, st, `INSERT INTO media_items (source_path, status, photos_media_id) VALUES
		('a', 'complete', 'm-1'), ('b', 'complete', 'm-1'), ('c', 'complete', 'm-2')`)
	g := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	const link = "https://photos.google.com/lr/photo/abc"

	if err := st.SetGoogleDates([]GoogleDate{{MediaID: "m-1", Time: g, URL: link}}, now); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "b"} {
		if r := dateRowByPath(t, st, p); r.GoogleURL != link {
			t.Errorf("%s GoogleURL = %q, want %q", p, r.GoogleURL, link)
		}
	}
	if r := dateRowByPath(t, st, "c"); r.GoogleURL != "" {
		t.Errorf("c was touched: %q", r.GoogleURL)
	}

	if err := st.SetGoogleDates([]GoogleDate{{MediaID: "m-1", Time: g.Add(time.Hour)}}, now); err != nil {
		t.Fatal(err)
	}
	if r := dateRowByPath(t, st, "a"); r.GoogleURL != link || !r.GoogleTime.Equal(g.Add(time.Hour)) {
		t.Errorf("date update without URL: %+v, want link kept and date updated", r)
	}
	if err := st.SetGoogleDates([]GoogleDate{{MediaID: "m-1", Missing: true}}, now); err != nil {
		t.Fatal(err)
	}
	if r := dateRowByPath(t, st, "b"); r.GoogleURL != link || !r.GoogleMissing {
		t.Errorf("missing answer: %+v, want link kept", r)
	}
}

// Break caught: the google_url migration failing on a ledger that already
// holds uploaded files, or leaving existing rows with a made-up link.
func TestMigration00008_GoogleURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpTo(db, "migrations", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO media_items (source_path, status, photos_media_id) VALUES ('/s/a.jpg', 'complete', 'm-1')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies later migrations): %v", err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.GetDateRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MediaID != "m-1" || rows[0].GoogleURL != "" {
		t.Errorf("GetDateRows = %+v, want the one row with no URL", rows)
	}
}

// Break caught: storing a link on its own also marking the photo as checked
// (which would hide it from the next date fetch) or touching its date.
func TestSetGoogleURL_OnlyTheLink(t *testing.T) {
	st := openTestStore(t)
	mustExec(t, st, `INSERT INTO media_items (source_path, status, photos_media_id) VALUES
		('a', 'complete', 'm-1'), ('b', 'complete', 'm-1'), ('c', 'complete', 'm-2')`)
	if err := st.SetGoogleURL("m-1", "https://photos.google.com/lr/photo/x"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "b"} {
		r := dateRowByPath(t, st, p)
		if r.GoogleURL != "https://photos.google.com/lr/photo/x" || !r.GoogleCheckedAt.IsZero() || !r.GoogleTime.IsZero() || r.GoogleMissing {
			t.Errorf("%s = %+v, want only the link set", p, r)
		}
	}
	if r := dateRowByPath(t, st, "c"); r.GoogleURL != "" {
		t.Errorf("c was touched: %+v", r)
	}
}
