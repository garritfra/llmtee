# Request headers

Clients can attach metadata to a request with these headers. llmtee also accepts the Helicone equivalents, so clients set up for Helicone keep working (see [Migrating from Helicone](migrating-from-helicone.md)).

| Header | Effect |
|---|---|
| `Llmtee-Key` / `Helicone-Auth: Bearer llmtee-…` | llmtee key, to attribute requests that carry a provider key |
| `Llmtee-User-Id` / `Helicone-User-Id` | user id (falls back to the body's `user` or `metadata.user_id`) |
| `Llmtee-Session-Id` / `Helicone-Session-Id` | session id |
| `Llmtee-Property-<Name>` / `Helicone-Property-<Name>` | custom property, filterable in the UI |
| `Llmtee-Omit-Request`, `Llmtee-Omit-Response`, `Llmtee-Omit-Body` (and Helicone equivalents) | do not store that body |

llmtee removes these headers before it forwards the request. Every response carries a `Llmtee-Request-Id` header.

## Logged endpoints

llmtee reads token usage from `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings` and `/v1/messages`, streamed or not. It forwards every other path too, for example `/v1/models` or the audio endpoints. It does not log `GET` requests or Anthropic's `count_tokens`.
