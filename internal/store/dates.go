package store

import (
	"database/sql"
	"time"
)

const (
	TakenFromSidecar = "sidecar" // taken_time_source values
	TakenNone        = "none"
)

// DateRow is one uploaded file for the date check. Zero times mean NULL.
type DateRow struct {
	SourcePath, SourceZip, ZipEntry, AlbumName, MediaID string
	TakenTime                                           time.Time
	TakenSource                                         string // "", TakenFromSidecar, TakenNone
	GoogleTime                                          time.Time
	GoogleCheckedAt                                     time.Time
	GoogleMissing                                       bool
	GoogleURL                                           string // "" = unknown
}

// TakenDate is a looked-up expected date; zero Time records TakenNone.
type TakenDate struct {
	SourcePath string
	Time       time.Time
}

// GoogleDate is Google's answer for one media ID.
type GoogleDate struct {
	MediaID string
	Time    time.Time // zero when Missing
	Missing bool
	URL     string // productUrl; stored only when non-empty
}

// unixOrNil converts a time to Unix seconds, or NULL for the zero time.
func unixOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// timeFromNull converts nullable Unix seconds back to a time (zero for NULL).
func timeFromNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0).UTC()
}

// GetDateRows returns every file really uploaded to Google Photos (complete,
// with a real media ID), ordered by source_zip then source_path.
func (s *Store) GetDateRows() ([]DateRow, error) {
	rows, err := s.db.Query(
		`SELECT source_path, COALESCE(source_zip, ''), COALESCE(zip_entry, ''),
		        COALESCE(album_name, ''), photos_media_id,
		        taken_time, COALESCE(taken_time_source, ''),
		        google_time, google_checked_at, google_missing, COALESCE(google_url, '')
		   FROM media_items
		  WHERE status = 'complete'
		    AND photos_media_id IS NOT NULL
		    AND photos_media_id NOT IN ('', 'dry-run')
		  ORDER BY COALESCE(source_zip, ''), source_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []DateRow
	for rows.Next() {
		var r DateRow
		var taken, google, checked sql.NullInt64
		var missing int
		if err := rows.Scan(&r.SourcePath, &r.SourceZip, &r.ZipEntry, &r.AlbumName, &r.MediaID,
			&taken, &r.TakenSource, &google, &checked, &missing, &r.GoogleURL); err != nil {
			return nil, err
		}
		r.TakenTime = timeFromNull(taken)
		r.GoogleTime = timeFromNull(google)
		r.GoogleCheckedAt = timeFromNull(checked)
		r.GoogleMissing = missing != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetTakenDates records the expected date for each file in one transaction.
// A zero Time records TakenNone (looked up, no date found).
func (s *Store) SetTakenDates(ds []TakenDate) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, d := range ds {
		src := TakenFromSidecar
		if d.Time.IsZero() {
			src = TakenNone
		}
		if _, err := tx.Exec(
			`UPDATE media_items SET taken_time = ?, taken_time_source = ? WHERE source_path = ?`,
			unixOrNil(d.Time), src, d.SourcePath,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetGoogleURL stores only the link of a media ID, on every row with that ID,
// without recording a date or marking the item checked.
func (s *Store) SetGoogleURL(mediaID, url string) error {
	_, err := s.db.Exec(`UPDATE media_items SET google_url = ? WHERE photos_media_id = ?`, url, mediaID)
	return err
}

// SetGoogleDates records Google's answer for each media ID in one
// transaction, on every row with that ID (Google dedupes identical uploads).
// An empty URL leaves a stored link alone.
func (s *Store) SetGoogleDates(ds []GoogleDate, checkedAt time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, d := range ds {
		missing := 0
		if d.Missing {
			missing = 1
		}
		if _, err := tx.Exec(
			`UPDATE media_items
			    SET google_time = ?, google_checked_at = ?, google_missing = ?,
			        google_url = COALESCE(NULLIF(?, ''), google_url)
			  WHERE photos_media_id = ?`,
			unixOrNil(d.Time), checkedAt.Unix(), missing, d.URL, d.MediaID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}
