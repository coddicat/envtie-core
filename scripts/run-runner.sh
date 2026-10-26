#!/usr/bin/env bash
# Runs an EnvTie.dev runner against the local control plane from the repository
# root. Builds the CLI and official adapters, then polls for approved jobs.
# The runner token comes from ENVTIE_RUNNER_TOKEN, .envtie/runner.token
# (owner-only mode), or a hidden prompt. Do not pass the token as an argument.
set -euo pipefail
set +x
cd "$(dirname "$0")/.."
root=$PWD
umask 077

if [ "$#" -ne 0 ]; then
  echo "Do not pass the runner token on the command line. Set ENVTIE_RUNNER_TOKEN, store it in .envtie/runner.token, or enter it at the prompt." >&2
  exit 2
fi

token_file="$root/.envtie/runner.token"

valid_token() {
  # Length is checked separately. Some regex libraries reject an upper bound above 255.
  local pattern='^[A-Za-z0-9_-]+$'
  [[ "$1" =~ $pattern ]] && [ "${#1}" -ge 16 ] && [ "${#1}" -le 256 ]
}

stat_field() {
  local gnu=$1 bsd=$2 path=$3
  if stat -c "$gnu" "$path" >/dev/null 2>&1; then
    stat -c "$gnu" "$path"
  else
    stat -f "$bsd" "$path"
  fi
}

