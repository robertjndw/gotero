-- +goose Up
-- Snapshots upload a zip, so the blob md5 (md5) differs from the md5 of the
-- unzipped file that belongs in the item. Empty means "same as md5".
ALTER TABLE upload_tokens ADD COLUMN item_md5 text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE upload_tokens DROP COLUMN item_md5;
