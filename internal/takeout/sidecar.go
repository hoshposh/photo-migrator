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
	"regexp"
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

// findSidecar finds the sidecar for media file base in one folder. parse
// opens a JSON file in that folder by name; list returns the folder's file
// names (it may return only the JSON names, as the zip index does) and is
// called at most once, only if the exact names (strategies 1–2) miss.
//
// After the original four strategies it tries the names Takeout gives
// sidecars in other cases: numbered duplicates, edited copies, Live Photo
// videos and names cut short. Each of those matches only when exactly one
// sidecar fits, because writing a wrong date into a file is worse than
// writing none.
func findSidecar(base string, parse func(name string) (*Sidecar, error), list func() []string) (name string, s *Sidecar) {
	f := &folderLookup{parse: parse, list: list}
	return f.find(base)
}

// numberedRe matches a numbered duplicate's name, "X(1).jpg": groups are the
// name before "(N)", N, and the extension.
var numberedRe = regexp.MustCompile(`^(.*)\((\d+)\)(\.[^.]+)$`)

// numberedJSONRe matches a numbered duplicate's sidecar, "….(1).json".
var numberedJSONRe = regexp.MustCompile(`^(.*)\((\d+)\)\.json$`)

// editedRe matches an edited copy, "IMG_x-edited.jpg" or "IMG_x-edited(1).jpg":
// groups are the original's name, an optional "(N)", and the extension.
var editedRe = regexp.MustCompile(`^(.*)-edited(\(\d+\))?(\.[^.]+)$`)

// liveVideoExts are the video halves of Live Photos and older motion photos;
// liveImageExts are the still images whose sidecar they share.
var (
	liveVideoExts = map[string]bool{".mp4": true, ".mov": true}
	liveImageExts = map[string]bool{"heic": true, "jpg": true, "jpeg": true}
)

// minCutShortPrefix is the shortest sidecar name (without ".json") that may
// match a longer media name by prefix. Takeout cuts names at about 47–51
// characters, so real cut-short names are far longer than this.
const minCutShortPrefix = 30

// folderLookup holds one folder's lookups, listing the folder once.
type folderLookup struct {
	parse  func(name string) (*Sidecar, error)
	list   func() []string
	jsons  []string
	listed bool
}

// jsonNames returns the folder's candidate sidecar names, in list order.
func (f *folderLookup) jsonNames() []string {
	if !f.listed {
		f.listed = true
		for _, n := range f.list() {
			if strings.HasSuffix(n, ".json") && !nonSidecarJSON[n] {
				f.jsons = append(f.jsons, n)
			}
		}
	}
	return f.jsons
}

// dupNumber returns the "(N)" number of a media name, or "".
func dupNumber(base string) string {
	if m := numberedRe.FindStringSubmatch(base); m != nil {
		return m[2]
	}
	return ""
}

// jsonDupNumber returns the "(N)" number of a sidecar name, or "".
func jsonDupNumber(name string) string {
	if m := numberedJSONRe.FindStringSubmatch(name); m != nil {
		return m[2]
	}
	return ""
}

func (f *folderLookup) find(base string) (string, *Sidecar) {
	// Strategy 1: exact supplemental-metadata.json (most common for short names)
	if s, err := f.parse(base + ".supplemental-metadata.json"); err == nil {
		return base + ".supplemental-metadata.json", s
	}
	// Strategy 2: exact legacy .json (BURST photos, older exports)
	if s, err := f.parse(base + ".json"); err == nil {
		return base + ".json", s
	}

	// A numbered duplicate's sidecar ("….(1).json") belongs only to the media
	// file with the same number: X.jpg must never take X(1).jpg's sidecar.
	n := dupNumber(base)
	sameNumber := func(json string) bool {
		m := jsonDupNumber(json)
		return m == "" || m == n
	}

	// Strategy 3: any sidecar whose name starts with "<base>." — this matches
	// the cut-down variants (<base>.supplemental-metada.json, …-me.json, …).
	prefix := base + "."
	for _, j := range f.jsonNames() {
		if strings.HasPrefix(j, prefix) && sameNumber(j) {
			if s, err := f.parse(j); err == nil {
				return j, s
			}
		}
	}

	// Strategy 4: a sidecar whose "title" field is the media name.
	for _, j := range f.jsonNames() {
		if !sameNumber(j) {
			continue
		}
		if s, err := f.parse(j); err == nil && s.Title == base {
			return j, s
		}
	}

	if j, s := f.numberedDuplicate(base); s != nil {
		return j, s
	}
	if j, s := f.editedCopy(base); s != nil {
		return j, s
	}
	if j, s := f.liveVideo(base); s != nil {
		return j, s
	}
	return f.cutShort(base)
}

