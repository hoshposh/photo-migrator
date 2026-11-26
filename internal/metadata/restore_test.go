package metadata_test

import (
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoshposh/photo-migrator/internal/metadata"
	"github.com/hoshposh/photo-migrator/internal/takeout"
)

func TestCheckExiftool(t *testing.T) {
	if err := metadata.CheckExiftool(); err != nil {
		t.Fatalf("CheckExiftool failed (is exiftool installed?): %v", err)
	}
}

func TestRestoreMetadata_NilSidecar(t *testing.T) {
	if err := metadata.RestoreMetadata("/dev/null", nil); err != nil {
		t.Fatalf("expected no error for nil sidecar, got: %v", err)
	}
}

func TestRestoreMetadata_EmptySidecar(t *testing.T) {
	tmp := createTestJPEG(t)
	sc := &takeout.Sidecar{}
	if err := metadata.RestoreMetadata(tmp, sc); err != nil {
		t.Fatalf("expected no error for empty sidecar, got: %v", err)
	}
}

func TestRestoreMetadata_DateTime(t *testing.T) {
	tmp := createTestJPEG(t)

	// 2024-03-15 14:30:00 UTC
	epoch := time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC).Unix()
	sc := &takeout.Sidecar{
		PhotoTakenTime: takeout.EpochTime{
			Timestamp: strconv.FormatInt(epoch, 10),
		},
	}

	if err := metadata.RestoreMetadata(tmp, sc); err != nil {
		t.Fatalf("RestoreMetadata failed: %v", err)
	}

	// Verify with exiftool.
	got := readExifJSON(t, tmp)
	want := "2024:03:15 14:30:00"
	if got["DateTimeOriginal"] != want {
		t.Errorf("DateTimeOriginal = %q, want %q", got["DateTimeOriginal"], want)
	}
	if got["CreateDate"] != want {
		t.Errorf("CreateDate = %q, want %q", got["CreateDate"], want)
	}
	if got["ModifyDate"] != want {
		t.Errorf("ModifyDate = %q, want %q", got["ModifyDate"], want)
	}
	// Verify UTC timezone offset is written (EXIF 2.31).
	if got["OffsetTimeOriginal"] != "+00:00" {
		t.Errorf("OffsetTimeOriginal = %q, want %q", got["OffsetTimeOriginal"], "+00:00")
	}
}

func TestRestoreMetadata_GPS(t *testing.T) {
	tmp := createTestJPEG(t)

	epoch := time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC).Unix()
	sc := &takeout.Sidecar{
		PhotoTakenTime: takeout.EpochTime{
			Timestamp: strconv.FormatInt(epoch, 10),
		},
		GeoData: takeout.GeoData{
			Latitude:  37.7749,
			Longitude: -122.4194,
			Altitude:  15.5,
		},
	}

	if err := metadata.RestoreMetadata(tmp, sc); err != nil {
		t.Fatalf("RestoreMetadata failed: %v", err)
	}

	got := readExifJSON(t, tmp)
	if got["GPSLatitudeRef"] != "North" {
		t.Errorf("GPSLatitudeRef = %q, want %q", got["GPSLatitudeRef"], "North")
	}
	if got["GPSLongitudeRef"] != "West" {
		t.Errorf("GPSLongitudeRef = %q, want %q", got["GPSLongitudeRef"], "West")
	}
}

func TestRestoreMetadata_FallbackCreationTime(t *testing.T) {
	tmp := createTestJPEG(t)

	epoch := time.Date(2023, 6, 1, 10, 0, 0, 0, time.UTC).Unix()
	sc := &takeout.Sidecar{
		// No PhotoTakenTime — should fall back to CreationTime.
		CreationTime: takeout.EpochTime{
			Timestamp: strconv.FormatInt(epoch, 10),
		},
	}

	if err := metadata.RestoreMetadata(tmp, sc); err != nil {
		t.Fatalf("RestoreMetadata failed: %v", err)
	}

	got := readExifJSON(t, tmp)
	want := "2023:06:01 10:00:00"
	if got["DateTimeOriginal"] != want {
		t.Errorf("DateTimeOriginal = %q, want %q", got["DateTimeOriginal"], want)
	}
}

