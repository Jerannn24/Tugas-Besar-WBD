set -euo pipefail

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
# creds_file relatif terhadap direktori pemanggil, bukan root repo.
[[ -z ${1:-} || $1 == /* ]] || set -- "$PWD/$1" "${@:2}"
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
creds=${1:-}
base=${2:-http://localhost:8080}
env_file=${I3_ENV_FILE:-.env}
target_host=${I3_TARGET_HOST:-postgres}
target_port=${I3_TARGET_PORT:-5432}
[[ -n $creds ]] || fail 'usage: bash db/testdata/i3_setup.sh <creds_file> [base_url]'
[[ -f $env_file ]] || fail "env file not found: $env_file"
[[ $target_port =~ ^[1-9][0-9]{0,4}$ ]] || fail 'I3_TARGET_PORT must be a port number'
for tool in curl jq openssl; do command -v "$tool" >/dev/null || fail "$tool is required"; done

# File sandi tidak boleh bisa ikut ter-commit.
creds_dir=$(cd "$(dirname "$creds")" 2>/dev/null && pwd) || fail "directory of creds_file does not exist"
case "$creds_dir/" in
  "$PWD"/*) git check-ignore -q "$creds_dir/$(basename "$creds")" 2>/dev/null \
    || fail 'creds_file is inside the repository and not ignored; store it outside (e.g. ~/wbd_demo_creds.env)' ;;
esac

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
jar=$work/cookies
umask 077

declare -A cfg=()
while IFS='=' read -r key value; do
  value=${value%$'\r'}
  case "$key" in
    ADMIN_BOOTSTRAP_USERNAME|ADMIN_BOOTSTRAP_PASSWORD|DB_DEV_TARGET_A|DB_DEV_TARGET_B|DB_DEV_TARGET_[AB]_*_PASSWORD) cfg[$key]=$value ;;
  esac
done < "$env_file"
[[ -n ${cfg[ADMIN_BOOTSTRAP_USERNAME]:-} && -n ${cfg[ADMIN_BOOTSTRAP_PASSWORD]:-} ]] || fail "set ADMIN_BOOTSTRAP_* in $env_file"

# Sandi user uji: dibuat sekali, disimpan di creds_file (mode 600), dipakai ulang bila sudah ada.
users=(i3_editor_lead i3_editor i3_viewer_lead i3_viewer i3_outsider)
touch "$creds"; chmod 600 "$creds"
for u in "${users[@]}"; do
  grep -q "^$u=" "$creds" || printf '%s=I3%sAa1#\n' "$u" "$(openssl rand -hex 10)" >> "$creds"
done

csrf=
refresh_csrf() {
  csrf=$(curl -sf -c "$jar" -b "$jar" "$base/api/auth/csrf" | jq -r '.data.csrf_token')
  [[ -n $csrf && $csrf != null ]] || fail 'cannot obtain CSRF token'
}
# api METHOD PATH  (body dibaca dari stdin bila ada); mencetak body respons, gagal bila status >= 400.
api() {
  local method=$1 path=$2 out=$work/out status
  if [[ $method == GET ]]; then
    status=$(curl -s -o "$out" -w '%{http_code}' -b "$jar" -c "$jar" "$base$path")
  else
    status=$(curl -s -o "$out" -w '%{http_code}' -b "$jar" -c "$jar" -X "$method" \
      -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" --data-binary @- "$base$path")
  fi
  if (( status >= 400 )); then
    printf 'FAIL: %s %s -> %s %s\n' "$method" "$path" "$status" "$(jq -c '.error | {code, message}' "$out" 2>/dev/null)" >&2
    return 1
  fi
  cat "$out"
}

refresh_csrf
U=${cfg[ADMIN_BOOTSTRAP_USERNAME]} P=${cfg[ADMIN_BOOTSTRAP_PASSWORD]} \
  jq -n '{username: env.U, password: env.P}' | api POST /api/auth/login >/dev/null
refresh_csrf
echo 'OK login admin'

# Connections
connection_id() { api GET /api/admin/connections | jq -r --arg n "$1" '.data.items[] | select(.name == $n) | .id'; }
for slot in a b; do
  S=${slot^^}
  name="i3-target-$slot"
  id=$(connection_id "$name")
  if [[ -z $id ]]; then
    id=$(N=$name DB=${cfg[DB_DEV_TARGET_$S]} SL=$slot H=$target_host PORT=$target_port \
         VP=${cfg[DB_DEV_TARGET_${S}_VIEWER_PASSWORD]} EP=${cfg[DB_DEV_TARGET_${S}_EXECUTOR_PASSWORD]} PP=${cfg[DB_DEV_TARGET_${S}_PREPARER_PASSWORD]} \
      jq -n '{name: env.N, host: env.H, port: (env.PORT | tonumber), database_name: env.DB, ssl_mode: "disable",
              username: ("wbd_target_" + env.SL + "_viewer"), password: env.VP,
              executor: {username: ("wbd_target_" + env.SL + "_executor"), password: env.EP},
              preparer: {username: ("wbd_target_" + env.SL + "_preparer"), password: env.PP}}' \
      | api POST /api/admin/connections | jq -r '.data.id')
    echo "OK connection $name dibuat"
  else
    echo "OK connection $name sudah ada"
  fi
  printf '' | api POST "/api/admin/connections/$id/test" >/dev/null && echo "OK connection $name reachable"
  declare "conn_$slot=$id"
done

# Users
user_id() { api GET /api/admin/users | jq -r --arg n "$1" '.data.items[] | select(.username == $n) | .id'; }
declare -A uid=()
for u in "${users[@]}"; do
  id=$(user_id "$u")
  if [[ -z $id ]]; then
    pw=$(grep "^$u=" "$creds" | cut -d= -f2-)
    id=$(N=$u P=$pw jq -n '{username: env.N, password: env.P, is_admin: false}' | api POST /api/admin/users | jq -r '.data.id')
    echo "OK user $u dibuat"
  else
    echo "OK user $u sudah ada"
  fi
  uid[$u]=$id
done

# Groups: members, leader, grants
group_id() { api GET /api/admin/groups | jq -r --arg n "$1" '.data.items[] | select(.name == $n) | .id'; }
setup_group() {
  local name=$1 access=$2 leader=$3; shift 3
  local id members
  id=$(group_id "$name")
  [[ -n $id ]] || id=$(N=$name jq -n '{name: env.N}' | api POST /api/admin/groups | jq -r '.data.id')
  members=$(for m in "$@"; do printf '%s\n' "${uid[$m]}"; done | jq -R . | jq -sc '{user_ids: .}')
  printf '%s' "$members" | api PUT "/api/admin/groups/$id/members" >/dev/null
  jq -n --arg u "${uid[$leader]}" '{user_id: $u}' | api PUT "/api/admin/groups/$id/leader" >/dev/null
  jq -n --arg a "$conn_a" --arg b "$conn_b" --arg x "$access" \
    '{grants: [{database_id: $a, access: $x}, {database_id: $b, access: $x}]}' \
    | api PUT "/api/admin/groups/$id/grants" >/dev/null
  echo "OK group $name ($access, leader $leader, anggota: $*)"
}
setup_group i3-editors edit i3_editor_lead i3_editor_lead i3_editor
setup_group i3-viewers view i3_viewer_lead i3_viewer_lead i3_viewer

printf '' | api POST /api/auth/logout >/dev/null || true
printf 'target_a=%s\ntarget_b=%s\n' "$conn_a" "$conn_b"
echo 'PASS i3 setup'
