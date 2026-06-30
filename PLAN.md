# Plan: llm-gateway (production-grade multi-provider LLM gateway in Go)

## Context

Developers using Claude Code (and other LLM clients) directly against provider APIs on metered billing produce runaway, unattributable token spend. The fix the industry has converged on is an "AI gateway": a single proxy that every LLM call flows through, so cost control, routing, caching, and observability happen in one place instead of in every client. The strategy is "make the change easy, then make the easy change", meaning build a transparent pass-through with logging first, then optimize spend centrally.

This project builds that gateway from scratch in Go, to a bar a public company could actually deploy. It doubles as a portfolio and learning artifact for the user's FinOps + AI-platform direction. Greenfield: a survey of `~/projects` found no existing gateway/proxy code to reuse.

## Decisions (locked with the user)

- **Build**: from scratch (no LiteLLM). **Language**: Go. **Intent**: production-grade, deployable.
- **Providers**: multi-provider day one. Anthropic, OpenAI, and GLM (OpenAI-compatible endpoint).
- **Storage**: MySQL (config: keys, teams, budgets, routing rules) + ClickHouse (request logs and FinOps analytics) + Redis (response cache and rate-limit / budget counters).
- **Dashboards**: Grafana over Prometheus + ClickHouse, provisioned as code.
- **Deploy**: Docker image + Helm chart; docker-compose for local dev.
- **Eval-gated model swaps**: v2. v1 ships the interface seams and ClickHouse schema only.
- **Module path**: `github.com/tylerpearson/llm-gateway` (placeholder handle, easy to change).
- **Docs convention**: no em dashes in README, comments, commits (matches the user's other projects).

## v1 feature scope

1. Core streaming proxy with multi-provider tier routing (cheap default, escalate to frontier).
2. Cost attribution + virtual keys (per team/dev), request logging with token and cost capture.
3. Exact-match response caching.
4. Observability (Prometheus metrics, health) + per-team budgets and soft rate limits.

## Architecture

```
Claude Code / apps  ->  llm-gateway (Go)  ->  Anthropic | OpenAI | GLM
                              |
              MySQL (config)  | ClickHouse (logs)  | Redis (cache + counters)
                              |
                  Prometheus  ->  Grafana (FinOps + reliability dashboards)
```

Two ingress endpoints so existing clients work unmodified:
- `POST /v1/messages` (Anthropic Messages shape; Claude Code points `ANTHROPIC_BASE_URL` here).
- `POST /v1/chat/completions` (OpenAI shape; apps and GLM clients).

Request middleware chain: recover + request-id -> auth (virtual key lookup, MySQL-backed with cache) -> rate-limit + budget check (Redis) -> route (alias/tier -> provider+model) -> cache lookup (Redis) -> provider forward with streaming tee for usage capture -> on completion: compute cost, async-write log to ClickHouse, increment Redis counters, update Prometheus metrics.

### Routing
Virtual model aliases (`default`, `fast`, `frontier`) mapped in config to concrete provider+model, with a per-key/team default. Escalation via the alias or an `x-llm-tier: frontier` header. Concrete model names pass through to their provider. Deterministic in v1 (no complexity heuristic yet).

### Provider abstraction
`internal/provider` defines one interface (`Complete(ctx, unifiedReq) -> (stream, usage, err)`) implemented by `anthropic/` and `openai/` adapters that translate the unified request to provider wire format and normalize token usage. GLM is served via the openai adapter configured with GLM's base URL. Same-shape routing is native; cross-shape cases (an Anthropic `/v1/messages` request routed to an OpenAI-shaped provider like GLM) go through a bounded translation module. This translation layer is the trickiest part and is built and tested in isolation.

### Cost attribution
`internal/pricing` holds a versioned table of per-model input/output (and Anthropic cache-read/write) rates. Each request computes cost from normalized usage and writes one row to ClickHouse `request_logs` (ts, key, team, requested vs served model, provider, tokens, cost, latency, cache hit, status). Grafana aggregates spend by team/model/day and surfaces top-spending keys (the Pareto heavy hitters).

### Caching, budgets, limits
Redis exact-match cache keyed on a hash of the normalized request (model + messages + params); streaming responses are assembled, cached, and replayed as a stream on hit. Per-key and per-team requests/min, tokens/min, and monthly dollar budgets tracked with atomic Redis counters. Default to soft mode (alert + response header) over hard 429s, following "better defaults, not usage caps".

### Security (public-company bar)
Virtual keys hashed at rest (never logged). Provider API keys sourced from env / k8s secrets, never in code. Prompt/response redaction toggle for ClickHouse logging. Admin/config changes audit-logged. TLS terminated at ingress.

### Observability
Prometheus collectors for request count, latency histograms, tokens, cost, cache hit/miss, limit rejections, upstream errors. `/healthz`, `/readyz`, `/metrics`. Structured JSON logs via slog. Grafana dashboards provisioned as JSON: Spend Overview, Per-Team FinOps, Latency/Reliability, Cache Effectiveness.

### v2 seams (built as no-ops in v1)
`internal/eval` defines a `MirrorHook` interface invoked post-routing for future shadow traffic, plus ClickHouse `eval_runs` / `eval_results` schema present but unused.

## Project layout

```
llm-gateway/
  cmd/gateway/main.go          # entrypoint + wiring
  cmd/gatewayctl/main.go       # admin CLI (keys, teams, budgets, rules)
  internal/
    config/  server/  middleware/  proxy/
    provider/{provider.go, anthropic/, openai/, translate/}
    router/  cache/  ratelimit/  attribution/  pricing/
    store/{mysql/, clickhouse/}  metrics/  eval/
  migrations/{mysql/, clickhouse/}
  deploy/{docker-compose.yml, prometheus/, grafana/}
  charts/llm-gateway/          # Helm chart (deploy, svc, hpa, configmap, secret, ingress)
  configs/config.example.yaml  # routing rules, provider endpoints, pricing
  test/                        # mock provider + integration/e2e
  Makefile  Dockerfile  README.md  CLAUDE.md  go.mod
  .github/workflows/ci.yml
```

Stack choices: chi router, streaming via net/http, slog logging, golang-migrate for migrations, official mysql / clickhouse-go / go-redis drivers, prometheus/client_golang. golangci-lint + govulncheck in CI. Table-driven tests with a mock provider (httptest).

## Build order (each phase leaves a working, testable gateway)

- **P0 Scaffold**: module, dirs, config loader, `/healthz`, Dockerfile, docker-compose (gateway + MySQL + ClickHouse + Redis + Prometheus + Grafana), Makefile, CI.
- **P1 Core proxy**: `/v1/messages` streaming pass-through to Anthropic, provider interface, mock-provider tests. First milestone: point `ANTHROPIC_BASE_URL` at it and get a streamed response.
- **P2 Virtual keys + auth**: MySQL store + migrations, auth middleware, `gatewayctl` seed.
- **P3 Attribution**: pricing table, cost calc, ClickHouse logging.
- **P4 Routing + providers**: aliases/tiers, OpenAI + GLM adapters, `/v1/chat/completions`, cross-shape translation module.
- **P5 Caching**: Redis exact-match cache with streaming replay.
- **P6 Budgets + rate limits**: Redis counters, soft/hard modes.
- **P7 Observability**: Prometheus metrics + provisioned Grafana dashboards.
- **P8 Hardening + ship**: redaction, admin API/audit, Helm chart, README, CLAUDE.md.
- **P9 v2 seams**: eval MirrorHook interface + ClickHouse eval schema (no-op).

## Verification

- `make up` brings up the full stack via docker-compose. Seed an admin key, a team, and a virtual key via `gatewayctl`.
- Point Claude Code at it: `ANTHROPIC_BASE_URL=http://localhost:8080`, `ANTHROPIC_API_KEY=<virtual key>` (gateway holds the real provider key). Run a prompt and confirm the response streams.
- Confirm the request appears in ClickHouse with tokens and cost. Confirm Grafana spend dashboard updates and `/metrics` exposes counters.
- Exercise routing (`default` vs `frontier`), caching (identical request twice; second is a hit), and budgets (small cap triggers soft-limit header).
- `go test ./... -race`, `golangci-lint run`, `govulncheck ./...` all green in CI.

**Caveat on real e2e**: tests use a mock provider by default. A true end-to-end run against Anthropic/OpenAI/GLM needs a real provider API key, which the user supplies via secret. The user's own Claude Code is on a subscription (no API key), so live Anthropic e2e specifically requires obtaining an API key.

## Open item

GitHub handle for the module path is a placeholder (`tylerpearson`). Confirm or it stays as-is and can be renamed with a single sed later.
