# Configuration

llmtee reads its settings from environment variables. A YAML file (`--config` or `LLMTEE_CONFIG`, see [examples/config.yaml](../examples/config.yaml)) can hold the same keys. If both set a value, the environment variable wins.

## Settings

| Variable | Default | |
|---|---|---|
| `LLMTEE_LISTEN` | `:8080` | listen address |
| `DATABASE_URL` | `sqlite://llmtee.db` (`/data/llmtee.db` in Docker) | `sqlite://path` or `postgres://…` |
| `LLMTEE_ADMIN_PASSWORD` | random, printed at startup | UI login, and bearer token for `/metrics` and `/mcp` |
| `LLMTEE_UI_AUTH` | `true` | `false` if a reverse proxy already authenticates |
| `LLMTEE_READ_ONLY` | `false` | read-only UI (demo instances) |
| `LLMTEE_PUBLIC_URL` | address of the UI page | URL clients use to reach llmtee, shown on the Setup page, e.g. `https://llm.example.com` |
| `LLMTEE_REQUIRE_PROXY_KEY` | `false` | reject requests without a valid llmtee key |
| `LLMTEE_LOG_BODIES` | `true` | store request and response bodies (zstd-compressed) |
| `LLMTEE_BODY_RETENTION_DAYS` | `30` | delete bodies after N days (0 = never) |
| `LLMTEE_RETENTION_DAYS` | `0` | delete request metadata after N days (0 = never) |
| `LLMTEE_MAX_BODY_BYTES` | `2097152` | truncate stored bodies to this size |
| `LLMTEE_REDACT_FIELDS` | | comma-separated JSON field names to replace with `[REDACTED]` before storage |
| `LLMTEE_INJECT_STREAM_USAGE` | `true` | add `include_usage` to OpenAI chat streams |
| `LLMTEE_PRICES_FILE` | | price overrides, see [Prices](#prices) |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY` | | provider keys that llmtee injects for llmtee keys |
| `LLMTEE_UPSTREAM_<NAME>` | | add an upstream at `/u/<name>/`, e.g. `LLMTEE_UPSTREAM_OLLAMA=http://ollama:11434` |
| `LLMTEE_UPSTREAM_<NAME>_TYPE` | `openai` | API format: `openai` or `anthropic` |
| `LLMTEE_UPSTREAM_<NAME>_API_KEY` | | provider key that llmtee injects for llmtee keys |
| `LLMTEE_UPSTREAM_<NAME>_LOG_BODIES` | | override `LLMTEE_LOG_BODIES` for this upstream |
| `LLMTEE_ALERT_WEBHOOK_URL` | | POST a JSON alert when a budget threshold is crossed |
| `LLMTEE_ALERT_NTFY_URL`, `LLMTEE_ALERT_NTFY_TOKEN` | | send alerts to an [ntfy](https://ntfy.sh) topic |
| `LLMTEE_ALERT_THRESHOLDS` | `0.8,1` | fractions of a budget that trigger an alert |
| `LLMTEE_ALERT_GLOBAL_BUDGET_DAY_USD`, `…_MONTH_USD` | | alert on total spend across all keys |
| `LLMTEE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

## Upstreams

`LLMTEE_UPSTREAM_<NAME>` adds an upstream that clients reach at `http://<host>:8080/u/<name>/v1`. The name must not contain underscores. Setting `LLMTEE_UPSTREAM_OPENAI`, `LLMTEE_UPSTREAM_ANTHROPIC` or `LLMTEE_UPSTREAM_OPENROUTER` changes the base URL of a built-in upstream.

## Prices

llmtee prices requests with [LiteLLM's price table](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json) (MIT). The table is embedded at build time, and CI refreshes it every week. It includes Anthropic's 1-hour cache writes and long-context tiers where LiteLLM lists them. When OpenRouter reports `usage.cost`, llmtee uses that value instead.

The cost of a request is:

```
cost = t_in·p_in + t_out·p_out + t_cache_read·p_cache_read + t_cache_write·p_cache_write
```

`t_in` counts uncached input tokens only. For OpenAI that is `prompt_tokens − cached_tokens`.

llmtee stores the cost with each request. A later price change does not alter past requests.

To override or add prices, point `LLMTEE_PRICES_FILE` at a YAML file. Prices are in USD per million tokens:

```yaml
models:
  "llama3.1:8b": {input: 0, output: 0}
  "my-org/*":    {input: 0.5, output: 1.5, cache_read: 0.05}
upstreams:
  ollama: {input: 0, output: 0}
```

## Several instances

With PostgreSQL, several llmtee instances can share one database. Each instance keeps its own budget counters in memory and re-reads them from the database every 10 seconds. A key can therefore overshoot its budget by what all instances spend within that window. With SQLite, run a single instance.
