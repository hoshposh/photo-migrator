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
