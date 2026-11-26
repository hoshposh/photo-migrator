package takeout

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// SidecarIndex holds every per-file sidecar JSON found in a set of zips,
// by folder path inside the zip ("Takeout/Google Photos/Trip 2019").
type SidecarIndex struct {
	// folder -> json file name -> parsed sidecar
	folders map[string]map[string]*Sidecar
}

// BuildSidecarIndex reads the JSON entries of every zip without extracting
// them. A zip it cannot open is skipped and returned in failed, not an error.
// It stops with ctx.Err() when ctx is cancelled.
//
// Takeout can put a photo's sidecar in a different zip than the photo, so the
// index spans all zips. When two zips hold the same JSON path, the first in
// zipPaths order wins.
func BuildSidecarIndex(ctx context.Context, zipPaths []string) (ix *SidecarIndex, failed []string, err error) {
	ix = &SidecarIndex{folders: make(map[string]map[string]*Sidecar)}
	for _, zp := range zipPaths {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		zr, oerr := zip.OpenReader(zp)
		if oerr != nil {
			failed = append(failed, zp)
			continue
		}
		cerr := ix.addZip(ctx, &zr.Reader)
		_ = zr.Close()
		if cerr != nil {
			return nil, nil, cerr
		}
	}
	return ix, failed, nil
}

func (ix *SidecarIndex) addZip(ctx context.Context, zr *zip.Reader) error {
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(f.Name, ".json") || nonSidecarJSON[path.Base(f.Name)] {
			continue
		}
		folder, name := path.Dir(f.Name), path.Base(f.Name)
		if _, dup := ix.folders[folder][name]; dup {
			continue // first zip wins
		}
		s := parseZipSidecar(f)
		if s == nil {
			continue
		}
		if ix.folders[folder] == nil {
			ix.folders[folder] = make(map[string]*Sidecar)
		}
		ix.folders[folder][name] = s
	}
	return nil
}

// parseZipSidecar decodes one JSON entry, or returns nil if it can't be read.
func parseZipSidecar(f *zip.File) *Sidecar {
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	var s Sidecar
	if err := json.NewDecoder(rc).Decode(&s); err != nil {
		return nil
	}
	return &s
}

// Lookup finds the sidecar for a file by its path inside a zip, using the same
// strategies as FindSidecar. nil when none matches.
func (ix *SidecarIndex) Lookup(zipEntry string) *Sidecar {
	folder := ix.folders[path.Dir(zipEntry)]
	if len(folder) == 0 {
		return nil
	}
	_, s := findSidecar(path.Base(zipEntry),
		func(name string) (*Sidecar, error) {
			if s, ok := folder[name]; ok {
				return s, nil
			}
			return nil, fs.ErrNotExist
		},
		func() []string {
			names := make([]string, 0, len(folder))
			for n := range folder {
				names = append(names, n)
			}
			sort.Strings(names)
			return names
		})
	return s
}
