CREATE TABLE app.job_batch_step_authorizations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID NOT NULL REFERENCES app.jobs(id),
    attempt_id UUID NOT NULL REFERENCES app.job_attempts(id),
    actor_id UUID NOT NULL REFERENCES app.users(id),
    job_execution_id UUID NOT NULL,
    scope_execution_id UUID NOT NULL,
    physical_target_id UUID NOT NULL REFERENCES app.migration_target_state(id),
    direction TEXT NOT NULL CHECK (direction IN ('up', 'down')),
    batch_index INTEGER NOT NULL CHECK (batch_index >= 0),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    chunk_ids JSONB NOT NULL CHECK (jsonb_typeof(chunk_ids) = 'array'),
    plan JSONB NOT NULL CHECK (jsonb_typeof(plan) = 'object'),
    sql_text TEXT NOT NULL,
    sql_sha256 BYTEA NOT NULL CHECK (octet_length(sql_sha256) = 32),
    target_xid TEXT NOT NULL CHECK (target_xid <> ''),
    target_version BIGINT NOT NULL CHECK (target_version > 0),
    authorized_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_batch_step_auth_attempt_index UNIQUE (attempt_id, batch_index),
    CONSTRAINT uq_batch_step_auth_target_xid UNIQUE (physical_target_id, target_xid)
);

CREATE INDEX job_batch_step_auth_lookup_idx
    ON app.job_batch_step_authorizations (scope_execution_id, physical_target_id, direction, batch_index);
CREATE TABLE app.job_batch_commit_proofs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    step_auth_id UUID NOT NULL REFERENCES app.job_batch_step_authorizations(id),
    job_id UUID NOT NULL REFERENCES app.jobs(id),
    attempt_id UUID NOT NULL REFERENCES app.job_attempts(id),
    scope_execution_id UUID NOT NULL,
    physical_target_id UUID NOT NULL REFERENCES app.migration_target_state(id),
    direction TEXT NOT NULL CHECK (direction IN ('up', 'down')),
    batch_index INTEGER NOT NULL CHECK (batch_index >= 0),
    target_xid TEXT NOT NULL CHECK (target_xid <> ''),
    row_count BIGINT NOT NULL CHECK (row_count >= 0),
    committed_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_batch_commit_proof_step UNIQUE (step_auth_id),
    CONSTRAINT uq_batch_commit_proof_scope_index UNIQUE (scope_execution_id, direction, batch_index)
);

CREATE INDEX job_batch_commit_proofs_lookup_idx
    ON app.job_batch_commit_proofs (scope_execution_id, physical_target_id, direction, batch_index);
REVOKE DELETE ON app.job_batch_step_authorizations, app.job_batch_commit_proofs FROM wbd_platform_runtime;
GRANT SELECT, INSERT ON app.job_batch_step_authorizations TO wbd_platform_runtime;
GRANT SELECT, INSERT ON app.job_batch_commit_proofs TO wbd_platform_runtime;
