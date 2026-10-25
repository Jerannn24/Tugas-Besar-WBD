#!/usr/bin/env bash
set -euo pipefail
required=(DB_TEST_BOOTSTRAP_USER DB_TEST_BOOTSTRAP_PASSWORD DB_TEST_RUNTIME_USER DB_TEST_RUNTIME_PASSWORD DB_TEST_OWNER_USER DB_TEST_OWNER_PASSWORD DB_TEST_PLATFORM_DB DB_TEST_TARGET_A DB_TEST_TARGET_A_EXECUTOR_PASSWORD DB_TEST_TARGET_A_PREPARER_PASSWORD DB_TEST_TARGET_A_VIEWER_PASSWORD DB_TEST_TARGET_B_EXECUTOR_PASSWORD DB_TEST_TARGET_B_PREPARER_PASSWORD DB_TEST_TARGET_B_VIEWER_PASSWORD)
for name in "${required[@]}"; do [[ -n ${!name:-} ]] || { echo "Missing test configuration: $name" >&2; exit 1; }; done
runtime="postgres://${DB_TEST_RUNTIME_USER}:${DB_TEST_RUNTIME_PASSWORD}@postgres-test:5432/${DB_TEST_PLATFORM_DB}?sslmode=disable"
owner="postgres://${DB_TEST_OWNER_USER}:${DB_TEST_OWNER_PASSWORD}@postgres-test:5432/${DB_TEST_PLATFORM_DB}?sslmode=disable"
export AUTH_TEST_DATABASE_URL="$runtime" AUTH_TEST_CLEANUP_DATABASE_URL="$owner" ADMIN_TEST_DATABASE_URL="$runtime" AUTHZ_TEST_DATABASE_URL="$runtime" CONNECTIONS_TEST_DATABASE_URL="$runtime"
export MIGRATION_TEST_TARGET_CLEANUP_URL="postgres://${DB_TEST_BOOTSTRAP_USER}:${DB_TEST_BOOTSTRAP_PASSWORD}@postgres-test:5432/${DB_TEST_TARGET_A}?sslmode=disable"
export HISTORY_TEST_DATABASE_URL="$owner" MIGRATION_TEST_PLATFORM_URL="$runtime" MIGRATION_TEST_OWNER_URL="$owner"
export MIGRATION_TEST_TARGET_EXECUTOR_URL="postgres://wbd_target_a_executor:${DB_TEST_TARGET_A_EXECUTOR_PASSWORD}@postgres-test:5432/${DB_TEST_TARGET_A}?sslmode=disable"
export MIGRATION_TEST_TARGET_PREPARER_URL="postgres://wbd_target_a_preparer:${DB_TEST_TARGET_A_PREPARER_PASSWORD}@postgres-test:5432/${DB_TEST_TARGET_A}?sslmode=disable"
export MIGRATION_TEST_TARGET_VIEWER_URL="postgres://wbd_target_a_viewer:${DB_TEST_TARGET_A_VIEWER_PASSWORD}@postgres-test:5432/${DB_TEST_TARGET_A}?sslmode=disable"
exec go test -mod=readonly -count=1 -p 1 -timeout 10m -v "${@:-./...}"
