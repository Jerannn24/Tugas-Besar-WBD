ALTER TABLE app.migration_executions
    DROP CONSTRAINT ck_executions_status,
    ADD CONSTRAINT ck_executions_status CHECK (
        (status = 'running' AND finished_at IS NULL AND error_code IS NULL AND error_message IS NULL)
        OR (status = 'unknown' AND finished_at IS NULL)
        OR (status = 'succeeded' AND finished_at IS NOT NULL AND finished_at >= started_at
            AND error_code IS NULL AND error_message IS NULL)
        OR (status = 'failed' AND finished_at IS NOT NULL AND finished_at >= started_at
            AND error_code IS NOT NULL AND error_message IS NOT NULL)
    );

CREATE FUNCTION app.guard_migration_target_state_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_migration_target_identity_immutable',
            MESSAGE = 'physical target identity fields are immutable';
    END IF;
    IF NEW.next_version < OLD.next_version THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_migration_target_counter_monotonic',
            MESSAGE = 'target version counter cannot move backward';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_target_state_update_guard
BEFORE UPDATE ON app.migration_target_state
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_target_state_update();

CREATE FUNCTION app.guard_migration_draft_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.revision <> 1 OR NEW.applied_execution_id IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_initial_state',
            MESSAGE = 'new draft must start at revision one and unapplied';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_draft_insert_guard
BEFORE INSERT ON app.migration_drafts
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_draft_insert();

CREATE OR REPLACE FUNCTION app.guard_migration_execution_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    bound_target uuid;
    target_row app.migration_target_state%ROWTYPE;
    draft_row app.migration_drafts%ROWTYPE;
    actor_username text;
BEGIN
    IF NEW.status <> 'running' OR NEW.finished_at IS NOT NULL
        OR NEW.error_code IS NOT NULL OR NEW.error_message IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_initial_running',
            MESSAGE = 'execution attempt must be reserved as running';
    END IF;

    SELECT execution_target_id INTO bound_target
    FROM app.database_connections
    WHERE id = NEW.database_id AND deleted_at IS NULL
    FOR UPDATE;
    IF NOT FOUND OR bound_target IS NULL OR bound_target <> NEW.execution_target_id THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_target_binding',
            MESSAGE = 'execution requires the active connection physical target binding';
    END IF;

    SELECT * INTO target_row
    FROM app.migration_target_state
    WHERE id = NEW.execution_target_id
    FOR UPDATE;
    IF NOT FOUND OR NEW.version <> target_row.next_version - 1 THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_target_version_reserved',
            MESSAGE = 'execution version must be the most recent target counter allocation';
    END IF;

    SELECT * INTO draft_row
    FROM app.migration_drafts
    WHERE id = NEW.draft_id AND database_id = NEW.database_id
    FOR UPDATE;
    IF NOT FOUND OR draft_row.applied_execution_id IS NOT NULL
        OR draft_row.revision <> NEW.draft_revision
        OR draft_row.title <> NEW.title OR draft_row.sql <> NEW.sql THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_draft_snapshot',
            MESSAGE = 'execution must reserve the current unapplied draft snapshot';
    END IF;

    SELECT username INTO actor_username FROM app.users WHERE id = NEW.executed_by;
    IF NOT FOUND OR actor_username <> NEW.executed_username THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_executions_actor_snapshot',
            MESSAGE = 'execution username must match the actor at reservation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION app.guard_migration_draft_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    execution_row app.migration_executions%ROWTYPE;
BEGIN
    IF OLD.id IS DISTINCT FROM NEW.id
        OR OLD.database_id IS DISTINCT FROM NEW.database_id
        OR OLD.created_by IS DISTINCT FROM NEW.created_by
        OR OLD.created_at IS DISTINCT FROM NEW.created_at THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_identity_immutable',
            MESSAGE = 'draft identity fields are immutable';
    END IF;

    IF OLD.applied_execution_id IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_applied_immutable',
            MESSAGE = 'applied draft is immutable';
    END IF;

    IF NEW.applied_execution_id IS DISTINCT FROM OLD.applied_execution_id THEN
        IF NEW.applied_execution_id IS NULL
            OR NEW.title IS DISTINCT FROM OLD.title
            OR NEW.sql IS DISTINCT FROM OLD.sql
            OR NEW.revision IS DISTINCT FROM OLD.revision
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

    IF EXISTS (SELECT 1 FROM app.migration_executions
               WHERE draft_id = OLD.id AND status IN ('running', 'unknown')) THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_unresolved_immutable',
            MESSAGE = 'draft with unresolved execution cannot be edited';
    END IF;

    IF NEW.revision <> OLD.revision + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'ck_drafts_revision_step',
            MESSAGE = 'draft edit must increment revision by one';
    END IF;
    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION app.guard_migration_target_state_update() FROM PUBLIC;
REVOKE ALL ON FUNCTION app.guard_migration_draft_insert() FROM PUBLIC;
REVOKE ALL ON FUNCTION app.guard_migration_execution_insert() FROM PUBLIC;
REVOKE ALL ON FUNCTION app.guard_migration_draft_update() FROM PUBLIC;
