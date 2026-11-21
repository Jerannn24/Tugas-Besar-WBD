CREATE TABLE app.job_commit_proofs (
    job_id UUID PRIMARY KEY REFERENCES app.job_dispatch_authorizations(job_id),
    attempt_id UUID NOT NULL REFERENCES app.job_attempts(id),
    target_xid TEXT NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL
);

GRANT SELECT, INSERT ON app.job_commit_proofs TO wbd_platform_runtime;
