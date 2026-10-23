# Local PostgreSQL

This repository runs PostgreSQL 16 in the root Compose project. The default `postgres` service is development. `postgres-test` is opt-in under the `test` profile. Each is an independent server with its own credentials, platform database, two empty target databases, and named data volume. Neither service publishes a host port. Other containers on the Compose network connect using `postgres:5432` or `postgres-test:5432`.

## Start

Create local credentials and start development:

```sh
test -f .env || cp .env.example .env
```

Fill the six blank password fields in `.env` with unique random 64-character hexadecimal values. Keep database values unquoted: SQL identifiers must be lowercase letters, digits, or underscores; passwords must be raw hexadecimal text. The example uses the stable `wbd-local` Compose project name. Then run:

```sh
docker compose config --quiet
docker compose up -d postgres
```

The checked runtime image is pinned as `postgres:16@sha256:1a6ab3f5345eb6dbe04a1349529caabdb0ab09293a09590fad07b2246bfa4b54` (PostgreSQL server 16.15; image ID `sha256:1a6ab3f5345eb6dbe04a1349529caabdb0ab09293a09590fad07b2246bfa4b54` in the verification environment). The health check uses TCP readiness, which becomes available after PostgreSQL finishes its initialization scripts and restarts as the normal server.

Start the isolated test server when needed:

```sh
docker compose --profile test up -d postgres-test
```

Local verification scripts are kept in `db/smoke-test/`, which is ignored by Git. If that folder is present locally, keep both database services running and execute:

```sh
bash db/smoke-test/smoke.sh
```

The check reads version, database ownership, role flags, and storage identity from both services. It authenticates over the Compose network as bootstrap, schema owner, and runtime roles, checks wrong-password rejection and denied runtime access, and writes one sentinel row only to the identified test target to prove reruns preserve data. It does not reset or clean either database.

## Roles and connections

Each server initializes a privileged bootstrap role, `wbd_schema_owner`, and `wbd_platform_runtime`. Bootstrap credentials are only for local initialization/administration. The schema owner owns the platform and the two empty target databases, but is not a request runtime. The runtime role can connect only to the platform database; it has no superuser, database creation, role creation, schema creation, target access, or access to the maintenance databases. It is not a member of the owner role. Target specific viewer/executor roles are not configured here.

Use these values from `.env` for local tools:

| Service | Platform database | Owner role | Runtime role |
| --- | --- | --- | --- |
| Development | `DB_DEV_PLATFORM_DB` | `DB_DEV_OWNER_USER` | `DB_DEV_RUNTIME_USER` |
| Test | `DB_TEST_PLATFORM_DB` | `DB_TEST_OWNER_USER` | `DB_TEST_RUNTIME_USER` |

From another Compose service, use the service hostname above and port `5432`. From a local SQL client, run it inside the service, for example:

```sh
docker compose exec postgres sh -eu -c 'PGPASSWORD="$DB_OWNER_PASSWORD" exec psql -h 127.0.0.1 -U "$DB_OWNER" -d "$DB_PLATFORM"'
```

The schema owner password is `DB_DEV_OWNER_PASSWORD`; the runtime password is `DB_DEV_RUNTIME_PASSWORD`. Keep `.env` private. The example file leaves password fields blank and Compose fails if they are unset.

## Persistence and schema changes

The init script runs only when a service's data directory is empty. Re-running `docker compose up` preserves databases, roles, and data. Editing a password in `.env` does not rotate a password already stored in an initialized volume; use a deliberate PostgreSQL `ALTER ROLE` operation if rotation is required. Adding a service or changing database names likewise does not rename existing resources.

The PostgreSQL init script provisions roles and databases only; it installs no application tables and does not apply application migrations. Apply the application schema explicitly with the command below. The backend connection environment/provider wiring is still a separate integration step; health only proves PostgreSQL provisioning. Keep the test connection fixed to `postgres-test`; do not pass development connection details to tests or cleanup commands.

Apply the platform schema from the repository root after starting the matching service:

```sh
bash db/apply.sh test
bash db/apply.sh dev
```

The command accepts only `test` or `dev`, maps them to the fixed `test_platform` or `dev_platform` database on the corresponding Compose service, and uses that service's `wbd_schema_owner` credentials. It verifies platform identity and ownership, then applies sorted `db/migrations/*.sql` files in a transaction. The runner hashes and executes the same byte-for-byte migration snapshot, without line-ending normalization. Each transaction takes a database advisory lock and records version, SHA-256 checksum, and apply time in `app.schema_migrations`. Reapplying a version with the same checksum is a no-op; a changed checksum or unsafe preexisting schema is rejected. A failed migration rolls back its DDL and ledger row. Released migration files are immutable; add a later numbered file for schema changes.

## Platform identity schema

Migration `001_identity.sql` creates six tables in schema `app`. IDs are caller-supplied UUIDs, timestamps are `timestamptz`, and `created_at`/`updated_at` default to the insert time. Application code updates `updated_at` explicitly.

