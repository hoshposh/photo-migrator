-- Columns for the wrong-dates report: the date a file should have (from its
-- Takeout sidecar) and the date Google gave it. Times are Unix seconds, UTC.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE media_items ADD COLUMN taken_time INTEGER;
ALTER TABLE media_items ADD COLUMN taken_time_source TEXT;
ALTER TABLE media_items ADD COLUMN google_time INTEGER;
ALTER TABLE media_items ADD COLUMN google_checked_at INTEGER;
ALTER TABLE media_items ADD COLUMN google_missing INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE media_items DROP COLUMN google_missing;
ALTER TABLE media_items DROP COLUMN google_checked_at;
ALTER TABLE media_items DROP COLUMN google_time;
ALTER TABLE media_items DROP COLUMN taken_time_source;
ALTER TABLE media_items DROP COLUMN taken_time;
-- +goose StatementEnd
