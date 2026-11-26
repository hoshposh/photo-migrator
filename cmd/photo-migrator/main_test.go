package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/oauth2"

	"github.com/hoshposh/photo-migrator/internal/photos"
	"github.com/hoshposh/photo-migrator/internal/pipeline"
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
	if err := st.RegisterMedia(bigFile, "", "", "Trip"); err != nil {
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

// runWithOutput runs the CLI like main does (cobra's own usage/error
// printing left at defaults) with ctx as the command context, returning the
// combined output cobra printed and the returned error.
func runWithOutput(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	isolate(t)
	viper.Reset()
	configPath = ""

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&buf)
	root.SetErr(&buf)
	err := root.ExecuteContext(ctx)
	return buf.String(), err
}

// Break caught: an interrupted run being reported as a usage mistake.
func TestInterruptedRun_PrintsNoUsage(t *testing.T) {
	takeoutDir := t.TempDir()
	writeTakeoutZip(t, takeoutDir, "takeout-001.zip")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := runWithOutput(t, ctx, "run",
		"--takeout-dir", takeoutDir,
		"--db-path", filepath.Join(t.TempDir(), "m.db"),
		"--dry-run", "--restore-metadata=false",
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(out, "Usage:") || strings.Contains(out, "Error:") {
		t.Errorf("interrupt printed usage/error text:\n%s", out)
	}
}

// Guard: genuine command-line mistakes must still show usage.
func TestUnknownFlag_StillPrintsUsage(t *testing.T) {
	out, err := runWithOutput(t, context.Background(), "run", "--no-such-flag")
	if err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("unknown flag did not print usage:\n%s", out)
	}
}

// Break caught: wrong exit code or message for interrupts vs real errors.
func TestExitCode(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantOut  string
	}{
		{"success", nil, 0, ""},
		{"interrupted", fmt.Errorf("walk directory: %w", context.Canceled), 130, "Interrupted"},
		{"failure", errors.New("open database: boom"), 1, "Error: open database: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if got := exitCode(tc.err, &buf); got != tc.wantCode {
				t.Errorf("exitCode = %d, want %d", got, tc.wantCode)
			}
			if !strings.Contains(buf.String(), tc.wantOut) {
				t.Errorf("output %q does not contain %q", buf.String(), tc.wantOut)
			}
			if tc.err == nil && buf.Len() != 0 {
				t.Errorf("success printed %q", buf.String())
			}
		})
	}
}

// rewriteTransport sends every request to target instead of Google.
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

