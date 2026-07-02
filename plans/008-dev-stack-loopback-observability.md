# Plan 008: Bind dev-stack Prometheus and Grafana to loopback

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md` unless the reviewer said they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 27f9036..HEAD -- deploy/ docs/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P3
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `27f9036`, 2026-07-01

## Why this matters

PR #33 deliberately bound the dev stack's MySQL, ClickHouse, and Redis ports
to 127.0.0.1 so the weakly-authenticated dev datastores are not reachable
from the LAN. Prometheus (no auth at all) and Grafana (default admin
credentials) were left publishing on all interfaces, which is the same
exposure class the datastore change closed: anyone on the local network can
read metrics or log into Grafana, whose provisioned ClickHouse datasource
reads the request log. Binding both to loopback finishes the job. The
gateway's own port (8080) stays on all interfaces on purpose: it is the
authenticated service under test, and pointing another device at it is a
legitimate dev workflow.

## Current state

- `deploy/docker-compose.yml` port bindings today:
  - mysql: `"127.0.0.1:3306:3306"` (line ~46)
  - clickhouse: `"127.0.0.1:9000:9000"`, `"127.0.0.1:8123:8123"` (lines ~73-74)
  - redis: `"127.0.0.1:6379:6379"` (line ~106)
  - gateway: `"8080:8080"` (line ~127) - stays as is
  - prometheus: `"9090:9090"` (line ~170) - change
  - grafana: `"3000:3000"` (line ~193) - change
- The loopback bindings added by PR #33 carry short comments explaining the
  rationale; match that style.
- Docs reference the UIs as `http://localhost:3000` and `localhost:9090`
  (check `docs/local-development.md` and README), which keep working with
  loopback binding; verify rather than assume.
- Conventions: no em dashes, comments are complete sentences.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Compose file validates | `docker compose -f deploy/docker-compose.yml config >/dev/null` | exit 0 |
| Live check (optional) | `make up`, then `curl -sf http://127.0.0.1:9090/-/ready` and `curl -sf http://127.0.0.1:3000/api/health` | both succeed |
| Em dash check | `grep -rPn "\xe2\x80\x94" deploy/ docs/ README.md` | no output |
| Full gates | `make build && make test && make lint` | exit 0 (no Go changes expected, run anyway) |

## Scope

**In scope** (the only files you should modify):

- `deploy/docker-compose.yml` (the prometheus and grafana `ports` entries and
  a one-line comment each)
- `docs/local-development.md` or README.md ONLY if you find a statement that
  becomes wrong (verify first; localhost URLs remain correct)

**Out of scope** (do NOT touch, even though they look related):

- The gateway service's `8080:8080` binding (intentional, see above).
- Grafana admin credentials (`admin`/`admin` is Grafana's own dev default;
  with the port on loopback the exposure is gone).
- The Helm chart: production networking is the chart's concern and it does
  not publish these ports.
- Prometheus scrape config and Grafana provisioning files.

## Git workflow

- Branch: `advisor/008-dev-loopback-observability`
- Commit style: imperative, under 72 chars, e.g.
  `deploy: bind dev Prometheus and Grafana to loopback`
- All work lands via PR into `main`.

## Steps

### Step 1: Change the two bindings

In `deploy/docker-compose.yml`, change prometheus's ports entry to
`"127.0.0.1:9090:9090"` and grafana's to `"127.0.0.1:3000:3000"`, each with a
short comment matching the datastore entries' style (published on loopback
only so the unauthenticated/default-credential dev UI is not reachable from
the LAN).

**Verify**: `docker compose -f deploy/docker-compose.yml config >/dev/null`
exits 0.

### Step 2: Check the docs stay truthful

`grep -rn "9090\|:3000" docs/ README.md` and read each hit. `localhost` and
`127.0.0.1` URLs remain correct; only fix a doc if it explicitly claims LAN
access to these UIs (none is known to).

**Verify**: em dash grep prints nothing; any doc edit is limited to a wrong
statement.

### Step 3: Optional live check and full gates

If Docker is available: `make up`, run the two curl checks, `make down`.

**Verify**: `make build && make test && make lint` exit 0.

## Test plan

- No Go tests apply. `docker compose config` is the machine check; the curl
  checks are the live proof when Docker is available.

## Done criteria

- [ ] Both bindings are loopback-prefixed with rationale comments
- [ ] `docker compose -f deploy/docker-compose.yml config` exits 0
- [ ] No unintended doc changes (`git status` shows only in-scope files)
- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The compose file's port entries do not match the "Current state" listing
  (drift).
- You find documentation that instructs users to access Grafana or
  Prometheus from another machine (that would make this a breaking change
  needing a maintainer decision).

## Maintenance notes

- If someone later needs LAN access to Grafana for a demo, the change is a
  one-line local override (`docker compose -f ... -f override.yml`), not a
  reason to revert this.
- Reviewer: confirm the gateway port stayed `8080:8080`.
