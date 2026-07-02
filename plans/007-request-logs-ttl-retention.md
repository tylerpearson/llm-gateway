# Plan 007: Add a TTL retention policy to the ClickHouse request_logs table

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md` unless the reviewer said they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 27f9036..HEAD -- migrations/ internal/store/clickhouse/ README.md`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: MED (schema change on the production analytics table)
- **Depends on**: none
- **Category**: migration
- **Planned at**: commit `27f9036`, 2026-07-01

## Why this matters

`request_logs` receives one row per proxied request and has no TTL, so it
grows without bound: on a busy gateway that is disk exhaustion on a schedule.
A FinOps tool should have an explicit, documented retention policy rather
than an implicit "forever". ClickHouse TTL handles this natively at the
storage layer; a 90 day default keeps a quarter of per-request detail, which
comfortably covers the monthly budget windows the gateway enforces.

## Current state

- `migrations/clickhouse/0001_request_logs.up.sql` creates the table with
  `ENGINE = MergeTree() PARTITION BY toYYYYMM(ts) ORDER BY (ts, team_id)` and
  no TTL clause. The `ts` column is `DateTime64(3)`.
- Existing ClickHouse migrations come in up/down pairs numbered 0001 to 0004
  (`0004_request_attribution_dims` is the latest).
- **How ClickHouse migrations are applied (important, read this)**:
  `internal/store/clickhouse/migrate.go` does NOT use golang-migrate. It
  reads every embedded `clickhouse/*.up.sql` file in filename order, strips
  one trailing semicolon, and executes each file as a SINGLE statement, on
  every `Migrate` call. Consequences for any new migration:
  - exactly one SQL statement per file (no multi-statement files);
  - the statement must be safe to re-execute on every migrate run
    (re-applying the same `ALTER TABLE ... MODIFY TTL` is safe and
    effectively a no-op);
  - down files exist as convention/documentation but are not executed by
    this tool.
- Files are embedded via `migrations/embed.go`
  (`//go:embed clickhouse/*.sql`), so new files are picked up automatically
  with no code change.
- The integration test convention: `go test -tags=integration ./...` with
  `CLICKHOUSE_DSN` set applies migrations in CI
  (`internal/store/clickhouse/clickhouse_integration_test.go` and the e2e
  test in `cmd/gateway/e2e_integration_test.go` both call `Migrate`).
- Conventions: no em dashes, SQL keywords uppercase as in the existing
  migration files.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build   | `make build` | exit 0 |
| Unit tests | `make test` | exit 0 (migrations are exercised only under the integration tag) |
| Integration (needs stack) | `make up` then `go test -tags=integration -race ./internal/store/clickhouse/ ./cmd/gateway/` | pass |
| Manual TTL check | `docker exec -i deploy-clickhouse-1 clickhouse-client -q "SHOW CREATE TABLE request_logs"` (container name may differ; `docker ps` to confirm) | output contains `TTL toDateTime(ts) + toIntervalDay(90)` |
| Lint | `make lint` | exit 0 |

## Scope

**In scope** (the only files you should create or modify):

- `migrations/clickhouse/0005_request_logs_ttl.up.sql` (create)
- `migrations/clickhouse/0005_request_logs_ttl.down.sql` (create)
- `README.md` (one sentence documenting the retention default)

**Out of scope** (do NOT touch, even though they look related):

- `internal/store/clickhouse/migrate.go`: the embed glob picks the files up.
- The `eval_runs` / `eval_results` tables: eval data retention is a v2
  decision (see PR #24); do not add TTLs there.
- The MySQL `audit_log` table: audit trails usually must NOT expire
  automatically; leave it alone.
- Making the window configurable: a fixed, documented 90 days is the v1
  policy; configurability is a later decision.

## Git workflow

- Branch: `advisor/007-request-logs-ttl`
- Commit style: imperative, under 72 chars, e.g.
  `migrations: retain request_logs for 90 days via ClickHouse TTL`
- All work lands via PR into `main`.

## Steps

### Step 1: Write the migration pair

`migrations/clickhouse/0005_request_logs_ttl.up.sql` (single statement, no
trailing content after the semicolon):

```sql
ALTER TABLE request_logs MODIFY TTL toDateTime(ts) + INTERVAL 90 DAY;
```

`migrations/clickhouse/0005_request_logs_ttl.down.sql`:

```sql
ALTER TABLE request_logs REMOVE TTL;
```

**Verify**: `make build` exits 0 (embed compiles the new files in).

### Step 2: Prove it applies against real ClickHouse

Bring up the dev stack (`make up`, wait for healthy) or use CI. Run:

```
go test -tags=integration -race ./internal/store/clickhouse/ ./cmd/gateway/
```

Both must pass (they call `Migrate`, which now also executes 0005). Then run
the manual TTL check from the commands table and confirm the TTL clause
appears in `SHOW CREATE TABLE request_logs`. Run `Migrate` twice (rerun the
test) to confirm re-application is clean. Tear down with `make down` if you
started the stack.

If Docker is unavailable, note it and rely on the CI integration job for
this verification, but say so explicitly in your report.

### Step 3: Document the policy

Add one sentence to README.md in the section that describes ClickHouse
request logging (search for `request_logs`): request rows are retained for
90 days by ClickHouse TTL (migration 0005); adjust the interval by adding a
later migration.

**Verify**: `grep -n "90 day" README.md` (case-insensitive) matches, and
`grep -rPn "\xe2\x80\x94" README.md migrations/` prints nothing.

### Step 4: Full gates

**Verify**: `make build && make test && make lint` all exit 0.

## Test plan

- The integration suite is the test: `Migrate` executes every up file, so CI
  proves the statement is valid and idempotent under re-runs. No new Go test
  is needed; do not write one that hardcodes SHOW CREATE output.

## Done criteria

- [ ] Both 0005 files exist, one statement each
- [ ] Integration tests pass with the new migration applied (locally or CI)
- [ ] `SHOW CREATE TABLE request_logs` shows the 90 day TTL (locally or a CI
      assertion via the existing tests passing twice)
- [ ] README documents the retention default
- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `migrate.go` no longer applies every up file on each run (the idempotency
  reasoning above would be stale).
- The ALTER fails against the CI/dev ClickHouse version.
- Anything suggests production data already exists whose sudden expiry would
  surprise an operator: this repo has no production deployment as far as the
  plans know, but if you find evidence otherwise, stop.

## Maintenance notes

- **Numbering interaction**: the deferred v2 plan (see PR #24 and the saved
  P10/P11 implementation plan) assumed the next ClickHouse migration number
  0005 for eval work; after this plan lands, v2's eval migration becomes
  0006. The reviewer will update the v2 plan pointer.
- When ClickHouse TTL deletes old partitions, monthly partition drops are
  cheap (PARTITION BY toYYYYMM); no operational action needed.
- If aggregate spend reporting beyond 90 days is ever needed, add a
  materialized rollup table before shortening this window further.
