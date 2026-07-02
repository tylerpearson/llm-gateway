# Plan 006: Surface audit log write failures in gatewayctl

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md` unless the reviewer said they maintain the index.
>
> **Drift check (run first)**: `git diff --stat 27f9036..HEAD -- cmd/gatewayctl/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `27f9036`, 2026-07-01

## Why this matters

Every administrative action in gatewayctl (team create, key create, key
disable) writes an audit entry, but all three call sites discard the error
with `_ =`. If the audit insert fails (MySQL hiccup, permissions, schema
drift), the action still completes and prints success, and nobody learns the
audit trail has a hole. An audit log that can fail silently defeats its
purpose. The action should still succeed (fail-open), but the operator must
see a warning on stderr when the audit write fails.

## Current state

- `cmd/gatewayctl/main.go` is the whole CLI. The three discard sites:

`cmd/gatewayctl/main.go:150` (team create):

```go
_ = s.RecordAudit(c, actor(), "team.create", t.ID, "name="+t.Name)
```

`cmd/gatewayctl/main.go:208` (key create):

```go
_ = s.RecordAudit(c, actor(), "key.create", vk.ID, "team="+vk.TeamID+" name="+vk.Name)
```

`cmd/gatewayctl/main.go:264` (key disable):

```go
_ = s.RecordAudit(c, actor(), "key.disable", *id, "")
```

- `RecordAudit` is defined on the MySQL store
  (`internal/store/mysql/mysql.go:162`) and returns an error. The `store.Store`
  interface in `internal/store/store.go` also declares it.
- Existing tests: `cmd/gatewayctl/main_test.go` contains small, direct unit
  tests of helpers (`TestDSNFromEnv`, `TestActor_NeverEmpty`,
  `TestCommandValidation`, `TestDSNGuardBeforeStore`). Match that style.
- Conventions: comments are complete sentences, no em dashes anywhere
  (verify with `grep -rPn "\xe2\x80\x94" cmd/gatewayctl/`), errors to stderr
  via `fmt.Fprintf(os.Stderr, ...)`.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build   | `make build` | exit 0 |
| Focused tests | `go test ./cmd/gatewayctl/ -race` | ok, all pass |
| Full tests | `make test` | exit 0 |
| Lint    | `make lint` | exit 0 |
| Em dash check | `grep -rPn "\xe2\x80\x94" cmd/gatewayctl/` | no output |

## Scope

**In scope** (the only files you should modify):

- `cmd/gatewayctl/main.go`
- `cmd/gatewayctl/main_test.go`

**Out of scope** (do NOT touch, even though they look related):

- `internal/store/` (RecordAudit itself is fine).
- Exit codes: the command still exits 0 when the action succeeded but the
  audit write failed. Scripts depend on exit status reflecting the action.
- The gateway server's audit paths (none exist server-side today).

## Git workflow

- Branch: `advisor/006-gatewayctl-audit-warning`
- Commit style: imperative, under 72 chars, e.g.
  `gatewayctl: warn on stderr when an audit log write fails`
- All work lands via PR into `main`.

## Steps

### Step 1: Add a small helper and use it at all three sites

In `cmd/gatewayctl/main.go`, add:

```go
// auditRecorder is the subset of the store needed to write audit entries,
// declared locally so the warning path is unit-testable with a fake.
type auditRecorder interface {
	RecordAudit(ctx context.Context, actor, action, target, details string) error
}

// recordAudit writes an audit entry and warns on stderr when the write
// fails. The admin action itself has already succeeded at this point, so
// the failure is surfaced but does not change the exit status.
func recordAudit(c context.Context, s auditRecorder, action, target, details string) {
	if err := s.RecordAudit(c, actor(), action, target, details); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s succeeded but audit log write failed: %v\n", action, err)
	}
}
```

Replace the three `_ = s.RecordAudit(...)` calls with
`recordAudit(c, s, "team.create", t.ID, "name="+t.Name)` and the equivalents
for `key.create` and `key.disable`.

**Verify**: `make build` exits 0 and
`grep -n "_ = s.RecordAudit" cmd/gatewayctl/main.go` prints nothing.

### Step 2: Test the warning path

In `cmd/gatewayctl/main_test.go`, add a test with a fake `auditRecorder`
whose `RecordAudit` returns an error. Capture stderr (swap `os.Stderr` via a
pipe, or refactor `recordAudit` to take an `io.Writer` if that is cleaner;
prefer the `io.Writer` parameter, with the call sites passing `os.Stderr`).
Assert:

1. A failing recorder produces one warning line containing the action name
   and the underlying error text.
2. A succeeding recorder produces no output.

**Verify**: `go test ./cmd/gatewayctl/ -race` passes, including the new test.

### Step 3: Full gates

**Verify**: `make build && make test && make lint` all exit 0; em dash grep
prints nothing.

## Test plan

- New table test for `recordAudit` covering the failing and succeeding
  recorder cases (Step 2). Model after the existing direct helper tests in
  `cmd/gatewayctl/main_test.go`.

## Done criteria

- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] No `_ = s.RecordAudit` remains in `cmd/gatewayctl/main.go`
- [ ] A test proves the stderr warning fires on audit write failure
- [ ] Command exit codes unchanged (action failure still nonzero, audit-only
      failure still zero)
- [ ] `git status` shows changes only to the two in-scope files
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The three call sites do not match the excerpts (drift).
- Making the warning testable seems to require changing the `store.Store`
  interface or any file under `internal/`.
- Any existing gatewayctl test fails.

## Maintenance notes

- If a server-side admin API is ever added, it should adopt the same
  fail-open-with-visibility policy, or a stricter one, deliberately.
- Reviewer should check the warning goes to stderr, not stdout: stdout
  carries the key material on `key create` and scripts parse it.
