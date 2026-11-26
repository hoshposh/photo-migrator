package takeout

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// yearFolderRe matches Google Takeout year-based folders, e.g. "Photos from 2016".
var yearFolderRe = regexp.MustCompile(`^Photos from \d{4}$`)

// MigrationJob is a single media file ready for upload.
type MigrationJob struct {
	// MediaPath is the absolute path to the media file on disk.
	MediaPath string
	// SidecarPath is the path to the JSON sidecar (empty if not found).
	SidecarPath string
	// AlbumName is the named album this file belongs to.
	// Empty means the file should be uploaded to the library root.
	AlbumName string
	// SourceZip is the originating zip archive path (empty in dir mode).
	SourceZip string
	// Sidecar holds the parsed metadata (nil if not found).
	Sidecar *Sidecar
}

// IsZipMode returns true if dir contains at least one .zip file.
func IsZipMode(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.ToLower(filepath.Ext(e.Name())) == ".zip" {
			return true, nil
		}
	}
	return false, nil
}

// ListZips returns the absolute paths of all .zip files in dir, sorted
// alphabetically so processing order is deterministic.
func ListZips(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var zips []string
	for _, e := range entries {
		if !e.IsDir() && strings.ToLower(filepath.Ext(e.Name())) == ".zip" {
			zips = append(zips, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(zips)
	return zips, nil
}

// ExtractZip extracts zipPath into <stagingDir>/<zip-stem>/ and returns the
// extraction root. It guards against zip-slip attacks and creates all needed
// parent directories. If ctx is cancelled it stops, removes the partial
// extraction, and returns ctx.Err().
func ExtractZip(ctx context.Context, zipPath, stagingDir string, logger *zap.Logger) (string, error) {
	stem := strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))
	destRoot := filepath.Join(stagingDir, stem)

	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return "", fmt.Errorf("mkdir staging %s: %w", destRoot, err)
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer func() { _ = r.Close() }()

	cleanRoot := filepath.Clean(destRoot) + string(os.PathSeparator)

	for _, f := range r.File {
		// Stop promptly on interrupt; a partial extraction is useless, so
		// remove it (the zip is simply re-extracted on the next run).
		if err := ctx.Err(); err != nil {
			_ = os.RemoveAll(destRoot)
			return "", err
		}
		fpath := filepath.Join(destRoot, filepath.FromSlash(f.Name))

		// Zip-slip guard: ensure extraction target is inside destRoot.
		if !strings.HasPrefix(filepath.Clean(fpath)+string(os.PathSeparator), cleanRoot) {
			logger.Warn("zip-slip: skipping entry", zap.String("entry", f.Name))
			continue
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, f.Mode()); err != nil {
				return "", err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
			return "", err
		}

		if err := extractFile(ctx, f, fpath); err != nil {
			if ctx.Err() != nil {
				_ = os.RemoveAll(destRoot)
				return "", ctx.Err()
			}
			return "", fmt.Errorf("extract %s: %w", f.Name, err)
		}
	}

	logger.Info("extracted zip", zap.String("zip", filepath.Base(zipPath)), zap.String("dest", destRoot))
	return destRoot, nil
}

func extractFile(ctx context.Context, f *zip.File, destPath string) (err error) {
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	_, err = io.Copy(out, &ctxReader{ctx: ctx, r: rc})
	return err
}

// ctxReader stops a copy part-way when ctx is cancelled, so an interrupt does
// not have to wait for a multi-GB file to finish extracting.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// WalkDirectory walks an extracted Takeout directory and calls emit for each
// media file that has not already been uploaded. It stops and returns
// ctx.Err() as soon as ctx is cancelled.
//
// isUploaded is called for every media file found; if it returns true the file
// is silently skipped (idempotency).
//
// emit is the callback that receives each MigrationJob. Typically the caller
// uses a closure that registers the item in the store and forwards it to a
// pipeline channel.
func WalkDirectory(
	ctx context.Context,
	dir, sourceZip string,
	isUploaded func(string) (bool, error),
	emit func(MigrationJob),
	logger *zap.Logger,
) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // interrupted: stop walking
		}
		if err != nil {
			logger.Warn("walk error", zap.String("path", path), zap.Error(err))
			return nil // log and continue
		}
		if d.IsDir() || !IsMediaFile(path) {
			return nil
		}

		uploaded, err := isUploaded(path)
		if err != nil {
			return fmt.Errorf("check uploaded %s: %w", path, err)
		}
		if uploaded {
			return nil
		}

		sidecarPath, sidecar, err := FindSidecar(path)
		if err != nil {
			logger.Warn("sidecar error", zap.String("media", path), zap.Error(err))
		}

		emit(MigrationJob{
			MediaPath:   path,
			SidecarPath: sidecarPath,
			AlbumName:   albumFromPath(path, dir),
			SourceZip:   sourceZip,
			Sidecar:     sidecar,
		})
		return nil
	})
}

// albumFromPath derives the album name from a media file's location inside
// the extraction root.
//
// Google Takeout wraps all content under "Takeout/Google Photos/<album>/<file>".
// This prefix is stripped before determining the album name, so both
// pre-extracted archives (with the prefix) and raw directory modes (without it)
// are handled correctly.
//
// Classification rules (applied after prefix stripping):
//   - Files directly in the root (no subdirectory)  → library root (empty).
//   - Year folders ("Photos from YYYY")              → library root (empty).
//   - Any other named top-level folder               → album name = folder name.
func albumFromPath(mediaPath, extractRoot string) string {
	rel, err := filepath.Rel(extractRoot, mediaPath)
	if err != nil {
		return ""
	}

	// Strip the "Takeout/Google Photos/" wrapper that Google Takeout adds.
	// filepath.Join normalises separators for the current OS.
	googlePhotosPrefix := filepath.Join("Takeout", "Google Photos") + string(os.PathSeparator)
	rel = strings.TrimPrefix(rel, googlePhotosPrefix)

	// rel is now "<album>/<file>" or just "<file>" for root-level items.
	parts := strings.SplitN(rel, string(os.PathSeparator), 2)
	if len(parts) < 2 {
		return "" // file directly in the root — no album
	}
	top := parts[0]
	if yearFolderRe.MatchString(top) {
		return "" // year folder → library root
	}
	return top
}

// ExtractFiles extracts a specific set of in-zip entry names from zipPath into
// destDir, preserving the relative path structure. It is used by the
// large-files --extract flag to pull oversized files back out of their source
// archives so the user can upload them manually.
//
// wantNames is a set of zip-entry paths (using forward slashes, as stored
// inside the zip). Only entries whose name is present in the set are written.
func ExtractFiles(zipPath, destDir string, wantNames map[string]struct{}, logger *zap.Logger) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer func() { _ = r.Close() }()

	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Zip entries always use forward slashes.
		entryName := filepath.ToSlash(f.Name)
		if _, ok := wantNames[entryName]; !ok {
			continue
		}

		destPath := filepath.Join(destDir, filepath.FromSlash(entryName))
		// Zip-slip guard.
		if !strings.HasPrefix(filepath.Clean(destPath)+string(os.PathSeparator), cleanDest) {
			logger.Warn("zip-slip: skipping entry", zap.String("entry", f.Name))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", destPath, err)
		}
		if err := extractFile(context.Background(), f, destPath); err != nil {
			return fmt.Errorf("extract %s: %w", f.Name, err)
		}
		logger.Info("extracted", zap.String("entry", entryName), zap.String("dest", destPath))
	}
	return nil
}
