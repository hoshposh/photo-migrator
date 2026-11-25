// Package takeout provides utilities for parsing Google Takeout archives.
// It handles the JSON sidecar metadata format and Google's notorious filename
// truncation quirk, where long filenames are truncated in the sidecar's name
// (e.g. "my_very_long_holiday_photo_name.jpg" →
// "my_very_long_holiday_photo_na.jp.json").
package takeout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EpochTime is a Google Takeout timestamp field.
type EpochTime struct {
	Timestamp string `json:"timestamp"`
	Formatted string `json:"formatted"`
}

// GeoData holds geographic coordinates from a sidecar.
type GeoData struct {
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	Altitude      float64 `json:"altitude"`
	LatitudeSpan  float64 `json:"latitudeSpan"`
	LongitudeSpan float64 `json:"longitudeSpan"`
}

// Sidecar represents the JSON metadata that Google Takeout places next to
// every media file.
type Sidecar struct {
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	PhotoTakenTime EpochTime `json:"photoTakenTime"`
	CreationTime   EpochTime `json:"creationTime"`
	GeoData        GeoData   `json:"geoData"`
	GeoDataExif    GeoData   `json:"geoDataExif"`
}

// mediaExtensions lists recognised media file extensions (lowercase).
var mediaExtensions = map[string]bool{
	// Images
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".heic": true,
	".heif": true,
	".gif":  true,
	".webp": true,
	// RAW formats
	".arw": true, // Sony
	".dng": true, // Adobe / Google Pixel
	".cr2": true, // Canon
	".cr3": true, // Canon (newer)
	".nef": true, // Nikon
	".raf": true, // Fujifilm
	".orf": true, // Olympus
	".rw2": true, // Panasonic
	// Video
	".mp4": true,
	".mov": true,
	".avi": true,
	".m4v": true,
	".3gp": true,
	".mkv": true,
	".wmv": true,
}

// IsMediaFile returns true if path has a recognised media extension.
//
// Files whose basename starts with "._" are macOS AppleDouble resource-fork
// sidecars (e.g. "._SANY0079.MP4"). They share the extension of the real file
// but contain only Mac-specific metadata and must not be uploaded.
func IsMediaFile(path string) bool {
	if strings.HasPrefix(filepath.Base(path), "._") {
		return false
	}
	return mediaExtensions[strings.ToLower(filepath.Ext(path))]
}

// nonSidecarJSON lists JSON filenames that exist inside album folders but are
// NOT per-file sidecars and must be skipped during sidecar matching.
var nonSidecarJSON = map[string]bool{
	"metadata.json":                     true,
	"shared_album_comments.json":        true,
	"user-generated-memory-titles.json": true,
}

// FindSidecar locates the JSON sidecar for a media file and parses it.
//
// Google Takeout's actual sidecar naming (observed from real archives):
//
//  1. Primary:   <file>.supplemental-metadata.json  (short filenames)
//  2. Truncated: <file>.supplemental-metada.json     (common)
//  3. Truncated: <file>.supplemental-me.json         (longer filenames)
//  4. Legacy:    <file>.json                         (BURST / older format)
//
// All truncated variants share the property that the sidecar filename STARTS
// WITH the media filename followed by a dot. A single HasPrefix check covers
// all cases.
//
// Returns ("", nil, nil) when no sidecar exists — not treated as an error.
func FindSidecar(mediaPath string) (string, *Sidecar, error) {
	dir := filepath.Dir(mediaPath)
	base := filepath.Base(mediaPath)

	// Strategy 1: exact supplemental-metadata.json (most common for short names)
	if s, err := parseSidecar(filepath.Join(dir, base+".supplemental-metadata.json")); err == nil {
		return filepath.Join(dir, base+".supplemental-metadata.json"), s, nil
	}

	// Strategy 2: exact legacy .json (BURST photos, older exports)
	if s, err := parseSidecar(filepath.Join(dir, base+".json")); err == nil {
		return filepath.Join(dir, base+".json"), s, nil
	}

	// Read directory once for strategies 3 and 4.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, nil // can't read dir — not fatal
	}

	// Strategy 3: any .json sibling whose name starts with "<base>." —
	// this matches ALL truncated supplemental-metadata variants in one check:
	//   <base>.supplemental-metada.json
	//   <base>.supplemental-me.json
	//   <base>.supplement.json  etc.
	prefix := base + "."
	for _, e := range entries {
		if e.IsDir() || nonSidecarJSON[e.Name()] {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) {
			candidate := filepath.Join(dir, e.Name())
			if s, err := parseSidecar(candidate); err == nil {
				return candidate, s, nil
			}
		}
	}

	// Strategy 4: title-field fallback — parse every non-excluded .json in the
	// same directory and compare the "title" field to the media basename.
	for _, e := range entries {
		if e.IsDir() || nonSidecarJSON[e.Name()] || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		candidate := filepath.Join(dir, e.Name())
		if s, err := parseSidecar(candidate); err == nil && s.Title == base {
			return candidate, s, nil
		}
	}

	return "", nil, nil // no sidecar found
}

// parseSidecar opens and decodes a JSON sidecar file.
func parseSidecar(path string) (_ *Sidecar, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	var s Sidecar
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode sidecar %s: %w", path, err)
	}
	return &s, nil
}
