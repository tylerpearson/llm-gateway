# Plan 003: Add a full-stack end-to-end integration test

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat 1825c44..HEAD -- cmd/gateway/ internal/store/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: tests
- **Planned at**: commit `1825c44`, 2026-07-01

## Why this matters

Every layer of the gateway is unit-tested and the storage packages have
integration tests, but nothing exercises the assembled system: auth against
real MySQL, cache against real Redis, attribution into real ClickHouse, all
through the real HTTP wiring in `cmd/gateway`. PLAN.md promises a `test/`
directory with e2e tests that was never created. A wiring regression (wrong
middleware order, a migration that breaks the key lookup, an attribution
writer that never flushes) would pass every existing test. CI already
provisions MySQL, ClickHouse, and Redis for the `integration` job, so the
infrastructure cost of this test is zero. This also becomes the safety net
required before any refactor of `internal/proxy/proxy.go`.

## Current state

- Integration test convention: files carry `//go:build integration` and skip
  without env DSNs. Exemplar: `internal/cache/cache_integration_test.go`
  (skips unless `REDIS_ADDR` is set). CI runs
  `go test -tags=integration -race ./...` with `MYSQL_DSN`, `CLICKHOUSE_DSN`,
  and `REDIS_ADDR` set (`.github/workflows/ci.yml`, integration job).
- `cmd/gateway/main_test.go` shows how to boot the entire gateway in a test:
  write a temp YAML config, call `run(cfgPath)` in a goroutine, poll
  `/readyz` via `waitForReady(url, timeout)`, then
  `syscall.Kill(syscall.Getpid(), syscall.SIGTERM)` and wait for `run` to
  return (see `TestRun_StartAndGracefulShutdown`, lines 132-162). The new e2e
  test must live in package `main` (same directory) to call `run`.
- Config YAML shape (from `configs/config.example.yaml`):

```yaml
server:
  addr: "127.0.0.1:<port>"
  shutdown_timeout: 5s
logging:
  level: error
  format: json
providers:
  mock:
    type: anthropic
    base_url: <httptest server URL>
    api_key_env: E2E_MOCK_KEY
routing:
  default_alias: default
  aliases:
    default:
      provider: mock
      model: mock-model
storage:
  mysql_dsn_env: MYSQL_DSN
  clickhouse_dsn_env: CLICKHOUSE_DSN
  redis_addr_env: REDIS_ADDR
security:
  redact_prompts: true
  auth_cache_ttl: 5s
```

- Store API for seeding (interface in `internal/store/store.go`, MySQL
  implementation in `internal/store/mysql/mysql.go`):
  - `mysql.Migrate(dsn string) error` (in `internal/store/mysql/migrate.go`)
  - `mysql.Open(dsn string) (*Store, error)`
  - `(*Store).CreateTeam(ctx, name) (*store.Team, error)`
  - `(*Store).CreateKey(ctx, teamID, name, keyHash, defaultAlias) (*store.VirtualKey, error)`
    where `keyHash` is the sha256 hex of the plaintext key (see the comment on
    `store.Store.CreateKey`).
- ClickHouse: `internal/store/clickhouse/` has `Open` and a migrate helper
  (`internal/store/clickhouse/migrate.go`); attribution rows land in the
  `request_logs` table (schema in `migrations/clickhouse/`). The attribution
  writer batches asynchronously and flushes on `Close`, which `run` invokes
  during graceful shutdown.
- Auth headers accepted: `x-api-key: <plaintext key>` (Anthropic style) or
  `Authorization: Bearer <plaintext key>` (see `internal/auth/middleware.go`,
  `extractKey`).
- Mock upstream SSE body: reuse the `anthropicSSE` fixture shape from
  `internal/proxy/proxy_test.go`:

```
event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}

```

- Conventions: no em dashes anywhere, comments are complete sentences.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build   | `make build` | exit 0 |
| Unit tests still pass | `make test` | exit 0 (new file is tag-gated, compiles only with the tag) |
| Compile the tagged file | `go vet -tags=integration ./cmd/gateway/` | exit 0 |
| Run e2e locally | `make up` then `MYSQL_DSN=... CLICKHOUSE_DSN=... REDIS_ADDR=... go test -tags=integration -race ./cmd/gateway/ -run TestE2E` | pass |
| Lint | `make lint` | exit 0 |

Local DSNs for the compose stack are documented in
`docs/local-development.md`; the CI values are in `.github/workflows/ci.yml`
(integration job env block).

## Scope

**In scope** (the only files you should create or modify):

- `cmd/gateway/e2e_integration_test.go` (create)

**Out of scope** (do NOT touch, even though they look related):

- Any production code. If the test reveals a bug, STOP and report; the fix is
  a separate change.
- Creating a top-level `test/` directory. PLAN.md sketched one, but the
  gateway boot entrypoint (`run`) is unexported in package `main`, so the e2e
  test lives next to it. Note this deviation in the PR description.
