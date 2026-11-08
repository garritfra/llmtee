# Contributing

Thanks for helping. llmtee stays small on purpose, so please read the scope rule before opening a larger pull request.

## Scope

Every feature request goes through three questions:

1. Does it need a second service? Then the answer is no, or an export/webhook.
2. Is it relevant for individual developers and small teams?
3. Can it be solved with the CSV export, `/metrics`, MCP or a webhook instead?

Out of scope: evals, prompt management, playground, response caching, load balancing, fallbacks, SSO, multi-tenancy, provider SDKs. Issues for these get the `out-of-scope` label with a short explanation.

Good contributions: support for more providers' usage formats, pricing fixes, bug fixes with a test, documentation, integration guides.

## Development

Requirements: Go (see `go.mod`), optionally Docker for PostgreSQL, Python and Node for the SDK tests.

```sh
make test           # all Go tests with the race detector
make lint           # golangci-lint
make integration    # official SDKs against a fake upstream
docker run -d -p 5432:5432 -e POSTGRES_USER=llmtee -e POSTGRES_PASSWORD=llmtee postgres:17-alpine
make test-postgres  # storage tests against PostgreSQL
make run            # llmtee on :8080 with password "dev"
llmtee seed         # demo data for the UI
```

Code map:

| Path | What |
|---|---|
| `internal/proxy` | request path: routing, auth, budget check, forwarding with stream tee |
| `internal/usage` | token usage extraction from JSON bodies and SSE streams |
| `internal/pricing` | embedded price table, overrides, cost formula |
| `internal/pipeline` | async processing and batched writes |
| `internal/store` | storage interface, SQLite and PostgreSQL, migrations |
| `internal/ui` | templates, htmx, CSV export |
| `internal/mcp` | MCP server |
| `internal/fakeupstream` | recorded responses for tests |

## Rules of thumb

- Nothing slow or fallible in the request path. Database, pricing and alerts belong in the pipeline.
- Both database backends must behave the same. Do not add features only one of them supports. New migrations go into both `internal/store/migrations/sqlite` and `.../postgres`.
- New provider formats come with a golden fixture in `internal/fakeupstream/fixtures` and a test case.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `docs:` …); the changelog is generated from them.

## Releases

Tag `vX.Y.Z` on `main`. GitHub Actions runs goreleaser, which publishes binaries, checksums and multi-arch images to `ghcr.io/garritfra/llmtee`.