// Break caught: Ctrl+C aborting an upload already in progress and recording
// it (and its zip) as failed, or starting new uploads after the interrupt.
func TestProcessZips_InterruptFinishesInFlightUpload(t *testing.T) {
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var uploads atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			uploads.Add(1)
			started <- struct{}{}
			<-release
			_, _ = io.WriteString(w, "upload-token")
		case strings.HasSuffix(r.URL.Path, "/mediaItems:batchCreate"):
			_, _ = io.WriteString(w, `{"newMediaItemResults":[{"uploadToken":"upload-token","status":{},"mediaItem":{"id":"media-1"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)
	client := photos.New(&http.Client{Transport: rewriteTransport{target}})

	zipPath := filepath.Join(t.TempDir(), "takeout-001.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		w, _ := zw.Create("Takeout/Google Photos/Photos from 2020/" + n)
		_, _ = w.Write([]byte(n))
	}
	_ = zw.Close()
	_ = f.Close()

	dbPath := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RegisterZip(zipPath); err != nil {
		t.Fatal(err)
	}

	logger := zap.NewNop()
	pool := pipeline.NewPool(pipeline.Config{Workers: 1, RateLimit: 1000, Logger: logger}, client, st)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processZips(ctx, []string{zipPath}, t.TempDir(), st, pool, logger) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("upload never started")
	}
	cancel()       // Ctrl+C while the first upload is in flight
	close(release) // ...then let that upload finish

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("processZips did not return after interrupt")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("processZips error = %v, want context.Canceled", runErr)
	}
	if n := uploads.Load(); n != 1 {
		t.Errorf("uploads started = %d, want 1 (no new uploads after interrupt)", n)
	}

	stats, err := st.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.CompleteMedia != 1 {
		t.Errorf("complete media = %d, want 1 (in-flight upload should finish)", stats.CompleteMedia)
	}
	if stats.ErrorMedia != 0 {
		t.Errorf("error media = %d, want 0 (interrupt is not a failure)", stats.ErrorMedia)
	}
	var zipStatus string
	var zipErr sql.NullString
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.QueryRow(`SELECT status, error_msg FROM zip_archives`).Scan(&zipStatus, &zipErr); err != nil {
		t.Fatal(err)
	}
	if zipStatus != "pending" || zipErr.String != "" {
		t.Errorf("zip after interrupt = (%q, %q), want (\"pending\", \"\")", zipStatus, zipErr.String)
	}
}

// Break caught: a second Ctrl+C being swallowed, leaving no way to quit.
func TestWithSignals_SecondSignalQuitsImmediately(t *testing.T) {
	exited := make(chan int, 1)
	origExit := exitFunc
	exitFunc = func(code int) { exited <- code }
	defer func() { exitFunc = origExit }()

	ctx, stop := withSignals(context.Background())
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first SIGINT did not cancel the context")
	}
	select {
	case code := <-exited:
		t.Fatalf("first SIGINT exited immediately with %d; it should stop gracefully", code)
	case <-time.After(100 * time.Millisecond):
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exited:
		if code != 130 {
			t.Errorf("exit code = %d, want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second SIGINT did not quit")
	}
}

// Break caught: a rejected sign-in (invalid_grant) being retried for about a
// minute per file and then recorded as a failure for every file and zip,
// instead of stopping the run with a pointer to `photo-migrator auth`.
func TestProcessZips_StopsOnRejectedSignIn(t *testing.T) {
	var tokenCalls, apiCalls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
			return
		}
		apiCalls.Add(1)
		http.NotFound(w, r)
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)

	// A saved token that has expired and whose refresh Google rejects.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: rewriteTransport{target}})
	cfg := &oauth2.Config{ClientID: "id", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: "https://oauth2.example/token"}}
	expired := &oauth2.Token{AccessToken: "old", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Hour)}
	client := photos.New(cfg.Client(ctx, expired))

	takeoutDir := t.TempDir()
	zip1 := writeTakeoutZip(t, takeoutDir, "takeout-001.zip")
	zip2 := writeTakeoutZip(t, takeoutDir, "takeout-002.zip")
	dbPath := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, z := range []string{zip1, zip2} {
		if err := st.RegisterZip(z); err != nil {
			t.Fatal(err)
		}
	}

	logger := zap.NewNop()
	pool := pipeline.NewPool(pipeline.Config{Workers: 2, RateLimit: 1000, Logger: logger}, client, st)

	done := make(chan error, 1)
	go func() {
		done <- processZips(context.Background(), []string{zip1, zip2}, t.TempDir(), st, pool, logger)
	}()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("run did not stop promptly on a rejected sign-in (retrying?)")
	}

	if !errors.Is(runErr, photos.ErrAuthFailed) {
		t.Fatalf("error = %v, want photos.ErrAuthFailed", runErr)
	}
	if !strings.Contains(runErr.Error(), "photo-migrator auth") {
		t.Errorf("error does not tell the user to run `photo-migrator auth`: %v", runErr)
	}
	if n := apiCalls.Load(); n != 0 {
		t.Errorf("Photos API called %d times without a valid sign-in", n)
	}

	stats, err := st.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.ErrorMedia != 0 {
		t.Errorf("error media = %d, want 0 (sign-in failure is not a file failure)", stats.ErrorMedia)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT zip_path, status, COALESCE(error_msg, '') FROM zip_archives`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, status, msg string
		if err := rows.Scan(&p, &status, &msg); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || msg != "" {
			t.Errorf("%s = (%q, %q), want (\"pending\", \"\")", filepath.Base(p), status, msg)
		}
	}
}

