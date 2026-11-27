set -euo pipefail

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
# creds_file relatif terhadap direktori pemanggil, bukan root repo.
[[ -z ${1:-} || $1 == /* ]] || set -- "$PWD/$1" "${@:2}"
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
creds=${1:-}
mode=${2:-}
batch_size=${3:-10000}
runs=${4:-2}
rows=${5:-1000000}
base=${6:-http://localhost:8080}
[[ -f $creds && ( $mode == standard || $mode == batch ) ]] || fail 'usage: i4_benchmark.sh <creds_file> <standard|batch> [batch_size] [runs] [rows] [base_url]'
[[ $batch_size =~ ^[1-9][0-9]*$ && $runs =~ ^[1-9]$ && $rows =~ ^[1-9][0-9]*$ ]] || fail 'batch_size/runs/rows must be positive integers'
for tool in curl jq; do command -v "$tool" >/dev/null || fail "$tool is required"; done
creds_dir=$(cd "$(dirname "$creds")" && pwd)
case "$creds_dir/" in
  "$PWD"/*) git check-ignore -q "$creds_dir/$(basename "$creds")" 2>/dev/null \
    || fail 'creds_file is inside the repository and not ignored; store it outside (e.g. ~/wbd_demo_creds.env)' ;;
esac
poll_timeout=${I4_POLL_TIMEOUT:-900}

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
umask 077

target_sql() {
  docker compose exec -T postgres sh -c 'psql -X -tA -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$DB_TARGET_B"' <<<"$1"
}
counts() { target_sql "select count(*) || ' ' || count(*) filter (where processed) from managed.batch_items"; }
ms_between() { jq -nr --arg a "$1" --arg b "$2" '
  def t: sub("(?<s>\\.[0-9]+)?Z$"; "Z") as $x | ($x | fromdateiso8601) + ((capture("(?<f>\\.[0-9]+)Z$").f // "0") | tonumber);
  (($b | t) - ($a | t)) * 1000 | floor'; }
now_ms() { date +%s%3N; }

csrf_of() { cat "${1%.jar}.csrf"; }
# api USER METHOD PATH  (body dari stdin untuk mutasi); mencetak body, gagal bila status >= 400.
api() {
  local user=$1 method=$2 path=$3 jar=$work/$1.jar out=$work/out status
  if [[ $method == GET ]]; then
    status=$(curl -s -o "$out" -w '%{http_code}' -b "$jar" -c "$jar" "$base$path")
  else
    status=$(curl -s -o "$out" -w '%{http_code}' -b "$jar" -c "$jar" -X "$method" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf_of "$jar")" --data-binary @- "$base$path")
  fi
  printf '%s' "$status" > "$work/status"
  if (( status >= 400 )); then
    printf 'FAIL: %s %s -> %s %s\n' "$method" "$path" "$status" "$(jq -c '.error | {code, message}' "$out" 2>/dev/null)" >&2
    return 1
  fi
  cat "$out"
}
declare -A me=()
login() {
  local user=$1 jar=$work/$1.jar
  curl -sf -c "$jar" -b "$jar" "$base/api/auth/csrf" | jq -r '.data.csrf_token' > "$work/$user.csrf" || fail 'cannot obtain CSRF token'
  U=$user P=$(grep "^$user=" "$creds" | cut -d= -f2-) jq -n '{username: env.U, password: env.P}' \
    | api "$user" POST /api/auth/login >/dev/null || fail "login $user"
  curl -sf -c "$jar" -b "$jar" "$base/api/auth/csrf" | jq -r '.data.csrf_token' > "$work/$user.csrf"
  me[$user]=$(api "$user" GET /api/session | jq -r '.data.user.id')
}

lead=i3_editor_lead
approvers=(i3_editor_lead i3_viewer_lead)
login i3_editor_lead
login i3_viewer_lead
db=$(api "$lead" GET /api/databases | jq -r '.data.items[] | select(.name == "i3-target-b") | .id')
[[ -n $db ]] || fail 'database i3-target-b not visible for i3_editor_lead'
D=/api/databases/$db

if [[ $mode == standard ]]; then
  up='UPDATE managed.batch_items SET processed = TRUE;'
  down='UPDATE managed.batch_items SET processed = FALSE;'
  batch=null
else
  up='UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);'
  down='UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);'
  batch=$(jq -nc --argjson n "$batch_size" '{schema: "managed", table: "batch_items", cursor_column: "id", batch_size: $n, parameters: {}}')
fi

printf '== I4 %s rows=%s batch_size=%s runs=%s db=%s ==\n' "$mode" "$rows" "$([[ $mode == batch ]] && echo "$batch_size" || echo -)" "$runs" "$db"
for ((i = 1; i <= runs; i++)); do
  bash db/testdata/benchmark_full_update.sh reset "$rows" | sed 's/^/  /'
  read -r total done_before <<<"$(counts)"
  [[ $total == "$rows" && $done_before == 0 ]] || fail "run $i: reset verification ($total/$done_before)"

  log_since=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
  title="I4 $mode$([[ $mode == batch ]] && echo " b$batch_size" || true) run $i $(date -u +%H%M%S)"
  draft=$(T=$title UP=$up DOWN=$down jq -n --argjson b "$batch" --arg m "$mode" \
      '{title: env.T, sql: env.UP, sql_down: env.DOWN, irreversible_reason: null, mode: $m, batch: $b}' \
    | api "$lead" POST "$D/drafts")
  mig=$(jq -r '.data.id' <<<"$draft"); rev=$(jq -r '.data.revision' <<<"$draft")
  prop=$(jq -n --arg m "$mig" --argjson r "$rev" '{migration_id: $m, revision: $r, action: "apply", source_execution_id: null}' \
    | api "$lead" POST "$D/proposals")
  pid=$(jq -r '.data.proposal_id' <<<"$prop")
  t=$(now_ms)
  jq -n --argjson r "$rev" '{revision: $r}' | api "$lead" POST "$D/proposals/$pid/submit-review" > "$work/p"
  review_ms=$(( $(now_ms) - t ))
  [[ $(jq -r '.data.validation.status' "$work/p") == passed ]] || fail "run $i: validation $(jq -c '.data.validation' "$work/p")"
  for u in "${approvers[@]}"; do
    gid=$(api "$u" GET "$D/proposals/$pid" | jq -r --arg me "${me[$u]}" '.data.approvals[] | select(.leader_user_id == $me and .status == "pending") | .group_id' | head -n 1)
    [[ -n $gid ]] || continue
    jq -n --argjson r "$rev" --arg g "$gid" '{revision: $r, group_id: $g}' | api "$u" POST "$D/proposals/$pid/approvals" >/dev/null
  done
  api "$lead" GET "$D/proposals/$pid" | jq -e '.data.allowed_actions | index("enqueue")' >/dev/null || fail "run $i: enqueue not allowed after approvals"

  t=$(now_ms)
  job=$(jq -n --arg q "$(cat /proc/sys/kernel/random/uuid)" --argjson r "$rev" '{request_id: $q, revision: $r}' \
    | api "$lead" POST "$D/proposals/$pid/jobs")
  enqueue_ms=$(( $(now_ms) - t )); enqueue_status=$(cat "$work/status")
  jid=$(jq -r '.data.job_id' <<<"$job")
  t0=$(now_ms); polls=0
  while :; do
    view=$(api "$lead" GET "$D/jobs/$jid")
    st=$(jq -r '.data.status' <<<"$view")
    [[ $st == queued || $st == running || $st == retry_wait ]] || break
    (( $(now_ms) - t0 < poll_timeout * 1000 )) || { st="TIMEOUT($st)"; break; }
    polls=$((polls + 1)); sleep 0.5
  done
  observed_ms=$(( $(now_ms) - t0 + enqueue_ms ))
  read -r total done_after <<<"$(counts)"

  created=$(jq -r '.data.created_at' <<<"$view"); started=$(jq -r '.data.started_at' <<<"$view"); finished=$(jq -r '.data.finished_at' <<<"$view")
  queue_wait=-; job_ms=-; total_ms=-
  [[ $started != null ]] && queue_wait=$(ms_between "$created" "$started")
  [[ $started != null && $finished != null ]] && job_ms=$(ms_between "$started" "$finished")
  [[ $finished != null ]] && total_ms=$(ms_between "$created" "$finished")

  sleep 1
  docker compose logs backend --no-log-prefix --since "$log_since" 2>/dev/null | grep '^{' > "$work/log" || true
  # Atribut log datar; .fields dipertahankan untuk log format lama.
  trace=$(jq -r --arg j "$jid" '(.fields? // {}) + . | select(.job_id? == $j and .traceparent?) | .traceparent | split("-")[1]' "$work/log" 2>/dev/null | head -n 1)
  sqlstats=$(jq -s -r --arg t "$trace" '
    [ .[] | select(.trace_id? == $t and .event? == "sql.query.completed" and ((.query // "") | test("^UPDATE managed\\.batch_items"))) | .duration_ms ] as $u
    | [ .[] | select(.trace_id? == $t and .event? == "sql.transaction.commit.completed") | .duration_ms ] as $c
    | ($u | sort) as $s
    | if ($u | length) == 0 then "n=0"
      else "n=\($u | length) sum=\($u | add | floor) min=\($s[0] * 10 | floor / 10) med=\($s[($s | length / 2 | floor)] * 10 | floor / 10) max=\($s[-1] * 10 | floor / 10) commits=\($c | length) commit_sum=\(($c | add // 0) | floor)" end' "$work/log")
  progress=$(jq -c '.data.progress' <<<"$view")
  verify=OK; [[ $total == "$rows" && $done_after == "$rows" ]] || verify=MISMATCH
  printf 'RUN %d mode=%s job=%s status=%s exec=%s enqueue_http=%s enqueue_ms=%s review_ms=%s queue_wait_ms=%s job_ms=%s created_to_finished_ms=%s observed_ms=%s\n' \
    "$i" "$mode" "$jid" "$st" "$(jq -r '.data.execution_status' <<<"$view")" "$enqueue_status" "$enqueue_ms" "$review_ms" "$queue_wait" "$job_ms" "$total_ms" "$observed_ms"
  printf 'RUN %d sql_update(%s) trace=%s\n' "$i" "$sqlstats" "${trace:-none}"
  printf 'RUN %d data processed=%s/%s %s progress=%s blocks_database=%s error=%s\n' "$i" "$done_after" "$total" "$verify" "$progress" \
    "$(jq -r '.data.blocks_database' <<<"$view")" "$(jq -c '.data.error' <<<"$view")"
  [[ $st == succeeded && $verify == OK ]] || { printf 'STOP: run %d tidak succeeded/terverifikasi\n' "$i"; exit 2; }
done
bash db/testdata/benchmark_full_update.sh reset "$rows" | sed 's/^/  /'
echo PASS
