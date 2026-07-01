# Plan 004: Pipeline the rate limiter's Redis round trips

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat 1825c44..HEAD -- internal/ratelimit/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: perf
- **Planned at**: commit `1825c44`, 2026-07-01

## Why this matters

`Limiter.Check` runs before every proxied request and issues up to four
sequential Redis commands per scope (INCR, EXPIRE, GET, GET), so up to eight
sequential round trips when both key and team limits are configured.
`RecordUsage` adds up to eight more after the response. With a networked
Redis at ~1 ms RTT this puts several milliseconds of avoidable, serialized
latency on the request path and multiplies Redis load. Batching each phase
into one pipelined round trip cuts Check and RecordUsage to one network hop
each with no semantic change.

## Current state

- `internal/ratelimit/ratelimit.go` is the whole limiter. `Check`
  (lines 106-117) calls `checkScope` once per scope; `checkScope`
  (lines 119-150) issues the sequential commands:

```go
if lim.RequestsPerMin > 0 {
    key := fmt.Sprintf("rl:req:%s:%s:%s", scope, id, minute)
    n, err := l.rdb.Incr(ctx, key).Result()
    if err == nil {
        _ = l.rdb.Expire(ctx, key, minuteWindow).Err()
        if n > lim.RequestsPerMin {
            exceeded = append(exceeded, scope+":requests_per_min")
        }
    } else {
        l.log.Warn("ratelimit incr failed", slog.Any("error", err))
    }
}
if lim.TokensPerMin > 0 {
    key := fmt.Sprintf("rl:tok:%s:%s:%s", scope, id, minute)
    if v, err := l.rdb.Get(ctx, key).Int64(); err == nil && v >= lim.TokensPerMin {
        exceeded = append(exceeded, scope+":tokens_per_min")
    }
}
if lim.MonthlyUSD > 0 {
    key := fmt.Sprintf("rl:usd:%s:%s:%s", scope, id, month)
    if v, err := l.rdb.Get(ctx, key).Float64(); err == nil && v >= lim.MonthlyUSD {
        exceeded = append(exceeded, scope+":monthly_usd")
    }
}
```

`RecordUsage` (lines 154-160) calls `recordScope` per scope, which issues
IncrBy + Expire and IncrByFloat + Expire sequentially (lines 162-178).

- Error handling convention to preserve: a failed Redis command is logged (or
  silently skipped for the GETs, where a missing key or an error both mean
  "not exceeded") and never blocks the request. Redis being down must degrade
  to allowing traffic, exactly as today.
- The package comment documents that limits are checked before usage is
  recorded, lagging by one request. That behavior stays.
- Client: `github.com/redis/go-redis/v9`. Use `l.rdb.Pipelined(ctx, fn)`,
  which returns the queued `*redis.Cmd` values for inspection after the round
  trip. `redis.Nil` errors from GET mean the key is absent, treat as zero.
- Tests: `internal/ratelimit/ratelimit_test.go` runs against miniredis, which
  supports pipelining. Existing tests must pass unchanged.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build   | `make build` | exit 0 |
| Focused tests | `go test ./internal/ratelimit/ -race` | ok, all pass |
| Full tests | `make test` | exit 0 |
| Lint    | `make lint` | exit 0 |
| Em dash check | `grep -rPn "\xe2\x80\x94" internal/ratelimit/` | no output |

## Scope

**In scope** (the only files you should modify):

- `internal/ratelimit/ratelimit.go`
- `internal/ratelimit/ratelimit_test.go` (add tests only if needed; existing
  tests should pass unchanged)

**Out of scope** (do NOT touch, even though they look related):

- The check-then-record lag: documented design, not changed by this plan.
  Do NOT convert the logic to a Lua script; pipelining is enough and keeps
  the code readable and miniredis-compatible.
- Callers in `internal/proxy/`: the `Check`/`RecordUsage` signatures must not
  change.
- Key naming (`rl:req:...`, `rl:tok:...`, `rl:usd:...`) and window constants:
  unchanged, or existing counters would be orphaned on deploy.

## Git workflow

- Branch: `advisor/004-ratelimit-pipeline`
- Commit style: imperative, under 72 chars, e.g.
  `ratelimit: batch Redis commands into one pipeline per phase`
- All work lands via PR into `main`.

## Steps

### Step 1: Pipeline Check

Rewrite `Check` to queue all commands for both scopes in a single
`l.rdb.Pipelined` call: for each configured limit, queue INCR + EXPIRE
(requests) and GET (tokens, USD) with the same keys as today. After the
pipeline returns, evaluate the results with the same comparisons and build
`exceeded` in the same order (key scope entries before team scope entries,
requests before tokens before USD within a scope) so tests asserting the
joined header value keep passing. Preserve the degrade-open behavior: a
pipeline transport error logs one warning and returns no exceedances; a
`redis.Nil` GET result means zero usage.

Note one intentional, invisible difference: today EXPIRE runs only after a
successful INCR; in a pipeline both are always queued. Because the key is
freshly INCRed in the same pipeline this is equivalent in effect.

**Verify**: `go test ./internal/ratelimit/ -race` passes unchanged.

### Step 2: Pipeline RecordUsage

Same treatment for `RecordUsage`: one `Pipelined` call queuing IncrBy +
Expire and IncrByFloat + Expire for both scopes, skipping unconfigured or
zero-value updates exactly as `recordScope` does now. `checkScope` and
`recordScope` can be removed or reduced to helpers that append to the
pipeline; keep whichever shape reads cleaner, but do not leave dead code.

**Verify**: `go test ./internal/ratelimit/ -race` passes unchanged.

### Step 3: Full gates

**Verify**: `make build && make test && make lint` exit 0 and the em dash
grep prints nothing.

## Test plan

- Existing table-driven tests in `ratelimit_test.go` are the primary safety
  net and must pass without modification (their assertions define the
  semantics being preserved).
- Add one new test only if coverage is missing for: both scopes configured at
  once, exceeding different limits, asserting the exact order of the
  `Exceeded` slice.

## Done criteria

- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] `Check` and `RecordUsage` each issue exactly one Redis round trip
      (code inspection: one `Pipelined` call per method, no `l.rdb.<Cmd>`
      calls outside pipelines in these paths)
- [ ] Existing tests pass without modification
- [ ] `git status` shows changes only to in-scope files
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The code does not match the excerpts (drift).
- Any existing test fails and the fix would require changing the test's
  assertions (that means a semantic change slipped in).
- miniredis misbehaves under pipelining (unlikely; report rather than
  swapping test infrastructure).

## Maintenance notes

- If hard-mode atomicity (no overshoot under concurrency) is ever required,
  the next step is a Lua script that checks and reserves in one atomic unit;
  this pipeline refactor neither helps nor hurts that migration.
- Reviewer should diff the exceedance ordering and the degrade-open error
  paths carefully; they are the only observable behaviors.
