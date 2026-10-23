#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
[[ $# == 1 && ( $1 == test || $1 == dev ) ]] || fail 'usage: bash db/apply.sh test|dev'
environment=$1
service=postgres
[[ $environment == test ]] && service=postgres-test
snapshot=
trap '[[ -z ${snapshot:-} ]] || rm -f -- "$snapshot"' EXIT

# Layanan Compose menentukan database platform dan kredensial pemiliknya.
compose=(docker compose)
[[ $service == postgres-test ]] && compose+=(--profile test)
"${compose[@]}" ps --status running "$service" | grep -q "$service" || fail "$service is not running"

container=$("${compose[@]}" ps -q "$service")
[[ -n $container ]] || fail "cannot identify $service container"
platform=$("${compose[@]}" exec -T "$service" sh -eu -c 'printf %s "$DB_PLATFORM"')
owner=$("${compose[@]}" exec -T "$service" sh -eu -c 'printf %s "$DB_OWNER"')
expected_platform=dev_platform
[[ $environment == test ]] && expected_platform=test_platform
[[ $owner == wbd_schema_owner && $platform == "$expected_platform" ]] || fail 'unexpected platform identity in selected Compose service'

for migration in db/migrations/*.sql; do
  [[ -f $migration ]] || fail 'no migration files found'
  version=${migration##*/}
  version=${version%.sql}
  snapshot=$(mktemp)
  cp -- "$migration" "$snapshot"
  checksum=$(sha256sum "$snapshot" | cut -d ' ' -f1)

  # Hash dan eksekusi memakai salinan yang sama.
  {
    cat <<'SQL'
\set ON_ERROR_STOP on
BEGIN;
SELECT pg_advisory_xact_lock(74102833912026::bigint);
SELECT (current_database() = :'expected_platform'
        AND current_user = :'expected_owner'
        AND pg_get_userbyid((SELECT datdba FROM pg_database WHERE datname = current_database())) = :'expected_owner') AS identity_ok \gset
\if :identity_ok
\else
\echo 'FAIL: selected platform database or owner identity mismatch'
SELECT 1 / 0;
\endif
SELECT EXISTS (
  SELECT 1 FROM pg_roles r WHERE r.rolname = 'wbd_platform_runtime' AND r.rolcanlogin
    AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls
    AND NOT EXISTS (SELECT FROM pg_auth_members m WHERE m.roleid = r.oid OR m.member = r.oid)
) AS runtime_ok \gset
\if :runtime_ok
\else
\echo 'FAIL: platform runtime role is missing or has unsafe flags/membership'
SELECT 1 / 0;
\endif
SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'app') AS app_exists \gset
\if :app_exists
SELECT pg_get_userbyid(nspowner) = :'expected_owner' AS schema_owned
FROM pg_namespace WHERE nspname = 'app' \gset
\if :schema_owned
\else
\echo 'FAIL: existing app schema has unexpected owner'
SELECT 1 / 0;
\endif
SELECT to_regclass('app.schema_migrations') IS NOT NULL AS ledger_exists \gset
\if :ledger_exists
SELECT pg_get_userbyid(c.relowner) = :'expected_owner' AS ledger_owned
FROM pg_class c WHERE c.oid = 'app.schema_migrations'::regclass \gset
SELECT (SELECT relkind = 'r' AND pg_get_userbyid(relowner) = :'expected_owner'
        FROM pg_class WHERE oid = 'app.schema_migrations'::regclass)
       AND (SELECT array_agg(attname::text || ':' || format_type(atttypid, atttypmod) || ':' || attnotnull::text ORDER BY attnum)
                   = ARRAY['version:text:true','checksum:text:true','applied_at:timestamp with time zone:true']
            FROM pg_attribute WHERE attrelid = 'app.schema_migrations'::regclass AND attnum > 0 AND NOT attisdropped)
       AND EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'app.schema_migrations'::regclass
                   AND contype = 'p' AND conkey = ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid = 'app.schema_migrations'::regclass AND attname = 'version')]::smallint[])
       AND EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'app.schema_migrations'::regclass
                   AND contype = 'c' AND pg_get_constraintdef(oid) ~ 'checksum.*[0-9a-f].*64')
       AS ledger_valid \gset
\if :ledger_valid
\else
\echo 'FAIL: existing migration ledger has unexpected structure or owner'
SELECT 1 / 0;
\endif
\if :ledger_owned
\else
\echo 'FAIL: existing migration ledger has unexpected owner'
SELECT 1 / 0;
\endif
\else
\echo 'FAIL: app schema exists without the expected migration ledger; refusing takeover'
SELECT 1 / 0;
\endif
\else
SELECT format('CREATE SCHEMA app AUTHORIZATION %I', :'expected_owner') \gexec
CREATE TABLE app.schema_migrations (
    version text PRIMARY KEY,
    checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    applied_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
REVOKE ALL ON app.schema_migrations FROM PUBLIC;
GRANT SELECT ON app.schema_migrations TO wbd_platform_runtime;
\endif
SELECT EXISTS (SELECT 1 FROM app.schema_migrations WHERE version = :'migration_version') AS version_exists \gset
\if :version_exists
SELECT checksum = :'migration_checksum' AS checksum_matches
FROM app.schema_migrations WHERE version = :'migration_version' \gset
\if :checksum_matches
\echo 'Migration already applied with matching checksum; no-op.'
COMMIT;
\else
\echo 'FAIL: migration checksum differs from the applied version'
SELECT 1 / 0;
\endif
\else
\set apply_migration true
SQL
    cat "$snapshot"
    cat <<'SQL'
INSERT INTO app.schema_migrations (version, checksum)
VALUES (:'migration_version', :'migration_checksum');
COMMIT;
\endif
SQL
  } | "${compose[@]}" exec -T "$service" sh -eu -c \
      'PGPASSWORD="$DB_OWNER_PASSWORD" exec psql -X -h "$1" -U "$2" -d "$3" -v expected_platform="$3" -v expected_owner="$2" -v migration_version="$4" -v migration_checksum="$5"' \
      sh "$service" "$owner" "$platform" "$version" "$checksum"
  rm -f -- "$snapshot"
done
printf 'PASS %s platform migrations applied or already current\n' "$environment"
