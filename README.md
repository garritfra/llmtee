# llmtee

**See what your LLM calls cost.** llmtee is a self-hosted proxy for the OpenAI and Anthropic APIs and for OpenAI-compatible providers such as OpenRouter, Ollama, vLLM and Groq. You point your client's base URL at it, and it logs every request with tokens, cost, latency and time to first token.

Like the Unix `tee`, it passes each response through to your client unchanged and keeps a copy.

![llmtee overview](docs/screenshot-overview.png)

- Any client that lets you set a base URL works without code changes, including the OpenAI and Anthropic SDKs, Claude Code, Codex and automation tools.
- It runs as one Go binary with a SQLite file, so a backup is a file copy. PostgreSQL works too.
- Each key can have a daily and monthly budget. Once a key reaches its limit, llmtee answers with HTTP 429 and does not forward the request.
- Local models (Ollama, vLLM) and hosted APIs show up in the same dashboard.
- You can read your data through the web UI, CSV export, Prometheus metrics or an MCP server.

llmtee is built for individuals and small teams who want to see and cap their LLM spend. It is not a gateway with routing, fallbacks or multi-tenancy. The current release is 0.x, so configuration may still change between minor versions.

## Quickstart

```sh
docker run -d --name llmtee -p 8080:8080 -v llmtee-data:/data \
  -e LLMTEE_ADMIN_PASSWORD=change-me ghcr.io/garritfra/llmtee
```

Point your client at llmtee. It forwards your provider key unchanged:

```sh
export OPENAI_BASE_URL=http://localhost:8080/openai/v1
export ANTHROPIC_BASE_URL=http://localhost:8080/anthropic
```

Send any request, then open <http://localhost:8080> and sign in with the admin password. The request appears in the list with its tokens and cost.

