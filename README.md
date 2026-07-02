# llm-gateway

> **Work in progress / personal test project.** This is a learning and portfolio
> build, not a finished or supported product. v1 (phases P0 through P9) is
> feature-complete (see Status below), but it has not been run in production and
> is not hardened for it. Do not deploy it as is. The public repository exists so
> CI can run on free Actions minutes.

A production-grade multi-provider LLM gateway in Go. Every LLM call from Claude Code and other clients flows through a single proxy so cost control, request routing, response caching, and observability happen in one place rather than in each client. The gateway supports Anthropic, OpenAI, and GLM out of the box, attributes spend to teams and virtual keys, and feeds Grafana dashboards backed by Prometheus and ClickHouse.

## Architecture

```
Claude Code / apps  ->  llm-gateway (Go)  ->  Anthropic | OpenAI | GLM
                              |
              MySQL (config)  | ClickHouse (logs)  | Redis (cache + counters)
                              |
                  Prometheus  ->  Grafana (FinOps + reliability dashboards)
```

Two ingress endpoints let existing clients work unmodified:

- `POST /v1/messages` (Anthropic Messages shape; point `ANTHROPIC_BASE_URL` here from Claude Code)
- `POST /v1/chat/completions` (OpenAI shape; compatible with OpenAI clients and GLM)

The middleware chain handles: request ID and recovery, virtual key auth (MySQL-backed), rate-limit and budget checks (Redis), alias/tier routing, response cache lookup (Redis), streaming tee for usage capture, cost computation, async ClickHouse logging, Redis counter increments, and Prometheus metric updates.

## Documentation

This README is the feature reference. Task-focused guides live under [`docs/`](docs/):

- [Local development](docs/local-development.md): run the full stack with Docker Compose, seed keys, send a first request, and reach Grafana.
- [Kubernetes deployment](docs/kubernetes.md): install the Helm chart, wire secrets and external datastores, enable ingress and autoscaling, and roll out upgrades.
- [Connecting Claude Code](docs/claude-code.md): point Claude Code at the gateway, choose a routing tier, and confirm spend is attributed to the right key and tool.

## Status

**Under active construction.** The project is built in phased increments where each phase leaves a working, testable gateway.

**v1 is feature-complete (P0 through P9).** What exists:

- YAML config loader with env-var secret injection
- HTTP server (`/healthz`, `/readyz`, `/metrics`) with graceful shutdown
- Docker Compose dev stack (gateway, MySQL, ClickHouse, Redis, Prometheus, Grafana)
- Makefile, GitHub Actions CI (build, race tests, lint, vuln, plus a MySQL, ClickHouse, and Redis integration job)
- **P1**: streaming `POST /v1/messages` pass-through to Anthropic with token usage capture
- **P2**: virtual key auth backed by MySQL (sha256-hashed keys), `gatewayctl` for migrations and seeding teams and keys
- **P3**: per-request cost attribution written asynchronously to ClickHouse `request_logs`. Request rows are retained for 90 days by ClickHouse TTL (migration 0005); adjust the interval by adding a later migration.
- **P4**: alias/tier routing, OpenAI and GLM adapters, `POST /v1/chat/completions`, and the bounded cross-shape translation module
- **P5**: Redis exact-match response cache with streaming replay
- **P6**: per-key and per-team budgets and rate limits (requests/min, tokens/min, monthly USD) with soft and hard modes
- **P7**: Prometheus metrics and four provisioned Grafana dashboards (spend, FinOps, latency, cache)
- **P8**: prompt redaction (on by default), audit logging of admin changes, and a Helm chart in `charts/llm-gateway`
- **P9**: v2 eval seams: the `MirrorHook` interface (invoked post-routing, no-op in v1) and the ClickHouse `eval_runs` / `eval_results` schema

Still a WIP/test project: tests use a mock provider by default, so a true end-to-end run against Anthropic, OpenAI, or GLM requires a real provider API key.

## Quickstart (local dev)

Requires Docker with the Compose plugin.

```bash
git clone https://github.com/tylerpearson/llm-gateway.git
cd llm-gateway

# Copy the example config and set provider API keys.
cp configs/config.example.yaml configs/config.yaml
export ANTHROPIC_API_KEY=sk-ant-...
export OPENAI_API_KEY=sk-...
export GLM_API_KEY=...

# Bring up the full dev stack (gateway, MySQL, ClickHouse, Redis, Prometheus,
# Grafana). This already runs the gateway at http://localhost:8080, so for a
# turnkey local run this one command is enough.
make up
```

