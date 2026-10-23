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

To run the real PostgreSQL check, keep both database services running and execute:

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

Later versioned application schema files should be applied explicitly to the intended platform database with the schema owner after confirming the target service and database. This setup installs no application tables and does not apply application migrations. The backend connection environment/provider wiring is still a separate integration step; health only proves PostgreSQL provisioning. Keep the test connection fixed to `postgres-test`; do not pass development connection details to tests or cleanup commands.

To keep data, leave the named volumes in place. Do not use volume removal or reset commands against this project unless intentionally discarding its local databases.