Without Docker, download a binary from the [releases](https://github.com/garritfra/llmtee/releases) (Linux, macOS and Windows on amd64 and arm64) and run `LLMTEE_ADMIN_PASSWORD=change-me ./llmtee`. llmtee creates `llmtee.db` in the current directory.

Other setups: [docker-compose.yml](docker-compose.yml), [PostgreSQL compose](deploy/docker-compose.postgres.yml), [systemd unit](deploy/systemd/llmtee.service), [Caddy with TLS](deploy/caddy/Caddyfile). The *Setup* page in the web UI shows how to connect Claude Code, Codex, opencode and Pi, with your instance's base URL filled in. Guides for these agents, the SDKs, Ollama, OpenRouter, vLLM and n8n are in [docs/integrations.md](docs/integrations.md).

## Endpoints

| Client base URL | Upstream |
|---|---|
| `http://<host>:8080/openai/v1` | `https://api.openai.com/v1` |
| `http://<host>:8080/anthropic` (SDKs append `/v1`) | `https://api.anthropic.com` |
| `http://<host>:8080/openrouter/v1` | `https://openrouter.ai/api/v1` |
| `http://<host>:8080/u/<name>/v1` | any upstream you configure, e.g. `LLMTEE_UPSTREAM_OLLAMA=http://localhost:11434` |

llmtee logs chat completions, responses, embeddings and Anthropic messages, streamed or not. It forwards all other paths without logging them. Details are in [docs/headers.md](docs/headers.md#logged-endpoints).

## Keys and budgets

llmtee works with two kinds of keys, and you can use both at once.

A **provider key** is your OpenAI, Anthropic or other API key. By default, clients send it as usual and llmtee forwards it. llmtee stores only its SHA-256 hash, as a key id starting with `pt-`. To give a provider key a name and a budget, label it in the UI or with `llmtee keys label`.

A **llmtee key** (`llmtee-…`) replaces the provider key on the client side. llmtee swaps in the real provider key from `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY` or `LLMTEE_UPSTREAM_<NAME>_API_KEY`, so teammates and agents never see it. Create one in the UI or on the command line:

```sh
llmtee keys create hermes-agent --budget-day 5 --budget-month 60
llmtee keys list
```

A client can also send a llmtee key next to its provider key, in a `Llmtee-Key` header, to attribute its requests to that key. To reject all requests without a valid llmtee key, set `LLMTEE_REQUIRE_PROXY_KEY=true`.

llmtee checks budgets in memory before it forwards a request. A key at or over its limit gets a `429` response in the provider's error format, with a `Retry-After` header. Limits reset at the start of the next UTC day or month. llmtee only knows the cost of a request after the response arrives. The request that crosses the limit therefore completes, and a key can overshoot its budget by that one request.

To tag requests with a user, session or custom property, or to skip storing a body, see [docs/headers.md](docs/headers.md).

## How it works

```
client ──► router ─► auth + budget ─► forwarder ─► upstream
             ▲                          │  (tee)
             └──── stream, unbuffered ──┤
                                        ▼  copy
          usage parser ─► pricing ─► batch writer ─► SQLite / PostgreSQL ─► UI, /metrics, MCP
```

Only routing, the in-memory key and budget check, and forwarding happen while the client waits. llmtee writes and flushes each chunk to the client as soon as it arrives, and passes a copy to a usage parser. Golden tests replay recorded OpenAI and Anthropic streams through the proxy and require byte-identical output.

Pricing, budget updates, metrics and database writes run in the background, in batches every 500 ms or 100 events. If the database is slow or unavailable, llmtee still forwards requests. It drops the log events and counts them in `llmtee_events_dropped_total`.

On an Apple M4 Pro at 200 requests per second against a local fake upstream, llmtee added 0.14 ms at p50 and 0.02 ms at p99, and used about 33 MB of memory. You can reproduce this with `make loadtest`.

The tradeoff is that llmtee sits in the request path. If it is down, your LLM calls fail. Monitor `/healthz` and keep the provider's direct base URL at hand as a fallback.

### Token counts and cost

For OpenAI chat streams, llmtee adds `stream_options.include_usage: true` if the client did not, so the stream ends with a usage chunk. You can turn this off with `LLMTEE_INJECT_STREAM_USAGE=false`. If a response contains no usage, llmtee marks the request as "usage unknown". It never estimates tokens.

Prices come from [LiteLLM's price table](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json), embedded at build time and refreshed weekly. llmtee stores the cost with each request, so later price changes do not alter past requests. To add prices for local or private models, see [docs/configuration.md](docs/configuration.md#prices).

## Configuration

llmtee reads its settings from environment variables or a YAML file. The settings you are most likely to need:

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | `sqlite://llmtee.db` | `sqlite://path` or `postgres://…` |
| `LLMTEE_ADMIN_PASSWORD` | random, printed at startup | UI login, and bearer token for `/metrics` and `/mcp` |
| `LLMTEE_UPSTREAM_<NAME>` | | add an upstream at `/u/<name>/` |
| `LLMTEE_PUBLIC_URL` | address of the UI page | base URL shown in the setup instructions |
| `LLMTEE_LOG_BODIES` | `true` | store request and response bodies |
| `LLMTEE_ALERT_WEBHOOK_URL` | | POST a JSON alert when a budget threshold is crossed |

All settings, including retention, redaction, alerts and running several instances against PostgreSQL, are in [docs/configuration.md](docs/configuration.md).

## Privacy

Body logging is on by default with 30-day retention, so you can inspect the exact prompt and response behind each cost. Bodies can contain personal or confidential data. You can turn body logging off globally (`LLMTEE_LOG_BODIES=false`), per upstream, or per request with the `Llmtee-Omit-*` headers. `LLMTEE_REDACT_FIELDS` replaces named JSON fields before storage.

llmtee never writes provider keys to the database, only their SHA-256 hashes. It sends no telemetry.

## UI, export, metrics, MCP

- **Web UI** at `/`: spend today and this month compared with the previous period, a cost chart that can be split by model, key, user or upstream, budget meters, a filterable request list and request details with bodies. The pages are rendered on the server with Go templates and htmx.
- **CSV export**: the *Export CSV* button exports the current filter, or run `llmtee export --range 30d > requests.csv`.
- **Prometheus** at `/metrics`: request, token and cost counters, request duration, time to first token, budget rejections and dropped events. Send `Authorization: Bearer <admin password>`.
- **Health** at `/healthz`: returns `200` with `{"status":"ok"}` while the proxy runs.
- **MCP** at `/mcp` (streamable HTTP, `Authorization: Bearer <admin password>`): tools `get_spend`, `list_requests` and `get_request`. To add it to Claude Code:

  ```sh
  claude mcp add --transport http llmtee http://localhost:8080/mcp --header "Authorization: Bearer $LLMTEE_ADMIN_PASSWORD"
  ```

## Compared to

| | llmtee | Helicone (self-hosted) | Langfuse | LiteLLM Proxy |
|---|---|---|---|---|
| Integration | base URL | base URL | SDK / OpenTelemetry | base URL |
| Services to run | 1 binary (+ optional Postgres) | several containers | web, worker, Postgres, ClickHouse, Redis, S3 | proxy + Postgres for spend tracking |
| Hard budgets per key | yes | cost-based rate-limit policies via headers | no | yes |
| Status | active | [maintenance mode](https://www.mintlify.com/blog/mintlify-acquires-helicone) since March 2026 | active | active |
| Scope | logging, cost, budgets | logging, analytics | tracing, evals, prompts | gateway: 100+ providers, routing, fallbacks |

If you need evals, prompt management, routing or fallbacks, use one of the others. Moving from Helicone is covered in [docs/migrating-from-helicone.md](docs/migrating-from-helicone.md).

## Limits

- llmtee has no evals, prompt management, playground, response caching, load balancing, fallbacks, SSO or multi-tenancy. These are left out on purpose. If a feature would need a second service, llmtee skips it or offers an export instead.
- The native Gemini, Azure OpenAI and AWS Bedrock APIs are not supported yet. Their OpenAI-compatible endpoints work.
- Token counts come from the provider's response. llmtee marks responses without usage and does not estimate them.
- Costs are as accurate as the price table. llmtee does not model batch, flex or priority pricing.

## Development

```sh
make test          # unit, golden and end-to-end tests (SQLite)
make test-postgres # storage tests against PostgreSQL
make integration   # official Python and TypeScript SDKs against the proxy
make loadtest      # added latency vs. a fake upstream (needs vegeta)
make prices        # refresh the embedded price table
```

`scripts/record-golden.sh` re-records the golden streams from the real APIs. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE). The embedded price data comes from [LiteLLM](https://github.com/BerriAI/litellm) (MIT).
