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
	"strconv"
	"strings"
	"time"
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
	name, s := findSidecar(filepath.Base(mediaPath),
		func(name string) (*Sidecar, error) { return parseSidecar(filepath.Join(dir, name)) },
		func() []string {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil // can't read dir — not fatal
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				if !e.IsDir() {
					names = append(names, e.Name())
				}
			}
			return names
		})
	if s == nil {
		return "", nil, nil // no sidecar found
	}
	return filepath.Join(dir, name), s, nil
}

// findSidecar applies FindSidecar's four strategies to one folder. parse opens
// a JSON file in that folder by name; list returns the folder's file names and
// is only called if strategies 1–2 miss.
func findSidecar(base string, parse func(name string) (*Sidecar, error), list func() []string) (name string, s *Sidecar) {
	// Strategy 1: exact supplemental-metadata.json (most common for short names)
	if s, err := parse(base + ".supplemental-metadata.json"); err == nil {
		return base + ".supplemental-metadata.json", s
	}

	// Strategy 2: exact legacy .json (BURST photos, older exports)
	if s, err := parse(base + ".json"); err == nil {
		return base + ".json", s
	}

	// List the folder once for strategies 3 and 4.
	names := list()

	// Strategy 3: any .json sibling whose name starts with "<base>." —
	// this matches ALL truncated supplemental-metadata variants in one check:
	//   <base>.supplemental-metada.json
	//   <base>.supplemental-me.json
	//   <base>.supplement.json  etc.
	prefix := base + "."
	for _, n := range names {
		if nonSidecarJSON[n] || !strings.HasSuffix(n, ".json") {
			continue
		}
		if strings.HasPrefix(n, prefix) {
			if s, err := parse(n); err == nil {
				return n, s
			}
		}
	}

	// Strategy 4: title-field fallback — parse every non-excluded .json in the
	// same directory and compare the "title" field to the media basename.
	for _, n := range names {
		if nonSidecarJSON[n] || !strings.HasSuffix(n, ".json") {
			continue
		}
		if s, err := parse(n); err == nil && s.Title == base {
			return n, s
		}
	}

	return "", nil
}

// TakenTime is the date the photo was taken: photoTakenTime, else creationTime,
// as UTC. ok is false when s is nil or neither holds a positive Unix time.
func (s *Sidecar) TakenTime() (t time.Time, ok bool) {
	if s == nil {
		return time.Time{}, false
	}
	ts := s.PhotoTakenTime.Timestamp
	if ts == "" || ts == "0" {
		ts = s.CreationTime.Timestamp
	}
	epoch, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || epoch <= 0 {
		return time.Time{}, false
	}
	return time.Unix(epoch, 0).UTC(), true
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
