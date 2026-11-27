CREATE TABLE app.jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id UUID NOT NULL REFERENCES app.database_connections(id) ON DELETE CASCADE,
    target_id UUID NOT NULL REFERENCES app.migration_target_state(id) ON DELETE CASCADE,
    request_id TEXT NOT NULL,
    proposal_id UUID NOT NULL REFERENCES app.migration_proposals(id) ON DELETE CASCADE,
    migration_id UUID NOT NULL REFERENCES app.migration_drafts(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision > 0),
    execution_id UUID NOT NULL,
    actor_id UUID NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('apply', 'rollback', 'forward', 'resume', 'compensate')),
    direction TEXT NOT NULL CHECK (direction IN ('up', 'down')),
    mode TEXT NOT NULL DEFAULT 'standard' CHECK (mode IN ('standard', 'batch')),
    source_execution_id UUID,
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'retry_wait', 'succeeded', 'failed', 'partial', 'partially_reverted', 'unknown')),
    execution_status TEXT NOT NULL DEFAULT 'queued' CHECK (execution_status IN ('queued', 'running', 'retry_wait', 'succeeded', 'failed', 'partial', 'partially_reverted', 'unknown')),
    attempt_no INTEGER NOT NULL DEFAULT 1 CHECK (attempt_no >= 1),
    blocks_database BOOLEAN NOT NULL DEFAULT false,
    snapshot JSONB NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    snapshot_hash TEXT NOT NULL,
    approval_fingerprint TEXT,
    title TEXT NOT NULL DEFAULT '',
    traceparent TEXT,
    tracestate TEXT,
    progress JSONB CHECK (progress IS NULL OR jsonb_typeof(progress) = 'object'),
    error_code TEXT,
    error_message TEXT,
    error_retryable BOOLEAN,
    claimed_by TEXT,
    claimed_at TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    lease_token UUID,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    next_retry_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_jobs_database_request UNIQUE (database_id, request_id)
);

CREATE INDEX jobs_claim_idx
    ON app.jobs (status, next_retry_at, created_at)
    WHERE status IN ('queued', 'retry_wait');
CREATE INDEX jobs_database_created_idx
    ON app.jobs (database_id, created_at DESC, id DESC);
CREATE INDEX jobs_target_status_idx
    ON app.jobs (target_id, status);
CREATE INDEX jobs_execution_idx
    ON app.jobs (execution_id);
CREATE INDEX jobs_proposal_idx
    ON app.jobs (proposal_id);

CREATE TABLE app.job_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID REFERENCES app.jobs(id) ON DELETE CASCADE,
    execution_id UUID NOT NULL,
    database_id UUID NOT NULL REFERENCES app.database_connections(id) ON DELETE CASCADE,
    migration_id UUID NOT NULL REFERENCES app.migration_drafts(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision > 0),
    actor_id UUID NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
    attempt_no INTEGER NOT NULL CHECK (attempt_no >= 1),
    status TEXT NOT NULL CHECK (status IN ('running', 'retry_wait', 'succeeded', 'failed', 'partial', 'partially_reverted', 'unknown')),
    sql_entered BOOLEAN NOT NULL DEFAULT false,
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    finished_at TIMESTAMPTZ,
    error_code TEXT,
    error_message TEXT,
    error_retryable BOOLEAN,
    legacy_execution_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX job_attempts_database_started_idx
    ON app.job_attempts (database_id, started_at DESC, id DESC);
CREATE INDEX job_attempts_execution_idx
    ON app.job_attempts (execution_id, attempt_no);
CREATE INDEX job_attempts_job_idx
    ON app.job_attempts (job_id);

CREATE TABLE app.job_statement_evidence (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id UUID NOT NULL REFERENCES app.job_attempts(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    batch_index INTEGER CHECK (batch_index IS NULL OR batch_index >= 0),
    sql_text TEXT NOT NULL,
    safe_parameters JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(safe_parameters) = 'object'),
    outcome TEXT NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'unknown')),
    row_count BIGINT CHECK (row_count IS NULL OR row_count >= 0),
    executed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_statement_evidence_attempt_ordinal UNIQUE (attempt_id, ordinal)
);

CREATE INDEX job_statement_evidence_attempt_idx
    ON app.job_statement_evidence (attempt_id, ordinal);

CREATE VIEW app.migration_jobs AS SELECT * FROM app.jobs;

GRANT SELECT, INSERT, UPDATE, DELETE ON app.jobs TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.job_attempts TO wbd_platform_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON app.job_statement_evidence TO wbd_platform_runtime;
GRANT SELECT ON app.migration_jobs TO wbd_platform_runtime;
