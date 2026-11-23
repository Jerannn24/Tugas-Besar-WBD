#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
[[ $# == 2 && ( $1 == test || $1 == dev ) && ( $2 == a || $2 == b ) ]] || fail 'usage: bash db/target-profile/apply.sh test|dev a|b'
environment=$1
slot=$2
service=postgres
compose=(docker compose)
test -n "${COMPOSE_PROJECT_NAME:-}" && compose+=(-p "$COMPOSE_PROJECT_NAME")
if [ "$environment" = test ]; then service=postgres-test; compose+=(--profile test); fi
target="${environment}_target_${slot}"
prefix="DB_${environment^^}_TARGET_${slot^^}"
declare -A passwords=()
while IFS='=' read -r key value; do
  value=${value%$'\r'}
  case "$key" in
    "${prefix}_EXECUTOR_PASSWORD"|"${prefix}_PREPARER_PASSWORD"|"${prefix}_VIEWER_PASSWORD")
      test -z "${passwords[$key]+present}" || fail 'duplicate target password field'
      passwords[$key]=$value ;;
  esac
done < .env
for role in EXECUTOR PREPARER VIEWER; do
  key="${prefix}_${role}_PASSWORD"
  [[ ${passwords[$key]:-} =~ ^[a-fA-F0-9]{64}$ ]] || fail "set $key to a unique 64-character hexadecimal password in .env"
done
test "${passwords[${prefix}_EXECUTOR_PASSWORD]}" != "${passwords[${prefix}_PREPARER_PASSWORD]}" && test "${passwords[${prefix}_EXECUTOR_PASSWORD]}" != "${passwords[${prefix}_VIEWER_PASSWORD]}" && test "${passwords[${prefix}_PREPARER_PASSWORD]}" != "${passwords[${prefix}_VIEWER_PASSWORD]}" || fail 'target passwords must differ'

mkdir -p db/smoke-test
snapshot_001=$(mktemp db/smoke-test/target-profile-001.XXXXXX.sql)
snapshot_002=$(mktemp db/smoke-test/target-profile-002.XXXXXX.sql)
snapshot_003=$(mktemp db/smoke-test/target-profile-003.XXXXXX.sql)
snapshot_004=$(mktemp db/smoke-test/target-profile-004.XXXXXX.sql)
snapshot_005=$(mktemp db/smoke-test/target-profile-005.XXXXXX.sql)
trap 'rm -f -- "$snapshot_001" "$snapshot_002" "$snapshot_003" "$snapshot_004" "$snapshot_005"' EXIT
cp db/target-profile/001_receipts.sql "$snapshot_001"
cp db/target-profile/002_batch_checkpoints.sql "$snapshot_002"
cp db/target-profile/003_batch_scopes.sql "$snapshot_003"
cp db/target-profile/004_batch_ticket_reservation.sql "$snapshot_004"
cp db/target-profile/005_batch_compensation.sql "$snapshot_005"
checksum_001=$(sha256sum "$snapshot_001" | cut -d ' ' -f1)
checksum_002=$(sha256sum "$snapshot_002" | cut -d ' ' -f1)
checksum_003=$(sha256sum "$snapshot_003" | cut -d ' ' -f1)
checksum_004=$(sha256sum "$snapshot_004" | cut -d ' ' -f1)
checksum_005=$(sha256sum "$snapshot_005" | cut -d ' ' -f1)
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
SELECT (SELECT count(*) = 1 FROM wbd_auth.profile_migrations WHERE version = '001_receipts' AND checksum = :'checksum_001')
       AND (SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'wbd_auth') = 'wbd_target_' || :'target_slot' || '_receipt_owner'
       AND (SELECT executor_role = 'wbd_target_' || :'target_slot' || '_executor' FROM wbd_auth.target_marker WHERE singleton) AS checksum_001_matches \gset
\if :checksum_001_matches
\else
\echo 'Target profile 001 checksum mismatch'
SELECT 1 / 0;
\endif
\else
SQL
  cat "$snapshot_001"
  cat <<'SQL'
INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('001_receipts', :'checksum_001');
\endif
SELECT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'profile_migrations')
       AND EXISTS (SELECT 1 FROM wbd_auth.profile_migrations WHERE version = '002_batch_checkpoints') AS v002_exists \gset
