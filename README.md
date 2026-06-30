# llm-gateway

> **Work in progress / personal test project.** This is a learning and portfolio
> build under active construction, not a finished or supported product. Phases
> P0 through P3 currently exist (see Status below). Do not deploy it as is. The
> public repository exists so CI can run on free Actions minutes.

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

## Status

**Under active construction.** The project is built in phased increments where each phase leaves a working, testable gateway.

**What currently exists (P0 through P8):**

- YAML config loader with env-var secret injection
- HTTP server (`/healthz`, `/readyz`, `/metrics`) with graceful shutdown
- Docker Compose dev stack (gateway, MySQL, ClickHouse, Redis, Prometheus, Grafana)
- Makefile, GitHub Actions CI (build, race tests, lint, vuln, plus a MySQL, ClickHouse, and Redis integration job)
- **P1**: streaming `POST /v1/messages` pass-through to Anthropic with token usage capture
- **P2**: virtual key auth backed by MySQL (sha256-hashed keys), `gatewayctl` for migrations and seeding teams and keys
- **P3**: per-request cost attribution written asynchronously to ClickHouse `request_logs`
- **P4**: alias/tier routing, OpenAI and GLM adapters, `POST /v1/chat/completions`, and the bounded cross-shape translation module
- **P5**: Redis exact-match response cache with streaming replay
- **P6**: per-key and per-team budgets and rate limits (requests/min, tokens/min, monthly USD) with soft and hard modes
- **P7**: Prometheus metrics and four provisioned Grafana dashboards (spend, FinOps, latency, cache)
- **P8**: prompt redaction (on by default), audit logging of admin changes, and a Helm chart in `charts/llm-gateway`

**Upcoming phases:**

| Phase | Description |
|-------|-------------|
| P9 | v2 seams: eval `MirrorHook` interface and ClickHouse eval schema (no-op placeholders) |

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

# Bring up the full dev stack (gateway, MySQL, ClickHouse, Redis, Prometheus, Grafana).
make up

# Or run the gateway binary directly against an already-running stack.
make run
```

**Seed a team and virtual key** (requires `MYSQL_DSN`):

```bash
gatewayctl migrate                                   # apply MySQL (and ClickHouse) schema
gatewayctl team create acme                          # prints the team id
gatewayctl key create --team <team-id> --name dev    # prints the key once, stores only its hash
```

**Pointing Claude Code at the gateway:**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=<virtual-key>   # the llmgw_... key from gatewayctl key create
```

The gateway holds the real provider key; clients authenticate with their virtual key. When `MYSQL_DSN` is not set the gateway runs unauthenticated and logs a prominent warning (local development only).

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

Routing is controlled by virtual model aliases (`default`, `fast`, `frontier`) and the `x-llm-tier` header. Responses carry `x-llm-cache` (hit or miss) and `x-llm-limit` (any exceeded budget or rate limit).

## Deployment

A Helm chart lives in `charts/llm-gateway` (Deployment, Service, Ingress, HPA, ConfigMap, Secret, ServiceAccount). Provider keys and DSNs are injected from a Kubernetes Secret; the gateway config is rendered into a ConfigMap. Run `helm lint charts/llm-gateway` and `helm template charts/llm-gateway` to validate before installing.

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
