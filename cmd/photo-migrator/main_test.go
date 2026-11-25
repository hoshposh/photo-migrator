package main

import (
	"archive/zip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/hoshposh/photo-migrator/internal/store"
)

// execute runs the CLI with args in an isolated HOME and working directory so
// no real config file, OAuth token, or database is touched.
func execute(t *testing.T, args ...string) error {
	t.Helper()
	isolate(t)
	return runCLI(t, args...)
}

// isolate points HOME and the working directory at fresh temp dirs and
// returns the new HOME. Call it before runCLI when the test must place files
// (config, database) under HOME first.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	return home
}

// runCLI runs the CLI with fresh global state but without isolating the
// environment; pair it with isolate.
func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	viper.Reset()
	configPath = ""

	root := newRootCmd()
	root.SetArgs(args)
	root.SilenceUsage = true
	root.SilenceErrors = true
	return root.Execute()
}

// captureStdout returns everything fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()

	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	return string(<-done)
}

// writeTakeoutZip creates dir/name containing one media file in a year folder.
func writeTakeoutZip(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("Takeout/Google Photos/Photos from 2020/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("not really a jpeg")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func pendingZips(t *testing.T, dbPath string) []string {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	zips, err := st.GetPendingZips()
	if err != nil {
		t.Fatal(err)
	}
	return zips
}

// Break caught: `scan` reading takeout-dir from another command's flag.
func TestScan_UsesItsOwnTakeoutDirFlag(t *testing.T) {
	takeoutDir := t.TempDir()
	zipPath := writeTakeoutZip(t, takeoutDir, "takeout-001.zip")
	dbPath := filepath.Join(t.TempDir(), "m.db")

	if err := execute(t, "scan", "--takeout-dir", takeoutDir, "--db-path", dbPath); err != nil {
		t.Fatalf("scan: %v", err)
	}

	got := pendingZips(t, dbPath)
	if len(got) != 1 || got[0] != zipPath {
		t.Fatalf("registered zips = %v, want [%s]", got, zipPath)
	}
}

// Break caught: `auth` ignoring --credentials and loading ./credentials.json.
func TestAuth_UsesItsOwnCredentialsFlag(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "my-creds.json")

	err := execute(t, "auth", "--credentials", missing)
	if err == nil {
		t.Fatal("expected an error for a missing credentials file")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error does not mention %s:\n%v", missing, err)
	}
}

// Break caught: `upload` ignoring --dry-run / --staging-dir / --restore-metadata.
func TestUpload_UsesItsOwnFlags(t *testing.T) {
	takeoutDir := t.TempDir()
	zipPath := writeTakeoutZip(t, takeoutDir, "takeout-001.zip")
	dbPath := filepath.Join(t.TempDir(), "m.db")
	stagingDir := filepath.Join(t.TempDir(), "staging")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterZip(zipPath); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	err = execute(t, "upload",
		"--db-path", dbPath,
		"--staging-dir", stagingDir,
		"--dry-run",
		"--restore-metadata=false",
	)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	if _, err := os.Stat(stagingDir); err != nil {
		t.Errorf("--staging-dir %s was not used: %v", stagingDir, err)
	}
	if _, err := os.Stat("_staging"); err == nil {
		t.Errorf("default ./_staging was created; --staging-dir was ignored")
	}
}

// Break caught: a dry run recording files as uploaded (so a later real run
// skips them), registering staging-path rows, or changing the zip's status.
func TestDryRun_DoesNotChangeLedger(t *testing.T) {
	takeoutDir := t.TempDir()
	zipPath := writeTakeoutZip(t, takeoutDir, "takeout-001.zip")
	dbPath := filepath.Join(t.TempDir(), "m.db")

	err := execute(t, "run",
		"--takeout-dir", takeoutDir,
		"--db-path", dbPath,
		"--staging-dir", filepath.Join(t.TempDir(), "staging"),
		"--dry-run",
		"--restore-metadata=false",
	)
	if err != nil {
		t.Fatalf("run --dry-run: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// No media rows at all: rows are keyed by staging path, so rows written
	// under a dry run's staging dir would never be completed by a real run
	// using a different one, leaving the zip permanently incomplete.
	var mediaRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM media_items`).Scan(&mediaRows); err != nil {
		t.Fatalf("query media_items: %v", err)
	}
	if mediaRows != 0 {
		t.Errorf("media_items rows after dry run = %d, want 0", mediaRows)
	}

	var zipStatus string
	var zipErr sql.NullString
	if err := db.QueryRow(`SELECT status, error_msg FROM zip_archives WHERE zip_path = ?`, zipPath).
		Scan(&zipStatus, &zipErr); err != nil {
		t.Fatalf("query zip_archives: %v", err)
	}
	if zipStatus != "pending" || zipErr.String != "" {
		t.Errorf("zip after dry run = (%q, %q), want (\"pending\", \"\")", zipStatus, zipErr.String)
	}
}

// Guard: a config-file value must still apply when the flag is not passed.
func TestScan_FallsBackToConfigFile(t *testing.T) {
	takeoutDir := t.TempDir()
	zipPath := writeTakeoutZip(t, takeoutDir, "takeout-001.zip")
	dbPath := filepath.Join(t.TempDir(), "m.db")
	cfg := filepath.Join(t.TempDir(), "config.toml")
	content := "takeout-dir = \"" + takeoutDir + "\"\ndb-path = \"" + dbPath + "\"\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := execute(t, "scan", "--config", cfg); err != nil {
		t.Fatalf("scan: %v", err)
	}

	got := pendingZips(t, dbPath)
	if len(got) != 1 || got[0] != zipPath {
		t.Fatalf("registered zips = %v, want [%s]", got, zipPath)
	}
}

// Guard: `config show` must still report built-in defaults for keys whose
// flags belong to other commands.
func TestConfigShow_ReportsDefaults(t *testing.T) {
	var runErr error
	out := captureStdout(t, func() { runErr = execute(t, "config", "show") })

	if runErr != nil {
		t.Fatalf("config show: %v", runErr)
	}
	for _, want := range []string{
		"workers        = 5 ",
		"rate-limit     = 4 ",
		"staging-dir    = ./_staging ",
		"restore-metadata = true ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config show output missing %q:\n%s", want, out)
		}
	}
}

// Break caught: `large-files` opening db-path without expanding "~", so a
// config value like "~/m.db" points at a literal "./~/m.db".
func TestLargeFiles_ExpandsTildeInDBPath(t *testing.T) {
	home := isolate(t)

	st, err := store.Open(filepath.Join(home, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	bigFile := "/staging/takeout-001/Takeout/Google Photos/Trip/big.mp4"
	if err := st.RegisterMedia(bigFile, "", "Trip"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMediaError(bigFile, "byte upload HTTP 413: Request Entity Too Large"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	cfgDir := filepath.Join(home, ".config", "photo-migrator")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("db-path = \"~/m.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() { runErr = runCLI(t, "large-files", "--format", "json") })

	if runErr != nil {
		t.Fatalf("large-files: %v", runErr)
	}
	if !strings.Contains(out, `"file": "big.mp4"`) {
		t.Errorf("large-files did not report big.mp4 from ~/m.db; output:\n%s", out)
	}
}