`make run` instead runs the gateway from source on your host, which is handy for
a fast edit-rebuild loop. It connects to MySQL, ClickHouse, and Redis only when
the matching `MYSQL_DSN`, `CLICKHOUSE_DSN`, and `REDIS_ADDR` variables are set in
your shell; with none set it starts unauthenticated with those features off (a
zero-dependency quick start). To run it fully wired, start the stack with
`make up` in another terminal (it publishes the datastore ports on `127.0.0.1`),
then point the gateway at them:

```bash
export MYSQL_DSN='gateway:gateway@tcp(127.0.0.1:3306)/llmgateway?parseTime=true'
export CLICKHOUSE_DSN='clickhouse://127.0.0.1:9000/default'
export REDIS_ADDR='127.0.0.1:6379'
make run
```

If `make run` reports `connection refused`, a `*_DSN` variable is set but the
stack is not reachable: start it with `make up`, or `unset` the variable to run
unauthenticated.

**Seed a team and virtual key.** With the dev stack from `make up` running, the
`make` helpers point `gatewayctl` at the local datastores for you, so there is no
DSN to set:

```bash
make migrate                             # apply MySQL and ClickHouse schema
make ctl ARGS="team create acme"         # prints the team id
make ctl ARGS="key create --team <team-id> --name dev"   # prints the key once, stores only its hash
```

`gatewayctl` is a project binary, not a globally installed command, which is why
a bare `gatewayctl ...` gives "command not found". The `make ctl` target runs it
via `go run` against the dev stack. To invoke it directly instead, build it with
`make build` and run `./bin/gatewayctl <command>` from the repo root, setting
`MYSQL_DSN` (and `CLICKHOUSE_DSN` for `migrate`) yourself. Override the DSNs the
`make` targets use by exporting `MYSQL_DSN` / `CLICKHOUSE_DSN` before running them.

**Pointing Claude Code at the gateway:**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=<virtual-key>   # the llmgw_... key from gatewayctl key create
```

The gateway holds the real provider key; clients authenticate with their virtual key. When `MYSQL_DSN` is not set the gateway runs unauthenticated and logs a prominent warning (local development only).

### Troubleshooting local dev

- **`docker compose ... unknown shorthand flag: 'f'` or `command not found`**: the
  Docker CLI has no Compose plugin. Install it with `brew install docker-compose`
  and ensure an engine is running (Docker Desktop or Colima). The `make` targets
  auto-detect either the `docker compose` plugin or the standalone `docker-compose`.
- **`gatewayctl: command not found`**: it is a project binary, not on your `PATH`.
  Use `make ctl ARGS="..."`, or `./bin/gatewayctl` after `make build`.
- **`connect: connection refused` on `make run` or `gatewayctl`**: a `*_DSN`
  variable is set but the datastore is not reachable. Start the stack with
  `make up` (it publishes MySQL, ClickHouse, and Redis on `127.0.0.1`), or `unset`
  the variable to run the gateway unauthenticated with those features off.
- **ClickHouse `Authentication failed ... default` (code 516)**: recent ClickHouse
  images lock the `default` user to localhost when given no credentials, blocking
  the gateway over the Docker network. The dev stack sets
  `CLICKHOUSE_SKIP_USER_SETUP=1` to keep the classic open dev behavior. If you see
  this after upgrading an old stack, recreate ClickHouse: `make down && make up`.

## Configuration

Copy `configs/config.example.yaml` to `configs/config.yaml` and adjust. The file controls server settings, log level/format, provider endpoints, model alias routing, and storage connection settings.

**Secrets and DSNs are never stored in the config file.** Each sensitive field names an environment variable from which the value is read at startup:

| Environment variable | Purpose |
|----------------------|---------|
| `ANTHROPIC_API_KEY` | Anthropic provider API key |
| `OPENAI_API_KEY` | OpenAI provider API key |
| `GLM_API_KEY` | GLM (BigModel) provider API key |
| `MYSQL_DSN` | MySQL connection string (config store) |
| `CLICKHOUSE_DSN` | ClickHouse connection string (request log store) |
| `REDIS_ADDR` | Redis address (cache and counters) |

Example config excerpt:

```yaml
server:
  addr: ":8080"
  read_timeout: 30s
  write_timeout: 0s      # keep at 0 so streaming responses are not cut off
  shutdown_timeout: 15s

logging:
  level: info            # debug | info | warn | error
  format: json           # json | text

