// Package metadata provides utilities for restoring EXIF metadata from
// Google Takeout sidecar JSON files into media files before upload.
package metadata

import (
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"time"

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

	// Date/time: prefer photoTakenTime, fall back to creationTime.
	ts := sidecar.PhotoTakenTime
	if ts.Timestamp == "" || ts.Timestamp == "0" {
		ts = sidecar.CreationTime
	}
	if ts.Timestamp != "" && ts.Timestamp != "0" {
		epoch, err := strconv.ParseInt(ts.Timestamp, 10, 64)
		if err != nil {
			return fmt.Errorf("parse timestamp %q: %w", ts.Timestamp, err)
		}
		if epoch > 0 {
			t := time.Unix(epoch, 0).UTC()
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

	args = append(args, mediaPath)

	cmd := exec.Command("exiftool", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exiftool failed: %w: %s", err, string(output))
	}
	return nil
}
