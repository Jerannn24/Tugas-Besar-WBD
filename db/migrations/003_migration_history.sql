CREATE TABLE app.migration_target_state (
    id uuid NOT NULL,
    next_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT pk_migration_target_state PRIMARY KEY (id),
    CONSTRAINT ck_migration_target_next_version CHECK (next_version >= 1)
);

ALTER TABLE app.database_connections
    ADD COLUMN execution_target_id uuid DEFAULT NULL,
    ADD CONSTRAINT fk_connections_execution_target
        FOREIGN KEY (execution_target_id) REFERENCES app.migration_target_state (id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uq_connections_execution_target_active
    ON app.database_connections (execution_target_id)
    WHERE deleted_at IS NULL AND execution_target_id IS NOT NULL;

CREATE TABLE app.migration_drafts (
    id uuid NOT NULL,
    database_id uuid NOT NULL,
    title text NOT NULL,
    sql text NOT NULL,
    revision integer NOT NULL DEFAULT 1,
    created_by uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    applied_execution_id uuid DEFAULT NULL,
    CONSTRAINT pk_migration_drafts PRIMARY KEY (id),
    CONSTRAINT uq_migration_drafts_database_id UNIQUE (database_id, id),
    CONSTRAINT fk_drafts_database FOREIGN KEY (database_id)
        REFERENCES app.database_connections (id) ON DELETE RESTRICT,
    CONSTRAINT fk_drafts_creator FOREIGN KEY (created_by)
        REFERENCES app.users (id) ON DELETE RESTRICT,
    CONSTRAINT ck_drafts_title CHECK (title = btrim(title) AND char_length(title) BETWEEN 1 AND 100),
    CONSTRAINT ck_drafts_sql CHECK (btrim(sql) <> '' AND octet_length(sql) <= 262144),
    CONSTRAINT ck_drafts_revision CHECK (revision >= 1),
    CONSTRAINT ck_drafts_timestamps CHECK (updated_at >= created_at)
);

CREATE INDEX ix_migration_drafts_database_updated
    ON app.migration_drafts (database_id, updated_at DESC, id);

CREATE TABLE app.migration_executions (
    id uuid NOT NULL,
    database_id uuid NOT NULL,
    execution_target_id uuid NOT NULL,
    draft_id uuid NOT NULL,
    request_id uuid NOT NULL,
    draft_revision integer NOT NULL,
    version bigint NOT NULL,
    title text NOT NULL,
    sql text NOT NULL,
    sql_sha256 bytea NOT NULL,
    executed_by uuid NOT NULL,
    executed_username text NOT NULL,
    started_at timestamptz NOT NULL,
    finished_at timestamptz DEFAULT NULL,
    status text NOT NULL,
    error_code text DEFAULT NULL,
    error_message text DEFAULT NULL,
    CONSTRAINT pk_migration_executions PRIMARY KEY (id),
    CONSTRAINT uq_executions_draft_identity UNIQUE (database_id, draft_id, id),
    CONSTRAINT uq_executions_request UNIQUE (database_id, request_id),
    CONSTRAINT uq_executions_target_version UNIQUE (execution_target_id, version),
    CONSTRAINT uq_executions_database_version UNIQUE (database_id, version),
    CONSTRAINT fk_executions_database FOREIGN KEY (database_id)
        REFERENCES app.database_connections (id) ON DELETE RESTRICT,
    CONSTRAINT fk_executions_target FOREIGN KEY (execution_target_id)
        REFERENCES app.migration_target_state (id) ON DELETE RESTRICT,
    CONSTRAINT fk_executions_draft_target FOREIGN KEY (database_id, draft_id)
        REFERENCES app.migration_drafts (database_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_executions_actor FOREIGN KEY (executed_by)
        REFERENCES app.users (id) ON DELETE RESTRICT,
    CONSTRAINT ck_executions_draft_revision CHECK (draft_revision >= 1),
    CONSTRAINT ck_executions_version CHECK (version >= 1),
    CONSTRAINT ck_executions_title CHECK (title = btrim(title) AND char_length(title) BETWEEN 1 AND 100),
    CONSTRAINT ck_executions_sql CHECK (btrim(sql) <> '' AND octet_length(sql) <= 262144),
    CONSTRAINT ck_executions_sql_sha256 CHECK (
        octet_length(sql_sha256) = 32 AND sql_sha256 = sha256(convert_to(sql, 'UTF8'))
    ),
    CONSTRAINT ck_executions_actor_username CHECK (
        executed_username = btrim(executed_username) AND executed_username <> ''
    ),
    CONSTRAINT ck_executions_error_pair CHECK (
        (error_code IS NULL AND error_message IS NULL)
        OR
        (error_code IS NOT NULL AND error_message IS NOT NULL
            AND error_code = btrim(error_code) AND char_length(error_code) BETWEEN 1 AND 64
            AND error_message = btrim(error_message) AND char_length(error_message) BETWEEN 1 AND 1024)
    ),
    CONSTRAINT ck_executions_status CHECK (
        (status = 'running' AND finished_at IS NULL AND error_code IS NULL AND error_message IS NULL)
        OR (status = 'unknown' AND finished_at IS NULL)
        OR (status = 'succeeded' AND finished_at >= started_at AND error_code IS NULL AND error_message IS NULL)
        OR (status = 'failed' AND finished_at >= started_at AND error_code IS NOT NULL AND error_message IS NOT NULL)
    )
);

ALTER TABLE app.migration_drafts
    ADD CONSTRAINT fk_drafts_applied_execution
    FOREIGN KEY (database_id, id, applied_execution_id)
    REFERENCES app.migration_executions (database_id, draft_id, id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uq_executions_unresolved_target
    ON app.migration_executions (execution_target_id)
    WHERE status IN ('running', 'unknown');
CREATE UNIQUE INDEX uq_executions_unresolved_draft
    ON app.migration_executions (draft_id)
    WHERE status IN ('running', 'unknown');
CREATE UNIQUE INDEX uq_executions_succeeded_draft
    ON app.migration_executions (draft_id)
    WHERE status = 'succeeded';

CREATE FUNCTION app.guard_migration_execution_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status <> 'running' OR NEW.finished_at IS NOT NULL
        OR NEW.error_code IS NOT NULL OR NEW.error_message IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_initial_running',
            MESSAGE = 'execution attempt must be reserved as running';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_execution_insert_guard
BEFORE INSERT ON app.migration_executions
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_execution_insert();

CREATE FUNCTION app.guard_migration_execution_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.id IS DISTINCT FROM NEW.id
        OR OLD.database_id IS DISTINCT FROM NEW.database_id
        OR OLD.execution_target_id IS DISTINCT FROM NEW.execution_target_id
        OR OLD.draft_id IS DISTINCT FROM NEW.draft_id
        OR OLD.request_id IS DISTINCT FROM NEW.request_id
        OR OLD.draft_revision IS DISTINCT FROM NEW.draft_revision
        OR OLD.version IS DISTINCT FROM NEW.version
        OR OLD.title IS DISTINCT FROM NEW.title
        OR OLD.sql IS DISTINCT FROM NEW.sql
        OR OLD.sql_sha256 IS DISTINCT FROM NEW.sql_sha256
        OR OLD.executed_by IS DISTINCT FROM NEW.executed_by
        OR OLD.executed_username IS DISTINCT FROM NEW.executed_username
        OR OLD.started_at IS DISTINCT FROM NEW.started_at THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_snapshot_immutable',
            MESSAGE = 'execution snapshot fields are immutable';
    END IF;

    IF OLD.status IN ('succeeded', 'failed') THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_terminal_immutable',
            MESSAGE = 'terminal execution is immutable';
    END IF;

    IF NOT ((OLD.status = 'running' AND NEW.status IN ('succeeded', 'failed', 'unknown'))
        OR (OLD.status = 'unknown' AND NEW.status IN ('succeeded', 'failed'))) THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_transition',
            MESSAGE = 'execution status transition is not allowed';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_execution_update_guard
BEFORE UPDATE ON app.migration_executions
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_execution_update();

CREATE FUNCTION app.guard_migration_draft_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    execution_row app.migration_executions%ROWTYPE;
BEGIN
    IF OLD.applied_execution_id IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_applied_immutable',
            MESSAGE = 'applied draft is immutable';
    END IF;

    IF NEW.applied_execution_id IS DISTINCT FROM OLD.applied_execution_id THEN
        IF NEW.applied_execution_id IS NULL
            OR NEW.database_id IS DISTINCT FROM OLD.database_id
            OR NEW.title IS DISTINCT FROM OLD.title
            OR NEW.sql IS DISTINCT FROM OLD.sql
            OR NEW.revision IS DISTINCT FROM OLD.revision
            OR NEW.created_by IS DISTINCT FROM OLD.created_by
            OR NEW.created_at IS DISTINCT FROM OLD.created_at
            OR NEW.updated_at IS DISTINCT FROM OLD.updated_at THEN
            RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_applied_link',
                MESSAGE = 'applied link must be the only draft change';
        END IF;

        SELECT * INTO execution_row
        FROM app.migration_executions
        WHERE id = NEW.applied_execution_id;
        IF NOT FOUND OR execution_row.status <> 'succeeded'
            OR execution_row.database_id <> NEW.database_id
            OR execution_row.draft_id <> NEW.id
            OR execution_row.draft_revision <> NEW.revision
            OR execution_row.title <> NEW.title
            OR execution_row.sql <> NEW.sql THEN
            RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_applied_success',
                MESSAGE = 'applied link must reference a matching successful execution';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.revision <> OLD.revision + 1
        OR NEW.updated_at < OLD.updated_at
        OR (NEW.title IS NOT DISTINCT FROM OLD.title AND NEW.sql IS NOT DISTINCT FROM OLD.sql) THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_revision_step',
            MESSAGE = 'draft edit must change SQL or title and increment revision by one';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_draft_update_guard
BEFORE UPDATE ON app.migration_drafts
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_draft_update();

REVOKE ALL ON app.migration_target_state, app.migration_drafts, app.migration_executions FROM PUBLIC;
REVOKE ALL ON FUNCTION app.guard_migration_execution_insert() FROM PUBLIC;
REVOKE ALL ON FUNCTION app.guard_migration_execution_update() FROM PUBLIC;
REVOKE ALL ON FUNCTION app.guard_migration_draft_update() FROM PUBLIC;
GRANT SELECT, INSERT ON app.migration_target_state TO wbd_platform_runtime;
GRANT UPDATE (next_version) ON app.migration_target_state TO wbd_platform_runtime;
GRANT SELECT, INSERT ON app.migration_drafts TO wbd_platform_runtime;
GRANT UPDATE (title, sql, revision, updated_at, applied_execution_id)
    ON app.migration_drafts TO wbd_platform_runtime;
GRANT SELECT, INSERT ON app.migration_executions TO wbd_platform_runtime;
GRANT UPDATE (status, finished_at, error_code, error_message)
    ON app.migration_executions TO wbd_platform_runtime;
