#!/usr/bin/env bash
# Records real provider responses as golden fixtures for the stream tests.
# Needs OPENAI_API_KEY and ANTHROPIC_API_KEY. Costs a fraction of a cent.
#
#   scripts/record-golden.sh && go test ./internal/proxy/ -run Golden
#
# After recording, update the expected usage in internal/proxy/proxy_test.go
# from the "usage" objects printed below.
set -euo pipefail
OUT=internal/fakeupstream/fixtures
: "${OPENAI_API_KEY:?}" "${ANTHROPIC_API_KEY:?}"

oa() { curl -sS --fail-with-body https://api.openai.com"$1" -H "Authorization: Bearer $OPENAI_API_KEY" -H 'Content-Type: application/json' -d "$2"; }
an() { curl -sS --fail-with-body https://api.anthropic.com"$1" -H "x-api-key: $ANTHROPIC_API_KEY" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' -d "$2"; }

oa /v1/chat/completions '{"model":"gpt-4o-mini","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Say hello in five words."}]}' > $OUT/openai_chat_stream.sse
oa /v1/chat/completions '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Say hello in five words."}]}' > $OUT/openai_chat.json
oa /v1/responses '{"model":"gpt-4.1-mini","stream":true,"input":"Say hello in five words."}' > $OUT/openai_responses_stream.sse
oa /v1/responses '{"model":"gpt-4.1-mini","input":"Say hello in five words."}' > $OUT/openai_responses.json
oa /v1/embeddings '{"model":"text-embedding-3-small","input":"hello","dimensions":8}' > $OUT/openai_embeddings.json
an /v1/messages '{"model":"claude-haiku-4-5","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"Say hello in five words."}]}' > $OUT/anthropic_stream.sse
an /v1/messages '{"model":"claude-haiku-4-5","max_tokens":50,"messages":[{"role":"user","content":"Say hello in five words."}]}' > $OUT/anthropic.json

grep -ho '"usage":{[^}]*}[^}]*}*' $OUT/* | sort -u
