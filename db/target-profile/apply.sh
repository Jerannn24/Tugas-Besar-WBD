#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
[[ $# == 2 && ( $1 == test || $1 == dev ) && ( $2 == a || $2 == b ) ]] || fail 'usage: bash db/target-profile/apply.sh test|dev a|b'
environment=$1
slot=$2
service=postgres
compose=(docker compose)
[[ $environment == test ]] && { service=postgres-test; compose+=(--profile test); }
target="${environment}_target_${slot}"
prefix="DB_${environment^^}_TARGET_${slot^^}"
declare -A passwords=()
while IFS='=' read -r key value; do
  value=${value%$'\r'}
  case "$key" in
    "${prefix}_EXECUTOR_PASSWORD"|"${prefix}_PREPARER_PASSWORD"|"${prefix}_VIEWER_PASSWORD")
      [[ -z ${passwords[$key]+present} ]] || fail 'duplicate target password field'
      passwords[$key]=$value ;;
  esac
done < .env
for role in EXECUTOR PREPARER VIEWER; do
  key="${prefix}_${role}_PASSWORD"
  [[ ${passwords[$key]:-} =~ ^[a-fA-F0-9]{64}$ ]] || fail "set $key to a unique 64-character hexadecimal password in .env"
done
[[ ${passwords[${prefix}_EXECUTOR_PASSWORD]} != "${passwords[${prefix}_PREPARER_PASSWORD]}" && ${passwords[${prefix}_EXECUTOR_PASSWORD]} != "${passwords[${prefix}_VIEWER_PASSWORD]}" && ${passwords[${prefix}_PREPARER_PASSWORD]} != "${passwords[${prefix}_VIEWER_PASSWORD]}" ]] || fail 'target passwords must differ'

mkdir -p db/smoke-test
snapshot=$(mktemp db/smoke-test/target-profile.XXXXXX.sql)
trap 'rm -f -- "$snapshot"' EXIT
cp db/target-profile/001_receipts.sql "$snapshot"
checksum=$(sha256sum "$snapshot" | cut -d ' ' -f1)

# Sandi dikirim melalui stdin, bukan argumen proses.
{
  printf '%s\n' "${passwords[${prefix}_EXECUTOR_PASSWORD]}" "${passwords[${prefix}_PREPARER_PASSWORD]}" "${passwords[${prefix}_VIEWER_PASSWORD]}"
  cat <<'SQL'
\set ON_ERROR_STOP on
\getenv target_slot TARGET_SLOT
BEGIN;
SELECT pg_advisory_xact_lock(74102833912027::bigint);
SELECT current_database() = :'expected_target'
       AND pg_get_userbyid(datdba) = 'wbd_schema_owner'
       AND current_setting('server_version_num')::int BETWEEN 160000 AND 169999
       AND current_setting('server_encoding') = 'UTF8'
       AND (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) AS identity_ok
FROM pg_database WHERE datname = current_database() \gset
\if :identity_ok
\else
\echo 'Unexpected target database or bootstrap identity'
SELECT 1 / 0;
\endif
SELECT EXISTS (SELECT FROM pg_namespace WHERE nspname = 'wbd_auth') AS profile_exists \gset
\if :profile_exists
SELECT (SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'wbd_auth') = 'wbd_target_' || :'target_slot' || '_receipt_owner'
       AND (SELECT count(*) = 1 FROM wbd_auth.profile_migrations)
       AND checksum = :'profile_checksum'
       AND (SELECT executor_role = 'wbd_target_' || :'target_slot' || '_executor' FROM wbd_auth.target_marker WHERE singleton) AS checksum_matches
FROM wbd_auth.profile_migrations WHERE version = '001_receipts' \gset
\if :checksum_matches
\echo 'Target profile already applied with matching checksum; no-op.'
\else
\echo 'Target profile checksum mismatch'
SELECT 1 / 0;
\endif
\else
SQL
  cat "$snapshot"
  cat <<'SQL'
INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('001_receipts', :'profile_checksum');
\endif
COMMIT;
SQL
} | "${compose[@]}" exec -T "$service" sh -eu -c '
  IFS= read -r TARGET_EXECUTOR_PASSWORD
  IFS= read -r TARGET_PREPARER_PASSWORD
  IFS= read -r TARGET_VIEWER_PASSWORD
  export TARGET_EXECUTOR_PASSWORD TARGET_PREPARER_PASSWORD TARGET_VIEWER_PASSWORD
  TARGET_SLOT=$2; export TARGET_SLOT
  case "$2" in a) [ "$1" = "$DB_TARGET_A" ] ;; b) [ "$1" = "$DB_TARGET_B" ] ;; esac || exit 1
  PGPASSWORD=$POSTGRES_PASSWORD exec psql -X -q -h "$3" -U "$POSTGRES_USER" -d "$1" -v expected_target="$1" -v profile_checksum="$4"
' sh "$target" "$slot" "$service" "$checksum"
printf 'PASS %s target %s profile applied or already current\n' "$environment" "$slot"
