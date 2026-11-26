// Package metadata provides utilities for restoring EXIF metadata from
// Google Takeout sidecar JSON files into media files before upload.
package metadata

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hoshposh/photo-migrator/internal/takeout"
)

// CheckExiftool verifies that the exiftool binary is available on the system
// PATH. It should be called at startup when restore-metadata is enabled.
func CheckExiftool() error {
	_, err := exec.LookPath("exiftool")
	if err != nil {
		return fmt.Errorf("exiftool not found in PATH: %w\n\n"+
			"  Install it with: sudo apt install libimage-exiftool-perl\n"+
			"  Or disable metadata restoration: restore-metadata = false", err)
	}
	return nil
}

// RestoreMetadata writes the sidecar's date/time and GPS coordinates into
// the media file's embedded metadata using exiftool. It operates in-place
// (overwrites the original file with -overwrite_original).
//
// It is a no-op when:
//   - sidecar is nil
//   - sidecar has no photoTakenTime/creationTime and no GPS coordinates
//
// Errors are returned but callers may choose to log and continue (best-effort).
func RestoreMetadata(mediaPath string, sidecar *takeout.Sidecar) error {
	if sidecar == nil {
		return nil
	}

	args := []string{"-overwrite_original"}

	// Date/time: photoTakenTime, falling back to creationTime.
	if t, ok := sidecar.TakenTime(); ok {
		exifDate := t.Format("2006:01:02 15:04:05")
		args = append(args,
			"-DateTimeOriginal="+exifDate,
			"-CreateDate="+exifDate,
			"-ModifyDate="+exifDate,
			// EXIF 2.31 timezone offset tags — marks dates as UTC so
			// Google Photos won't misinterpret them as local time.
			"-OffsetTimeOriginal=+00:00",
			"-OffsetTimeDigitized=+00:00",
			"-OffsetTime=+00:00",
		)
	}

	// GPS: prefer geoDataExif, fall back to geoData.
	geo := sidecar.GeoDataExif
	if geo.Latitude == 0 && geo.Longitude == 0 {
		geo = sidecar.GeoData
	}
	if geo.Latitude != 0 || geo.Longitude != 0 {
		lat := geo.Latitude
		lon := geo.Longitude

		latRef := "N"
		if lat < 0 {
			latRef = "S"
			lat = math.Abs(lat)
		}
		lonRef := "E"
		if lon < 0 {
			lonRef = "W"
			lon = math.Abs(lon)
		}

		args = append(args,
			fmt.Sprintf("-GPSLatitude=%.10f", lat),
			"-GPSLatitudeRef="+latRef,
			fmt.Sprintf("-GPSLongitude=%.10f", lon),
			"-GPSLongitudeRef="+lonRef,
		)

		if geo.Altitude != 0 {
			altRef := "0" // 0 = above sea level
			alt := geo.Altitude
			if alt < 0 {
				altRef = "1" // 1 = below sea level
				alt = math.Abs(alt)
			}
			args = append(args,
				fmt.Sprintf("-GPSAltitude=%.2f", alt),
				"-GPSAltitudeRef="+altRef,
			)
		}
	}

	// Nothing to write — all sidecar fields were empty/zero.
	if len(args) == 1 {
		return nil
	}

	target, commit, cleanup, err := writableTarget(mediaPath)
	if err != nil {
		return err
	}
	defer cleanup()

	// -overwrite_original (not _in_place) makes exiftool write a new file and
	// rename it over target; when target is a hard link, the original file is
	// left untouched until commit renames the result over it.
	cmd := exec.Command("exiftool", append(args, target)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exiftool: %s", firstLine(output, target, err))
	}
	return commit()
}

// writableTarget returns the path exiftool should write. Normally that is
// mediaPath itself. But Takeout exports RAW photos that Google converted to
// JPEG (storage-saver accounts) under their original .ARW/.DNG/... names, and
// exiftool refuses to write a "RAW" file whose content is JPEG. For those it
// returns a temporary hard link (or copy) with a .jpg name; commit renames
// the updated file back over mediaPath, and cleanup removes any leftover.
func writableTarget(mediaPath string) (target string, commit func() error, cleanup func(), err error) {
	noop := func() error { return nil }
	ext := strings.ToLower(filepath.Ext(mediaPath))
	if ext == ".jpg" || ext == ".jpeg" || !isJPEG(mediaPath) {
		return mediaPath, noop, func() {}, nil
	}
	tmp := mediaPath + ".photo-migrator.jpg"
	_ = os.Remove(tmp) // stale leftover from an interrupted run
	if err := os.Link(mediaPath, tmp); err != nil {
		if err := copyFile(mediaPath, tmp); err != nil { // e.g. exFAT: no hard links
			return "", nil, nil, fmt.Errorf("prepare %s for exiftool: %w", filepath.Base(mediaPath), err)
		}
	}
	return tmp, func() error { return os.Rename(tmp, mediaPath) }, func() { _ = os.Remove(tmp) }, nil
}

// isJPEG reports whether the file starts with the JPEG signature FF D8 FF.
func isJPEG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var b [3]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	return b == [3]byte{0xFF, 0xD8, 0xFF}
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// firstLine condenses exiftool's output to one message, without the file
// path it appends, so a failure is one readable log line. It reports the
// first "Error:" line when there is one: exiftool often prints a harmless
// warning (e.g. "[minor] ... Fixed.") before the error that stopped it.
func firstLine(output []byte, path string, runErr error) string {
	clean := func(line string) string {
		return strings.TrimSpace(strings.TrimSuffix(line, " - "+path))
	}
	first := ""
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Error:") {
			return clean(line)
		}
		if first == "" {
			first = clean(line)
		}
	}
	if first != "" {
		return first
	}
	return runErr.Error()
}