is_restricted() {
  local path=$1 mode uid
  [ -f "$path" ] && [ ! -L "$path" ] || return 1
  mode=$(stat_field '%a' '%OLp' "$path") || return 1
  mode=$((10#$mode))
  uid=$(stat_field '%u' '%u' "$path") || return 1
  [ "$uid" = "$(id -u)" ] || return 1
  [ "$mode" -eq 600 ] || [ "$mode" -eq 400 ]
}

ensure_token_dir() {
  local dir="$root/.envtie" mode uid
  if [ -L "$dir" ] || { [ -e "$dir" ] && [ ! -d "$dir" ]; }; then
    echo "Refusing to use .envtie because it is not a regular directory." >&2
    exit 1
  fi
  mkdir -p "$dir"
  chmod 700 "$dir"
  mode=$(stat_field '%a' '%OLp' "$dir") || exit 1
  mode=$((10#$mode))
  uid=$(stat_field '%u' '%u' "$dir") || exit 1
  if [ "$mode" -ne 700 ] || [ "$uid" != "$(id -u)" ]; then
    echo "Could not restrict permissions on .envtie." >&2
    exit 1
  fi
}

read_token_text() {
  local text
  text=$(tr -d '\r' < "$1") || return 1
  if [[ "$text" == *$'\n'* ]]; then
    return 1
  fi
  printf '%s' "$text"
}

save_token() {
  local tmp mode uid
  ensure_token_dir
  if [ -L "$token_file" ] || [ -d "$token_file" ]; then
    echo "Refusing to write the runner token because .envtie/runner.token is not a regular file." >&2
    exit 1
  fi
  if [ -L "${token_file}.tmp" ]; then
    echo "Refusing to write the runner token through a symlink." >&2
    exit 1
  fi
  tmp=$(mktemp "$root/.envtie/runner.token.XXXXXX")
  chmod 600 "$tmp"
  printf '%s\n' "$token" > "$tmp"
  chmod 600 "$tmp"
  mv -f "$tmp" "$token_file"
  chmod 600 "$token_file"
  mode=$(stat_field '%a' '%OLp' "$token_file")
  mode=$((10#$mode))
  uid=$(stat_field '%u' '%u' "$token_file")
  if [ "$mode" -ne 600 ] || [ "$uid" != "$(id -u)" ]; then
    rm -f "$token_file"
    echo "Could not restrict permissions on .envtie/runner.token." >&2
    exit 1
  fi
}

save_token_if_needed() {
  local current
  if [ -L "$token_file" ] || [ -d "$token_file" ]; then
    echo "Refusing to write the runner token because .envtie/runner.token is not a regular file." >&2
    exit 1
  fi
  if [ -f "$token_file" ] && [ ! -L "$token_file" ] && is_restricted "$token_file"; then
    if current=$(read_token_text "$token_file") && [ "$current" = "$token" ]; then
      return 0
    fi
  fi
  save_token
  echo "Saved the runner token to .envtie/runner.token."
}

wait_control_plane() {
  local i body
  for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
    body=$(curl -fsS --max-time 3 http://localhost:8080/healthz 2>/dev/null || true)
    case "$body" in
      *'"status":"ok"'*|*'status": "ok"'*) return 0 ;;
    esac
    sleep 0.4
  done
  echo "The control plane is not healthy at http://localhost:8080/healthz. Start it with ./scripts/start-local.sh first." >&2
  return 1
}

public_key_bytes() {
  local len
  if len=$(printf '%s' "$1" | base64 -d 2>/dev/null | wc -c | tr -d '[:space:]'); then
    printf '%s' "$len"
    return 0
  fi
  if len=$(printf '%s' "$1" | base64 -D 2>/dev/null | wc -c | tr -d '[:space:]'); then
    printf '%s' "$len"
    return 0
  fi
  return 1
}

command -v curl >/dev/null 2>&1 || { echo "curl is required to check the control plane." >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "Go 1.27+ is required (https://go.dev/dl/)." >&2; exit 1; }
wait_control_plane
ensure_token_dir

key_file="$root/.envtie/controlplane-public.key"
if [ ! -e "$key_file" ]; then
  echo "Missing control plane public key at .envtie/controlplane-public.key. Start the local control plane first." >&2
  exit 1
fi
if [ -L "$key_file" ] || [ ! -f "$key_file" ]; then
  echo "Refusing to read .envtie/controlplane-public.key because it is not a regular file." >&2
  exit 1
fi
public_key=$(tr -d '[:space:]' < "$key_file")
key_len=$(public_key_bytes "$public_key") || key_len=""
if [ "$key_len" != "32" ]; then
  echo "The control plane public key must be a base64-encoded Ed25519 public key." >&2
  exit 1
fi

token=""
source=""
if [ -n "${ENVTIE_RUNNER_TOKEN+x}" ]; then
  token=${ENVTIE_RUNNER_TOKEN//$'\r'/}
fi
if [ -n "$token" ] || [ -n "${ENVTIE_RUNNER_TOKEN+x}" ]; then
  token=${token%$'\n'}
  if [ -z "$token" ]; then
    echo "A runner token is required." >&2
    exit 1
  fi
  if ! valid_token "$token"; then
    echo "The runner token is invalid." >&2
    exit 1
  fi
  source=environment
elif [ -e "$token_file" ]; then
  if [ -L "$token_file" ] || [ ! -f "$token_file" ]; then
    echo "Refusing to read .envtie/runner.token because it is not a regular file." >&2
    exit 1
  fi
  if ! is_restricted "$token_file"; then
    echo "Refusing to read .envtie/runner.token because its permissions are not limited to the current user. Delete the file and enter the token when prompted, or restrict the file to your user and run this script again." >&2
    exit 1
  fi
  token=$(read_token_text "$token_file") || {
    echo "The runner token file is invalid." >&2
    exit 1
  }
  if [ -z "$token" ]; then
    echo "The runner token file is empty." >&2
    exit 1
  fi
  if ! valid_token "$token"; then
    echo "The runner token file is invalid." >&2
    exit 1
  fi
  source=file
else
  echo "Register a runner in the dashboard (Runners > Register runner) and paste its token."
  echo "The token is not accepted as a command-line argument."
  read -r -s -p "Runner token: " token
  echo
  token=${token//$'\r'/}
  token=${token%$'\n'}
  if [ -z "$token" ]; then
    echo "A runner token is required." >&2
    exit 1
  fi
  if ! valid_token "$token"; then
    echo "The runner token is invalid." >&2
    exit 1
  fi
  source=prompt
fi
if [ "$source" != file ]; then
  save_token_if_needed
fi

./scripts/build.sh
for name in envtie adapter-neon adapter-upstash adapter-github adapter-stripe adapter-sentry adapter-mailtrap adapter-vercel; do
  if [ ! -f "$root/dist/$name" ] || [ ! -s "$root/dist/$name" ]; then
    echo "Missing adapter artifact: dist/$name" >&2
    exit 1
  fi
done
echo "Official adapters are in dist/ for local development. The control plane pins them in signed plans."

# Credentials are not loaded from the repo .env. Each job asks the adapters in
# that plan which variables they need, including a custom adapter's own names.
echo "Provider credentials stay in this process and are not loaded from .env."
echo "A job runs only the adapters in its plan. Each adapter checks the credentials it requires."
echo "Runner token: present ($source)"
echo "Starting the runner from the repository root."

export ENVTIE_API_URL=http://localhost:8080
export ENVTIE_RUNNER_TOKEN="$token"
export ENVTIE_PUBLIC_KEY="$public_key"
unset token public_key
cd "$root"
exec "$root/dist/envtie" runner execute
