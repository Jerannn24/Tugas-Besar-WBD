CREATE TABLE app.session_families (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    auth_version bigint NOT NULL,
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    rotated_at timestamptz NOT NULL,
    revoked_at timestamptz DEFAULT NULL,
    CONSTRAINT pk_session_families PRIMARY KEY (id),
    CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES app.users (id) ON DELETE RESTRICT,
    CONSTRAINT ck_sessions_auth_version CHECK (auth_version >= 1),
    CONSTRAINT ck_sessions_absolute CHECK (absolute_expires_at > created_at),
    CONSTRAINT ck_sessions_activity CHECK (created_at <= last_seen_at AND last_seen_at <= absolute_expires_at),
    CONSTRAINT ck_sessions_rotation CHECK (created_at <= rotated_at AND rotated_at <= absolute_expires_at),
    CONSTRAINT ck_sessions_revocation CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

CREATE INDEX ix_sessions_user ON app.session_families (user_id, revoked_at);
CREATE INDEX ix_sessions_expiry ON app.session_families (absolute_expires_at);

CREATE TABLE app.session_tokens (
    token_hash bytea NOT NULL,
    family_id uuid NOT NULL,
    state text NOT NULL,
    created_at timestamptz NOT NULL,
    valid_until timestamptz NOT NULL,
    token_ciphertext bytea DEFAULT NULL,
    token_key_id text DEFAULT NULL,
    CONSTRAINT pk_session_tokens PRIMARY KEY (token_hash),
    CONSTRAINT fk_tokens_family FOREIGN KEY (family_id) REFERENCES app.session_families (id) ON DELETE CASCADE,
    CONSTRAINT ck_tokens_hash_length CHECK (octet_length(token_hash) = 32),
    CONSTRAINT ck_tokens_state CHECK (state IN ('current', 'grace', 'retired')),
    CONSTRAINT ck_tokens_lifetime CHECK (valid_until > created_at),
    CONSTRAINT ck_tokens_recovery CHECK (
        (state = 'current'
            AND token_ciphertext IS NOT NULL AND octet_length(token_ciphertext) > 0
            AND token_key_id IS NOT NULL AND btrim(token_key_id) <> '')
        OR
        (state IN ('grace', 'retired')
            AND token_ciphertext IS NULL AND token_key_id IS NULL)
    )
);

CREATE UNIQUE INDEX uq_tokens_current_family
    ON app.session_tokens (family_id) WHERE state = 'current';
CREATE INDEX ix_tokens_grace_family
    ON app.session_tokens (family_id, valid_until) WHERE state = 'grace';
CREATE INDEX ix_tokens_family ON app.session_tokens (family_id);

REVOKE ALL ON app.session_families, app.session_tokens FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON app.session_families, app.session_tokens TO wbd_platform_runtime;
