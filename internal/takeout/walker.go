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

// motionPartRe matches the names Takeout gives the video half of a motion
// photo: PXL_x.MP, PXL_x.MP~2, PXL_x.RAW-01.MP.COVER (the still is the same
// name plus ".jpg", possibly in another zip).
var motionPartRe = regexp.MustCompile(`(?i)\.mp(~\d+)?(\.cover)?$`)

// videoBrands maps ISO base-media "ftyp" brands that mean video to a MIME
// type and extension. Image formats in the same container family (HEIC,
// AVIF, CR3) use other brands and are deliberately not listed.
var videoBrands = map[string][2]string{
	"isom": {"video/mp4", ".mp4"}, "iso2": {"video/mp4", ".mp4"}, "iso4": {"video/mp4", ".mp4"},
	"iso5": {"video/mp4", ".mp4"}, "iso6": {"video/mp4", ".mp4"}, "mp41": {"video/mp4", ".mp4"},
	"mp42": {"video/mp4", ".mp4"}, "avc1": {"video/mp4", ".mp4"}, "dash": {"video/mp4", ".mp4"},
	"M4V ": {"video/x-m4v", ".m4v"}, "M4VP": {"video/x-m4v", ".m4v"},
	"qt  ": {"video/quicktime", ".mov"},
	"3gp4": {"video/3gpp", ".3gp"}, "3gp5": {"video/3gpp", ".3gp"}, "3gp6": {"video/3gpp", ".3gp"},
	"3g2a": {"video/3gpp2", ".3g2"},
}

// sniffVideo reports whether the file is an MP4/QuickTime-family video by
// its content, returning its MIME type and usual extension.
func sniffVideo(path string) (mime, ext string, ok bool) {
	if strings.HasPrefix(filepath.Base(path), "._") || strings.EqualFold(filepath.Ext(path), ".json") {
		return "", "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer func() { _ = f.Close() }()
	var b [12]byte
	if _, err := io.ReadFull(f, b[:]); err != nil || string(b[4:8]) != "ftyp" {
		return "", "", false
	}
	v, ok := videoBrands[string(b[8:12])]
	return v[0], v[1], ok
}

// LockedFolder is Takeout's folder for Google Photos' private, passcode-
// protected Locked Folder. The API cannot put files there, so callers hold
// these files back from upload (MigrationJob.Locked) unless approved.
const LockedFolder = "Locked Folder"

// nonAlbumFolders are special Takeout folders that are not albums: "Archive"
// holds photos archived in Google Photos (a state the API cannot reproduce),
// "Failed Videos" holds videos Google could not process, and the Locked
// Folder is private. Their files go to the library root (if uploaded at all),
// like the year folders.
var nonAlbumFolders = map[string]bool{
	"Archive":       true,
	"Failed Videos": true,
	LockedFolder:    true,
}

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
	// ZipEntry is the file's path inside SourceZip, with forward slashes
	// (empty in dir mode). Together with SourceZip it identifies the file in
	// the ledger independently of where the zip was extracted.
	ZipEntry string
	// Sidecar holds the parsed metadata (nil if not found).
	Sidecar *Sidecar
	// Locked reports that the file came from Google Photos' Locked Folder.
	Locked bool
	// MotionPart reports that the file is the video half of a motion photo
	// (e.g. PXL_x.MP or PXL_x.MP.COVER next to PXL_x.MP.jpg). The API cannot
	// rebuild a motion photo, so callers hold these back.
	MotionPart bool
	// UploadName and MimeType override the file name and MIME type sent to
	// Google for files whose extension does not describe them (a video named
	// "talk - 3.12.20" is uploaded as "talk - 3.12.20.mp4", video/mp4).
	// Empty means derive both from the file name.
	UploadName string
	MimeType   string
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
// isDone is called with each media file's path and zip entry (its path
// relative to dir, which is the path inside the zip when sourceZip is set);
// if it returns true the file needs no upload (already uploaded, or
// permanently too large) and is skipped.
//
// emit is the callback that receives each MigrationJob. Typically the caller
// uses a closure that registers the item in the store and forwards it to a
// pipeline channel.
func WalkDirectory(
	ctx context.Context,
	dir, sourceZip string,
	isDone func(path, zipEntry string) (bool, error),
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
		if d.IsDir() {
			return nil
		}
		var motion bool
		var uploadName, mime string
		if !IsMediaFile(path) {
			// Takeout exports some videos under names with no usable
			// extension (motion-photo halves, "clip - 3.12.20"); recognise
			// them by content.
			vmime, vext, ok := sniffVideo(path)
			if !ok {
				return nil
			}
			if base := filepath.Base(path); motionPartRe.MatchString(base) {
				motion = true
			} else {
				uploadName, mime = base+vext, vmime
			}
		}

		zipEntry := ""
		if sourceZip != "" {
			if rel, rerr := filepath.Rel(dir, path); rerr == nil {
				zipEntry = filepath.ToSlash(rel)
			}
		}

		done, err := isDone(path, zipEntry)
		if err != nil {
			return fmt.Errorf("check uploaded %s: %w", path, err)
		}
		if done {
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
			ZipEntry:    zipEntry,
			Sidecar:     sidecar,
			Locked:      topFolder(path, dir) == LockedFolder,
			MotionPart:  motion,
			UploadName:  uploadName,
			MimeType:    mime,
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
//   - "Archive", "Failed Videos", "Locked Folder"     → library root (empty).
//   - Any other named top-level folder               → album name = folder name.
func albumFromPath(mediaPath, extractRoot string) string {
	top := topFolder(mediaPath, extractRoot)
	if top == "" {
		return "" // file directly in the root — no album
	}
	if yearFolderRe.MatchString(top) || nonAlbumFolders[top] {
		return "" // year or special folder → library root
	}
	return top
}

// topFolder returns the Takeout folder a media file sits in (the first path
// component after the "Takeout/Google Photos/" wrapper), or "" for a file
// directly in the root.
func topFolder(mediaPath, extractRoot string) string {
	rel, err := filepath.Rel(extractRoot, mediaPath)
	if err != nil {
		return ""
	}
	// Strip the "Takeout/Google Photos/" wrapper that Google Takeout adds.
	// filepath.Join normalises separators for the current OS.
	rel = strings.TrimPrefix(rel, filepath.Join("Takeout", "Google Photos")+string(os.PathSeparator))
	parts := strings.SplitN(rel, string(os.PathSeparator), 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
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
