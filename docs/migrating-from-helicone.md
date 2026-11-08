# Migrating from Helicone

llmtee reads Helicone's request headers, so most clients only need a new base URL.

## Base URLs

| Helicone | llmtee |
|---|---|
| `https://oai.helicone.ai/v1` (or self-hosted equivalent) | `http://<llmtee>:8080/openai/v1` |
| `https://anthropic.helicone.ai` | `http://<llmtee>:8080/anthropic` |
| `https://openrouter.helicone.ai/api/v1` | `http://<llmtee>:8080/openrouter/v1` |
| Gateway with `Helicone-Target-Url` | add the target as an upstream: `LLMTEE_UPSTREAM_<NAME>=<url>`, then use `/u/<name>/v1` |

## Headers

| Helicone header | llmtee |
|---|---|
| `Helicone-Auth: Bearer sk-helicone-…` | Ignored (Helicone keys mean nothing to llmtee). Replace it with a llmtee key (`Helicone-Auth: Bearer llmtee-…` or `Llmtee-Key: llmtee-…`) to attribute requests and apply budgets. |
| `Helicone-User-Id` | user id |
| `Helicone-Session-Id` | session id |
| `Helicone-Session-Name`, `Helicone-Session-Path` | properties `session_name`, `session_path` |
| `Helicone-Property-<Name>` | property `<name>` (lower case) |
| `Helicone-Omit-Request`, `Helicone-Omit-Response` | do not store that body |
| `Helicone-Cache-Enabled`, `Helicone-Retry-Enabled`, `Helicone-Fallbacks`, `Helicone-RateLimit-Policy` | not supported; removed before forwarding. Use per-key budgets instead of cost-based rate limits. |

All `Helicone-*` and `Llmtee-*` headers are stripped before the request is sent upstream. Responses carry `Llmtee-Request-Id` instead of `Helicone-Id`.

## Running both in parallel

You do not have to switch everything at once:

1. Start llmtee next to Helicone.
2. Move one client (or one environment) to the llmtee base URL. Keep its headers.
3. Compare cost per day for that client in both UIs for a few days.
4. Move the rest. Keep Helicone's base URL as a fallback until you are confident.

Historic Helicone data is not imported. Export it from Helicone if you need it.

## What you lose

Helicone has features llmtee deliberately leaves out: caching, retries, fallbacks, prompt management, experiments, evaluations, and a hosted version. If you rely on them, llmtee is not a drop-in replacement for that part.
