#!/bin/sh
# Railway entrypoint: boot genuine opencode service, wire gateway to it.
# No secrets baked: service password is parsed from serve's own stdout.
# Ephemeral disk is fine: every pool miss mints a fresh session (milliseconds).
set -eu

SVC_PORT="${SVC_PORT:-4096}"
opencode serve --port "$SVC_PORT" >/tmp/serve.log 2>&1 &
for i in $(seq 1 40); do
  grep -q "server listening" /tmp/serve.log 2>/dev/null && break
  sleep 3
done
grep -E "server (listening|password)" /tmp/serve.log | sed 's/password .*/password ********/'

export OPENCODE_SERVICE_URL="http://127.0.0.1:${SVC_PORT}"
export OPENCODE_SERVER_PASSWORD="$(grep -o 'server password [^ ]*' /tmp/serve.log | head -1 | awk '{print $3}')"
if [ -z "$OPENCODE_SERVER_PASSWORD" ]; then
  echo "FATAL: could not read service password from serve log" >&2
  exit 1
fi

export PORT="${PORT:-8081}"
export DIRECT_DIAG="${DIRECT_DIAG:-1}"
export DIRECT_DEBUG="${DIRECT_DEBUG:-0}"
export OPENCODE_ZEN_BASE="${OPENCODE_ZEN_BASE:-https://opencode.ai/zen/v1}"

exec zen-direct