| Table | Purpose and key rules |
| --- | --- |
| `app.users` | Identity and encoded password hash; trimmed, case-sensitive unique username, 3–64 characters; active/admin flags and `auth_version` starting at 1. |
| `app.groups` | Named groups; trimmed, case-sensitive unique name, 1–100 characters. |
| `app.database_connections` | Connection metadata and encrypted credential bytes/key ID; SSL mode is `disable`, `require`, or `verify-full`; port is 1–65535; active `target_key` values are unique. Soft deletion preserves records. |
| `app.group_memberships` | Group/user composite key; group deletion cascades, user deletion is restricted. |
| `app.group_database_grants` | Group/connection composite key; access is `view` or `edit`; group deletion cascades, connection deletion is restricted. |
| `app.security_guard` | Exactly one `identity_admin` lock row for serialized identity administration. |

The database checks nonblank hashes and ciphertext presence, but does not hash passwords, encrypt credentials, validate cryptographic formats, or calculate `target_key`. Database errors can be mapped using the stable constraint names in the migration: malformed values produce check violations (`23514`), duplicate keys produce unique violations (`23505`), and missing references or restricted deletion produce foreign-key violations (`23503`).

`wbd_schema_owner` owns the schema, migration ledger, and application tables. `wbd_platform_runtime` receives `USAGE` on `app`; `SELECT`, `INSERT`, and `UPDATE` on users and connections; and `SELECT`, `INSERT`, `UPDATE`, and `DELETE` on groups, memberships, and grants. It can `SELECT` the guard and update only its `name` column for row locking; the check constraint preserves its singleton identity. It can read migration versions, but cannot change the ledger, perform hard deletes on users/connections, truncate tables, create schema objects, or assume the owner role. `PUBLIC` receives no application table access. No default privileges grant access to future tables.

## Session persistence

Migration `002_sessions.sql` adds `app.session_families` and `app.session_tokens`. A family records its user, authentication-version snapshot, login/activity/rotation timestamps, absolute expiry, and optional revocation time. A token row stores a SHA-256 identifier hash, its family, `current`/`grace`/`retired` state, creation and validity times, and optional encrypted recovery bytes plus a key identifier.

Session IDs and timestamps are supplied by the application. The database enforces row-local time ordering, positive `auth_version`, a 32-byte token hash, allowed token states, nonempty current-token recovery material, and NULL recovery fields for grace/retired tokens. A partial unique index allows at most one current token per family; multiple grace tokens may coexist.

| Table | Column | Type | Null/default |
| --- | --- | --- | --- |
| `session_families` | `id`, `user_id` | `uuid` | NOT NULL; no default |
| `session_families` | `auth_version` | `bigint` | NOT NULL; no default |
| `session_families` | `created_at`, `last_seen_at`, `absolute_expires_at`, `rotated_at` | `timestamptz` | NOT NULL; no default |
| `session_families` | `revoked_at` | `timestamptz` | NULL; default NULL |
| `session_tokens` | `token_hash` | `bytea` | NOT NULL; no default |
| `session_tokens` | `family_id` | `uuid` | NOT NULL; no default |
| `session_tokens` | `state` | `text` | NOT NULL; no default |
| `session_tokens` | `created_at`, `valid_until` | `timestamptz` | NOT NULL; no default |
| `session_tokens` | `token_ciphertext` | `bytea` | NULL; default NULL |
| `session_tokens` | `token_key_id` | `text` | NULL; default NULL |

Deleting a user referenced by a family is restricted. Owner deletion of a family cascades to its token rows.

The runtime role has `SELECT`, `INSERT`, and `UPDATE` on both session tables so it can lock and update rows. It has no `DELETE`, `TRUNCATE`, DDL, or migration-ledger write privilege. Maintenance cleanup of expired or revoked families is an owner operation; there is no automatic cleanup job. `PUBLIC` receives no session-table access.

The schema does not implement token generation or validation, cryptography, cookies, CSRF, idle/absolute expiry evaluation, user-active/auth-version rechecks, exact-one-current semantics, or lifecycle transaction ordering. Login/family creation and security changes use the `identity_admin` guard before locking user rows; ordinary activity and rotation do not need that global guard. Runtime locks user rows in ID order before family and token rows, then re-reads state. Family revocation and token transitions stay in the same transaction. Use one server-time value after acquiring locks; activity and rotation must never extend `absolute_expires_at`. Recovery ciphertext exists only for the current token and must not contain or expose plaintext identifiers or key material in logs or API data.

The local optional checks exercise PostgreSQL storage and runtime privileges against the test platform. `db/smoke-test/fresh-race.sh` starts only `postgres-test` in a unique temporary Compose project, verifies two concurrent first applies, then stops that container and leaves its named volume in place. Both scripts are ignored by Git and are not required for normal database startup.

To keep data, leave the named volumes in place. Do not use volume removal or reset commands against this project unless intentionally discarding its local databases.
