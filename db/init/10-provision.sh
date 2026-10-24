#!/bin/sh
set -eu

valid_identifier() {
  [ "${#1}" -le 63 ] && case "$1" in [a-z]* ) case "$1" in *[!a-z0-9_]* ) return 1;; esac;; *) return 1;; esac
}
valid_secret() {
  [ "${#1}" -eq 64 ] && case "$1" in *[!a-fA-F0-9]* ) return 1;; esac
}
for value in "$POSTGRES_USER" "$DB_PLATFORM" "$DB_OWNER" "$DB_RUNTIME" "$DB_TARGET_A" "$DB_TARGET_B"; do
  valid_identifier "$value" || { echo 'database setup requires lowercase SQL identifiers (max 63 characters)' >&2; exit 1; }
done
for value in "$POSTGRES_PASSWORD" "$DB_OWNER_PASSWORD" "$DB_RUNTIME_PASSWORD"; do
  valid_secret "$value" || { echo 'database setup requires six unique 64-character hexadecimal passwords in .env' >&2; exit 1; }
done
[ "$DB_OWNER" = wbd_schema_owner ] && [ "$DB_RUNTIME" = wbd_platform_runtime ] || {
  echo 'database setup requires the platform role names wbd_schema_owner and wbd_platform_runtime' >&2; exit 1;
}
[ "$DB_PLATFORM" != "$DB_TARGET_A" ] && [ "$DB_PLATFORM" != "$DB_TARGET_B" ] && [ "$DB_TARGET_A" != "$DB_TARGET_B" ] || {
  echo 'platform and target database names must be unique' >&2; exit 1;
}

safe_roles=$(psql --username "$POSTGRES_USER" --dbname postgres --tuples-only --no-align --set ON_ERROR_STOP=1 \
  --set owner="$DB_OWNER" --set runtime="$DB_RUNTIME" <<'SQL'
SELECT count(*)
FROM pg_roles
WHERE rolname IN (:'owner', :'runtime')
  AND (NOT rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls);
SQL
)
[ "$safe_roles" = 0 ] || { echo 'existing platform role has unsafe attributes; refusing provisioning' >&2; exit 1; }

safe_memberships=$(psql --username "$POSTGRES_USER" --dbname postgres --tuples-only --no-align --set ON_ERROR_STOP=1 \
  --set owner="$DB_OWNER" --set runtime="$DB_RUNTIME" <<'SQL'
SELECT count(*) FROM pg_auth_members
WHERE roleid IN (SELECT oid FROM pg_roles WHERE rolname IN (:'owner', :'runtime'))
   OR member IN (SELECT oid FROM pg_roles WHERE rolname IN (:'owner', :'runtime'));
SQL
)
[ "$safe_memberships" = 0 ] || { echo 'existing platform role membership found; refusing provisioning' >&2; exit 1; }

safe_databases=$(psql --username "$POSTGRES_USER" --dbname postgres --tuples-only --no-align --set ON_ERROR_STOP=1 \
  --set owner="$DB_OWNER" --set platform="$DB_PLATFORM" --set target_a="$DB_TARGET_A" --set target_b="$DB_TARGET_B" <<'SQL'
SELECT count(*)
FROM pg_database
WHERE datname IN (:'platform', :'target_a', :'target_b')
  AND pg_get_userbyid(datdba) <> :'owner';
SQL
)
[ "$safe_databases" = 0 ] || { echo 'existing platform or target database has an unexpected owner; refusing provisioning' >&2; exit 1; }

psql --username "$POSTGRES_USER" --dbname postgres --set ON_ERROR_STOP=1 \
  --set owner="$DB_OWNER" --set owner_password="$DB_OWNER_PASSWORD" \
  --set runtime="$DB_RUNTIME" --set runtime_password="$DB_RUNTIME_PASSWORD" \
  --set platform="$DB_PLATFORM" --set target_a="$DB_TARGET_A" --set target_b="$DB_TARGET_B" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', :'owner', :'owner_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'owner') \gexec
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', :'runtime', :'runtime_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'runtime') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'platform', :'owner')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'platform') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'target_a', :'owner')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'target_a') \gexec
SELECT format('CREATE DATABASE %I OWNER %I', :'target_b', :'owner')
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'target_b') \gexec
SQL

psql --username "$POSTGRES_USER" --dbname postgres --set ON_ERROR_STOP=1 <<'SQL'
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
REVOKE CONNECT ON DATABASE template1 FROM PUBLIC;
SQL

psql --username "$POSTGRES_USER" --dbname "$DB_PLATFORM" --set ON_ERROR_STOP=1 \
  --set runtime="$DB_RUNTIME" <<'SQL'
SELECT format('REVOKE CONNECT ON DATABASE %I FROM PUBLIC', current_database()) \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO %I', current_database(), :'runtime') \gexec
REVOKE ALL ON SCHEMA public FROM PUBLIC;
SQL
for database in "$DB_TARGET_A" "$DB_TARGET_B"; do
  psql --username "$POSTGRES_USER" --dbname "$database" --set ON_ERROR_STOP=1 <<'SQL'
SELECT format('REVOKE CONNECT ON DATABASE %I FROM PUBLIC', current_database()) \gexec
SQL
done

