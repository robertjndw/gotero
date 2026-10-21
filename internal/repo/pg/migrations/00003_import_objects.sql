-- +goose Up
-- What the zotero.org importer last wrote per object. A rerun overwrites an
-- object only while its stored content hash still equals content_hash, so
-- edits made in gotero afterwards are never replaced by older source data.
CREATE TABLE import_objects (
    library_id     bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    object_type    text   NOT NULL,
    key            text   NOT NULL,
    source_version bigint NOT NULL,
    content_hash   bytea  NOT NULL,
    PRIMARY KEY (library_id, object_type, key)
);

-- +goose Down
DROP TABLE import_objects;
