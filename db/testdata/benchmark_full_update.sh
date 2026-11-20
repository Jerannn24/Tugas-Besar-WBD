#!/usr/bin/env bash
set -euo pipefail

default_rows=1000000
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
usage='usage: bash db/testdata/benchmark_full_update.sh [run|seed|reset] [rows] [runs]'
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

mode=${1:-run}
rows=${2:-$default_rows}
runs=${3:-3}
[[ $mode == run || $mode == seed || $mode == reset ]] || fail "$usage"
[[ $rows =~ ^[1-9][0-9]{0,8}$ ]] || fail 'rows must be a positive integer (max 9 digits)'
[[ $runs =~ ^[1-9][0-9]?$ ]] || fail 'runs must be 1-99'
[[ -f .env ]] || fail '.env not found in repository root'

service=postgres
role=wbd_target_b_executor
compose=(docker compose)
db=
password=
while IFS='=' read -r key value; do
  value=${value%$'\r'}
  case "$key" in
    DB_DEV_TARGET_B) db=$value ;;
    DB_DEV_TARGET_B_EXECUTOR_PASSWORD) password=$value ;;
  esac
done < .env
[[ $db =~ ^[a-z][a-z0-9_]{0,62}$ ]] || fail 'set DB_DEV_TARGET_B in .env'
[[ $password =~ ^[a-fA-F0-9]{64}$ ]] || fail 'set DB_DEV_TARGET_B_EXECUTOR_PASSWORD in .env'
"${compose[@]}" ps --status running "$service" | grep -q "$service" || fail "$service is not running (docker compose up -d $service)"

# Sandi lewat stdin; -h memakai jalur TCP berpassword, bukan trust lokal.
run_sql() {
  { printf '%s\n' "$password"; cat "$@"; } | "${compose[@]}" exec -T "$service" sh -eu -c '
    IFS= read -r PGPASSWORD; export PGPASSWORD
    exec psql -X -q -tA -v ON_ERROR_STOP=1 -h "$1" -U "$2" -d "$3" -v expected_db="$3" -v rows="$4"
  ' sh "$service" "$role" "$db" "$rows"
}

result() { sed -n "s/^RESULT .*$1=\([^ ]*\).*/\1/p" | tail -n 1; }

seed_and_reset() {
  local out
  out=$(run_sql db/testdata/batch_items_seed.sql db/testdata/batch_items_reset.sql) || fail 'seed/reset failed'
  printf '%s\n' "$out" | result reset_ms
}

if [[ $mode != run ]]; then
  ms=$(seed_and_reset)
  printf 'PASS %s rows=%s in %s.managed.batch_items (%s ms)\n' "$mode" "$rows" "$db" "$ms"
  exit 0
fi

query='UPDATE managed.batch_items SET processed = TRUE;'
mkdir -p db/testdata/results
report="db/testdata/results/full_update_$(date -u +%Y%m%dT%H%M%SZ)_${rows}.txt"
exec > >(tee "$report") 2>&1

container_info=$("${compose[@]}" exec -T "$service" sh -c 'printf "%s cpu, %s kB RAM" "$(nproc)" "$(sed -n "s/^MemTotal: *\([0-9]*\).*/\1/p" /proc/meminfo)"')
image=$(docker inspect --format '{{.Config.Image}}' "$("${compose[@]}" ps -q "$service")")
settings=$(printf '%s\n' "SELECT version();" \
  "SELECT 'shared_buffers=' || current_setting('shared_buffers') || ' work_mem=' || current_setting('work_mem') || ' max_wal_size=' || current_setting('max_wal_size') || ' synchronous_commit=' || current_setting('synchronous_commit') || ' fsync=' || current_setting('fsync') || ' wal_level=' || current_setting('wal_level');" \
  | run_sql)

printf '== Lingkungan ==\n'
printf 'waktu (UTC)     : %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'host kernel     : %s\n' "$(uname -sr)"
printf 'host CPU        : %s (%s logical)\n' "$(sed -n 's/^model name[[:space:]]*: //p' /proc/cpuinfo | head -n 1)" "$(nproc)"
printf 'host RAM        : %s kB\n' "$(sed -n 's/^MemTotal: *\([0-9]*\).*/\1/p' /proc/meminfo)"
printf 'docker          : ya, service %s, image %s, server %s\n' "$service" "$image" "$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '?')"
printf 'container       : %s\n' "$container_info"
printf 'postgres        : %s\n' "$(sed -n 1p <<<"$settings")"
printf 'setting         : %s\n' "$(sed -n 2p <<<"$settings")"
printf 'database / role : %s / %s\n' "$db" "$role"
printf 'rows / runs     : %s / %s\n' "$rows" "$runs"
printf 'query           : %s\n\n' "$query"

update_sql=$(cat <<SQL
\\timing on
$query
\\timing off
\\echo RESULT updated=:ROW_COUNT
SELECT count(*) AS total, count(*) FILTER (WHERE processed) AS processed_true FROM managed.batch_items \\gset
\\echo RESULT total=:total processed_true=:processed_true
SQL
)

durations=()
pass=true
printf '== Run ==\n'
for ((i = 1; i <= runs; i++)); do
  reset_ms=$(seed_and_reset)
  out=$(run_sql <<<"$update_sql") || fail "update run $i failed"
  update_ms=$(sed -n 's/^Time: \([0-9.]*\) ms.*/\1/p' <<<"$out" | head -n 1)
  updated=$(result updated <<<"$out")
  total=$(result total <<<"$out")
  processed=$(result processed_true <<<"$out")
  [[ -n $update_ms ]] || fail "run $i: no timing captured"
  verify=OK
  [[ $updated == "$rows" && $total == "$rows" && $processed == "$rows" ]] || { verify=MISMATCH; pass=false; }
  awk -v d="$update_ms" 'BEGIN { exit !(d > 1000) }' || pass=false
  durations+=("$update_ms")
  printf 'run %d: reset %s ms | UPDATE %s ms | updated=%s processed_true=%s/%s %s\n' \
    "$i" "$reset_ms" "$update_ms" "$updated" "$processed" "$total" "$verify"
done
final_reset_ms=$(seed_and_reset)
printf 'final reset: %s ms (processed=false untuk %s row)\n\n' "$final_reset_ms" "$rows"

printf '== Ringkasan ==\n'
printf '%s\n' "${durations[@]}" | sort -n | awk '
  { d[NR] = $1 }
  END { m = (NR % 2) ? d[(NR + 1) / 2] : (d[NR / 2] + d[NR / 2 + 1]) / 2
        printf "UPDATE ms min=%s median=%.3f max=%s\n", d[1], m, d[NR] }'
if $pass; then
  printf 'PASS: semua run > 1000 ms dan hasil terverifikasi\n'
else
  printf 'FAIL: ada run <= 1000 ms atau verifikasi tidak cocok\n'
fi
printf 'laporan: %s\n' "$report"
$pass
