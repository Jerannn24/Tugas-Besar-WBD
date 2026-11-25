DROP VIEW app.migration_jobs;

REVOKE DELETE ON app.jobs, app.job_attempts, app.job_statement_evidence FROM wbd_platform_runtime;

ALTER TABLE app.jobs
    DROP CONSTRAINT jobs_database_id_fkey,
    ADD CONSTRAINT jobs_database_id_fkey FOREIGN KEY (database_id) REFERENCES app.database_connections(id),
    DROP CONSTRAINT jobs_target_id_fkey,
    ADD CONSTRAINT jobs_target_id_fkey FOREIGN KEY (target_id) REFERENCES app.migration_target_state(id),
    DROP CONSTRAINT jobs_proposal_id_fkey,
    ADD CONSTRAINT jobs_proposal_id_fkey FOREIGN KEY (proposal_id) REFERENCES app.migration_proposals(id),
    DROP CONSTRAINT jobs_migration_id_fkey,
    ADD CONSTRAINT jobs_migration_id_fkey FOREIGN KEY (migration_id) REFERENCES app.migration_drafts(id),
    DROP CONSTRAINT jobs_actor_id_fkey,
    ADD CONSTRAINT jobs_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES app.users(id);

ALTER TABLE app.job_attempts
    DROP CONSTRAINT job_attempts_job_id_fkey,
    ADD CONSTRAINT job_attempts_job_id_fkey FOREIGN KEY (job_id) REFERENCES app.jobs(id),
    DROP CONSTRAINT job_attempts_database_id_fkey,
    ADD CONSTRAINT job_attempts_database_id_fkey FOREIGN KEY (database_id) REFERENCES app.database_connections(id),
    DROP CONSTRAINT job_attempts_migration_id_fkey,
    ADD CONSTRAINT job_attempts_migration_id_fkey FOREIGN KEY (migration_id) REFERENCES app.migration_drafts(id),
    DROP CONSTRAINT job_attempts_actor_id_fkey,
    ADD CONSTRAINT job_attempts_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES app.users(id);

ALTER TABLE app.job_statement_evidence
    DROP CONSTRAINT job_statement_evidence_attempt_id_fkey,
    ADD CONSTRAINT job_statement_evidence_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES app.job_attempts(id);

ALTER TABLE app.job_dispatch_authorizations
    DROP CONSTRAINT job_dispatch_authorizations_job_id_fkey,
    ADD CONSTRAINT job_dispatch_authorizations_job_id_fkey FOREIGN KEY (job_id) REFERENCES app.jobs(id),
    DROP CONSTRAINT job_dispatch_authorizations_attempt_id_fkey,
    ADD CONSTRAINT job_dispatch_authorizations_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES app.job_attempts(id);
