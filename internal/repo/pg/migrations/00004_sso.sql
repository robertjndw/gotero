-- +goose Up
-- Links an external (OIDC or SAML) account to a gotero user. A user has at
-- most one identity per provider, and an external account maps to one user.
CREATE TABLE user_identities (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider      text   NOT NULL,
    subject       text   NOT NULL,
    email         citext,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    UNIQUE (provider, subject),
    UNIQUE (user_id, provider)
);

CREATE INDEX user_identities_user_idx ON user_identities (user_id);

-- Server-side state of an SSO round trip. Only hashes of the state value and
-- the browser binding cookie are stored.
CREATE TABLE sso_states (
    id_hash       bytea PRIMARY KEY,
    provider      text  NOT NULL,
    binding_hash  bytea NOT NULL,
    nonce         text  NOT NULL DEFAULT '',
    pkce_verifier text  NOT NULL DEFAULT '',
    request_id    text  NOT NULL DEFAULT '',
    invite_hash   bytea,
    link_user_id  bigint REFERENCES users (id) ON DELETE CASCADE,
    return_to     text  NOT NULL DEFAULT '',
    identity      jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);

CREATE INDEX sso_states_expiry_idx ON sso_states (expires_at);

-- +goose Down
DROP TABLE sso_states;
DROP TABLE user_identities;
