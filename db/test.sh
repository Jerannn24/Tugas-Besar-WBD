#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export BATCH_SCOPE_ISOLATED_TEST_TARGET="${BATCH_SCOPE_ISOLATED_TEST_TARGET:-1}"
docker compose --profile test up -d --wait postgres-test
bash db/apply.sh test
bash db/target-profile/apply.sh test a
bash db/target-profile/apply.sh test b
docker compose -f docker-compose.yaml -f docker-compose.test.yaml --profile test run --rm backend-test "$@"