\if :v002_exists
SELECT (SELECT count(*) = 1 FROM wbd_auth.profile_migrations WHERE version = '002_batch_checkpoints' AND (checksum = :'checksum_002' OR checksum = '0f29beeb481e041c55a2fc30bd49593eee9496cb155c6f749b9e26116ce7f4ab')) AS checksum_002_matches \gset
\if :checksum_002_matches
\else
\echo 'Target profile 002 checksum mismatch'
SELECT 1 / 0;
\endif
SELECT to_regclass('wbd_auth.batch_tickets') IS NULL AS tickets_missing \gset
\if :tickets_missing
CREATE TABLE wbd_auth.batch_tickets (
  execution_id uuid NOT NULL,
  physical_target_id uuid NOT NULL REFERENCES wbd_auth.target_marker(physical_target_id),
  target_xid xid8 NOT NULL,
  batch_index integer NOT NULL CHECK (batch_index >= 0),
  direction text NOT NULL CHECK (direction IN ('up', 'down')),
  capability_sha256 bytea NOT NULL CHECK (pg_catalog.octet_length(capability_sha256) = 32),
  PRIMARY KEY (execution_id, direction, batch_index)
);
SELECT pg_catalog.format('ALTER TABLE wbd_auth.batch_tickets OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON TABLE wbd_auth.batch_tickets FROM PUBLIC;
SELECT pg_catalog.format('GRANT INSERT ON TABLE wbd_auth.batch_tickets TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
\endif
SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'wbd_auth' AND p.proname = 'record_checkpoint' AND pronargs = 5) AS checkpoint_5args_missing \gset
\if :checkpoint_5args_missing
CREATE FUNCTION wbd_auth.record_checkpoint(
  p_execution_id uuid,
  p_batch_index integer,
  p_direction text,
  p_rows_affected bigint,
  p_capability bytea
)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $function$
DECLARE
  marker wbd_auth.target_marker%ROWTYPE;
  ticket wbd_auth.batch_tickets%ROWTYPE;
BEGIN
  SELECT * INTO STRICT marker FROM wbd_auth.target_marker WHERE singleton;
  IF session_user <> marker.executor_role::text OR p_capability IS NULL
     OR pg_catalog.octet_length(p_capability) <> 32 THEN
    RAISE EXCEPTION 'checkpoint authorization rejected' USING ERRCODE = '42501';
  END IF;
  IF p_execution_id IS NULL OR p_batch_index IS NULL OR p_batch_index < 0
     OR p_direction IS NULL OR p_direction NOT IN ('up', 'down')
     OR p_rows_affected IS NULL OR p_rows_affected < 0 THEN
    RAISE EXCEPTION 'invalid checkpoint parameters' USING ERRCODE = '22023';
  END IF;
  SELECT * INTO ticket FROM wbd_auth.batch_tickets
    WHERE execution_id = p_execution_id AND batch_index = p_batch_index AND direction = p_direction;
  IF NOT FOUND OR ticket.target_xid <> pg_catalog.pg_current_xact_id()
     OR ticket.physical_target_id <> marker.physical_target_id
     OR ticket.capability_sha256 <> pg_catalog.sha256(p_capability) THEN
    RAISE EXCEPTION 'checkpoint authorization rejected' USING ERRCODE = '42501';
  END IF;
  INSERT INTO wbd_auth.batch_checkpoints
    (execution_id, physical_target_id, target_xid, batch_index, direction, rows_affected)
  VALUES (ticket.execution_id, ticket.physical_target_id, ticket.target_xid,
          ticket.batch_index, ticket.direction, p_rows_affected);
END
$function$;
SELECT pg_catalog.format('ALTER FUNCTION wbd_auth.record_checkpoint(uuid, integer, text, bigint, bytea) OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON FUNCTION wbd_auth.record_checkpoint(uuid, integer, text, bigint, bytea) FROM PUBLIC;
SELECT pg_catalog.format('GRANT EXECUTE ON FUNCTION wbd_auth.record_checkpoint(uuid, integer, text, bigint, bytea) TO %I', 'wbd_target_' || :'target_slot' || '_executor') \gexec
\endif
\else
SQL
  cat "$snapshot_002"
  cat <<'SQL'
INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('002_batch_checkpoints', :'checksum_002');
\endif
SELECT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'profile_migrations')
       AND EXISTS (SELECT 1 FROM wbd_auth.profile_migrations WHERE version = '003_batch_scopes') AS v003_exists \gset
\if :v003_exists
SELECT (SELECT count(*) = 1 FROM wbd_auth.profile_migrations WHERE version = '003_batch_scopes' AND checksum = :'checksum_003') AS checksum_003_matches \gset
\if :checksum_003_matches
\else
\echo 'Target profile 003 checksum mismatch'
SELECT 1 / 0;
\endif
\else
SQL
  cat "$snapshot_003"
  cat <<'SQL'
INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('003_batch_scopes', :'checksum_003');
\endif
SELECT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'profile_migrations')
       AND EXISTS (SELECT 1 FROM wbd_auth.profile_migrations WHERE version = '004_batch_ticket_reservation') AS v004_exists \gset
