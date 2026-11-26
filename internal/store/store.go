// Package store manages the SQLite state ledger used to track migration
// progress across interrupted runs.
//
// Schema migrations are handled by pressly/goose with SQL files embedded
// directly into the binary. To add a future schema change, create a new
// numbered file under migrations/ (e.g. 00002_add_column.sql) in goose
// format and rebuild — goose applies only unapplied migrations on startup.
package store

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

// Status values for zip_archives and media_items rows.
type Status string

const (
	StatusPending       Status = "pending"
	StatusExtracting    Status = "extracting"
	StatusUploading     Status = "uploading"
	StatusBytesUploaded Status = "bytes_uploaded"
	StatusComplete      Status = "complete"
	StatusError         Status = "error"
	// StatusTooLarge marks a file Google rejected as too large for the API
	// (HTTP 413). It is never retried; `large-files` lists it for manual upload.
	StatusTooLarge Status = "too_large"
	// StatusLocked marks a file from Google Photos' private Locked Folder,
	// held back from upload. Setting it to pending (after review) releases it.
	StatusLocked Status = "locked"
	// StatusMotionVideo marks the video half of a motion photo, held back
	// because the API cannot rebuild motion photos (it would appear as a
	// separate video). Setting it to pending releases it for upload.
	StatusMotionVideo Status = "motion_video"
	// StatusRawPair marks the half of a Pixel RAW pair held back by the
	// raw-pairs setting. Setting it to pending releases it.
	StatusRawPair Status = "raw_pair"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, then runs any pending
// goose migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000", path))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Serialise writers — SQLite only supports one concurrent writer.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.runMigrations(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) runMigrations() error {
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	// Suppress goose's default stdout logging.
	goose.SetLogger(goose.NopLogger())
	return goose.Up(s.db, "migrations")
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// ─────────────────────────────────────────────────────────────
// Zip archive operations
// ─────────────────────────────────────────────────────────────

// RegisterZip adds a zip path to the ledger if not already present.
// Safe to call repeatedly — uses INSERT OR IGNORE semantics.
func (s *Store) RegisterZip(zipPath string) error {
	_, err := s.db.Exec(
		`INSERT INTO zip_archives (zip_path) VALUES (?) ON CONFLICT(zip_path) DO NOTHING`,
		zipPath,
	)
	return err
}

// GetPendingZips returns all zip archive paths not yet marked complete,
// ordered alphabetically for deterministic processing.
func (s *Store) GetPendingZips() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT zip_path FROM zip_archives WHERE status != 'complete' ORDER BY zip_path ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// SetZipStatus updates the status (and optional error message) for a zip.
func (s *Store) SetZipStatus(zipPath string, status Status, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE zip_archives
		    SET status = ?, error_msg = ?, updated_at = CURRENT_TIMESTAMP
		  WHERE zip_path = ?`,
		string(status), errMsg, zipPath,
	)
	return err
}

// AllZipItemsComplete returns true when every media_item for the given zip is
// done: complete, too large for the API (reported by `large-files`), or held
// back from the Locked Folder (`locked-files`), or a held-back motion-photo
// video half (`motion-videos`).
func (s *Store) AllZipItemsComplete(zipPath string) (bool, error) {
	var pending int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM media_items WHERE source_zip = ? AND status NOT IN ('complete', 'too_large', 'locked', 'motion_video', 'raw_pair')`,
		zipPath,
	).Scan(&pending)
	return pending == 0, err
}

// ─────────────────────────────────────────────────────────────
// Media item operations
// ─────────────────────────────────────────────────────────────

// MediaItem is the projection used by the upload pipeline.
type MediaItem struct {
	SourcePath  string
	SourceZip   string
	AlbumName   string
	UploadToken string
	Status      Status
}

