# Integrations

<!-- setup-page:skip -->
Replace `localhost:8080` with wherever llmtee runs. The *Setup* page in the web UI shows this guide with your instance's base URL filled in.
<!-- /setup-page:skip -->

In every example the client keeps its own provider key (passthrough). To use a managed llmtee key instead, put the `llmtee-…` key where the provider key would go.

Add `Llmtee-User-Id`, `Llmtee-Session-Id` or `Llmtee-Property-<Name>` headers to attribute requests (see [Request headers](headers.md)).

## OpenAI SDK

Python:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/openai/v1",
    default_headers={"Llmtee-Property-Feature": "summarizer"},
)
```

TypeScript:

```ts
import OpenAI from "openai";
const client = new OpenAI({ baseURL: "http://localhost:8080/openai/v1" });
```

Or without code changes: `export OPENAI_BASE_URL=http://localhost:8080/openai/v1`.

Chat Completions, Responses and Embeddings are priced, streamed or not.

## Anthropic SDK

```python
import anthropic
client = anthropic.Anthropic(base_url="http://localhost:8080/anthropic")
```

```ts
import Anthropic from "@anthropic-ai/sdk";
const client = new Anthropic({ baseURL: "http://localhost:8080/anthropic" });
```

Or `export ANTHROPIC_BASE_URL=http://localhost:8080/anthropic`. Prompt caching (cache reads, 5-minute and 1-hour cache writes) is priced.

## Claude Code

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080/anthropic
claude
```

To make it permanent, add it to `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8080/anthropic"
  }
}
```

This works with API keys and with a Claude subscription login; the credential is passed through and only its hash is stored. To attribute Claude Code traffic to a llmtee key with a budget, add the key as an extra header:

```sh
export ANTHROPIC_CUSTOM_HEADERS="Llmtee-Key: llmtee-..."
```

Claude Code puts a user/session identifier into `metadata.user_id`, which llmtee shows as the user.

To let Claude Code ask llmtee about spend, add the MCP server:

```sh
claude mcp add --transport http llmtee http://localhost:8080/mcp \
  --header "Authorization: Bearer $LLMTEE_ADMIN_PASSWORD"
```

## Codex CLI

Codex talks to the Responses API. With an API key:

```sh
export OPENAI_BASE_URL=http://localhost:8080/openai/v1
codex
```

Or as a provider in `~/.codex/config.toml`:

```toml
model_provider = "llmtee"

[model_providers.llmtee]
name = "llmtee"
base_url = "http://localhost:8080/openai/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
```

Signing in with a ChatGPT account uses a different backend that llmtee does not proxy.

## opencode

Set the base URL of each provider in `~/.config/opencode/opencode.json` or in `opencode.json` in your project. Keep only the providers you use:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "anthropic": { "options": { "baseURL": "http://localhost:8080/anthropic/v1" } },
    "openai": { "options": { "baseURL": "http://localhost:8080/openai/v1" } },
    "openrouter": { "options": { "baseURL": "http://localhost:8080/openrouter/v1" } }
  }
}
```

opencode's Anthropic provider expects the `/v1` in the base URL.

## Pi

Set the base URL of each built-in provider in `~/.pi/agent/models.json`. Models and login stay unchanged:

```json
{
  "providers": {
    "anthropic": { "baseUrl": "http://localhost:8080/anthropic" },
    "openai": { "baseUrl": "http://localhost:8080/openai/v1" },
    "openrouter": { "baseUrl": "http://localhost:8080/openrouter/v1" }
  }
}
```

## Ollama

```sh
docker run -d -p 8080:8080 -v llmtee-data:/data \
  -e LLMTEE_ADMIN_PASSWORD=change-me \
  -e LLMTEE_UPSTREAM_OLLAMA=http://host.docker.internal:11434 \
  --add-host=host.docker.internal:host-gateway \
  ghcr.io/garritfra/llmtee
```

Clients use `http://localhost:8080/u/ollama/v1` with any API key (Ollama ignores it). Local models are not in the price table; mark them as free so they are not listed as "without known price":

```yaml
# prices.yaml, mounted and set via LLMTEE_PRICES_FILE
upstreams:
  ollama: {input: 0, output: 0}
```

Ollama's OpenAI-compatible API reports usage for streams only when `stream_options.include_usage` is set, which llmtee adds automatically.

## vLLM, LM Studio, llama.cpp server, Groq, Together, DeepSeek …

Any OpenAI-compatible endpoint works the same way:

```sh
LLMTEE_UPSTREAM_VLLM=http://gpu-box:8000      # served at /u/vllm/v1
LLMTEE_UPSTREAM_GROQ=https://api.groq.com/openai/v1
LLMTEE_UPSTREAM_GROQ_API_KEY=gsk_...           # only for managed llmtee keys
```

A trailing `/v1` in the upstream URL is stripped, because clients send it.

## OpenRouter

```python
client = OpenAI(base_url="http://localhost:8080/openrouter/v1", api_key=os.environ["OPENROUTER_API_KEY"])
```

If OpenRouter reports `usage.cost` in the response, llmtee stores that value. Otherwise it looks the model up in the price table (`anthropic/claude-sonnet-4.5` → `claude-sonnet-4-5`).

## n8n and other automation tools

Most LLM nodes have a base URL field in their credentials. Use `http://<llmtee>:8080/openai/v1` or `http://<llmtee>:8080/anthropic`. To see cost per workflow, create one managed llmtee key per workflow (or team) and use it as the API key in that credential. Budgets then stop a runaway workflow.

## curl

```sh
curl http://localhost:8080/openai/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" -H "Content-Type: application/json" \
  -H "Llmtee-User-Id: me" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```