providers:
  anthropic:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY

routing:
  default_alias: default
  aliases:
    default:
      provider: anthropic
      model: claude-haiku-4-5-20251001
    frontier:
      provider: anthropic
      model: claude-opus-4-8
```

## Endpoints

Operational endpoints available today:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/healthz` | Process liveness: returns `{"status":"ok"}` while the process is running |
| GET | `/readyz` | Readiness: returns 503 until startup wiring completes, then `{"status":"ready"}` |
| GET | `/metrics` | Prometheus metrics (Go runtime, process, and `llmgw_*` request collectors) |
| POST | `/v1/messages` | Anthropic Messages proxy: authenticated, routed, cached, rate-limited, streamed, with usage and cost capture |
| POST | `/v1/chat/completions` | OpenAI Chat Completions proxy: same pipeline, cross-shape translation when routed to an Anthropic provider |
| GET | `/cache/ping` | Cache backend health probe (when the cache is enabled and auth is configured) |
| POST | `/cache/delete` | Evict one cache entry by key: JSON body `{"key": "..."}` (when the cache is enabled and auth is configured) |

Routing is controlled by virtual model aliases (`default`, `fast`, `frontier`) and the `x-llm-tier` request header.

### Request headers

Clients steer the gateway with a small set of `x-llm-*` request headers. All are optional.

