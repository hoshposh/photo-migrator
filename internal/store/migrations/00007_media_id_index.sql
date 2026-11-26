-- Index on the Google media ID: the wrong-dates report saves Google's dates
-- by media ID, and without an index every update scans the whole table.

-- +goose Up
CREATE INDEX IF NOT EXISTS idx_media_photos_media_id ON media_items(photos_media_id);

-- +goose Down
DROP INDEX IF EXISTS idx_media_photos_media_id;
