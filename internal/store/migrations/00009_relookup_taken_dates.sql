-- Sidecar matching improved (edited copies, Live Photo videos, numbered
-- duplicates, cut-short names), so forget every saved expected date: the next
-- wrong-dates run looks every photo up again with the new rules.

-- +goose Up
UPDATE media_items SET taken_time = NULL, taken_time_source = NULL;

-- +goose Down
-- Nothing to restore: cleared dates are simply looked up again.
SELECT 1;
