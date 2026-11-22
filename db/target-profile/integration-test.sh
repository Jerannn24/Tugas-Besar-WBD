#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
project_base="wbd-profile-test-$(basename "$work" | tr '[:upper:].' '[:lower:]x')"
run=0
active=0
cleanup() {
  local status=$?
  if (( active )); then
    if ! "${compose[@]}" stop postgres-test; then
      echo "FAIL: could not stop isolated service for $COMPOSE_PROJECT_NAME" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT
mkdir -p "$work/db/target-profile"
cp "$root/db/target-profile/"{apply.sh,001_receipts.sql,002_batch_checkpoints.sql,003_batch_scopes.sql} "$work/db/target-profile/"
cat > "$work/.env" <<'ENV'
DB_TEST_TARGET_A_EXECUTOR_PASSWORD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DB_TEST_TARGET_A_PREPARER_PASSWORD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
DB_TEST_TARGET_A_VIEWER_PASSWORD=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
ENV
cat > "$work/compose.yaml" <<'YAML'
services:
  postgres-test:
    image: postgres:16@sha256:1a6ab3f5345eb6dbe04a1349529caabdb0ab09293a09590fad07b2246bfa4b54
    network_mode: none
    extra_hosts:
      - "postgres-test:127.0.0.1"
    profiles: [test]
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: integration-only
      POSTGRES_DB: postgres
      DB_TARGET_A: test_target_a
    tmpfs:
      - /var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres -d postgres"]
      interval: 1s
      timeout: 5s
      retries: 60
YAML
cd "$work"
compose=()
psql() { "${compose[@]}" exec -T postgres-test psql -X -q -v ON_ERROR_STOP=1 -U postgres -d "${1:-test_target_a}"; }
start() {
  run=$((run + 1))
  export COMPOSE_PROJECT_NAME="${project_base}-${run}"
  compose=(docker compose -p "$COMPOSE_PROJECT_NAME" --profile test)
  active=1
  "${compose[@]}" up -d --wait postgres-test >/dev/null
  psql postgres <<'SQL'
CREATE ROLE wbd_schema_owner NOLOGIN;
CREATE DATABASE test_target_a OWNER wbd_schema_owner;
SQL
}
stop() {
  "${compose[@]}" stop postgres-test
  active=0
}
apply() { bash db/target-profile/apply.sh test a; }
expect_failure() {
  local expected=$1 output
  if output=$(apply 2>&1); then
    echo "FAIL: expected rejection: $expected" >&2
    exit 1
  fi
  if [[ $output != *"$expected"* ]]; then
    echo "FAIL: expected rejection message: $expected" >&2
    printf '%s\n' "$output" >&2
    exit 1
  fi
}
assert_sql() {
  local result
  result=$(psql <<< "$1")
  if [[ $result != *" t"* ]]; then
    echo "FAIL: SQL assertion: $1" >&2
    exit 1
  fi
}
legacy() {
  local checksum
  checksum=$(sha256sum db/target-profile/001_receipts.sql | cut -d ' ' -f1)
  {
    printf '%s\n' '\set ON_ERROR_STOP on' 'BEGIN;'
    cat db/target-profile/001_receipts.sql
    printf "INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('001_receipts', '%s');\nCOMMIT;\n" "$checksum"
  } | "${compose[@]}" exec -T \
    -e TARGET_SLOT=a \
    -e TARGET_EXECUTOR_PASSWORD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    -e TARGET_PREPARER_PASSWORD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    -e TARGET_VIEWER_PASSWORD=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
    postgres-test psql -X -q -v ON_ERROR_STOP=1 -U postgres -d test_target_a
}
profile_002() {
  local checksum
  checksum=$(sha256sum db/target-profile/002_batch_checkpoints.sql | cut -d ' ' -f1)
  {
    printf '%s\n' '\set ON_ERROR_STOP on' 'BEGIN;'
    cat db/target-profile/002_batch_checkpoints.sql
    printf "INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('002_batch_checkpoints', '%s');\nCOMMIT;\n" "$checksum"
  } | "${compose[@]}" exec -T -e TARGET_SLOT=a postgres-test psql -X -q -v ON_ERROR_STOP=1 -U postgres -d test_target_a
}
checkpoint() {
  psql <<'SQL'
BEGIN;
SET SESSION AUTHORIZATION wbd_target_a_preparer;
INSERT INTO wbd_auth.batch_tickets
  (execution_id, physical_target_id, target_xid, batch_index, direction, capability_sha256)
SELECT '40000000-0000-0000-0000-000000000001', physical_target_id, pg_current_xact_id(),
       0, 'up', sha256(decode(repeat('cd', 32), 'hex'))
FROM wbd_auth.target_marker;
RESET SESSION AUTHORIZATION;
SET SESSION AUTHORIZATION wbd_target_a_executor;
CREATE TABLE managed.checkpoint_fixture (id bigint PRIMARY KEY, processed boolean NOT NULL);
INSERT INTO managed.checkpoint_fixture VALUES (1, true);
SELECT wbd_auth.record_checkpoint('40000000-0000-0000-0000-000000000001', 0, 'up', 1, decode(repeat('cd', 32), 'hex'));
RESET SESSION AUTHORIZATION;
COMMIT;
SQL
}

receipt() {
  local id=$1 version=$2
  psql <<SQL
BEGIN;
SET SESSION AUTHORIZATION wbd_target_a_preparer;
INSERT INTO wbd_auth.execution_tickets
  (execution_id, physical_target_id, database_id, target_xid, version, sql_sha256, actor_id, capability_sha256)
SELECT '$id', physical_target_id, '10000000-0000-0000-0000-000000000001',
       pg_current_xact_id(), $version, sha256('sql'::bytea),
       '20000000-0000-0000-0000-000000000001', sha256(decode(repeat('ab', 32), 'hex'))
FROM wbd_auth.target_marker;
RESET SESSION AUTHORIZATION;
SET SESSION AUTHORIZATION wbd_target_a_executor;
CREATE TABLE managed.receipt_test_$version (id integer);
INSERT INTO managed.receipt_test_$version VALUES ($version);
SELECT wbd_auth.record_receipt('$id', decode(repeat('ab', 32), 'hex'));
RESET SESSION AUTHORIZATION;
COMMIT;
SQL
}
assert_receipt() {
  local id=$1 version=$2
  assert_sql "SET SESSION AUTHORIZATION wbd_target_a_preparer;
    SELECT count(*) = 1 FROM wbd_auth.execution_receipts r
    JOIN wbd_auth.target_marker m USING (physical_target_id)
    WHERE r.execution_id = '$id' AND r.version = $version
      AND r.database_id = '10000000-0000-0000-0000-000000000001'
      AND r.actor_id = '20000000-0000-0000-0000-000000000001'
      AND r.sql_sha256 = sha256('sql'::bytea);"
}

start
apply
assert_sql "SELECT count(*) = 3 FROM wbd_auth.profile_migrations;"
before=$(psql <<< "SELECT row_to_json(m)::text FROM wbd_auth.target_marker m;")
ledger=$(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m;")
output=$(apply)
[[ $output == *'no-op'* ]] || { echo 'FAIL: rerun did not report no-op' >&2; exit 1; }
[[ $(psql <<< "SELECT row_to_json(m)::text FROM wbd_auth.target_marker m;") == "$before" ]]
[[ $(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m;") == "$ledger" ]]
psql <<< "UPDATE wbd_auth.profile_migrations SET checksum = repeat('0', 64) WHERE version = '001_receipts';"
expect_failure 'Target profile 001 checksum mismatch'
psql <<< "UPDATE wbd_auth.profile_migrations SET checksum = '$(sha256sum db/target-profile/001_receipts.sql | cut -d ' ' -f1)' WHERE version = '001_receipts'; UPDATE wbd_auth.profile_migrations SET checksum = repeat('0', 64) WHERE version = '002_batch_checkpoints';"
expect_failure 'Target profile 002 checksum mismatch'
psql <<< "UPDATE wbd_auth.profile_migrations SET checksum = '$(sha256sum db/target-profile/002_batch_checkpoints.sql | cut -d ' ' -f1)' WHERE version = '002_batch_checkpoints'; UPDATE wbd_auth.profile_migrations SET checksum = repeat('0', 64) WHERE version = '003_batch_scopes';"
expect_failure 'Target profile 003 checksum mismatch'
psql <<< "UPDATE wbd_auth.profile_migrations SET checksum = '$(sha256sum db/target-profile/003_batch_scopes.sql | cut -d ' ' -f1)' WHERE version = '003_batch_scopes'; DELETE FROM wbd_auth.profile_migrations WHERE version = '001_receipts';"
expect_failure 'Target profile 001 checksum mismatch'
psql <<< "INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('001_receipts', '$(sha256sum db/target-profile/001_receipts.sql | cut -d ' ' -f1)'), ('unexpected', repeat('0', 64));"
expect_failure 'Unexpected profile migrations present'
psql <<< "DELETE FROM wbd_auth.profile_migrations WHERE version = '003_batch_scopes';"
expect_failure 'Prerequisite profile 001/002 missing or batch_scopes already exists'
psql <<< "INSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('003_batch_scopes', '$(sha256sum db/target-profile/003_batch_scopes.sql | cut -d ' ' -f1)');"
stop

start
legacy
receipt '30000000-0000-0000-0000-000000000001' 1
before=$(psql <<< "SELECT row_to_json(m)::text FROM wbd_auth.target_marker m;")
assert_receipt '30000000-0000-0000-0000-000000000001' 1
old_receipt=$(psql <<< "SET SESSION AUTHORIZATION wbd_target_a_preparer; SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;")
apply
[[ $(psql <<< "SELECT row_to_json(m)::text FROM wbd_auth.target_marker m;") == "$before" ]]
[[ $(psql <<< "SET SESSION AUTHORIZATION wbd_target_a_preparer; SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;") == "$old_receipt" ]]
assert_receipt '30000000-0000-0000-0000-000000000001' 1
assert_sql "SELECT count(*) = 3 FROM wbd_auth.profile_migrations;"
receipt '30000000-0000-0000-0000-000000000002' 2
assert_receipt '30000000-0000-0000-0000-000000000002' 2
assert_sql "SELECT count(*) = 2 FROM wbd_auth.execution_receipts;"
stop

start
legacy
profile_002
receipt '30000000-0000-0000-0000-000000000001' 1
checkpoint
psql <<< "SET SESSION AUTHORIZATION wbd_target_a_executor; CREATE TABLE managed.scope_unrelated_before (id bigint);"
old_receipt=$(psql <<< "SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;")
old_checkpoint=$(psql <<< "SELECT row_to_json(c)::text FROM wbd_auth.batch_checkpoints c;")
old_ledger=$(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m;")
apply
psql <<< "SET SESSION AUTHORIZATION wbd_target_a_executor; CREATE TABLE managed.scope_unrelated_after (id bigint);"
assert_sql "SELECT NOT has_table_privilege('wbd_target_a_receipt_owner', 'managed.scope_unrelated_before', 'SELECT') AND NOT has_table_privilege('wbd_target_a_receipt_owner', 'managed.scope_unrelated_after', 'SELECT');"
[[ $(psql <<< "SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;") == "$old_receipt" ]]
[[ $(psql <<< "SELECT row_to_json(c)::text FROM wbd_auth.batch_checkpoints c;") == "$old_checkpoint" ]]
[[ $(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m WHERE version IN ('001_receipts', '002_batch_checkpoints');") == "$old_ledger" ]]
ledger=$(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m;")
output=$(apply)
[[ $output == *'no-op'* ]]
[[ $(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m;") == "$ledger" ]]
[[ $(psql <<< "SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;") == "$old_receipt" ]]
[[ $(psql <<< "SELECT row_to_json(c)::text FROM wbd_auth.batch_checkpoints c;") == "$old_checkpoint" ]]
assert_sql "SELECT count(*) = 1 FROM managed.checkpoint_fixture WHERE id = 1 AND processed;"
stop

start
legacy
profile_002
receipt '30000000-0000-0000-0000-000000000001' 1
checkpoint
old_receipt=$(psql <<< "SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;")
old_checkpoint=$(psql <<< "SELECT row_to_json(c)::text FROM wbd_auth.batch_checkpoints c;")
old_ledger=$(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m;")
cp db/target-profile/003_batch_scopes.sql "$work/003-original.sql"
printf '\nSELECT 1 / 0;\n' >> db/target-profile/003_batch_scopes.sql
expect_failure 'division by zero'
[[ $(psql <<< "SELECT json_agg(m ORDER BY version)::text FROM wbd_auth.profile_migrations m;") == "$old_ledger" ]]
[[ $(psql <<< "SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;") == "$old_receipt" ]]
[[ $(psql <<< "SELECT row_to_json(c)::text FROM wbd_auth.batch_checkpoints c;") == "$old_checkpoint" ]]
assert_sql "SELECT to_regclass('wbd_auth.batch_scopes') IS NULL AND to_regclass('wbd_auth.batch_scope_items') IS NULL;"
assert_sql "SELECT count(*) = 1 FROM managed.checkpoint_fixture WHERE id = 1 AND processed;"
cp "$work/003-original.sql" db/target-profile/003_batch_scopes.sql
stop

start
legacy
receipt '30000000-0000-0000-0000-000000000001' 1
before=$(psql <<< "SELECT row_to_json(m)::text FROM wbd_auth.target_marker m;")
old_receipt=$(psql <<< "SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;")
printf "\nINSERT INTO wbd_auth.profile_migrations(version, checksum) VALUES ('injected_test', repeat('0', 64));\nSELECT 1 / 0;\n" >> db/target-profile/003_batch_scopes.sql
expect_failure 'division by zero'
assert_sql "SELECT count(*) = 1 AND min(version) = '001_receipts' FROM wbd_auth.profile_migrations;"
assert_sql "SELECT NOT EXISTS (SELECT FROM wbd_auth.profile_migrations WHERE version IN ('injected_test', '002_batch_checkpoints', '003_batch_scopes'));"
assert_sql "SELECT to_regclass('wbd_auth.batch_tickets') IS NULL AND to_regclass('wbd_auth.batch_checkpoints') IS NULL AND to_regclass('wbd_auth.batch_scopes') IS NULL AND to_regclass('wbd_auth.batch_scope_items') IS NULL AND to_regprocedure('wbd_auth.freeze_batch_scope(uuid,uuid,uuid,uuid)') IS NULL;"
[[ $(psql <<< "SELECT row_to_json(m)::text FROM wbd_auth.target_marker m;") == "$before" ]]
[[ $(psql <<< "SELECT row_to_json(r)::text FROM wbd_auth.execution_receipts r;") == "$old_receipt" ]]
receipt '30000000-0000-0000-0000-000000000002' 2
assert_sql "SELECT count(*) = 2 FROM wbd_auth.execution_receipts;"
stop
echo 'PASS: target profile integration'