func TestRestoreMetadata_GeoDataExifPreferred(t *testing.T) {
	tmp := createTestJPEG(t)

	epoch := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	sc := &takeout.Sidecar{
		PhotoTakenTime: takeout.EpochTime{
			Timestamp: strconv.FormatInt(epoch, 10),
		},
		GeoData: takeout.GeoData{
			Latitude:  10.0,
			Longitude: 20.0,
		},
		GeoDataExif: takeout.GeoData{
			Latitude:  -33.8688,
			Longitude: 151.2093,
		},
	}

	if err := metadata.RestoreMetadata(tmp, sc); err != nil {
		t.Fatalf("RestoreMetadata failed: %v", err)
	}

	got := readExifJSON(t, tmp)
	// Should use GeoDataExif (South, East) not GeoData (North, East).
	if got["GPSLatitudeRef"] != "South" {
		t.Errorf("GPSLatitudeRef = %q, want %q (should prefer geoDataExif)", got["GPSLatitudeRef"], "South")
	}
	if got["GPSLongitudeRef"] != "East" {
		t.Errorf("GPSLongitudeRef = %q, want %q", got["GPSLongitudeRef"], "East")
	}
}

// ── helpers ──────────────────────────────────────────────────

// createTestJPEG writes a minimal 1×1 JPEG to a temp file and returns its path.
func createTestJPEG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jpg")

	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp JPEG: %v", err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		_ = f.Close()
		t.Fatalf("encode JPEG: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close JPEG: %v", err)
	}
	return path
}

// readExifJSON runs exiftool -json on the file and returns a flat key→value map.
func readExifJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	out, err := exec.Command("exiftool", "-json", path).Output()
	if err != nil {
		t.Fatalf("exiftool -json: %v", err)
	}
	var results []map[string]any
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("unmarshal exiftool output: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("exiftool returned no results")
	}
	return results[0]
}

// Break caught: Takeout exports RAW photos that Google converted to JPEG
// (storage-saver accounts) under their original .ARW/.DNG/... names;
// exiftool refuses to write a "RAW" file whose content is JPEG, so the
// sidecar date and GPS were never restored for them.
func TestRestoreMetadata_JPEGWithRawExtension(t *testing.T) {
	jpg := createTestJPEG(t)
	arw := filepath.Join(filepath.Dir(jpg), "DSC01475.ARW")
	if err := os.Rename(jpg, arw); err != nil {
		t.Fatal(err)
	}

	epoch := time.Date(2017, 10, 13, 20, 1, 23, 0, time.UTC).Unix()
	sc := &takeout.Sidecar{PhotoTakenTime: takeout.EpochTime{Timestamp: strconv.FormatInt(epoch, 10)}}
	if err := metadata.RestoreMetadata(arw, sc); err != nil {
		t.Fatalf("RestoreMetadata on JPEG named .ARW: %v", err)
	}

	if got := readExifJSON(t, arw)["DateTimeOriginal"]; got != "2017:10:13 20:01:23" {
		t.Errorf("DateTimeOriginal = %v, want 2017:10:13 20:01:23", got)
	}
	entries, err := os.ReadDir(filepath.Dir(arw))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "DSC01475.ARW" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only DSC01475.ARW (no temp files left)", names)
	}
}

// Break caught: exiftool's multi-line output (with the full file path)
// flooding the log for every file it cannot update.
func TestRestoreMetadata_ErrorIsOneLine(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.jpg")
	if err := os.WriteFile(bad, []byte("not an image at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	sc := &takeout.Sidecar{PhotoTakenTime: takeout.EpochTime{Timestamp: strconv.FormatInt(epoch, 10)}}

	err := metadata.RestoreMetadata(bad, sc)
	if err == nil {
		t.Fatal("expected an error for a file exiftool cannot write")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error spans several lines:\n%v", err)
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("error repeats the full file path: %v", err)
	}
}
