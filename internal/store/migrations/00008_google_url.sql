-- The link that opens a photo in Google Photos (mediaItem.productUrl), for the
-- wrong-dates report. NULL = not known.

-- +goose Up
ALTER TABLE media_items ADD COLUMN google_url TEXT;

-- +goose Down
ALTER TABLE media_items DROP COLUMN google_url;
