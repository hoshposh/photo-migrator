-- Files from Google Photos' Locked Folder are private; the API cannot put
-- them in the destination's Locked Folder, so uploading them would expose
-- them in the main library. They are now held back with status 'locked'.
-- Hold back any Locked Folder rows registered before that (not yet
-- uploaded); rows already complete are left alone.

-- +goose Up
-- +goose StatementBegin
UPDATE media_items
   SET status = 'locked', updated_at = CURRENT_TIMESTAMP
 WHERE status IN ('pending', 'bytes_uploaded', 'error')
   AND (zip_entry LIKE 'Takeout/Google Photos/Locked Folder/%'
        OR zip_entry LIKE 'Locked Folder/%'
        OR (source_zip IS NULL AND source_path LIKE '%/Locked Folder/%'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE media_items SET status = 'pending' WHERE status = 'locked';
-- +goose StatementEnd
