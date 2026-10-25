ALTER TABLE app.database_connections
    ADD COLUMN executor_username text DEFAULT NULL,
    ADD COLUMN executor_password_ciphertext bytea DEFAULT NULL,
    ADD COLUMN preparer_username text DEFAULT NULL,
    ADD COLUMN preparer_password_ciphertext bytea DEFAULT NULL,
    ADD CONSTRAINT ck_connections_executor_credential CHECK (
        (executor_username IS NULL AND executor_password_ciphertext IS NULL)
        OR (executor_username IS NOT NULL AND executor_password_ciphertext IS NOT NULL
            AND executor_username = btrim(executor_username) AND executor_username <> ''
            AND octet_length(executor_password_ciphertext) > 0)
    ),
    ADD CONSTRAINT ck_connections_preparer_credential CHECK (
        (preparer_username IS NULL AND preparer_password_ciphertext IS NULL)
        OR (preparer_username IS NOT NULL AND preparer_password_ciphertext IS NOT NULL
            AND preparer_username = btrim(preparer_username) AND preparer_username <> ''
            AND octet_length(preparer_password_ciphertext) > 0)
    ),
    ADD CONSTRAINT ck_connections_target_roles_together CHECK (
        (executor_username IS NULL) = (preparer_username IS NULL)
    );
