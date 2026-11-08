#!/usr/bin/env bash
# Measures the latency llmtee adds on top of a fake upstream.
# Requires vegeta: go install github.com/tsenart/vegeta/v12@latest
# Usage: scripts/loadtest.sh [rate] [duration]
set -euo pipefail
RATE=${1:-200}
DURATION=${2:-20s}
TMP=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$TMP"' EXIT

go build -o "$TMP/llmtee" ./cmd/llmtee
go build -o "$TMP/fake" ./cmd/fakeupstream
"$TMP/fake" -listen 127.0.0.1:19999 >/dev/null 2>&1 &
LLMTEE_LISTEN=127.0.0.1:18080 LLMTEE_LOG_BODIES=false LLMTEE_ADMIN_PASSWORD=x \
  LLMTEE_UPSTREAM_OPENAI=http://127.0.0.1:19999 DATABASE_URL="sqlite://$TMP/load.db" \
  LLMTEE_LOG_LEVEL=warn "$TMP/llmtee" &
sleep 1.5

BODY='{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
echo "$BODY" > "$TMP/body.json"
attack() { # name url
  printf 'POST %s\nContent-Type: application/json\nAuthorization: Bearer sk-load\n@%s\n' "$2" "$TMP/body.json" |
    vegeta attack -rate="$RATE" -duration="$DURATION" -keepalive=true | tee "$TMP/$1.bin" | vegeta report -type=text >/dev/null
  vegeta report -type=json < "$TMP/$1.bin" > "$TMP/$1.json"
}
attack warmup http://127.0.0.1:18080/openai/v1/chat/completions
attack direct http://127.0.0.1:19999/v1/chat/completions
attack proxy  http://127.0.0.1:18080/openai/v1/chat/completions

python3 - "$TMP" <<'PY'
import json, sys
d = json.load(open(sys.argv[1] + "/direct.json")); p = json.load(open(sys.argv[1] + "/proxy.json"))
ms = lambda r, k: r["latencies"][k] / 1e6
print(f"{'':8} {'p50':>8} {'p90':>8} {'p99':>8} {'success':>8}")
for name, r in (("direct", d), ("proxy", p)):
    print(f"{name:8} {ms(r,'50th'):7.2f}ms {ms(r,'90th'):7.2f}ms {ms(r,'99th'):7.2f}ms {r['success']*100:7.1f}%")
print(f"added p50 latency: {ms(p,'50th') - ms(d,'50th'):.2f} ms (criterion: < 5 ms)")
PY
curl -s http://127.0.0.1:18080/metrics | grep -E '^llmtee_(events_dropped_total|db_events_written_total|db_write_errors_total)'