// RegisterMedia adds a media item to the ledger if not already present.
//
// Zip-mode items (sourceZip != "") are identified by (sourceZip, zipEntry),
// the file's path inside its zip, so the staging directory can change between
// runs. If the entry is already known, its source_path is refreshed to the
// current staging path (status updates look rows up by source_path).
// Directory-mode items are identified by sourcePath.
func (s *Store) RegisterMedia(sourcePath, sourceZip, zipEntry, albumName string) error {
	if sourceZip == "" {
		_, err := s.db.Exec(
			`INSERT INTO media_items (source_path, album_name)
			      VALUES (?, ?)
			      ON CONFLICT(source_path) DO NOTHING`,
			sourcePath, nullStr(albumName),
		)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO media_items (source_path, source_zip, zip_entry, album_name)
		      VALUES (?, ?, ?, ?)
		      ON CONFLICT(source_zip, zip_entry) WHERE source_zip IS NOT NULL AND zip_entry IS NOT NULL
		      DO UPDATE SET source_path = excluded.source_path`,
		sourcePath, sourceZip, zipEntry, nullStr(albumName),
	)
	return err
}

// RegisterHeld registers a file that is held back from upload (status is
// StatusLocked or StatusMotionVideo) if it is new, and reports whether it is
// still held back. A file whose status was later set to pending (released,
// e.g. after review) is not held back and should be uploaded. Identity rules
// are the same as RegisterMedia.
func (s *Store) RegisterHeld(status Status, sourcePath, sourceZip, zipEntry string) (held bool, err error) {
	if sourceZip == "" {
		_, err = s.db.Exec(
			`INSERT INTO media_items (source_path, status) VALUES (?, ?)
			      ON CONFLICT(source_path) DO NOTHING`,
			sourcePath, string(status),
		)
	} else {
		_, err = s.db.Exec(
			`INSERT INTO media_items (source_path, source_zip, zip_entry, status)
			      VALUES (?, ?, ?, ?)
			      ON CONFLICT(source_zip, zip_entry) WHERE source_zip IS NOT NULL AND zip_entry IS NOT NULL
			      DO UPDATE SET source_path = excluded.source_path`,
			sourcePath, sourceZip, zipEntry, string(status),
		)
	}
	if err != nil {
		return false, err
	}
	var current string
	if err := s.db.QueryRow(`SELECT status FROM media_items WHERE source_path = ?`, sourcePath).Scan(&current); err != nil {
		return false, err
	}
	return Status(current) == StatusLocked || Status(current) == StatusMotionVideo || Status(current) == StatusRawPair, nil
}

// IsDone reports whether a media file needs no further upload attempts: it
// is complete, permanently too large for the API, or held back (locked,
// motion_video). Zip-mode files are
// looked up by (sourceZip, zipEntry), directory-mode files by sourcePath.
func (s *Store) IsDone(sourceZip, zipEntry, sourcePath string) (bool, error) {
	var status string
	var err error
	if sourceZip != "" {
		err = s.db.QueryRow(
			`SELECT status FROM media_items WHERE source_zip = ? AND zip_entry = ?`,
			sourceZip, zipEntry,
		).Scan(&status)
	} else {
		err = s.db.QueryRow(
			`SELECT status FROM media_items WHERE source_path = ?`, sourcePath,
		).Scan(&status)
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch Status(status) {
	case StatusComplete, StatusTooLarge, StatusLocked, StatusMotionVideo, StatusRawPair:
		return true, nil
	}
	return false, nil
}

// SetMediaUploadToken records the upload token after the byte-upload step.
func (s *Store) SetMediaUploadToken(sourcePath, uploadToken string) error {
	_, err := s.db.Exec(
		`UPDATE media_items
		    SET status = ?, upload_token = ?, updated_at = CURRENT_TIMESTAMP
		  WHERE source_path = ?`,
		string(StatusBytesUploaded), uploadToken, sourcePath,
	)
	return err
}

// SetMediaComplete records the Google Photos media ID on successful creation.
func (s *Store) SetMediaComplete(sourcePath, photosMediaID string) error {
	_, err := s.db.Exec(
		`UPDATE media_items
		    SET status = ?, photos_media_id = ?, updated_at = CURRENT_TIMESTAMP
		  WHERE source_path = ?`,
		string(StatusComplete), photosMediaID, sourcePath,
	)
	return err
}

// SetMediaTooLarge records that Google rejected the file as too large; it
// will not be retried.
func (s *Store) SetMediaTooLarge(sourcePath, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE media_items
		    SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP
		  WHERE source_path = ?`,
		string(StatusTooLarge), errMsg, sourcePath,
	)
	return err
}

