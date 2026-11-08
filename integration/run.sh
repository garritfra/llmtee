#!/usr/bin/env bash
# Starts the fake upstream and llmtee, then runs the Python and TypeScript
# SDK tests. DATABASE_URL selects the backend (default: temporary SQLite).
set -euo pipefail
cd "$(dirname "$0")/.."
TMP=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$TMP"' EXIT
export LLMTEE_ADMIN_PASSWORD=integration LLMTEE_URL=http://127.0.0.1:18081

go build -o "$TMP/llmtee" ./cmd/llmtee
go build -o "$TMP/fake" ./cmd/fakeupstream
"$TMP/fake" -listen 127.0.0.1:19998 -delay 2ms >/dev/null 2>&1 &
LLMTEE_LISTEN=127.0.0.1:18081 DATABASE_URL="${DATABASE_URL:-sqlite://$TMP/it.db}" \
  LLMTEE_UPSTREAM_OPENAI=http://127.0.0.1:19998 LLMTEE_UPSTREAM_ANTHROPIC=http://127.0.0.1:19998 "$TMP/llmtee" > "$TMP/llmtee.log" 2>&1 &
for _ in $(seq 50); do curl -sf "$LLMTEE_URL/healthz" >/dev/null && break; sleep 0.1; done

if [[ "${SKIP_PYTHON:-}" == "" ]]; then
  python3 integration/python/test_sdk.py
fi
if [[ "${SKIP_TS:-}" == "" ]]; then
  (cd integration/typescript && { [ -d node_modules ] || npm install --silent --no-audit --no-fund; } && node test.mjs)
fi
echo "integration tests passed"
