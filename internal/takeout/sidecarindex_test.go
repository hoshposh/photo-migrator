package takeout

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeZipFiles writes a zip holding name -> content entries.
func writeZipFiles(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
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

const sidecarJSON1560535320 = `{"title":"a.jpg","photoTakenTime":{"timestamp":"1560535320"}}`

func TestSidecarIndex_SidecarInOtherZip(t *testing.T) {
	dir := t.TempDir()
	z1 := filepath.Join(dir, "takeout-1.zip")
	z2 := filepath.Join(dir, "takeout-2.zip")
	writeZipFiles(t, z1, map[string]string{"Takeout/Google Photos/Trip/a.jpg": "x"})
	writeZipFiles(t, z2, map[string]string{
		"Takeout/Google Photos/Trip/a.jpg.supplemental-metadata.json": sidecarJSON1560535320,
	})

	ix, failed, err := BuildSidecarIndex(context.Background(), []string{z1, z2})
	if err != nil || len(failed) != 0 {
		t.Fatalf("BuildSidecarIndex: failed=%v err=%v", failed, err)
	}
	s := ix.Lookup("Takeout/Google Photos/Trip/a.jpg")
	got, ok := s.TakenTime()
	if !ok || !got.Equal(time.Date(2019, 6, 14, 18, 2, 0, 0, time.UTC)) {
		t.Errorf("TakenTime = %v, %v; want 2019-06-14 18:02:00 UTC", got, ok)
	}
}

func TestSidecarIndex_Strategies(t *testing.T) {
	dir := t.TempDir()
	z := filepath.Join(dir, "takeout-1.zip")
	const f = "Takeout/Google Photos/Trip/"
	writeZipFiles(t, z, map[string]string{
		f + "long-name.jpg.supplemental-me.json": `{"title":"long-name.jpg"}`,
		f + "b.jpg.json":                         `{"title":"b.jpg"}`,
		f + "x.json":                             `{"title":"c.jpg"}`,
		f + "metadata.json":                      `{"title":"d.jpg"}`,
		"Takeout/Google Photos/Other/e.jpg.json": `{"title":"e.jpg"}`,
	})
	ix, failed, err := BuildSidecarIndex(context.Background(), []string{z})
	if err != nil || len(failed) != 0 {
		t.Fatalf("BuildSidecarIndex: failed=%v err=%v", failed, err)
	}

	for name, title := range map[string]string{
		"long-name.jpg": "long-name.jpg", // truncated supplemental name
		"b.jpg":         "b.jpg",         // legacy <file>.json
		"c.jpg":         "c.jpg",         // title fallback
	} {
		s := ix.Lookup(f + name)
		if s == nil || s.Title != title {
			t.Errorf("Lookup(%q) = %+v, want sidecar titled %q", name, s, title)
		}
	}
	if s := ix.Lookup(f + "d.jpg"); s != nil {
		t.Errorf("Lookup(d.jpg) matched metadata.json: %+v", s)
	}
	// e.jpg's sidecar lives in another folder: lookups are per folder.
	if s := ix.Lookup(f + "e.jpg"); s != nil {
		t.Errorf("Lookup(e.jpg) matched a sidecar from another folder: %+v", s)
	}
	if s := ix.Lookup("Takeout/Google Photos/Other/e.jpg"); s == nil {
		t.Error("Lookup in the sidecar's own folder found nothing")
	}
}

func TestSidecarIndex_FirstZipWinsOnDuplicatePath(t *testing.T) {
	dir := t.TempDir()
	z1 := filepath.Join(dir, "a.zip")
	z2 := filepath.Join(dir, "b.zip")
	const p = "Takeout/Google Photos/Trip/a.jpg.json"
	writeZipFiles(t, z1, map[string]string{p: `{"title":"first"}`})
	writeZipFiles(t, z2, map[string]string{p: `{"title":"second"}`})
	ix, _, err := BuildSidecarIndex(context.Background(), []string{z1, z2})
	if err != nil {
		t.Fatal(err)
	}
	// Lookup resolves via the .json name, so title "first" identifies the winner.
	if s := ix.Lookup("Takeout/Google Photos/Trip/a.jpg"); s == nil || s.Title != "first" {
		t.Errorf("Lookup = %+v, want the first zip's sidecar", s)
	}
}

func TestSidecarIndex_UnreadableZipReported(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.zip")
	bad := filepath.Join(dir, "bad.zip")
	writeZipFiles(t, good, map[string]string{
		"Takeout/Google Photos/Trip/a.jpg.json": sidecarJSON1560535320,
	})
	if err := os.WriteFile(bad, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}

	ix, failed, err := BuildSidecarIndex(context.Background(), []string{bad, good})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(failed) != 1 || failed[0] != bad {
		t.Errorf("failed = %v, want [%s]", failed, bad)
	}
	if ix.Lookup("Takeout/Google Photos/Trip/a.jpg") == nil {
		t.Error("lookup in the valid zip found nothing")
	}
}

func TestSidecarIndex_StopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	z := filepath.Join(dir, "a.zip")
	writeZipFiles(t, z, map[string]string{"Takeout/Google Photos/Trip/a.jpg.json": `{}`})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := BuildSidecarIndex(ctx, []string{z}); err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestSidecarIndex_TakeoutNames(t *testing.T) {
	for _, g := range takeoutNameCases {
		t.Run(g.name, func(t *testing.T) {
			const folder = "Takeout/Google Photos/Photos from 2019/"
			files := map[string]string{}
			for name, title := range g.sidecars {
				files[folder+name] = takeoutNameJSON(name, title)
			}
			for media := range g.want {
				files[folder+media] = "x"
			}
			zp := filepath.Join(t.TempDir(), "takeout-001.zip")
			writeZipFiles(t, zp, files)
			ix, failed, err := BuildSidecarIndex(context.Background(), []string{zp})
			if err != nil || len(failed) != 0 {
				t.Fatalf("BuildSidecarIndex: failed=%v err=%v", failed, err)
			}
			for media, wantFile := range g.want {
				got := ""
				if s := ix.Lookup(folder + media); s != nil {
					got = s.Description
				}
				if got != wantFile {
					t.Errorf("Lookup(%s) = %q; want %q", media, got, wantFile)
				}
			}
		})
	}
}