// SetMediaError records a terminal error for a media item.
func (s *Store) SetMediaError(sourcePath, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE media_items
		    SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP
		  WHERE source_path = ?`,
		string(StatusError), errMsg, sourcePath,
	)
	return err
}

// GetPendingMedia returns all media items still to upload (used in dir mode).
func (s *Store) GetPendingMedia() ([]MediaItem, error) {
	return s.queryMedia(
		`SELECT source_path,
		        COALESCE(source_zip, ''),
		        COALESCE(album_name, ''),
		        COALESCE(upload_token, ''),
		        status
		   FROM media_items
		  WHERE status NOT IN ('complete', 'too_large', 'locked', 'motion_video', 'raw_pair')
		  ORDER BY id ASC`,
	)
}

// ManualItem is a file the user must handle by hand: one too large for the
// API (GetLargeFileErrors), one held back from the Locked Folder
// (GetLockedFiles), or a held-back motion-photo video half (GetMotionVideos). It holds enough context to find the file again.
type ManualItem struct {
	SourcePath string
	SourceZip  string // empty in dir mode
	ZipEntry   string // path inside SourceZip; empty in dir mode
	AlbumName  string
	ErrorMsg   string
}

// GetLargeFileErrors returns all media_items that failed because the file was
// too large for the Google Photos API (HTTP 413 / "Uploaded photo must not
// have more than N bytes").
func (s *Store) GetLargeFileErrors() ([]ManualItem, error) {
	return s.queryManual(`
		 WHERE status = 'too_large'
		    OR (status = 'error'
		   AND (error_message LIKE '%HTTP 413%'
		     OR error_message LIKE '%too large%'
		     OR error_message LIKE '%exceeds%'
		     OR error_message LIKE '%FILE_SIZE_TOO_LARGE%'))`)
}

// GetLockedFiles returns the Locked Folder files held back from upload.
func (s *Store) GetLockedFiles() ([]ManualItem, error) {
	return s.queryManual(` WHERE status = 'locked'`)
}

// GetMotionVideos returns the motion-photo video halves held back from upload.
func (s *Store) GetMotionVideos() ([]ManualItem, error) {
	return s.queryManual(` WHERE status = 'motion_video'`)
}

// GetRawPairs returns the Pixel RAW-pair halves held back by raw-pairs.
func (s *Store) GetRawPairs() ([]ManualItem, error) {
	return s.queryManual(` WHERE status = 'raw_pair'`)
}

func (s *Store) queryManual(where string) ([]ManualItem, error) {
	rows, err := s.db.Query(`
		SELECT source_path,
		       COALESCE(source_zip, ''),
		       COALESCE(zip_entry, ''),
		       COALESCE(album_name, ''),
		       COALESCE(error_message, '')
		  FROM media_items` + where + `
		 ORDER BY source_zip ASC, source_path ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []ManualItem
	for rows.Next() {
		var it ManualItem
		if err := rows.Scan(&it.SourcePath, &it.SourceZip, &it.ZipEntry, &it.AlbumName, &it.ErrorMsg); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) queryMedia(query string, args ...any) ([]MediaItem, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []MediaItem
	for rows.Next() {
		var m MediaItem
		if err := rows.Scan(
			&m.SourcePath, &m.SourceZip, &m.AlbumName, &m.UploadToken, &m.Status,
		); err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

// ─────────────────────────────────────────────────────────────
// Album operations
// ─────────────────────────────────────────────────────────────

// GetAlbumID returns the Google Photos album ID for the given album name.
// The second return value is false if the album has not yet been created.
func (s *Store) GetAlbumID(name string) (string, bool, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT COALESCE(photos_album_id, '')
		   FROM albums
		  WHERE source_name = ? AND status = 'complete'`,
		name,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return id, id != "", err
}

// UpsertAlbum ensures the album name is registered in the ledger.
func (s *Store) UpsertAlbum(name string) error {
	_, err := s.db.Exec(
		`INSERT INTO albums (source_name) VALUES (?) ON CONFLICT(source_name) DO NOTHING`,
		name,
	)
	return err
}

// SetAlbumComplete records the Google Photos album ID for a source album.
func (s *Store) SetAlbumComplete(name, albumID string) error {
	_, err := s.db.Exec(
		`UPDATE albums SET photos_album_id = ?, status = 'complete' WHERE source_name = ?`,
		albumID, name,
	)
	return err
}

// ─────────────────────────────────────────────────────────────
// Stats
// ─────────────────────────────────────────────────────────────

// Stats holds progress counters for the status command.
type Stats struct {
	TotalZips      int
	CompleteZips   int
	TotalMedia     int
	CompleteMedia  int
	PendingMedia   int
	ErrorMedia     int
	TooLargeMedia  int
	LockedMedia    int
	MotionMedia    int
	RawPairMedia   int
	TotalAlbums    int
	CompleteAlbums int
}

// GetStats returns current progress counts.
func (s *Store) GetStats() (*Stats, error) {
	st := &Stats{}

	if err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status='complete' THEN 1 ELSE 0 END), 0)
		  FROM zip_archives`,
	).Scan(&st.TotalZips, &st.CompleteZips); err != nil {
		return nil, fmt.Errorf("stats zip_archives: %w", err)
	}

	if err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status='complete'                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status IN ('pending','bytes_uploaded')    THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='error'                            THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='too_large'                        THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='locked'                           THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='motion_video'                     THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='raw_pair'                         THEN 1 ELSE 0 END), 0)
		  FROM media_items`,
	).Scan(&st.TotalMedia, &st.CompleteMedia, &st.PendingMedia, &st.ErrorMedia, &st.TooLargeMedia, &st.LockedMedia, &st.MotionMedia, &st.RawPairMedia); err != nil {
		return nil, fmt.Errorf("stats media_items: %w", err)
	}

	if err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status='complete' THEN 1 ELSE 0 END), 0)
		  FROM albums`,
	).Scan(&st.TotalAlbums, &st.CompleteAlbums); err != nil {
		return nil, fmt.Errorf("stats albums: %w", err)
	}

	return st, nil
}

// nullStr converts an empty string to a nil interface (stored as SQL NULL).
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
