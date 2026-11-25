-- +goose Up
-- +goose StatementBegin
CREATE TABLE zip_archives (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    zip_path    TEXT UNIQUE NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    -- pending | extracting | uploading | complete | error
    error_msg   TEXT,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE media_items (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    -- In zip mode: absolute path inside the staging dir.
    -- In dir mode: absolute path in the original takeout dir.
    source_path     TEXT UNIQUE NOT NULL,
    -- Originating zip_archives.zip_path (NULL in dir mode).
    source_zip      TEXT REFERENCES zip_archives(zip_path),
    album_name      TEXT,
    upload_token    TEXT,
    photos_media_id TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    -- pending | bytes_uploaded | complete | error
    error_message   TEXT,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_media_source_zip ON media_items(source_zip);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE albums (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    source_name     TEXT UNIQUE NOT NULL,
    photos_album_id TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    -- pending | complete
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS albums;
DROP TABLE IF EXISTS media_items;
DROP TABLE IF EXISTS zip_archives;