\if :v004_exists
SELECT (SELECT count(*) = 1 FROM wbd_auth.profile_migrations WHERE version = '004_batch_ticket_reservation' AND checksum = :'checksum_004') AS checksum_004_matches \gset
\if :checksum_004_matches
\else
\echo 'Target profile 004 checksum mismatch'
SELECT 1 / 0;
\endif
\else
SQL
  cat "$snapshot_004"
  cat <<'SQL'
INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('004_batch_ticket_reservation', :'checksum_004');
\endif
SELECT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'profile_migrations')
       AND EXISTS (SELECT 1 FROM wbd_auth.profile_migrations WHERE version = '005_batch_compensation') AS v005_exists \gset
\if :v005_exists
SELECT (SELECT count(*) = 1 FROM wbd_auth.profile_migrations WHERE version = '005_batch_compensation' AND checksum = :'checksum_005') AS checksum_005_matches \gset
\if :checksum_005_matches
\else
\echo 'Target profile 005 checksum mismatch'
SELECT 1 / 0;
\endif
\else
SQL
  cat "$snapshot_005"
  cat <<'SQL'
INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('005_batch_compensation', :'checksum_005');
\endif
SELECT (SELECT count(*) = 5 FROM wbd_auth.profile_migrations) AS profile_count_ok \gset
\if :profile_count_ok
\else
\echo 'Unexpected profile migrations present'
SELECT 1 / 0;
\endif
\if :profile_exists
\if :v002_exists
\if :v003_exists
\if :v004_exists
\if :v005_exists
\echo 'Target profile already applied with matching checksum; no-op.'
\endif
\endif
\endif
\endif
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
  PGPASSWORD=$POSTGRES_PASSWORD exec psql -X -q -h "$3" -U "$POSTGRES_USER" -d "$1" -v expected_target="$1" -v profile_checksum="$4" -v checksum_001="$4" -v checksum_002="$5" -v checksum_003="$6" -v checksum_004="$7" -v checksum_005="$8"
' sh "$target" "$slot" "$service" "$checksum_001" "$checksum_002" "$checksum_003" "$checksum_004" "$checksum_005"
printf 'PASS %s target %s profile applied or already current\n' "$environment" "$slot"
