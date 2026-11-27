CREATE OR REPLACE FUNCTION app.guard_migration_draft_revision()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.revision_locked_at IS NOT NULL OR EXISTS (
        SELECT FROM app.migration_history_entries h
        WHERE h.database_id = OLD.database_id AND h.migration_id = OLD.id AND h.revision = OLD.revision
          AND h.job_id IS NOT NULL AND h.action = 'apply' AND h.direction = 'up'
    )) AND (
        NEW.title IS DISTINCT FROM OLD.title
        OR NEW.sql IS DISTINCT FROM OLD.sql
        OR NEW.sql_down IS DISTINCT FROM OLD.sql_down
        OR NEW.mode IS DISTINCT FROM OLD.mode
        OR NEW.batch IS DISTINCT FROM OLD.batch
        OR NEW.revision IS DISTINCT FROM OLD.revision
        OR NEW.revision_locked_at IS DISTINCT FROM OLD.revision_locked_at
    ) THEN
        RAISE EXCEPTION 'executed migration revision is immutable'
            USING ERRCODE = '23514';
    END IF;

    IF OLD.revision_locked_at IS NOT NULL
       AND NEW.revision_locked_at IS DISTINCT FROM OLD.revision_locked_at THEN
        RAISE EXCEPTION 'migration revision lock is immutable'
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TABLE app.job_execution_versions (
    execution_id UUID PRIMARY KEY,
    target_id UUID NOT NULL REFERENCES app.migration_target_state(id),
    version BIGINT NOT NULL CHECK (version > 0),
    UNIQUE (target_id, version)
);
GRANT SELECT, INSERT ON app.job_execution_versions TO wbd_platform_runtime;

ALTER TABLE app.migration_history_entries
    DROP CONSTRAINT migration_history_entries_execution_id_fkey,
    ADD COLUMN job_id UUID REFERENCES app.jobs(id),
    ADD COLUMN target_version BIGINT CHECK (target_version > 0),
    ADD COLUMN executed_username TEXT,
    ADD COLUMN legacy_execution_id UUID GENERATED ALWAYS AS (
        CASE WHEN job_id IS NULL THEN execution_id END
    ) STORED REFERENCES app.migration_executions(id),
    ADD CONSTRAINT migration_history_backing_check CHECK (
        (job_id IS NULL AND target_version IS NULL AND executed_username IS NULL AND is_complete)
        OR (job_id IS NOT NULL AND attempt_id IS NOT NULL AND proposal_id IS NOT NULL
            AND target_version IS NOT NULL AND executed_username IS NOT NULL
            AND (batch_index IS NOT NULL OR is_complete))
    );

CREATE OR REPLACE FUNCTION app.guard_migration_history_entry_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.job_id IS NULL THEN
        IF NOT EXISTS (
            SELECT FROM app.migration_executions e
            WHERE e.id = NEW.execution_id AND e.database_id = NEW.database_id
        ) THEN
            RAISE EXCEPTION 'history execution must belong to the same database'
                USING ERRCODE = '23514';
        END IF;
        IF NOT EXISTS (
            SELECT FROM app.migration_drafts d
            WHERE d.id = NEW.migration_id AND d.database_id = NEW.database_id AND d.revision = NEW.revision
        ) THEN
            RAISE EXCEPTION 'history migration must match database_id and revision'
                USING ERRCODE = '23514';
        END IF;
    ELSIF NOT EXISTS (
        SELECT FROM app.jobs j
        JOIN app.job_attempts a ON a.id = NEW.attempt_id AND a.job_id = j.id
            AND a.execution_id = j.execution_id AND a.database_id = j.database_id
            AND a.migration_id = j.migration_id AND a.revision = j.revision AND a.actor_id = j.actor_id
        WHERE j.id = NEW.job_id AND j.execution_id = NEW.execution_id AND j.database_id = NEW.database_id
            AND j.proposal_id = NEW.proposal_id AND j.migration_id = NEW.migration_id AND j.revision = NEW.revision
            AND j.actor_id = NEW.executed_by AND j.action = NEW.action AND j.direction = NEW.direction
            AND j.source_execution_id IS NOT DISTINCT FROM NEW.source_execution_id
            AND (j.mode = 'batch') = (NEW.batch_index IS NOT NULL)
    ) THEN
        RAISE EXCEPTION 'history job and attempt must match the committed execution'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.proposal_id IS NOT NULL AND NOT EXISTS (
        SELECT FROM app.migration_proposals p
        WHERE p.id = NEW.proposal_id AND p.database_id = NEW.database_id
            AND p.migration_id = NEW.migration_id AND p.revision = NEW.revision
    ) THEN
        RAISE EXCEPTION 'history proposal, migration, execution and database must match'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.committed_at IS NULL OR NEW.sql_snapshot IS NULL OR length(trim(NEW.sql_snapshot)) = 0
       OR NEW.executed_by IS NULL OR NEW.version <= 0 THEN
        RAISE EXCEPTION 'commit history entry is missing required commit evidence'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

-- The head references complete append-only history, not a legacy attempt row.
ALTER TABLE app.migration_active_heads DROP CONSTRAINT migration_active_heads_active_head_execution_id_fkey;
CREATE OR REPLACE FUNCTION app.guard_migration_active_head_update()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.database_id IS DISTINCT FROM OLD.database_id THEN
        RAISE EXCEPTION 'active-head database_id is immutable'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.active_head_execution_id IS NOT NULL AND NOT EXISTS (
        SELECT FROM app.migration_history_entries h
        WHERE h.database_id = NEW.database_id AND h.execution_id = NEW.active_head_execution_id AND h.is_complete
    ) THEN
        RAISE EXCEPTION 'active head must reference a completed execution for the same database'
            USING ERRCODE = '23514';
    END IF;
    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$;
DROP TRIGGER migration_active_heads_update_guard ON app.migration_active_heads;
CREATE TRIGGER migration_active_heads_update_guard
BEFORE INSERT OR UPDATE ON app.migration_active_heads
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_active_head_update();

GRANT INSERT (database_id) ON app.migration_active_heads TO wbd_platform_runtime;
GRANT INSERT (database_id) ON app.migration_history_counters TO wbd_platform_runtime;
