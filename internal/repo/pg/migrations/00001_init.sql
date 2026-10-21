-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

-- A library is the unit of versioning and locking. Every write takes
-- SELECT ... FOR UPDATE on its row, which serializes writes per library while
-- leaving other libraries and all reads untouched.
CREATE TABLE libraries (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind             text   NOT NULL CHECK (kind IN ('user', 'group')),
    version          bigint NOT NULL DEFAULT 0,
    -- deleted?since=V below this floor answers 409 so clients do a full sync.
    -- Raised when old deleted_log rows are pruned.
    delete_log_floor bigint NOT NULL DEFAULT 0,
    storage_used     bigint NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    -- Zotero user IDs are public and baked into client profiles, so they are
    -- assigned explicitly (importers keep the zotero.org ID) instead of IDENTITY.
    id            bigint PRIMARY KEY CHECK (id > 0),
    library_id    bigint NOT NULL UNIQUE REFERENCES libraries (id),
    username      citext NOT NULL UNIQUE,
    display_name  text   NOT NULL DEFAULT '',
    email         citext,
    password_hash text   NOT NULL,
    is_admin      boolean NOT NULL DEFAULT false,
    disabled      boolean NOT NULL DEFAULT false,
    quota_bytes   bigint CHECK (quota_bytes IS NULL OR quota_bytes >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_sync_at  timestamptz
);
CREATE UNIQUE INDEX users_email_key ON users (email) WHERE email IS NOT NULL;
CREATE SEQUENCE user_id_seq START 1;

CREATE TABLE groups (
    id              bigint PRIMARY KEY CHECK (id > 0),
    library_id      bigint NOT NULL UNIQUE REFERENCES libraries (id),
    owner_id        bigint NOT NULL REFERENCES users (id),
    name            text   NOT NULL,
    description     text   NOT NULL DEFAULT '',
    url             text   NOT NULL DEFAULT '',
    library_reading text   NOT NULL DEFAULT 'members' CHECK (library_reading IN ('all', 'members')),
    library_editing text   NOT NULL DEFAULT 'members' CHECK (library_editing IN ('admins', 'members')),
    file_editing    text   NOT NULL DEFAULT 'members' CHECK (file_editing IN ('none', 'admins', 'members')),
    -- Group metadata version, separate from the library version, as on zotero.org.
    version         bigint NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE SEQUENCE group_id_seq START 1;

CREATE TABLE group_members (
    group_id bigint NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    user_id  bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role     text   NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user_idx ON group_members (user_id);

CREATE TABLE api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- sha256 of the key; the plaintext is only shown once at creation.
    key_hash     bytea  NOT NULL UNIQUE,
    key_prefix   text   NOT NULL,
    name         text   NOT NULL,
    access       jsonb  NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
CREATE INDEX api_keys_user_idx ON api_keys (user_id);

CREATE TABLE login_sessions (
    token_hash       bytea  PRIMARY KEY,
    required_user_id bigint,
    client_name      text   NOT NULL DEFAULT '',
    status           text   NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'completed', 'cancelled')),
    user_id          bigint REFERENCES users (id) ON DELETE CASCADE,
    -- The key is created on approval and handed to the polling client once.
    api_key          text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL
);
CREATE INDEX login_sessions_expiry_idx ON login_sessions (expires_at);

CREATE TABLE web_sessions (
    id_hash    bytea  PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token text   NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX web_sessions_expiry_idx ON web_sessions (expires_at);

CREATE TABLE invites (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_hash bytea  NOT NULL UNIQUE,
    email      citext,
    is_admin   boolean NOT NULL DEFAULT false,
    created_by bigint REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_by    bigint REFERENCES users (id) ON DELETE SET NULL,
    used_at    timestamptz
);

CREATE TABLE instance_settings (
    key   text PRIMARY KEY,
    value jsonb NOT NULL
);

-- Library objects. data holds the Zotero JSON (without key/version, which are
-- columns). content_hash is a canonical hash used to report "unchanged" without
-- comparing JSON trees.
CREATE TABLE collections (
    library_id   bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    key          text   NOT NULL,
    version      bigint NOT NULL,
    data         jsonb  NOT NULL,
    content_hash bytea  NOT NULL,
    PRIMARY KEY (library_id, key)
);
CREATE INDEX collections_version_idx ON collections (library_id, version) INCLUDE (key);

CREATE TABLE searches (
    library_id   bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    key          text   NOT NULL,
    version      bigint NOT NULL,
    data         jsonb  NOT NULL,
    content_hash bytea  NOT NULL,
    PRIMARY KEY (library_id, key)
);
CREATE INDEX searches_version_idx ON searches (library_id, version) INCLUDE (key);

CREATE TABLE items (
    library_id   bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    key          text   NOT NULL,
    version      bigint NOT NULL,
    item_type    text   NOT NULL,
    parent_key   text,
    in_trash     boolean NOT NULL DEFAULT false,
    data         jsonb  NOT NULL,
    content_hash bytea  NOT NULL,
    PRIMARY KEY (library_id, key)
);
-- INCLUDE makes format=versions an index-only scan on large libraries.
CREATE INDEX items_version_idx ON items (library_id, version) INCLUDE (key, in_trash, parent_key);
CREATE INDEX items_parent_idx ON items (library_id, parent_key) WHERE parent_key IS NOT NULL;

CREATE TABLE item_tags (
    library_id bigint NOT NULL,
    item_key   text   NOT NULL,
    tag        text   NOT NULL,
    type       smallint NOT NULL DEFAULT 0,
    PRIMARY KEY (library_id, item_key, tag),
    FOREIGN KEY (library_id, item_key) REFERENCES items (library_id, key) ON DELETE CASCADE
);
CREATE INDEX item_tags_tag_idx ON item_tags (library_id, tag);

CREATE TABLE settings (
    library_id bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    key        text   NOT NULL,
    value      jsonb  NOT NULL,
    version    bigint NOT NULL,
    PRIMARY KEY (library_id, key)
);
CREATE INDEX settings_version_idx ON settings (library_id, version);

CREATE TABLE deleted_log (
    library_id  bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    object_type text   NOT NULL CHECK (object_type IN ('collection', 'search', 'item', 'tag', 'setting')),
    key         text   NOT NULL,
    version     bigint NOT NULL,
    -- Lets Prune age entries out; the log itself is ordered by version.
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (library_id, object_type, key)
);
CREATE INDEX deleted_log_version_idx ON deleted_log (library_id, version);
CREATE INDEX deleted_log_created_idx ON deleted_log (created_at);

CREATE TABLE fulltext (
    library_id    bigint NOT NULL,
    item_key      text   NOT NULL,
    content       text   NOT NULL,
    indexed_chars integer,
    total_chars   integer,
    indexed_pages integer,
    total_pages   integer,
    version       bigint NOT NULL,
    PRIMARY KEY (library_id, item_key),
    FOREIGN KEY (library_id, item_key) REFERENCES items (library_id, key) ON DELETE CASCADE
);
CREATE INDEX fulltext_version_idx ON fulltext (library_id, version);

-- Blobs are content-addressed by md5 and shared across libraries. Garbage
-- collection removes blobs no attachment_files row references.
CREATE TABLE blobs (
    md5        text   PRIMARY KEY CHECK (md5 ~ '^[0-9a-f]{32}$'),
    size       bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE attachment_files (
    library_id  bigint NOT NULL,
    item_key    text   NOT NULL,
    md5         text   NOT NULL REFERENCES blobs (md5),
    filename    text   NOT NULL,
    mtime_ms    bigint NOT NULL,
    size        bigint NOT NULL,
    compressed  boolean NOT NULL DEFAULT false,
    PRIMARY KEY (library_id, item_key),
    FOREIGN KEY (library_id, item_key) REFERENCES items (library_id, key) ON DELETE CASCADE
);
CREATE INDEX attachment_files_md5_idx ON attachment_files (md5);

CREATE TABLE upload_tokens (
    upload_key  text   PRIMARY KEY,
    library_id  bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    item_key    text   NOT NULL,
    md5         text   NOT NULL,
    filename    text   NOT NULL,
    size        bigint NOT NULL,
    mtime_ms    bigint NOT NULL,
    compressed  boolean NOT NULL DEFAULT false,
    -- pending -> uploaded (bytes verified and committed) -> registered
    state       text   NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'uploaded', 'registered')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);
CREATE INDEX upload_tokens_md5_idx ON upload_tokens (md5);
CREATE INDEX upload_tokens_expiry_idx ON upload_tokens (expires_at);

CREATE TABLE write_tokens (
    library_id bigint NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    token      text   NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (library_id, token)
);
CREATE INDEX write_tokens_created_idx ON write_tokens (created_at);

CREATE TABLE import_jobs (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id           bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status            text   NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
    -- The zotero.org key is needed until the job finishes and is wiped afterwards.
    source_api_key    text,
    include_groups    boolean NOT NULL,
    include_files     boolean NOT NULL,
    cursor            jsonb  NOT NULL DEFAULT '{}',
    progress          jsonb  NOT NULL DEFAULT '{}',
    error             text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz
);
CREATE INDEX import_jobs_user_idx ON import_jobs (user_id);

-- +goose Down
DROP TABLE import_jobs, write_tokens, upload_tokens, attachment_files, blobs, fulltext,
    deleted_log, settings, item_tags, items, searches, collections, instance_settings,
    invites, web_sessions, login_sessions, api_keys, group_members, groups, users, libraries;
DROP SEQUENCE IF EXISTS user_id_seq, group_id_seq;
