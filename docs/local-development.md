# Local development

This guide gets the gateway running on your machine two ways: a fast
unauthenticated smoke test with no datastores, and the full stack (auth,
analytics, dashboards) via Docker Compose.

Prerequisites:

- Go 1.26 (for building and for `make run`).
- Docker with the Compose plugin (for `make up`).
- At least one real provider API key if you want live upstream responses.
  Without one the proxy still runs, but routed requests return an upstream
  error.

## Option A: fast smoke test, no datastores

The quickest way to exercise the proxy path. With `MYSQL_DSN` unset the gateway
runs unauthenticated (it logs a prominent warning) and, with `CLICKHOUSE_DSN`
and `REDIS_ADDR` unset, skips the request log, cache, and counters. This is for
local development only.

```bash
git clone https://github.com/tylerpearson/llm-gateway.git
cd llm-gateway

cp configs/config.example.yaml configs/config.yaml
export ANTHROPIC_API_KEY=sk-ant-...

# Runs the gateway with the example config on :8080.
make run
```

Send a request (the gateway speaks the Anthropic Messages shape on
`/v1/messages`):

```bash
curl -sN http://localhost:8080/v1/messages \
  -H 'content-type: application/json' \
  -d '{
        "model": "default",
        "max_tokens": 128,
        "messages": [{"role": "user", "content": "Say hello in one word."}]
      }'
```

`model` here is a virtual alias (`default`, `fast`, or `frontier`), not a raw
provider model. Routing resolves the alias to a concrete provider and model.

Check liveness and metrics while it runs:

```bash
curl -s http://localhost:8080/healthz   # {"status":"ok"}
curl -s http://localhost:8080/readyz    # {"status":"ready"}
curl -s http://localhost:8080/metrics | grep llmgw_
```

## Option B: full stack with Docker Compose

`make up` builds the gateway image and starts it alongside MySQL, ClickHouse,
Redis, Prometheus, and Grafana on a shared network. Provider API keys are read
from your host environment; the datastore connection strings are wired to the
companion services automatically.

```bash
export ANTHROPIC_API_KEY=sk-ant-...
export OPENAI_API_KEY=sk-...      # optional
export GLM_API_KEY=...            # optional

make up      # docker compose up (foreground); Ctrl-C to stop
make down    # stop and keep volumes
```

Published ports on your host:

| Service | URL | Notes |
|---------|-----|-------|
| Gateway | http://localhost:8080 | Proxy and operational endpoints |
| Prometheus | http://localhost:9090 | Scrapes the gateway `/metrics` |
| Grafana | http://localhost:3000 | Login `admin` / `admin`; dashboards are provisioned on startup |

The MySQL, ClickHouse, and Redis containers are reachable only inside the
Compose network, not from your host by default.

### Apply migrations and seed a virtual key

The gateway does not migrate on startup. To use virtual keys and see request
logs you apply the schema once and seed a key with `gatewayctl`. Build the
binaries first:

```bash
make build   # produces bin/gateway and bin/gatewayctl
```

`gatewayctl` needs to reach MySQL. The simplest local approach is to publish the
MySQL port. Add a port mapping to the `mysql` service in
`deploy/docker-compose.yml`:

```yaml
  mysql:
    image: mysql:8
    ports:
      - "3306:3306"      # add this line for local admin access
```

Re-run `make up`, then run the admin commands against the published port. The
Compose dev credentials are `gateway` / `gateway`, database `llmgateway`:

```bash
export MYSQL_DSN='gateway:gateway@tcp(127.0.0.1:3306)/llmgateway?parseTime=true'
export CLICKHOUSE_DSN='clickhouse://127.0.0.1:9000/default'   # if you also publish 9000

bin/gatewayctl migrate                                   # applies MySQL (and ClickHouse) schema
bin/gatewayctl team create acme                          # prints the team id
bin/gatewayctl key create --team <team-id> --name dev    # prints the key once
```

`key create` prints the raw `llmgw_...` key exactly once and stores only its
sha256 hash. Copy it now; it cannot be recovered later. Other admin commands:

```bash
bin/gatewayctl team list
bin/gatewayctl key list --team <team-id>
bin/gatewayctl key disable --id <key-id>
bin/gatewayctl audit --limit 20
```

### Send an authenticated request

Once a key exists, authenticate with it (the gateway holds the real provider
key; clients never see it):

```bash
curl -sN http://localhost:8080/v1/messages \
  -H 'content-type: application/json' \
  -H 'x-api-key: llmgw_...' \
  -H 'x-llm-tier: frontier' \
  -d '{"model":"default","max_tokens":128,"messages":[{"role":"user","content":"Hello"}]}'
```

Inspect the response headers to see what the gateway did: `x-llm-cache`
(hit or miss), `x-llm-cache-key`, and `x-llm-cost-usd` (a header on cache hits,
an HTTP trailer on live responses). See the [Response signals](../README.md#response-signals)
table for the full list.

### See spend in Grafana

Open http://localhost:3000 (admin / admin) and browse the provisioned
dashboards: Spend Overview, Per-Team FinOps, Latency and Reliability, and Cache
Effectiveness. Send a few requests, then refresh; the Spend Overview dashboard
includes a Top Spending Client Tools panel keyed on the captured `User-Agent`,
so Claude Code traffic shows up distinctly from other clients.

## The quality gates

All four must be green before a change lands on `main`:

```bash
make build   # compile the binaries
make test    # go test ./... -race
make lint    # golangci-lint run
make vuln    # govulncheck ./...
```

`make ci` runs the vet, lint, test, and vuln gates together. Integration tests
that need MySQL or ClickHouse live behind the `integration` build tag and skip
without a DSN; run them with `go test -tags=integration ./...`.
