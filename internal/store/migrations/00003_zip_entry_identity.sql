-- Identify zip-mode media by (source_zip, zip_entry), the file's path inside
-- its zip, instead of its absolute staging path. Before this, changing
-- staging-dir (or running with a relative one from another directory) made
-- already-uploaded files look new, so they were uploaded again and the stale
-- rows kept their zip from ever completing.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE media_items ADD COLUMN zip_entry TEXT;
-- +goose StatementEnd

-- Backfill: staging paths look like <staging>/<zip-stem>/<entry>; take what
-- follows "/<zip-stem>/". The zip stem is the source_zip basename minus ".zip".
-- +goose StatementBegin
UPDATE media_items
   SET zip_entry = substr('/' || source_path,
                          instr('/' || source_path, '/' || s.stem || '/') + length(s.stem) + 2)
  FROM (SELECT id AS mid, substr(base, 1, length(base) - 4) AS stem
          FROM (SELECT id, replace(source_zip, rtrim(source_zip, replace(source_zip, '/', '')), '') AS base
                  FROM media_items
                 WHERE source_zip IS NOT NULL)) AS s
 WHERE media_items.id = s.mid
   AND instr('/' || source_path, '/' || s.stem || '/') > 0;
-- +goose StatementEnd

-- macOS AppleDouble files ("._name") are not media and are no longer walked,
-- so their old rows could never be retried and only blocked their zip.
-- +goose StatementBegin
DELETE FROM media_items
 WHERE source_path LIKE '%/.\_%' ESCAPE '\' OR source_path LIKE '.\_%' ESCAPE '\';
-- +goose StatementEnd

-- Merge rows for the same zip entry recorded under different staging paths,
-- keeping the most useful one: complete > too_large > error > pending, then newest.
-- +goose StatementBegin
DELETE FROM media_items
 WHERE id IN (
   SELECT id FROM (
     SELECT id, ROW_NUMBER() OVER (
              PARTITION BY source_zip, zip_entry
              ORDER BY CASE status WHEN 'complete' THEN 0 WHEN 'too_large' THEN 1
                                   WHEN 'error' THEN 2 ELSE 3 END,
                       id DESC) AS rn
       FROM media_items
      WHERE source_zip IS NOT NULL AND zip_entry IS NOT NULL)
   WHERE rn > 1);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_media_zip_entry ON media_items(source_zip, zip_entry)
 WHERE source_zip IS NOT NULL AND zip_entry IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- Merged and deleted rows are not restored.
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_media_zip_entry;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE media_items DROP COLUMN zip_entry;
-- +goose StatementEnd
