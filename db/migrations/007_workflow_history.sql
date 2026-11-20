ALTER TABLE app.migration_drafts
    ADD COLUMN IF NOT EXISTS sql_down TEXT,
    ADD COLUMN IF NOT EXISTS irreversible_reason TEXT,
    ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'standard',
    ADD COLUMN IF NOT EXISTS batch JSONB,
    ADD COLUMN IF NOT EXISTS revision_locked_at TIMESTAMPTZ;

UPDATE app.migration_drafts
SET irreversible_reason = 'Legacy M1 migration: no rollback SQL was recorded.'
WHERE applied_execution_id IS NOT NULL
  AND sql_down IS NULL
  AND irreversible_reason IS NULL;

ALTER TABLE app.migration_drafts
    ADD CONSTRAINT migration_drafts_mode_check
    CHECK (mode IN ('standard', 'batch'));

ALTER TABLE app.migration_drafts
    ADD CONSTRAINT migration_drafts_batch_mode_check
    CHECK (
        (mode = 'standard' AND batch IS NULL)
        OR (mode = 'batch' AND batch IS NOT NULL
            AND jsonb_typeof(batch) = 'object')
    );

ALTER TABLE app.migration_drafts
    ADD CONSTRAINT migration_drafts_irreversible_reason_check
    CHECK (
        (sql_down IS NOT NULL AND length(trim(sql_down)) > 0)
        OR (irreversible_reason IS NOT NULL
            AND length(trim(irreversible_reason)) > 0)
        OR applied_execution_id IS NULL
    );

CREATE OR REPLACE FUNCTION app.guard_migration_draft_revision()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.revision_locked_at IS NOT NULL AND (
        NEW.title IS DISTINCT FROM OLD.title
        OR NEW.sql IS DISTINCT FROM OLD.sql
        OR NEW.sql_down IS DISTINCT FROM OLD.sql_down
        OR NEW.mode IS DISTINCT FROM OLD.mode
        OR NEW.batch IS DISTINCT FROM OLD.batch
        OR NEW.revision IS DISTINCT FROM OLD.revision
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

CREATE TRIGGER migration_drafts_revision_guard
BEFORE UPDATE ON app.migration_drafts
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_draft_revision();

ALTER TABLE app.groups
    ADD COLUMN IF NOT EXISTS leader_user_id UUID
        REFERENCES app.users(id) ON DELETE SET NULL;

CREATE TABLE app.migration_proposals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id UUID NOT NULL REFERENCES app.database_connections(id),
    migration_id UUID NOT NULL REFERENCES app.migration_drafts(id),
    revision INTEGER NOT NULL CHECK (revision > 0),
    action TEXT NOT NULL CHECK (action IN
        ('apply', 'rollback', 'forward', 'resume', 'compensate')),
    source_execution_id UUID REFERENCES app.migration_executions(id),
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN
        ('draft', 'pending_review', 'approved', 'queued', 'superseded')),
    snapshot JSONB NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    snapshot_hash BYTEA NOT NULL CHECK (octet_length(snapshot_hash) = 32),
    approval_fingerprint BYTEA CHECK
        (approval_fingerprint IS NULL OR octet_length(approval_fingerprint) = 32),
    validation JSONB CHECK
        (validation IS NULL OR jsonb_typeof(validation) = 'object'),
    created_by UUID NOT NULL REFERENCES app.users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX migration_proposals_database_created_idx
    ON app.migration_proposals (database_id, created_at DESC, id DESC);
CREATE INDEX migration_proposals_migration_revision_idx
    ON app.migration_proposals (migration_id, revision);
CREATE INDEX migration_proposals_status_idx
    ON app.migration_proposals (database_id, status);
CREATE UNIQUE INDEX migration_proposals_active_unique
    ON app.migration_proposals (database_id, migration_id, revision, action)
    WHERE status <> 'superseded';

