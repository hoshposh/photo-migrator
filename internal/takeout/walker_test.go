package takeout

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func writeZip(t *testing.T, path string, names ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Break caught: extraction ignoring an interrupt and running to completion
// (minutes for a multi-GB Takeout zip), or leaving a partial staging dir.
func TestExtractZip_StopsWhenCancelled(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "takeout-001.zip")
	writeZip(t, zipPath, "Takeout/Google Photos/Trip/a.jpg", "Takeout/Google Photos/Trip/b.jpg")
	staging := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ExtractZip(ctx, zipPath, staging, zap.NewNop())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExtractZip error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "takeout-001")); !os.IsNotExist(err) {
		t.Errorf("partial staging dir left behind (stat err = %v)", err)
	}
}

// Break caught: the walk continuing (and registering files) after an interrupt.
func TestWalkDirectory_StopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	album := filepath.Join(dir, "Takeout", "Google Photos", "Trip")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(album, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	emitted := 0
	err := WalkDirectory(ctx, dir, "",
		func(string, string) (bool, error) { return false, nil },
		func(MigrationJob) { emitted++ },
		zap.NewNop(),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WalkDirectory error = %v, want context.Canceled", err)
	}
	if emitted != 0 {
		t.Errorf("emitted %d jobs after cancellation, want 0", emitted)
	}
}

// Break caught: a single large file (multi-GB videos are common in Takeout)
// being copied to the end after an interrupt.
func TestCtxReader_StopsMidCopyWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &ctxReader{ctx: ctx, r: strings.NewReader(strings.Repeat("x", 1<<20))}

	buf := make([]byte, 1024)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("read before cancel: %v", err)
	}
	cancel()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel error = %v, want context.Canceled", err)
	}
}
