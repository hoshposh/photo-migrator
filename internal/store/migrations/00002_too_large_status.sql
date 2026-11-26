-- Files Google rejects as too large (HTTP 413) can never be uploaded through
-- the API. Give them their own status so they are reported, not retried.
-- +goose Up
UPDATE media_items
   SET status = 'too_large'
 WHERE status = 'error'
   AND (error_message LIKE '%HTTP 413%' OR error_message LIKE '%FILE_SIZE_TOO_LARGE%');

-- +goose Down
UPDATE media_items SET status = 'error' WHERE status = 'too_large';
