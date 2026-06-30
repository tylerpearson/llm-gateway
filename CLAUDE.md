# CLAUDE.md: llm-gateway repo conventions

This file is read by Claude Code at the start of every session in this repo. Follow these conventions exactly.

## Module and language

- Module path: `github.com/tylerpearson/llm-gateway`
- Go version: 1.26 (see `go.mod`)
- No CGO. Standard `net/http` for streaming; no framework other than chi.

## Package layout

```
llm-gateway/
  cmd/gateway/main.go          # entrypoint: config load, wiring, serve
  cmd/gatewayctl/main.go       # admin CLI: keys, teams, budgets, rules
  internal/
    config/                    # YAML loader with env-var secret injection
    server/                    # HTTP listener, chi router, health/metrics handlers
    middleware/                # request-id, auth, rate-limit, logging
    proxy/                     # streaming tee, response forwarding
    provider/                  # provider interface + anthropic/, openai/, glm/, translate/
    router/                    # alias and tier routing logic
    cache/                     # Redis exact-match response cache
    ratelimit/                 # Redis counters for rate limits and budgets
    attribution/               # cost computation using pricing table
    pricing/                   # versioned per-model token rate table
    store/mysql/               # config store: keys, teams, budgets, routing rules
    store/clickhouse/          # request log writer and analytics queries
    metrics/                   # Prometheus collector registration
    eval/                      # v2 seam: MirrorHook interface (no-op in v1)
  migrations/mysql/            # golang-migrate SQL migrations
  migrations/clickhouse/
  deploy/                      # docker-compose.yml, Prometheus config, Grafana dashboards
  charts/llm-gateway/          # Helm chart (Deployment, Service, HPA, ConfigMap, Secret, Ingress)
  configs/config.example.yaml  # reference config; copy to config.yaml locally
  test/                        # mock provider server + integration / e2e tests
  Makefile
  Dockerfile
  go.mod  go.sum
  .github/workflows/ci.yml
```

Not all of these directories exist yet; they are added phase by phase. See `PLAN.md` for the build order.

## Build, test, lint, vuln

```bash
make build   # compile cmd/gateway
make test    # go test ./... -race
make lint    # golangci-lint run
make vuln    # govulncheck ./...
make run     # run gateway locally (requires configs/config.yaml)
make up      # docker compose up (full dev stack)
make down    # docker compose down
```

All four gates (build, test, lint, vuln) must be green before a commit lands on `main`.

## Writing conventions (strict)

- **No em dashes.** The Unicode em dash (U+2014) is banned in README files, code comments, commit messages, and PR descriptions. Use a comma, colon, or rewrite the sentence instead. After editing, verify with `grep -Pn "\xe2\x80\x94" <file>` (matches the UTF-8 encoding of U+2014).
- No trailing whitespace. No BOM.
- Comments: complete sentences, no em dashes, no excessive abbreviation.
- Commit messages: imperative mood, under 72 chars for the subject line.

## Phased build order

The project is built in phases P0..P9. Each phase leaves a working, testable gateway. `PLAN.md` is the source of truth for what each phase delivers and the overall architecture. Read it before starting any phase.

Current state: P0 through P3 complete. P0 scaffold (config loader, HTTP server, dev stack, CI); P1 streaming `/v1/messages` proxy to Anthropic with usage capture; P2 virtual key auth backed by MySQL with `gatewayctl` seeding; P3 per-request cost attribution to ClickHouse. Next: P4 (routing, OpenAI and GLM adapters, `/v1/chat/completions`).

Integration tests that need MySQL or ClickHouse live behind the `integration` build tag and skip without a DSN; run them with `go test -tags=integration ./...` (the CI integration job provides both services).

## Secrets and security

- Provider API keys come from environment variables named in the config (`api_key_env` field). They are never written to the config file or committed to the repo.
- MySQL DSN, ClickHouse DSN, and Redis address come from `MYSQL_DSN`, `CLICKHOUSE_DSN`, and `REDIS_ADDR` environment variables.
- Virtual keys are hashed at rest. The raw key is shown once at creation and never stored.
- Add `.env` files to `.gitignore`. Never commit any file containing a real secret.

## Code review and PR process

- Run `/code-review` (or the equivalent) before opening a PR.
- `go test ./... -race` must pass. New behavior gets table-driven tests using `httptest` for HTTP layers and a mock provider for proxy logic.
- All commits go through PRs into `main`. Direct pushes to `main` are not allowed.
- PR titles follow the same convention as commit subjects: imperative mood, phase prefix where applicable (e.g. `P1: add streaming proxy for /v1/messages`).