| Header | Effect |
|--------|--------|
| `x-llm-tier` | Select a routing tier or alias for this request, overriding the key's default alias. |
| `x-llm-tags` | Comma-separated spend tags recorded on the request log (see [spend attribution](#spend-attribution-dimensions)). |
| `x-llm-end-user` | End-customer id recorded on the request log, taking precedence over any `user` field in the body. |
| `Cache-Control` | Per-request cache directives (see [per-request cache control](#per-request-cache-control)). |

### Response signals

Every proxied response carries `x-llm-*` signals so clients and log scrapers can see what the gateway did. Because token usage is only known after a streamed body finishes, the per-request cost is delivered as an HTTP **trailer** on live responses and as a normal header on cache hits (where the cost is known up front and is zero).

| Signal | Kind | Meaning |
|--------|------|---------|
| `x-llm-cache` | Header | `hit` when served from the response cache, `miss` when fetched from the upstream. |
| `x-llm-cache-key` | Header | The cache entry key, for inspection or targeted eviction via `POST /cache/delete`. |
| `x-llm-cost-usd` | Header on cache hits, trailer on live responses | The computed USD cost of this response, formatted to six decimals. A cache hit reports `0.000000` because it incurred no upstream spend, which surfaces cache savings directly. Live responses announce it in the `Trailer` header and set the value once usage is captured. |
| `x-llm-limit` | Header | Comma-separated budgets or rate limits this request exceeded (present only on a breach). |

Reading the cost trailer requires a client that surfaces HTTP trailers (for example Go's `http.Response.Trailer` after the body is fully read, or `curl --raw` over a chunked response). Clients that ignore trailers still receive the full response body unaffected.

### Per-request cache control

Clients can steer caching per request with a `Cache-Control` header, honoring an HTTP-aligned subset:

| Directive | Effect |
|-----------|--------|
| `no-store` | Do not read from or write to the cache for this request |
| `no-cache` | Skip the cached response and fetch fresh, but still store the result |
| `s-maxage=<seconds>` | Serve a cached hit only if it is younger than this |
| `ttl=<seconds>` | Override the store expiry for this response |

Read the `x-llm-cache-key` from a response, then `POST /cache/delete` with that key to bust a specific stored entry.

### Spend attribution dimensions

Every request log carries the virtual key and team. Requests can add finer dimensions so cost can be sliced by tool, customer, or cost center in ClickHouse and Grafana:

| Dimension | Source |
|-----------|--------|
| `user_agent` | The client `User-Agent` header, so spend can be split by tool (for example Claude Code versus a CLI). Always captured. |
| `end_user` | The end customer, resolved from the `x-llm-end-user` header, then the request body `user` field (OpenAI shape), then `metadata.user_id` (Anthropic shape). First non-empty wins. |
| `tags` | The comma-separated `x-llm-tags` header, plus any headers named in `attribution.tag_headers` captured as `name:value`. Tags are deduplicated and sorted. |

Configure the tag headers under `attribution.tag_headers` in the config; `User-Agent`, `x-llm-end-user`, and `x-llm-tags` need no configuration.

### Upstream failover

An alias can declare an ordered `fallbacks` chain, and a `routing.resilience` block turns on bounded retries and a circuit breaker:

```yaml
routing:
  aliases:
    default:
      provider: anthropic
      model: claude-haiku-4-5-20251001
      fallbacks:
        - provider: openai
          model: gpt-4o-mini
  resilience:
    max_retries: 2
    retry_backoff: 200ms
    request_timeout: 0s      # 0 means no added deadline; never cuts a stream
    cooldown: 30s
    cooldown_threshold: 5
    retryable_status: [429, 500, 502, 503, 504]
```

When the primary target fails with a retryable status (or a transport error), the gateway retries with exponential backoff and then fails over to the next candidate, all before the first response byte is relayed. Once a streamed response starts, it cannot be retried, so a mid-stream upstream drop surfaces as a truncated response. A status not in `retryable_status` (including client errors and success) is relayed verbatim and never triggers failover. Repeated failures against one target open a Redis-shared circuit breaker that ejects it for `cooldown`, so every replica skips it until it recovers. Retries and fallbacks work without Redis; only the shared cooldown needs `REDIS_ADDR`. A failed-over request is attributed to the target that actually served it and logged with `failover=true`; new metrics `llmgw_upstream_retries_total`, `llmgw_failover_total`, and `llmgw_breaker_open` track the behavior.

### Context-window pre-check

With `routing.resilience.context_check.enabled: true`, the gateway estimates a request's token size and skips any candidate model whose context window cannot fit it, failing over to a larger-context model in the chain. When no candidate fits, the request is rejected with 413 before any upstream call rather than sent to fail. The estimate is conservative: without a bundled tokenizer it approximates from the request's character count (`chars_per_token`, default 4), inflates by `safety_margin` (default 0.15), and adds the requested `max_tokens`. It is a guard, not an exact token count. Per-model context windows live in the pricing table; unknown models fail open (the check is skipped). Skips are counted by `llmgw_context_skips_total{model}`.

### Request guardrails

`security.guard` installs a pre-call guard that acts on the request body actually sent upstream, distinct from `redact_prompts` (which only affects logs). A guard can **mask** the body or **block** the request:

```yaml
security:
  guard:
    enabled: true
    type: regex_mask
```

The built-in `regex_mask` guard is a reference implementation that redacts emails, secret API keys (`sk-`, `AKIA...`), credit-card numbers, and US Social Security numbers, replacing each match with `[REDACTED]` before the request is hashed for caching and sent upstream. A blocked request returns 403 and is never sent upstream or cached. The guard runs after rate-limit checks and before the cache lookup, so a masked body caches consistently and a blocked request neither serves nor stores a cache entry. Actions are counted by `llmgw_guard_actions_total{action,category}`. The seam (`internal/guard`) is the extension point for real PII, secret, or prompt-injection detectors; response/output guarding is out of scope in v1 because it conflicts with streaming.

## Deployment

A Helm chart lives in `charts/llm-gateway` (Deployment, Service, Ingress, HPA, ConfigMap, Secret, ServiceAccount). Provider keys and DSNs are injected from a Kubernetes Secret; the gateway config is rendered into a ConfigMap. Run `helm lint charts/llm-gateway` and `helm template charts/llm-gateway` to validate before installing.

See the [Kubernetes deployment guide](docs/kubernetes.md) for a full walkthrough: managing secrets, pointing at external MySQL, ClickHouse, and Redis, enabling ingress and the HPA, Prometheus scraping, and rolling upgrades.

## Development

```bash
make build   # compile the gateway binary
make test    # go test ./... -race
make lint    # golangci-lint run
make vuln    # govulncheck ./...
make run     # run the gateway locally (requires configs/config.yaml)
make up      # docker compose up (full dev stack)
make down    # docker compose down
```

All four quality gates (build, test, lint, vuln) must be green before committing.

## Tech Stack

| Layer | Choice |
|-------|--------|
| Language | Go 1.26 |
| HTTP router | [chi](https://github.com/go-chi/chi) |
| Logging | `log/slog` (structured JSON) |
| Metrics | [Prometheus client_golang](https://github.com/prometheus/client_golang) |
| Config store | MySQL + [golang-migrate](https://github.com/golang-migrate/migrate) |
| Request log / analytics | ClickHouse |
| Cache and counters | Redis |
| Dashboards | Grafana (provisioned as JSON) |
| Dev stack | Docker Compose |
| Production deploy | Docker image + Helm chart |
| CI | GitHub Actions (golangci-lint, govulncheck) |

## License

TBD.