CREATE OR REPLACE FUNCTION app.guard_migration_proposal_update()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.database_id IS DISTINCT FROM OLD.database_id
       OR NEW.migration_id IS DISTINCT FROM OLD.migration_id
       OR NEW.revision IS DISTINCT FROM OLD.revision
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.source_execution_id IS DISTINCT FROM OLD.source_execution_id
       OR NEW.snapshot IS DISTINCT FROM OLD.snapshot
       OR NEW.snapshot_hash IS DISTINCT FROM OLD.snapshot_hash
       OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'proposal identity and snapshot are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'draft' AND NEW.status IN ('pending_review', 'superseded'))
        OR (OLD.status = 'pending_review' AND NEW.status IN ('approved', 'superseded'))
        OR (OLD.status = 'approved' AND NEW.status IN ('queued', 'superseded'))
        OR (OLD.status = 'queued' AND NEW.status = 'pending_review')
    ) THEN
        RAISE EXCEPTION 'invalid proposal status transition: % -> %',
            OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;

    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_proposals_update_guard
BEFORE UPDATE ON app.migration_proposals
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_proposal_update();

CREATE OR REPLACE FUNCTION app.guard_migration_proposal_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    v_draft_database_id UUID;
    v_draft_revision INTEGER;
    v_execution_database_id UUID;
BEGIN
    IF NEW.status <> 'draft' THEN
        RAISE EXCEPTION 'new proposal must start in draft status'
            USING ERRCODE = '23514';
    END IF;

    SELECT database_id, revision
      INTO v_draft_database_id, v_draft_revision
      FROM app.migration_drafts
     WHERE id = NEW.migration_id;

    IF NOT FOUND
       OR v_draft_database_id IS DISTINCT FROM NEW.database_id
       OR v_draft_revision IS DISTINCT FROM NEW.revision THEN
        RAISE EXCEPTION 'proposal migration must match database_id and revision'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.source_execution_id IS NOT NULL THEN
        SELECT database_id
          INTO v_execution_database_id
          FROM app.migration_executions
         WHERE id = NEW.source_execution_id;

        IF NOT FOUND
           OR v_execution_database_id IS DISTINCT FROM NEW.database_id THEN
            RAISE EXCEPTION 'proposal source execution must belong to the same database'
                USING ERRCODE = '23514';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_proposals_insert_guard
BEFORE INSERT ON app.migration_proposals
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_proposal_insert();

CREATE TABLE app.migration_proposal_approvals (
    proposal_id UUID NOT NULL REFERENCES app.migration_proposals(id) ON DELETE CASCADE,
    group_id UUID NOT NULL REFERENCES app.groups(id),
    approval_round INTEGER NOT NULL CHECK (approval_round > 0),
    leader_user_id UUID REFERENCES app.users(id),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved')),
    approved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (proposal_id, group_id, approval_round),
    CHECK (
        (status = 'pending' AND approved_at IS NULL)
        OR (status = 'approved' AND approved_at IS NOT NULL)
    )
);

CREATE INDEX migration_proposal_approvals_group_idx
    ON app.migration_proposal_approvals (group_id, status);
CREATE INDEX migration_proposal_approvals_current_idx
    ON app.migration_proposal_approvals
       (proposal_id, approval_round DESC, group_id);

CREATE OR REPLACE FUNCTION app.guard_migration_approval_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status <> 'pending' OR NEW.approved_at IS NOT NULL THEN
        RAISE EXCEPTION 'approval must be inserted as pending without approved_at'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_proposal_approvals_insert_guard
BEFORE INSERT ON app.migration_proposal_approvals
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_approval_insert();

CREATE OR REPLACE FUNCTION app.guard_migration_approval_update()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    v_current_group_leader_id UUID;
BEGIN
    IF NEW.proposal_id IS DISTINCT FROM OLD.proposal_id
       OR NEW.group_id IS DISTINCT FROM OLD.group_id
       OR NEW.approval_round IS DISTINCT FROM OLD.approval_round
       OR NEW.leader_user_id IS DISTINCT FROM OLD.leader_user_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'approval identity is immutable'
            USING ERRCODE = '23514';
    END IF;

    IF OLD.status = 'approved' THEN
        IF NEW.status IS DISTINCT FROM OLD.status
           OR NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
            RAISE EXCEPTION 'approved decision is immutable; create a new round'
                USING ERRCODE = '23514';
        END IF;
    ELSIF OLD.status = 'pending' AND NEW.status = 'approved' THEN
        SELECT leader_user_id
          INTO v_current_group_leader_id
          FROM app.groups
         WHERE id = NEW.group_id;

        IF NEW.leader_user_id IS NULL
           OR v_current_group_leader_id IS NULL
           OR NEW.leader_user_id IS DISTINCT FROM v_current_group_leader_id THEN
            RAISE EXCEPTION 'approval actor must be the current leader of the group'
                USING ERRCODE = '23514';
        END IF;
        NEW.approved_at := clock_timestamp();
    ELSIF NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'invalid approval status transition'
            USING ERRCODE = '23514';
    ELSIF NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
        RAISE EXCEPTION 'approved_at is managed by the database'
            USING ERRCODE = '23514';
    END IF;

    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_proposal_approvals_update_guard
