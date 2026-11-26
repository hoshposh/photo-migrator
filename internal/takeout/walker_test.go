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
	_, err := WalkDirectory(ctx, dir, "",
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
	_, err := WalkDirectory(context.Background(), dir, "/t/takeout-001.zip",
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

// isoHeader returns the first bytes of an ISO base-media file (MP4/MOV/HEIC)
// with the given 4-character brand.
func isoHeader(brand string) []byte {
	return append([]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p'}, []byte(brand+"\x00\x00\x02\x00rest-of-file")...)
}

// Break caught: videos Takeout exports under unrecognised names being
// skipped silently (e.g. a video named "... 3.12.20"), motion-photo video
// halves (PXL_x.MP, PXL_x.MP.COVER) not being told apart from standalone
// videos, or non-video files with unknown extensions being uploaded.
func TestWalkDirectory_RecognisesVideosByContent(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"Photos from 2020/talk - 3.12.20":            isoHeader("isom"),
		"Photos from 2022/PXL_a.MP":                  isoHeader("isom"),
		"Photos from 2022/PXL_a.MP.jpg":              {0xFF, 0xD8, 0xFF, 0xE0},
		"Photos from 2022/PXL_b.MP~2":                isoHeader("mp42"),
		"Photos from 2024/PXL_c.RAW-01.MP.COVER":     isoHeader("mp42"),
		"Photos from 2024/PXL_c.RAW-01.MP.COVER.jpg": {0xFF, 0xD8, 0xFF, 0xE0},
		"Photos from 2019/old clip.20":               isoHeader("qt  "),
		"Photos from 2019/print-order.pdf":           []byte("%PDF-1.4"),
		"Photos from 2019/test.h264":                 {0, 0, 0, 1, 0x67, 0x64},
		"Photos from 2019/not-a-video.bin":           isoHeader("heic"),
	}
	for rel, b := range files {
		p := filepath.Join(dir, "Takeout", "Google Photos", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := map[string]MigrationJob{}
	_, err := WalkDirectory(context.Background(), dir, "/t/takeout-001.zip",
		func(string, string) (bool, error) { return false, nil },
		func(j MigrationJob) { got[filepath.Base(j.MediaPath)] = j },
		zap.NewNop(),
	)
	if err != nil {
		t.Fatal(err)
	}

	type want struct {
		motion     bool
		uploadName string
		mime       string
	}
	wants := map[string]want{
		"talk - 3.12.20":            {false, "talk - 3.12.20.mp4", "video/mp4"},
		"old clip.20":               {false, "old clip.20.mov", "video/quicktime"},
		"PXL_a.MP":                  {true, "", ""},
		"PXL_b.MP~2":                {true, "", ""},
		"PXL_c.RAW-01.MP.COVER":     {true, "", ""},
		"PXL_a.MP.jpg":              {false, "", ""},
		"PXL_c.RAW-01.MP.COVER.jpg": {false, "", ""},
	}
	for name, w := range wants {
		j, ok := got[name]
		if !ok {
			t.Errorf("%s was not emitted", name)
			continue
		}
		if j.MotionPart != w.motion || j.UploadName != w.uploadName || j.MimeType != w.mime {
			t.Errorf("%s: MotionPart=%v UploadName=%q MimeType=%q, want %v %q %q",
				name, j.MotionPart, j.UploadName, j.MimeType, w.motion, w.uploadName, w.mime)
		}
	}
	for _, name := range []string{"print-order.pdf", "test.h264", "not-a-video.bin"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s was emitted; it is not an uploadable video", name)
		}
	}
}

// Break caught: files being dropped for an unknown reason, or the same file
// classified differently by a run and by `scan --skipped`.
func TestClassify(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	cases := []struct {
		name string
		head []byte
		want Classification
	}{
		{"Trip/a.jpg", jpeg, Classification{}},
		{"Trip/a.jpg.supplemental-metadata.json", nil, Classification{Skip: SkipMetadata}},
		{"Trip/._a.jpg", jpeg, Classification{Skip: SkipAppleDouble}},
		{"Trip/print-order.pdf", []byte("%PDF-1.4"), Classification{Skip: SkipNotMedia}},
		{"Trip/test.h264", []byte{0, 0, 0, 1, 0x67, 0x64}, Classification{Skip: SkipNotMedia}},
		{"Trip/notes", []byte("hello"), Classification{Skip: SkipNotMedia}},
		{"Trip/PXL_a.MP", isoHeader("isom"), Classification{MotionPart: true}},
		{"Trip/talk - 3.12.20", isoHeader("isom"), Classification{UploadName: "talk - 3.12.20.mp4", MimeType: "video/mp4"}},
		{"Trip/still.bin", isoHeader("heic"), Classification{Skip: SkipNotMedia}},
	}
	for _, c := range cases {
		head := c.head
		got := Classify(c.name, func() []byte { return head })
		if got != c.want {
			t.Errorf("Classify(%q) = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Break caught: WalkDirectory not reporting what it skipped, so a run
// cannot summarise skipped files per zip.
func TestWalkDirectory_ReportsSkipped(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"Trip/a.jpg":                            {0xFF, 0xD8, 0xFF, 0xE0},
		"Trip/a.jpg.supplemental-metadata.json": []byte("{}"),
		"Trip/._a.jpg":                          {0, 5, 22, 7},
		"Trip/print-order.pdf":                  []byte("%PDF-1.4"),
		"Trip/receipt.pdf":                      []byte("%PDF-1.4"),
		"Trip/test.h264":                        {0, 0, 0, 1, 0x67},
	}
	for rel, b := range files {
		p := filepath.Join(dir, "Takeout", "Google Photos", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skipped, err := WalkDirectory(context.Background(), dir, "",
		func(string, string) (bool, error) { return false, nil },
		func(MigrationJob) {}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if got := skipped.Count(SkipNotMedia); got != 3 {
		t.Errorf("not-media skipped = %d, want 3", got)
	}
	if got := skipped.ByExt(SkipNotMedia); got != ".pdf ×2, .h264 ×1" {
		t.Errorf("not-media by extension = %q, want \".pdf ×2, .h264 ×1\"", got)
	}
	if got := skipped.Count(SkipAppleDouble); got != 1 {
		t.Errorf("AppleDouble skipped = %d, want 1", got)
	}
	if got := skipped.Count(SkipMetadata); got != 1 {
		t.Errorf("metadata skipped = %d, want 1", got)
	}
}

// Break caught: each Google Photos print order ("Print Order <number>",
// holding the ordered photos and a print-order.pdf receipt) becoming an
// album with a meaningless numeric name.
func TestAlbumFromPath_PrintOrderFolders(t *testing.T) {
	root := filepath.Join("/staging", "takeout-001")
	for rel, want := range map[string]string{
		"Takeout/Google Photos/Print Order 405124314898443163936/IMG_1.jpg": "",
		"Takeout/Google Photos/Print Order Ideas/IMG_2.jpg":                 "Print Order Ideas", // a real album name
	} {
		if got := albumFromPath(filepath.Join(root, filepath.FromSlash(rel)), root); got != want {
			t.Errorf("albumFromPath(%q) = %q, want %q", rel, got, want)
		}
	}
}
