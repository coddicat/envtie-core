#!/usr/bin/env bash
# Non-mutating Neon and Upstash preflight. Prints presence and HTTP status
# codes only. It does not print credential values, response bodies, or tokens,
# and it does not load the repository .env.
set -euo pipefail
set +x
cd "$(dirname "$0")/.."
root=$PWD
failures=0

fail() {
  failures=$((failures + 1))
  printf 'FAIL %s\n' "$1"
}
ok() { printf 'ok   %s\n' "$1"; }
skip() { printf 'skip %s\n' "$1"; }

present() {
  # shellcheck disable=SC2086
  eval "[ -n \"\${$1:-}\" ]"
}

control_plane_url() {
  local raw=${ENVTIE_API_URL:-http://localhost:8080}
  raw=${raw%/}
  if [[ "$raw" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || [[ "$raw" =~ ^http://(localhost|127\.0\.0\.1)(:[0-9]+)?$ ]]; then
    printf '%s' "$raw"
    return 0
  fi
  echo "ENVTIE_API_URL must be an https host or http://localhost with no user info." >&2
  return 1
}

status_code() {
  local uri=$1 code
  shift
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$@" "$uri" || true)
  if [ -z "$code" ]; then
    code=000
  fi
  printf '%s' "$code"
}

health_code() {
  local base=$1 tmp code
  tmp=$(mktemp)
  code=$(curl -s -o "$tmp" -w '%{http_code}' --max-time 20 "$base/healthz" || true)
  if [ -z "$code" ]; then
    code=000
  fi
  if [ "$code" = 200 ] && grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' "$tmp"; then
    rm -f "$tmp"
    printf 'ok'
    return 0
  fi
  rm -f "$tmp"
  printf '%s' "$code"
  return 1
}

field_count() {
  local field=$1 file=$2
  grep -Ec "^[[:space:]]*${field}:[[:space:]]+" "$file" || true
}

check_canonical() {
  local relative=$1
  shift
  local path="$root/$relative" line
  if [ ! -f "$path" ]; then
    fail "$relative is missing"
    return
  fi
  if grep -q '://' "$path" || grep -Eiq '^.*(password|api_key|secret)[[:space:]]*:' "$path"; then
    fail "$relative contains a secret-shaped value"
  fi
  for line in "$@"; do
    if ! grep -Fq "$line" "$path"; then
      fail "$relative is missing $line"
    fi
  done
  if grep -Eq '^[[:space:]]*(executable|sha256):' "$path"; then
    fail "$relative must leave official adapter installation to the runner"
  else
    ok "$relative uses logical official adapters"
  fi
}

check_blueprint() {
  local relative=$1 path executable pin binary actual cli
  path="$root/$relative"
  if [ ! -f "$path" ]; then
    fail "blueprint $relative is missing"
    return 1
  fi
  if grep -q '://' "$path" || grep -Eiq '^.*(password|api_key|secret)[[:space:]]*:' "$path"; then
    fail "blueprint contains a secret-shaped value"
  fi
  local count_exe count_pin
  count_exe=$(field_count executable "$path")
  count_pin=$(field_count sha256 "$path")
  if [ "$count_exe" != "$count_pin" ]; then
    fail "blueprint executables and sha256 pins do not pair"
    return 1
  fi
  cli="$root/dist/envtie"
  local pairs executable_line pin_line
  pairs=$(paste -d ' ' \
    <(grep -E '^[[:space:]]*executable:[[:space:]]+' "$path" | sed -E 's/^[[:space:]]*executable:[[:space:]]+//') \
    <(grep -E '^[[:space:]]*sha256:[[:space:]]+' "$path" | sed -E 's/^[[:space:]]*sha256:[[:space:]]+//'))
  while read -r executable pin; do
    [ -n "$executable" ] || continue
    case "$executable" in
      /* | [A-Za-z]:* | *..*) fail "executable must be relative to the runner working directory: $executable"; continue ;;
    esac
    case "$pin" in
      *REPLACE_WITH_* | your-*)
        fail "pin for $executable is a placeholder. schemas/blueprint.schema.json requires 64 hex characters, so schema validation waits until it is replaced."
        continue
        ;;
    esac
    if [[ ! "$pin" =~ ^[a-fA-F0-9]{64}$ ]]; then
      fail "pin for $executable is not 64 hex characters"
      continue
    fi
    binary="$root/$executable"
    if [ ! -f "$binary" ]; then
      fail "missing binary $executable"
      continue
    fi
    if command -v sha256sum >/dev/null 2>&1; then
      actual=$(sha256sum "$binary" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
      actual=$(shasum -a 256 "$binary" | awk '{print $1}')
    else
      fail "sha256 tool is missing"
      continue
    fi
    actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
    pin=$(printf '%s' "$pin" | tr '[:upper:]' '[:lower:]')
    if [ "$actual" != "$pin" ]; then
      fail "pin does not match $executable"
    else
      ok "pin matches $executable"
    fi
  done <<<"$pairs"
  if [ -x "$cli" ] && ! grep -E '^[[:space:]]*sha256:' "$path" | grep -Evq '^[[:space:]]*sha256:[[:space:]]+[a-fA-F0-9]{64}[[:space:]]*$'; then
    if "$cli" blueprint validate "$path"; then
      ok "envtie blueprint validate passed"
    else
      fail "envtie blueprint validate failed"
    fi
  elif [ ! -x "$cli" ]; then
    skip "envtie blueprint validate (dist/envtie is absent)"
  fi
}

report_credentials() {
  local name text=$1 live=$2 names
  names="NEON_API_KEY UPSTASH_EMAIL UPSTASH_API_KEY"
  if grep -q 'github.environment.' <<<"$text"; then
    names="$names GITHUB_TOKEN GITHUB_REPOSITORY ENVTIE_GITHUB_ENVIRONMENT"
  fi
  for name in $names; do
    if present "$name"; then
      printf 'credential %s: present\n' "$name"
    else
      printf 'credential %s: missing\n' "$name"
      if [ "$live" = yes ]; then
        fail "missing $name"
      fi
    fi
  done
}

neon_access() {
  local text=$1 id key code
  if ! present NEON_API_KEY; then
    skip "Neon project access (NEON_API_KEY missing)"
    return
  fi
  local checked=no
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    case "$id" in
      *REPLACE_WITH_* | your-*) continue ;;
    esac
    checked=yes
    if [[ ! "$id" =~ ^[A-Za-z0-9][A-Za-z0-9_-]{0,80}$ ]]; then
      fail "Neon project id is not a safe path segment"
      continue
    fi
    key=$NEON_API_KEY
    code=$(status_code "https://console.neon.tech/api/v2/projects/$id" -H "Authorization: Bearer $key" -H "Accept: application/json")
    unset key
    if [ "$code" = 200 ]; then
      ok "Neon project access HTTP 200"
    else
      fail "Neon project access HTTP $code"
    fi
  done < <(grep -E '^[[:space:]]*project_id:[[:space:]]+' <<<"$text" | sed -E 's/^[[:space:]]*project_id:[[:space:]]+//')
  if [ "$checked" = no ]; then
    skip "Neon project access (project id is still a placeholder)"
  fi
}

upstash_access() {
  local email key basic code
  if ! present UPSTASH_EMAIL || ! present UPSTASH_API_KEY; then
    skip "Upstash management list (UPSTASH_EMAIL or UPSTASH_API_KEY missing)"
    return
  fi
  email=$UPSTASH_EMAIL
  key=$UPSTASH_API_KEY
  basic=$(printf '%s:%s' "$email" "$key" | base64 | tr -d '\n')
  code=$(status_code "https://api.upstash.com/v2/redis/databases?credentials=hide" -H "Authorization: Basic $basic" -H "Accept: application/json")
  unset email key basic
  if [ "$code" = 200 ]; then
    ok "Upstash management list HTTP 200"
  else
    fail "Upstash management list HTTP $code"
  fi
}

examples=no
blueprint=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --examples) examples=yes ;;
    --blueprint)
      shift
      blueprint=${1:-}
      ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

if [ "$examples" = yes ]; then
  check_canonical blueprints/neon-upstash-local.yaml \
    $'name: consumer-bench' 'adapter: neon@1' 'adapter: upstash@1' \
    'project_id: REPLACE_WITH_NEON_PROJECT_ID' 'database_name: REPLACE_WITH_NEON_DATABASE' 'role_name: REPLACE_WITH_NEON_ROLE' \
    'platform: aws' 'primary_region: us-east-1' 'expires_in: 1d' 'protect: false' \
    'to: dotenv.DATABASE_URL' 'to: dotenv.UPSTASH_REDIS_REST_URL' 'to: dotenv.UPSTASH_REDIS_REST_TOKEN'
  check_canonical blueprints/neon-upstash-windows.yaml \
    'name: consumer-bench' 'adapter: neon@1' 'adapter: upstash@1' \
    'project_id: REPLACE_WITH_NEON_PROJECT_ID' 'database_name: REPLACE_WITH_NEON_DATABASE' 'role_name: REPLACE_WITH_NEON_ROLE' \
    'platform: aws' 'primary_region: us-east-1' 'expires_in: 1d' 'protect: false' \
    'to: dotenv.DATABASE_URL' 'to: dotenv.UPSTASH_REDIS_REST_URL' 'to: dotenv.UPSTASH_REDIS_REST_TOKEN'
  check_canonical blueprints/neon-upstash-ci.yaml \
    'name: consumer-bench' 'adapter: neon@1' 'adapter: upstash@1' \
    'project_id: REPLACE_WITH_NEON_PROJECT_ID' 'database_name: REPLACE_WITH_NEON_DATABASE' 'role_name: REPLACE_WITH_NEON_ROLE' \
    'platform: aws' 'primary_region: us-east-1' 'expires_in: 1d' 'protect: false' \
    'to: github.environment.secret.DATABASE_URL' 'to: github.environment.variable.UPSTASH_REDIS_REST_URL' 'to: github.environment.secret.UPSTASH_REDIS_REST_TOKEN'
  echo "schema: official adapters are resolved and pinned by the control plane when the blueprint is saved."
  if [ "$failures" -ne 0 ]; then
    exit 1
  fi
  echo "preflight examples passed"
  exit 0
fi

if [ -z "$blueprint" ]; then
  blueprint="blueprints/neon-upstash-local.yaml"
fi

if command -v go >/dev/null 2>&1 || [ -x "$root/.toolchain/go/bin/go" ]; then
  ok "go is available"
else
  fail "go is not available"
fi

check_blueprint "$blueprint" || true
blueprint_text=$(cat "$root/$blueprint" 2>/dev/null || true)
if [ -n "$blueprint_text" ]; then
  report_credentials "$blueprint_text" yes
  if base=$(control_plane_url); then
    if health=$(health_code "$base") && [ "$health" = ok ]; then
      ok "control plane healthy"
    else
      fail "control plane unhealthy HTTP ${health:-0}"
    fi
  else
    fail "control plane URL rejected"
  fi
  neon_access "$blueprint_text"
  if grep -q 'adapter: upstash@1' <<<"$blueprint_text"; then
    upstash_access
  fi
fi
unset blueprint_text

if [ "$failures" -ne 0 ]; then
  exit 1
fi
echo "preflight passed"