// numberedDuplicate: "X(1).jpg" uses "X.jpg.<anything>(1).json".
func (f *folderLookup) numberedDuplicate(base string) (string, *Sidecar) {
	m := numberedRe.FindStringSubmatch(base)
	if m == nil {
		return "", nil
	}
	orig, suffix := m[1]+m[3]+".", "("+m[2]+").json"
	return f.onlyOne(func(j string) bool {
		return strings.HasPrefix(j, orig) && strings.HasSuffix(j, suffix)
	})
}

// editedCopy: "IMG_x-edited.jpg" uses the original "IMG_x.jpg"'s sidecar.
func (f *folderLookup) editedCopy(base string) (string, *Sidecar) {
	m := editedRe.FindStringSubmatch(base)
	if m == nil {
		return "", nil
	}
	return f.find(m[1] + m[2] + m[3])
}

// liveVideo: "IMG_7297.MP4" uses the sidecar of the still image with the same
// stem ("IMG_7297.HEIC"), found from the sidecar names alone. Exactly one
// such image must have a sidecar.
func (f *folderLookup) liveVideo(base string) (string, *Sidecar) {
	ext := filepath.Ext(base)
	if !liveVideoExts[strings.ToLower(ext)] {
		return "", nil
	}
	stem := strings.TrimSuffix(base, ext) + "."
	images := map[string]bool{}
	for _, j := range f.jsonNames() {
		if rest, ok := strings.CutPrefix(j, stem); ok {
			if imgExt, _, ok := strings.Cut(rest, "."); ok && liveImageExts[strings.ToLower(imgExt)] {
				images[stem+imgExt] = true
			}
		}
	}
	var foundName string
	var found *Sidecar
	for img := range images {
		j, s := f.find(img)
		if s == nil {
			continue
		}
		if found != nil && j != foundName {
			return "", nil // two images with sidecars: ambiguous
		}
		foundName, found = j, s
	}
	return foundName, found
}

// cutShort: Takeout cuts long names short, in the media name and (shorter
// still) in the sidecar's name, but keeps the full original name in the
// sidecar's title. A sidecar matches when its name (without ".json" and a
// matching "(N)") is a long enough prefix of the media name and its title
// starts with the media name's stem. Exactly one must match.
func (f *folderLookup) cutShort(base string) (string, *Sidecar) {
	n, name := "", base
	if m := numberedRe.FindStringSubmatch(base); m != nil {
		n, name = m[2], m[1]+m[3]
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	var foundName string
	var found *Sidecar
	for _, j := range f.jsonNames() {
		js := strings.TrimSuffix(j, ".json")
		if m := numberedJSONRe.FindStringSubmatch(j); m != nil {
			if m[2] != n {
				continue
			}
			js = m[1]
		} else if n != "" {
			continue
		}
		if len(js) < minCutShortPrefix || !strings.HasPrefix(name, js) {
			continue
		}
		s, err := f.parse(j)
		if err != nil || !strings.HasPrefix(s.Title, stem) {
			continue
		}
		if found != nil {
			return "", nil // more than one fits: ambiguous
		}
		foundName, found = j, s
	}
	return foundName, found
}

// onlyOne returns the single sidecar whose name satisfies match and parses,
// or nothing when none or several do.
func (f *folderLookup) onlyOne(match func(json string) bool) (string, *Sidecar) {
	var foundName string
	var found *Sidecar
	for _, j := range f.jsonNames() {
		if !match(j) {
			continue
		}
		s, err := f.parse(j)
		if err != nil {
			continue
		}
		if found != nil {
			return "", nil
		}
		foundName, found = j, s
	}
	return foundName, found
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
