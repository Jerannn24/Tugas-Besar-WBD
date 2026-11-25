CREATE TABLE app.job_dispatch_authorizations (
    job_id UUID PRIMARY KEY REFERENCES app.jobs(id) ON DELETE CASCADE,
    attempt_id UUID NOT NULL REFERENCES app.job_attempts(id) ON DELETE CASCADE,
    plan JSONB NOT NULL CHECK (jsonb_typeof(plan) = 'object'),
    sql_text TEXT NOT NULL,
    sql_sha256 BYTEA NOT NULL CHECK (octet_length(sql_sha256) = 32),
    target_xid TEXT NOT NULL CHECK (target_xid <> ''),
    target_version BIGINT NOT NULL CHECK (target_version > 0),
    authorized_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

GRANT SELECT, INSERT ON app.job_dispatch_authorizations TO wbd_platform_runtime;