BEFORE UPDATE ON app.migration_proposal_approvals
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_approval_update();

CREATE TABLE app.migration_active_heads (
    database_id UUID PRIMARY KEY REFERENCES app.database_connections(id),
    active_head_execution_id UUID REFERENCES app.migration_executions(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO app.migration_active_heads (database_id, active_head_execution_id)
SELECT id, NULL FROM app.database_connections
ON CONFLICT (database_id) DO NOTHING;

CREATE TABLE app.migration_history_counters (
    database_id UUID PRIMARY KEY REFERENCES app.database_connections(id),
    last_version BIGINT NOT NULL DEFAULT 0 CHECK (last_version >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO app.migration_history_counters (database_id, last_version)
SELECT id, 0 FROM app.database_connections
ON CONFLICT (database_id) DO NOTHING;


CREATE OR REPLACE FUNCTION app.guard_migration_active_head_update()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.database_id IS DISTINCT FROM OLD.database_id THEN
        RAISE EXCEPTION 'active-head database_id is immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.active_head_execution_id IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
           FROM app.migration_history_entries h
           WHERE h.database_id = NEW.database_id
             AND h.execution_id = NEW.active_head_execution_id
       ) THEN
        RAISE EXCEPTION 'active head must reference a committed execution for the same database'
            USING ERRCODE = '23514';
    END IF;

    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_active_heads_update_guard
BEFORE UPDATE ON app.migration_active_heads
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_active_head_update();

CREATE OR REPLACE FUNCTION app.guard_migration_history_counter_update()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.database_id IS DISTINCT FROM OLD.database_id THEN
        RAISE EXCEPTION 'history counter database_id is immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.last_version <> OLD.last_version + 1 THEN
        RAISE EXCEPTION 'history version counter must increase by exactly one'
            USING ERRCODE = '23514';
    END IF;

    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_history_counters_update_guard
BEFORE UPDATE ON app.migration_history_counters
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_history_counter_update();

CREATE TABLE app.migration_history_entries (
    commit_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id UUID,
    execution_id UUID NOT NULL REFERENCES app.migration_executions(id),
    database_id UUID NOT NULL REFERENCES app.database_connections(id),
    migration_id UUID NOT NULL REFERENCES app.migration_drafts(id),
    revision INTEGER NOT NULL CHECK (revision > 0),
    version BIGINT NOT NULL CHECK (version > 0),
    proposal_id UUID REFERENCES app.migration_proposals(id),
    action TEXT NOT NULL CHECK (action IN
        ('apply', 'rollback', 'forward', 'resume', 'compensate')),
    direction TEXT NOT NULL CHECK (direction IN ('up', 'down')),
    batch_index INTEGER CHECK (batch_index IS NULL OR batch_index >= 0),
    row_count BIGINT CHECK (row_count IS NULL OR row_count >= 0),
    receipt TEXT,
    range_start TEXT,
    range_end TEXT,
    safe_parameters JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(safe_parameters) = 'object'),
    executed_by UUID NOT NULL REFERENCES app.users(id),
    committed_at TIMESTAMPTZ NOT NULL,
    sql_snapshot TEXT NOT NULL,
    source_execution_id UUID REFERENCES app.migration_executions(id),
    reverts_execution_id UUID REFERENCES app.migration_executions(id),
    is_complete BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT migration_history_database_version_unique UNIQUE (database_id, version)
);

CREATE UNIQUE INDEX migration_history_commit_identity_unique
    ON app.migration_history_entries
       (execution_id, direction, COALESCE(batch_index, -1));
CREATE INDEX migration_history_database_committed_idx
    ON app.migration_history_entries
       (database_id, committed_at DESC, execution_id DESC, commit_id DESC);
CREATE INDEX migration_history_migration_idx
    ON app.migration_history_entries (migration_id, revision);
CREATE INDEX migration_history_source_execution_idx
    ON app.migration_history_entries (source_execution_id);
CREATE INDEX migration_history_attempt_idx
    ON app.migration_history_entries (attempt_id) WHERE attempt_id IS NOT NULL;

CREATE OR REPLACE FUNCTION app.guard_migration_history_entry_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    v_execution_database_id UUID;
    v_proposal_database_id UUID;
    v_proposal_migration_id UUID;
    v_proposal_revision INTEGER;
    v_draft_database_id UUID;
    v_draft_revision INTEGER;
BEGIN
    SELECT database_id
      INTO v_execution_database_id
      FROM app.migration_executions
     WHERE id = NEW.execution_id;

    IF NOT FOUND OR v_execution_database_id IS DISTINCT FROM NEW.database_id THEN
        RAISE EXCEPTION 'history execution must belong to the same database'
            USING ERRCODE = '23514';
    END IF;

    SELECT database_id, revision
      INTO v_draft_database_id, v_draft_revision
      FROM app.migration_drafts
     WHERE id = NEW.migration_id;

    IF NOT FOUND
       OR v_draft_database_id IS DISTINCT FROM NEW.database_id
       OR v_draft_revision IS DISTINCT FROM NEW.revision THEN
        RAISE EXCEPTION 'history migration must match database_id and revision'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.proposal_id IS NOT NULL THEN
        SELECT database_id, migration_id, revision
          INTO v_proposal_database_id, v_proposal_migration_id, v_proposal_revision
          FROM app.migration_proposals
         WHERE id = NEW.proposal_id;

        IF NOT FOUND
           OR v_proposal_database_id IS DISTINCT FROM NEW.database_id
           OR v_proposal_migration_id IS DISTINCT FROM NEW.migration_id
           OR v_proposal_revision IS DISTINCT FROM NEW.revision THEN
            RAISE EXCEPTION 'history proposal, migration, execution and database must match'
                USING ERRCODE = '23514';
        END IF;
    END IF;

    IF NEW.is_complete IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION 'commit history entry must be complete at insertion'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.committed_at IS NULL
       OR NEW.sql_snapshot IS NULL
       OR length(trim(NEW.sql_snapshot)) = 0
       OR NEW.executed_by IS NULL
       OR NEW.version <= 0 THEN
        RAISE EXCEPTION 'commit history entry is missing required commit evidence'
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER migration_history_entries_insert_guard
BEFORE INSERT ON app.migration_history_entries
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_history_entry_insert();

CREATE OR REPLACE FUNCTION app.guard_migration_history_immutable()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'migration history entries are append-only'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER migration_history_no_update
BEFORE UPDATE OR DELETE ON app.migration_history_entries
FOR EACH ROW EXECUTE FUNCTION app.guard_migration_history_immutable();

GRANT SELECT, INSERT, UPDATE ON app.migration_proposals TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE ON app.migration_proposal_approvals TO wbd_platform_runtime;
GRANT SELECT ON app.migration_active_heads TO wbd_platform_runtime;
GRANT UPDATE (active_head_execution_id)
    ON app.migration_active_heads TO wbd_platform_runtime;
GRANT SELECT ON app.migration_history_counters TO wbd_platform_runtime;
GRANT UPDATE (last_version)
    ON app.migration_history_counters TO wbd_platform_runtime;
GRANT SELECT, INSERT ON app.migration_history_entries TO wbd_platform_runtime;
GRANT UPDATE (
    title, sql, sql_down, irreversible_reason, mode, batch,
    revision, revision_locked_at, updated_at, applied_execution_id
) ON app.migration_drafts TO wbd_platform_runtime;
GRANT UPDATE (leader_user_id) ON app.groups TO wbd_platform_runtime;

