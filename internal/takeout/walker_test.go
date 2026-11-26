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

// Break caught: Takeout's special folders becoming albums. "Archive" holds
// photos archived in Google Photos (a state the API cannot reproduce), and
// "Failed Videos" holds videos Google could not process; neither is a real
// album.
func TestAlbumFromPath(t *testing.T) {
	root := filepath.Join("/staging", "takeout-001")
	cases := []struct{ rel, want string }{
		{"Takeout/Google Photos/Trip to Maine/a.jpg", "Trip to Maine"},
		{"Takeout/Google Photos/Photos from 2019/a.jpg", ""},
		{"Takeout/Google Photos/Archive/a.jpg", ""},
		{"Takeout/Google Photos/Failed Videos/a.mp4", ""},
		{"Takeout/Google Photos/Locked Folder/a.jpg", ""},
		{"Takeout/Google Photos/a.jpg", ""},
	}
	for _, c := range cases {
		if got := albumFromPath(filepath.Join(root, filepath.FromSlash(c.rel)), root); got != c.want {
			t.Errorf("albumFromPath(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
}

// Break caught: the walker not flagging Locked Folder files, so the caller
// cannot hold them back.
func TestWalkDirectory_FlagsLockedFolderFiles(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"Takeout/Google Photos/Locked Folder/s.jpg", "Takeout/Google Photos/Trip/t.jpg"} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locked := map[string]bool{}
	err := WalkDirectory(context.Background(), dir, "/t/takeout-001.zip",
		func(string, string) (bool, error) { return false, nil },
		func(j MigrationJob) { locked[filepath.Base(j.MediaPath)] = j.Locked },
		zap.NewNop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !locked["s.jpg"] || locked["t.jpg"] {
		t.Errorf("Locked flags = %v, want s.jpg locked and t.jpg not", locked)
	}
}