// Break caught: a file Google rejects as too large (HTTP 413) being
// re-uploaded on every run (minutes each for a >200 MB file) and keeping its
// zip from ever completing.
func TestProcessZips_TooLargeFileIsNotRetried(t *testing.T) {
	var bigAttempts atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			body, _ := io.ReadAll(r.Body)
			if string(body) == "TOO-BIG" {
				bigAttempts.Add(1)
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = io.WriteString(w, `{"code": 3, "message": "Uploaded photo must not have more than 209715200 bytes."}`)
				return
			}
			_, _ = io.WriteString(w, "upload-token")
		case strings.HasSuffix(r.URL.Path, "/mediaItems:batchCreate"):
			_, _ = io.WriteString(w, `{"newMediaItemResults":[{"uploadToken":"upload-token","status":{},"mediaItem":{"id":"media-1"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)
	client := photos.New(&http.Client{Transport: rewriteTransport{target}})

	zipPath := filepath.Join(t.TempDir(), "takeout-001.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range map[string]string{"small.jpg": "ok", "big.3gp": "TOO-BIG"} {
		w, _ := zw.Create("Takeout/Google Photos/Photos from 2011/" + name)
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()
	_ = f.Close()

	dbPath := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RegisterZip(zipPath); err != nil {
		t.Fatal(err)
	}
	logger := zap.NewNop()
	pool := pipeline.NewPool(pipeline.Config{Workers: 2, RateLimit: 1000, Logger: logger}, client, st)
	staging := t.TempDir()

	for run := 1; run <= 2; run++ {
		if err := processZips(context.Background(), []string{zipPath}, staging, st, pool, logger); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}

	if n := bigAttempts.Load(); n != 1 {
		t.Errorf("too-large file uploaded %d times across 2 runs, want 1", n)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var zipStatus string
	if err := db.QueryRow(`SELECT status FROM zip_archives`).Scan(&zipStatus); err != nil {
		t.Fatal(err)
	}
	if zipStatus != "complete" {
		t.Errorf("zip status = %q, want \"complete\" (too-large files are reported, not retried)", zipStatus)
	}
	large, err := st.GetLargeFileErrors()
	if err != nil {
		t.Fatal(err)
	}
	if len(large) != 1 || filepath.Base(large[0].SourcePath) != "big.3gp" {
		t.Errorf("large-files report = %+v, want just big.3gp", large)
	}
}

// Break caught: files identified by their staging path, so changing
// staging-dir between runs (as happened going from ./_staging to an absolute
// path) makes every already-uploaded file look new and uploads it again.
func TestProcessZips_StagingDirChangeDoesNotReupload(t *testing.T) {
	var uploads atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			uploads.Add(1)
			_, _ = io.WriteString(w, "upload-token")
		case strings.HasSuffix(r.URL.Path, "/mediaItems:batchCreate"):
			_, _ = io.WriteString(w, `{"newMediaItemResults":[{"uploadToken":"upload-token","status":{},"mediaItem":{"id":"media-1"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)
	client := photos.New(&http.Client{Transport: rewriteTransport{target}})

	zipPath := writeTakeoutZip(t, t.TempDir(), "takeout-001.zip")
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RegisterZip(zipPath); err != nil {
		t.Fatal(err)
	}
	logger := zap.NewNop()
	pool := pipeline.NewPool(pipeline.Config{Workers: 1, RateLimit: 1000, Logger: logger}, client, st)

	for _, staging := range []string{t.TempDir(), t.TempDir()} { // two different staging dirs
		if err := processZips(context.Background(), []string{zipPath}, staging, st, pool, logger); err != nil {
			t.Fatal(err)
		}
	}

	if n := uploads.Load(); n != 1 {
		t.Errorf("file uploaded %d times after a staging-dir change, want 1", n)
	}
	stats, err := st.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalMedia != 1 || stats.CompleteMedia != 1 {
		t.Errorf("media rows: total=%d complete=%d, want 1 and 1", stats.TotalMedia, stats.CompleteMedia)
	}
}

// Break caught: zap's production sampling silently dropping repeated per-file
// log lines (it keeps ~100 identical messages per second), which hides
// uploads and errors from the user's log during fast bursts.
func TestBuildLogger_DoesNotDropRepeatedLines(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	orig := os.Stderr
	os.Stderr = w
	viper.Reset()
	logger := buildLogger()
	for i := 0; i < 250; i++ {
		logger.Info("✓ uploaded", zap.Int("i", i))
	}
	_ = logger.Sync()
	os.Stderr = orig
	_ = w.Close()
	out := <-done

	if n := bytes.Count(out, []byte(`"msg":"✓ uploaded"`)); n != 250 {
		t.Errorf("logged %d of 250 lines; repeated messages are being sampled away", n)
	}
}

// writeZipEntries creates a zip at path with the given entry -> content.
func writeZipEntries(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// lockedFixture is a fake Photos API plus a ledger holding one zip with a
// Locked Folder file ("SECRET") and an ordinary photo.
type lockedFixture struct {
	secretUploads, albumsCreated atomic.Int32
	st                           *store.Store
	dbPath, zipPath, staging     string
	pool                         *pipeline.Pool
}

func newLockedFixture(t *testing.T) *lockedFixture {
	t.Helper()
	fx := &lockedFixture{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			body, _ := io.ReadAll(r.Body)
			if string(body) == "SECRET" {
				fx.secretUploads.Add(1)
			}
			_, _ = io.WriteString(w, "upload-token")
		case strings.HasSuffix(r.URL.Path, "/mediaItems:batchCreate"):
			_, _ = io.WriteString(w, `{"newMediaItemResults":[{"uploadToken":"upload-token","status":{},"mediaItem":{"id":"media-1"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/albums"):
			fx.albumsCreated.Add(1)
			_, _ = io.WriteString(w, `{"id":"album-1","title":"x"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)
	target, _ := url.Parse(fake.URL)
	client := photos.New(&http.Client{Transport: rewriteTransport{target}})

	fx.zipPath = filepath.Join(t.TempDir(), "takeout-001.zip")
	writeZipEntries(t, fx.zipPath, map[string]string{
		"Takeout/Google Photos/Locked Folder/secret.jpg": "SECRET",
		"Takeout/Google Photos/Photos from 2020/a.jpg":   "ok",
	})
	fx.dbPath = filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(fx.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.RegisterZip(fx.zipPath); err != nil {
		t.Fatal(err)
	}
	fx.st = st
	fx.staging = t.TempDir()
	logger := zap.NewNop()
	fx.pool = pipeline.NewPool(pipeline.Config{Workers: 2, RateLimit: 1000, Logger: logger}, client, st)
	return fx
}

func (fx *lockedFixture) run(t *testing.T) {
	t.Helper()
	if err := processZips(context.Background(), []string{fx.zipPath}, fx.staging, fx.st, fx.pool, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
}

func (fx *lockedFixture) query(t *testing.T, q string) string {
	t.Helper()
	db, err := sql.Open("sqlite", fx.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow(q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// Break caught: photos from Google Photos' private Locked Folder being
// uploaded into the destination's main library (and an album named
// "Locked Folder"), where they are visible on every device.
func TestProcessZips_LockedFolderIsHeldBack(t *testing.T) {
	fx := newLockedFixture(t)
	fx.run(t)
	fx.run(t)

	if n := fx.secretUploads.Load(); n != 0 {
		t.Errorf("Locked Folder file uploaded %d times, want 0", n)
	}
	if n := fx.albumsCreated.Load(); n != 0 {
		t.Errorf("%d albums created, want 0 (no \"Locked Folder\" album)", n)
	}
	if got := fx.query(t, `SELECT status FROM media_items WHERE zip_entry LIKE '%secret.jpg'`); got != "locked" {
		t.Errorf("secret.jpg status = %q, want \"locked\"", got)
	}
	if got := fx.query(t, `SELECT status FROM zip_archives`); got != "complete" {
		t.Errorf("zip status = %q, want \"complete\" (held-back files don't block it)", got)
	}
	held, err := fx.st.GetLockedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || filepath.Base(held[0].SourcePath) != "secret.jpg" {
		t.Errorf("GetLockedFiles = %+v, want just secret.jpg", held)
	}
}

// Break caught: a held-back file that a review has approved (status reset to
// pending, zip reopened) being held back again instead of uploaded.
func TestProcessZips_ApprovedLockedFileIsUploaded(t *testing.T) {
	fx := newLockedFixture(t)
	fx.run(t)

	db, err := sql.Open("sqlite", fx.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE media_items SET status = 'pending' WHERE zip_entry LIKE '%secret.jpg'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE zip_archives SET status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	fx.run(t)

	if n := fx.secretUploads.Load(); n != 1 {
		t.Errorf("approved file uploaded %d times, want 1", n)
	}
	if n := fx.albumsCreated.Load(); n != 0 {
		t.Errorf("%d albums created, want 0 (approved files go to the library root)", n)
	}
	if got := fx.query(t, `SELECT status FROM media_items WHERE zip_entry LIKE '%secret.jpg'`); got != "complete" {
		t.Errorf("secret.jpg status = %q, want \"complete\"", got)
	}
}

// Break caught: `locked-files` not reporting held-back files, leaving the
// user no way to find and handle them.
func TestLockedFilesCommand_ListsHeldFiles(t *testing.T) {
	home := isolate(t)
	dbPath := filepath.Join(home, "m.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO media_items (source_path, source_zip, zip_entry, status)
		VALUES ('/s/takeout-001/Takeout/Google Photos/Locked Folder/secret.jpg', '/t/takeout-001.zip',
		        'Takeout/Google Photos/Locked Folder/secret.jpg', 'locked')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var runErr error
	out := captureStdout(t, func() {
		runErr = runCLI(t, "locked-files", "--db-path", dbPath, "--format", "json")
	})
	if runErr != nil {
		t.Fatalf("locked-files: %v", runErr)
	}
	if !strings.Contains(out, `"file": "secret.jpg"`) || !strings.Contains(out, `"in_zip_path": "Takeout/Google Photos/Locked Folder/secret.jpg"`) {
		t.Errorf("locked-files output does not list secret.jpg:\n%s", out)
	}
}

// Break caught: standalone videos with unrecognised names never being
// uploaded (or uploaded without a video name/type), and motion-photo video
// halves being uploaded as separate videos instead of held back and listed.
func TestProcessZips_UnrecognisedVideos(t *testing.T) {
	video := func(marker string) string { return "\x00\x00\x00\x20ftypisom\x00\x00\x02\x00" + marker }
	var mu sync.Mutex
	uploads := map[string]int{}        // marker -> count
	contentType := map[string]string{} // marker -> X-Goog-Upload-Content-Type
	fileName := map[string]string{}    // upload token -> batchCreate fileName
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			body, _ := io.ReadAll(r.Body)
			marker := "other"
			for _, m := range []string{"STANDALONE", "MOTION", "STILL"} {
				if strings.Contains(string(body), m) {
					marker = m
				}
			}
			mu.Lock()
			uploads[marker]++
			contentType[marker] = r.Header.Get("X-Goog-Upload-Content-Type")
			mu.Unlock()
			_, _ = io.WriteString(w, "tok-"+marker)
		case strings.HasSuffix(r.URL.Path, "/mediaItems:batchCreate"):
			var req struct {
				NewMediaItems []struct {
					SimpleMediaItem struct{ UploadToken, FileName string } `json:"simpleMediaItem"`
				} `json:"newMediaItems"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			tok := req.NewMediaItems[0].SimpleMediaItem.UploadToken
			mu.Lock()
			fileName[tok] = req.NewMediaItems[0].SimpleMediaItem.FileName
			mu.Unlock()
			_, _ = io.WriteString(w, `{"newMediaItemResults":[{"uploadToken":"`+tok+`","status":{},"mediaItem":{"id":"m-`+tok+`"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)
	client := photos.New(&http.Client{Transport: rewriteTransport{target}})

	zipPath := filepath.Join(t.TempDir(), "takeout-001.zip")
	writeZipEntries(t, zipPath, map[string]string{
		"Takeout/Google Photos/Photos from 2020/talk - 3.12.20": video("STANDALONE"),
		"Takeout/Google Photos/Photos from 2022/PXL_a.MP":       video("MOTION"),
		"Takeout/Google Photos/Photos from 2022/PXL_a.MP.jpg":   "\xff\xd8\xff\xe0STILL",
	})
	dbPath := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RegisterZip(zipPath); err != nil {
		t.Fatal(err)
	}
	logger := zap.NewNop()
	pool := pipeline.NewPool(pipeline.Config{Workers: 2, RateLimit: 1000, Logger: logger}, client, st)
	staging := t.TempDir()
	for run := 0; run < 2; run++ {
		if err := processZips(context.Background(), []string{zipPath}, staging, st, pool, logger); err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if uploads["STANDALONE"] != 1 {
		t.Errorf("standalone video uploaded %d times, want 1", uploads["STANDALONE"])
	}
	if contentType["STANDALONE"] != "video/mp4" {
		t.Errorf("standalone video content type = %q, want video/mp4", contentType["STANDALONE"])
	}
	if fileName["tok-STANDALONE"] != "talk - 3.12.20.mp4" {
		t.Errorf("standalone video uploaded as %q, want \"talk - 3.12.20.mp4\"", fileName["tok-STANDALONE"])
	}
	if uploads["MOTION"] != 0 {
		t.Errorf("motion-photo video half uploaded %d times, want 0 (held back)", uploads["MOTION"])
	}
	if uploads["STILL"] != 1 {
		t.Errorf("motion photo still uploaded %d times, want 1", uploads["STILL"])
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var motionStatus, zipStatus string
	if err := db.QueryRow(`SELECT status FROM media_items WHERE zip_entry LIKE '%PXL_a.MP'`).Scan(&motionStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM zip_archives`).Scan(&zipStatus); err != nil {
		t.Fatal(err)
	}
	if motionStatus != "motion_video" || zipStatus != "complete" {
		t.Errorf("motion half status = %q, zip = %q; want \"motion_video\" and \"complete\"", motionStatus, zipStatus)
	}
	listed, err := st.GetMotionVideos()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || filepath.Base(listed[0].SourcePath) != "PXL_a.MP" {
		t.Errorf("GetMotionVideos = %+v, want just PXL_a.MP", listed)
	}
}

// Break caught: no way to find out, before or during a migration, which
// files in a Takeout will not be migrated (they leave no log line or row).
func TestScanSkipped_ReportsUnmigratedFiles(t *testing.T) {
	takeoutDir := t.TempDir()
	writeZipEntries(t, filepath.Join(takeoutDir, "takeout-001.zip"), map[string]string{
		"Takeout/Google Photos/Trip/a.jpg":                            "\xff\xd8\xff\xe0",
		"Takeout/Google Photos/Trip/a.jpg.supplemental-metadata.json": "{}",
		"Takeout/Google Photos/Trip/._a.jpg":                          "\x00\x05\x16\x07",
		"Takeout/Google Photos/Trip/print-order.pdf":                  "%PDF-1.4",
		"Takeout/Google Photos/Trip/PXL_a.MP":                         "\x00\x00\x00\x20ftypisom\x00\x00\x02\x00",
		"Takeout/Google Photos/Trip/talk - 3.12.20":                   "\x00\x00\x00\x20ftypisom\x00\x00\x02\x00",
	})
	dbPath := filepath.Join(t.TempDir(), "m.db")

	isolate(t)
	var runErr error
	out := captureStdout(t, func() {
		runErr = runCLI(t, "scan", "--skipped", "--takeout-dir", takeoutDir, "--db-path", dbPath, "--format", "json")
	})
	if runErr != nil {
		t.Fatalf("scan --skipped: %v", runErr)
	}

	var report struct {
		Summary map[string]int `json:"summary"`
		Skipped []struct {
			Zip, Entry, Reason string
		} `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, s := range report.Skipped {
		got[filepath.Base(s.Entry)] = s.Reason
	}
	want := map[string]string{
		"print-order.pdf": "not a recognised photo or video",
		"._a.jpg":         "macOS resource fork (._ file)",
	}
	if len(got) != len(want) {
		t.Errorf("skipped = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: reason %q, want %q", k, got[k], v)
		}
	}
	if report.Summary["metadata_json"] != 1 || report.Summary["recognised_media"] != 3 {
		t.Errorf("summary = %v, want metadata_json=1 and recognised_media=3", report.Summary)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("scan --skipped created or touched the database (stat err = %v); it must be read-only", err)
	}
}

// Break caught: a run skipping files without saying so in the log.
func TestProcessZips_LogsSkippedFilesPerZip(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "takeout-001.zip")
	writeZipEntries(t, zipPath, map[string]string{
		"Takeout/Google Photos/Trip/print-order.pdf": "%PDF-1.4",
		"Takeout/Google Photos/Trip/receipt.pdf":     "%PDF-1.4",
	})
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RegisterZip(zipPath); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	pool := pipeline.NewPool(pipeline.Config{Workers: 1, RateLimit: 1000, DryRun: true, Logger: logger}, nil, st)

	if err := processZips(context.Background(), []string{zipPath}, t.TempDir(), st, pool, logger); err != nil {
		t.Fatal(err)
	}

	found := logs.FilterMessageSnippet("not recognised as photos or videos").All()
	if len(found) != 1 {
		t.Fatalf("want 1 skipped-files log line, got %d: %v", len(found), logs.All())
	}
	f := found[0].ContextMap()
	if f["types"] != ".pdf ×2" || f["zip"] != "takeout-001.zip" {
		t.Errorf("log fields = %v, want types=\".pdf ×2\" zip=\"takeout-001.zip\"", f)
	}
}
