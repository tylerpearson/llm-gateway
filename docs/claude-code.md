# Connecting Claude Code

Claude Code talks to the Anthropic Messages API. Because the gateway exposes the
same `POST /v1/messages` shape, you point Claude Code at the gateway instead of
`api.anthropic.com` and it works unmodified. Every call then flows through the
gateway, so routing, caching, budgets, and spend attribution apply to your
Claude Code usage.

The gateway holds the real Anthropic key. Claude Code authenticates with a
virtual `llmgw_...` key, which the gateway accepts on the `x-api-key` header
(the same header Claude Code already sends).

## Prerequisites

- A running gateway (see [local development](local-development.md) or
  [Kubernetes deployment](kubernetes.md)).
- A virtual key. Create one with `gatewayctl`:

  ```bash
  gatewayctl team create acme
  gatewayctl key create --team <team-id> --name my-laptop
  ```

  Copy the `llmgw_...` key it prints; it is shown once.

## Option A: environment variables (quick, per shell)

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080     # your gateway URL
export ANTHROPIC_API_KEY=llmgw_...                  # your virtual key

claude
```

Use the base URL without a path; Claude Code appends `/v1/messages` itself. For
a remote gateway, use its ingress URL, for example
`https://llm-gateway.example.com`.

## Option B: settings.json (persistent, recommended)

Set the same values in your Claude Code settings so every session uses the
gateway. In `~/.claude/settings.json` (user scope) or `.claude/settings.json`
(project scope):

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8080",
    "ANTHROPIC_API_KEY": "llmgw_..."
  }
}
```

Project-scoped settings are handy for pointing a specific repo's Claude Code
sessions at a gateway (and thus a specific team key and budget) without changing
your global default.

## Verify traffic is flowing through the gateway

Start a session and send a prompt, then confirm the gateway saw it:

- Watch the gateway logs. Each proxied request logs a `proxy request` line with
  the request id, served model, token counts, and status.
- If analytics are enabled, the request appears in the ClickHouse `request_logs`
  table and on the Grafana Spend Overview dashboard within a few seconds.
- A direct probe with the same key should return a normal response and carry the
  gateway's `x-llm-*` headers:

  ```bash
  curl -sND - http://localhost:8080/v1/messages \
    -H 'content-type: application/json' \
    -H "x-api-key: $ANTHROPIC_API_KEY" \
    -d '{"model":"default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}' \
    | grep -i '^x-llm-'
  ```

## Choosing a routing tier

The gateway maps virtual model aliases (`default`, `fast`, `frontier`) to
concrete provider models. Claude Code sends whatever model string you configure,
but the cleanest way to control routing for Claude Code is the key's default
alias, applied on every request with no client changes:

```bash
# Route this key's traffic to the frontier tier (for example claude-opus-4-8).
gatewayctl key create --team <team-id> --name power-user --alias frontier
```

Clients that can send custom headers may override per request with
`x-llm-tier: frontier`, but Claude Code does not expose per-request header
control, so prefer per-key default aliases for it.

## How Claude Code spend is attributed

This is the point of routing Claude Code through the gateway. Each request log
records:

- The virtual key and team, so you can see spend per developer or per team.
- The `user_agent`, captured automatically. Claude Code identifies itself in its
  User-Agent, so its spend is separable from other tools (a CLI, a script, a
  web app) sharing the same gateway. The Spend Overview dashboard has a Top
  Spending Client Tools panel keyed on this.
- The computed cost, surfaced on the response as `x-llm-cost-usd` (an HTTP
  trailer on live responses) and written to `request_logs` for aggregation.

To attribute further, add spend tags or an end-user id. Claude Code cannot set
these per request, but scripts and other clients pointed at the same gateway
can, using the `x-llm-tags` and `x-llm-end-user` headers or a configured
`attribution.tag_headers` allowlist. See
[spend attribution dimensions](../README.md#spend-attribution-dimensions).

## Budgets and limits

If the key's team has a monthly budget or rate limit configured under `limits`,
Claude Code sessions are subject to it. In soft mode a breach is allowed but
flagged on the `x-llm-limit` response header; in hard mode the request is
rejected with HTTP 429. Set budgets per key or per team in the gateway config.

## Reverting to the direct API

Unset the overrides (or remove them from `settings.json`) to send Claude Code
straight to Anthropic again:

```bash
unset ANTHROPIC_BASE_URL
unset ANTHROPIC_API_KEY   # or reset to your real Anthropic key
```