- CI workflow changes: the integration job already runs
  `go test -tags=integration ./...`, which picks this up automatically.

## Git workflow

- Branch: `advisor/003-e2e-integration-test`
- Commit style: imperative, under 72 chars, e.g.
  `test: add full-stack e2e integration test for the proxy path`
- All work lands via PR into `main`.

## Steps

### Step 1: Scaffold the tag-gated test file

Create `cmd/gateway/e2e_integration_test.go` with `//go:build integration`,
package `main`. At the top of the test, read `MYSQL_DSN`, `CLICKHOUSE_DSN`,
and `REDIS_ADDR` from the environment and `t.Skip` if any is empty, matching
the pattern in `internal/cache/cache_integration_test.go`.

**Verify**: `go vet -tags=integration ./cmd/gateway/` exits 0 and `make test`
still exits 0 (file excluded without the tag).

### Step 2: Boot the stack

In the test:

1. Run migrations: `mysql.Migrate(mysqlDSN)` and the ClickHouse equivalent
   from `internal/store/clickhouse/migrate.go` (check its exact signature
   before use).
2. Seed config state: open the MySQL store, `CreateTeam(ctx, "e2e-team")`,
   generate a plaintext key (any random hex string), compute
   `sha256.Sum256` hex, `CreateKey(ctx, team.ID, "e2e-key", hash, "default")`.
   Use a unique team name per run (append a timestamp) so reruns do not
   collide.
3. Start a mock upstream: `httptest.NewServer` returning status 200,
   `Content-Type: text/event-stream`, and the SSE fixture body above.
4. `t.Setenv("E2E_MOCK_KEY", "test-key")` (the provider requires the env var
   to exist; the mock ignores it).
5. Write the temp YAML config from "Current state" with a free port (copy the
   `freePort` helper usage from `main_test.go`; note that helper already
   exists in the package, do not redefine it).
6. Start `run(cfgPath)` in a goroutine and wait for `/readyz`.

**Verify**: run the test locally against the compose stack; it should reach
readiness (fail the test with a clear message if not).

### Step 3: Exercise and assert the full path

1. POST `/v1/messages` with header `x-api-key: <plaintext key>` and body
   `{"model":"default","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"e2e"}]}`.
   Assert: status 200, body contains `message_stop`, response header
   `x-llm-cache` is `miss`.
2. Repeat the identical request. Assert `x-llm-cache` is `hit`.
3. Send a request with a wrong key. Assert 401.
4. SIGTERM the process (as in `TestRun_StartAndGracefulShutdown`) and wait for
   `run` to return, which flushes the attribution writer.
5. Open ClickHouse (`clickhouse.Open(dsn)` from `internal/store/clickhouse/`)
   and query `request_logs` for rows with this run's key ID. Assert at least
   two rows exist (miss and hit) and the miss row has nonzero input and output
   tokens (10 and 25 from the fixture).

**Verify**: `go test -tags=integration -race ./cmd/gateway/ -run TestE2E`
passes against the local compose stack.

### Step 4: Full gates

**Verify**: `make build && make test && make lint` exit 0. Push the branch and
confirm the CI integration job passes with the new test included.

## Test plan

This plan IS the test. Cases covered: auth success, auth failure, cache miss
then hit, streaming relay, usage capture, attribution row in ClickHouse.
Structural patterns: `TestRun_StartAndGracefulShutdown` (boot/shutdown),
`internal/cache/cache_integration_test.go` (tag + skip convention).

## Done criteria

- [ ] `cmd/gateway/e2e_integration_test.go` exists, tag-gated, and skips
      cleanly when the DSN env vars are absent (`go test ./cmd/gateway/` in a
      bare shell shows no failure)
- [ ] `go test -tags=integration -race ./cmd/gateway/ -run TestE2E` passes
      against the compose stack
- [ ] CI integration job green on the PR
- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] No production code modified (`git status` shows only the new test file)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The ClickHouse migrate helper's signature differs materially from the MySQL
  one and needs plumbing changes to call from a test.
- The attribution row does not appear after graceful shutdown (this means
  flush-on-close does not hold; that is a product bug to report, not to fix
  here).
- Any assertion requires changing production code to pass.
- The test is flaky on CI (two consecutive runs disagree): report the flake
  rather than adding retries.

## Maintenance notes

- This test is the precondition for the deferred `internal/proxy/proxy.go`
  decomposition (see plans/README.md, rejected and deferred section). Keep it
  green before attempting that refactor.
- When a second provider shape is added to the e2e config later, assert the
  cross-shape translation path too (route an Anthropic-shaped request to an
  OpenAI-shaped mock).
- Reviewer should check the test cleans up after itself well enough for
  reruns (unique team name per run) since CI databases persist for the job.
