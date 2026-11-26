package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// Break caught: files that already failed with HTTP 413 before the
// too_large status existed staying as retryable errors after an upgrade.
func TestMigration_ConvertsExisting413ErrorsToTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")

	// Build a database at schema version 1 holding an old-style 413 error,
	// a different error, and a completed item.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpTo(db, "migrations", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO media_items (source_path, status, error_message) VALUES
		('/s/big.3gp', 'error', 'byte upload: byte upload HTTP 413: {"message": "Uploaded photo must not have more than 209715200 bytes."}'),
		('/s/flaky.jpg', 'error', 'batchCreate: max retries exceeded: HTTP 429'),
		('/s/done.jpg', 'complete', NULL)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies later migrations): %v", err)
	}
	defer func() { _ = st.Close() }()

	want := map[string]string{"/s/big.3gp": "too_large", "/s/flaky.jpg": "error", "/s/done.jpg": "complete"}
	for p, w := range want {
		var got string
		if err := st.db.QueryRow(`SELECT status FROM media_items WHERE source_path = ?`, p).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s status = %q, want %q", p, got, w)
		}
	}
}

// Break caught: an upgraded ledger keeping both the old- and new-staging-path
// rows for one zip entry (so its zip can never complete), losing the better
// status when merging, or keeping macOS "._" junk rows that are never retried.
func TestMigration_IdentifiesZipItemsByEntryAndMergesDuplicates(t *testing.T) {
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
	if err := goose.UpTo(db, "migrations", 2); err != nil {
		t.Fatal(err)
	}
	const z = "/takeout/takeout-001.zip"
	const in = "/takeout-001/Takeout/Google Photos/Trip/"
	if _, err := db.Exec(`INSERT INTO zip_archives (zip_path) VALUES (?)`, z); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ path, zip, status string }{
		{"_staging" + in + "x.jpg", z, "complete"}, // uploaded under both paths
		{"/data/stage" + in + "x.jpg", z, "complete"},
		{"_staging" + in + "y.jpg", z, "error"}, // failed, later uploaded under the new path
		{"/data/stage" + in + "y.jpg", z, "complete"},
		{"_staging" + in + "z.jpg", z, "pending"}, // only the old path: keep, still pending
		{"_staging" + in + "._v.MP4", z, "error"}, // macOS AppleDouble junk
		{"/photos/w.jpg", "", "pending"},          // dir mode: untouched
	} {
		var zipArg any
		if r.zip != "" {
			zipArg = r.zip
		}
		if _, err := db.Exec(`INSERT INTO media_items (source_path, source_zip, status) VALUES (?, ?, ?)`, r.path, zipArg, r.status); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (applies later migrations): %v", err)
	}
	defer func() { _ = st.Close() }()

	got := map[string]string{}
	rows, err := st.db.Query(`SELECT COALESCE(zip_entry, source_path), status FROM media_items`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, s string
		if err := rows.Scan(&k, &s); err != nil {
			t.Fatal(err)
		}
		got[k] = s
	}
	want := map[string]string{
		"Takeout/Google Photos/Trip/x.jpg": "complete",
		"Takeout/Google Photos/Trip/y.jpg": "complete",
		"Takeout/Google Photos/Trip/z.jpg": "pending",
		"/photos/w.jpg":                    "pending",
	}
	if len(got) != len(want) {
		t.Errorf("rows after migration = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: status = %q, want %q (all rows: %v)", k, got[k], w, got)
		}
	}
}
