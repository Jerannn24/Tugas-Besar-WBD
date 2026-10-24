CREATE TABLE app.security_guard (
    name text NOT NULL,
    CONSTRAINT pk_security_guard PRIMARY KEY (name),
    CONSTRAINT ck_security_guard_name CHECK (name = 'identity_admin')
);

INSERT INTO app.security_guard (name) VALUES ('identity_admin');

CREATE TABLE app.users (
    id uuid NOT NULL,
    username text NOT NULL,
    password_hash text NOT NULL,
    is_admin boolean NOT NULL DEFAULT false,
    is_active boolean NOT NULL DEFAULT true,
    auth_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT pk_users PRIMARY KEY (id),
    CONSTRAINT uq_users_username UNIQUE (username),
    CONSTRAINT ck_users_username CHECK (username = btrim(username) AND char_length(username) BETWEEN 3 AND 64),
    CONSTRAINT ck_users_password_hash CHECK (btrim(password_hash) <> ''),
    CONSTRAINT ck_users_auth_version CHECK (auth_version >= 1)
);

CREATE TABLE app.groups (
    id uuid NOT NULL,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT pk_groups PRIMARY KEY (id),
    CONSTRAINT uq_groups_name UNIQUE (name),
    CONSTRAINT ck_groups_name CHECK (name = btrim(name) AND char_length(name) BETWEEN 1 AND 100)
);

CREATE TABLE app.database_connections (
    id uuid NOT NULL,
    name text NOT NULL,
    host text NOT NULL,
    port integer NOT NULL,
    database_name text NOT NULL,
    username text NOT NULL,
    password_ciphertext bytea NOT NULL,
    credential_key_id text NOT NULL,
    ssl_mode text NOT NULL,
    target_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at timestamptz,
    CONSTRAINT pk_database_connections PRIMARY KEY (id),
    CONSTRAINT ck_connections_name CHECK (name = btrim(name) AND char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT ck_connections_host CHECK (host = btrim(host) AND host <> ''),
    CONSTRAINT ck_connections_port CHECK (port BETWEEN 1 AND 65535),
    CONSTRAINT ck_connections_database_name CHECK (database_name = btrim(database_name) AND database_name <> ''),
    CONSTRAINT ck_connections_username CHECK (username = btrim(username) AND username <> ''),
    CONSTRAINT ck_connections_ciphertext CHECK (octet_length(password_ciphertext) > 0),
    CONSTRAINT ck_connections_key_id CHECK (credential_key_id = btrim(credential_key_id) AND credential_key_id <> ''),
    CONSTRAINT ck_connections_ssl_mode CHECK (ssl_mode IN ('disable', 'require', 'verify-full')),
    CONSTRAINT ck_connections_target_key CHECK (target_key = btrim(target_key) AND target_key <> '')
);

CREATE UNIQUE INDEX uq_connections_target_key_active
    ON app.database_connections (target_key) WHERE deleted_at IS NULL;

CREATE TABLE app.group_memberships (
    group_id uuid NOT NULL,
    user_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT pk_group_memberships PRIMARY KEY (group_id, user_id),
    CONSTRAINT fk_memberships_group FOREIGN KEY (group_id) REFERENCES app.groups (id) ON DELETE CASCADE,
    CONSTRAINT fk_memberships_user FOREIGN KEY (user_id) REFERENCES app.users (id) ON DELETE RESTRICT
);

CREATE INDEX ix_memberships_user ON app.group_memberships (user_id, group_id);

CREATE TABLE app.group_database_grants (
    group_id uuid NOT NULL,
    database_id uuid NOT NULL,
    access text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT pk_group_database_grants PRIMARY KEY (group_id, database_id),
    CONSTRAINT fk_grants_group FOREIGN KEY (group_id) REFERENCES app.groups (id) ON DELETE CASCADE,
    CONSTRAINT fk_grants_database FOREIGN KEY (database_id) REFERENCES app.database_connections (id) ON DELETE RESTRICT,
    CONSTRAINT ck_grants_access CHECK (access IN ('view', 'edit'))
);

CREATE INDEX ix_grants_database ON app.group_database_grants (database_id, group_id);

REVOKE ALL ON ALL TABLES IN SCHEMA app FROM PUBLIC;
GRANT USAGE ON SCHEMA app TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE ON app.users TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.groups TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE ON app.database_connections TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.group_memberships TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.group_database_grants TO wbd_platform_runtime;
GRANT SELECT ON app.security_guard TO wbd_platform_runtime;
GRANT UPDATE (name) ON app.security_guard TO wbd_platform_runtime;
GRANT SELECT ON app.schema_migrations TO wbd_platform_runtime;
