package takeout

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// LivePairs records stills (.heic/.jpg/.jpeg) by folder and stem, to tell
// which .mp4/.mov files are Live Photo / motion video halves. Entries are
// paths inside a zip ("Takeout/Google Photos/Trip/IMG_1.HEIC") or, in dir
// mode, slash-separated paths relative to the takeout directory. A nil
// *LivePairs knows no stills.
type LivePairs struct {
	stills map[string]struct{} // folder + "/" + stem
}

// NewLivePairs returns an empty index.
func NewLivePairs() *LivePairs {
	return &LivePairs{stills: make(map[string]struct{})}
}

func pairKey(entry string) (key, ext string) {
	ext = strings.ToLower(path.Ext(entry))
	stem := strings.TrimSuffix(path.Base(entry), path.Ext(entry))
	return path.Dir(entry) + "/" + stem, ext
}

// Add records entry if it is a still; anything else is ignored.
func (p *LivePairs) Add(entry string) {
	key, ext := pairKey(entry)
	switch ext {
	case ".heic", ".jpg", ".jpeg":
		p.stills[key] = struct{}{}
	}
}

// IsVideoHalf reports whether entry is an .mp4/.mov with a recorded still of
// the same folder and stem (compared exactly, the extension case aside).
func (p *LivePairs) IsVideoHalf(entry string) bool {
	if p == nil {
		return false
	}
	key, ext := pairKey(entry)
	if ext != ".mp4" && ext != ".mov" {
		return false
	}
	_, ok := p.stills[key]
	return ok
}

// AddZips records the stills of every zip, reading central directories only.
// A zip it cannot open is returned in failed, not as an error. It stops with
// ctx.Err() when ctx is cancelled.
func (p *LivePairs) AddZips(ctx context.Context, zips []string) (failed []string, err error) {
	for _, zp := range zips {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		zr, oerr := zip.OpenReader(zp)
		if oerr != nil {
			failed = append(failed, zp)
			continue
		}
		for _, f := range zr.File {
			if err := ctx.Err(); err != nil {
				_ = zr.Close()
				return nil, err
			}
			if !f.FileInfo().IsDir() {
				p.Add(f.Name)
			}
		}
		_ = zr.Close()
	}
	return failed, nil
}

// AddDir records the stills under dir (directory mode), by path relative to
// dir with forward slashes.
func (p *LivePairs) AddDir(dir string) error {
	err := filepath.WalkDir(dir, func(fp string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, fp)
		if rerr != nil {
			rel = fp
		}
		p.Add(filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", dir, err)
	}
	return nil
}
