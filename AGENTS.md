# AGENTS.md: how AI agents should run work in this repo

Conventions for AI coding agents (Claude Code and any other agent runner) when
doing multi-step or batched work in llm-gateway. This file complements CLAUDE.md:
read CLAUDE.md first for the build, test, lint, and phased-build conventions.
This file covers how to parallelize and land larger changes.

## Parallelize with git worktrees

- When a task splits into two or more independent changes (for example, fixing a
  batch of code-review findings), dispatch them as parallel subagents, each in
  its own git worktree. Do not run two agents against the same working tree.
- Group the changes so each worktree touches a DISJOINT set of files. Bundle
  changes that share a file into one worktree (and one PR). Disjoint files are
  what let the resulting PRs merge into main without conflicts.
- One concern per PR. Keep each diff focused and reviewable.

## Use the cheapest capable model

- Prefer the cheapest model that can do the job correctly. Use Sonnet for routine
  code changes, Haiku for documentation-only or mechanical text edits, and
  reserve Opus for genuinely tricky design or cross-cutting work.
- Do not reach for a more expensive model than the task needs.

## PR and merge flow

- Never push directly to main. Every change lands on its own branch through a PR
  into main (see CLAUDE.md).
- All four gates must be green before a merge: `make build`, `make test` (runs
  `-race`), `make lint`, `make vuln`. Wait for CI to pass on the PR before
  merging.
- Merge disjoint PRs one at a time. After merging, pull main and run the full
  gate once more to confirm the combined result is still green.
- Commit subjects: imperative mood, under 72 chars, with a phase prefix where it
  applies. End commit messages with the Co-Authored-By trailer. End PR bodies
  with the Claude Code generated-with line.

## Test thoroughly

- Every change ships with thorough tests. New behavior gets table-driven tests
  using `httptest` for HTTP layers and a mock provider for proxy logic (see
  CLAUDE.md). A change is not done until its tests are.
- Cover the edges, not just the happy path: error returns, context cancellation,
  upstream non-2xx status, empty and malformed input, and boundary values.
- Keep the command binaries (`cmd/gateway`, `cmd/gatewayctl`) under test. Pure
  wiring helpers and argument-validation paths are unit-testable without a
  database; exercise them rather than leaving the binaries at zero coverage.
- Logic that needs MySQL, ClickHouse, or Redis goes behind the `integration`
  build tag and must skip cleanly when its DSN is unset. Do not let an
  integration dependency leave a code path with no test at all: cover the
  in-process logic with a unit test and the backend round trip with an
  integration test.
- Before opening a PR, confirm coverage did not regress for the packages you
  touched (`go test ./... -cover`).

## Writing rules (same as CLAUDE.md, repeated here for agents)

- No em dashes (U+2014) anywhere in code, comments, commits, PRs, or docs. Verify
  with `grep -Pn "\xe2\x80\x94" <file>`.
- No trailing whitespace, no BOM. Comments are complete sentences.
