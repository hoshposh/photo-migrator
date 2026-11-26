-- Before the dry-run fix, --dry-run marked every file it saw as complete with
-- the placeholder media ID 'dry-run', and then marked the zip complete. Those
-- files were never uploaded, and completed zips are never revisited, so they
-- would silently never be migrated. Put them (and their zips) back to pending.

-- +goose Up
-- +goose StatementBegin
UPDATE zip_archives
   SET status = 'pending', error_msg = NULL, updated_at = CURRENT_TIMESTAMP
 WHERE zip_path IN (SELECT DISTINCT source_zip
                      FROM media_items
                     WHERE photos_media_id = 'dry-run' AND source_zip IS NOT NULL);
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE media_items
   SET status = 'pending', photos_media_id = NULL, error_message = NULL,
       updated_at = CURRENT_TIMESTAMP
 WHERE photos_media_id = 'dry-run';
-- +goose StatementEnd

-- +goose Down
-- Irreversible: the reset rows were never uploaded, so there is nothing to restore.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
