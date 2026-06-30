# Design: automatic per-request model routing

Status: draft for review. No code has been written against this document yet.
This is a companion to `docs/v2-shadow-eval-design.md` and depends on it.

This document specifies automatic per-request routing: deciding, on each
request, whether a cheap default model can handle it or whether it should
escalate to a frontier model, without the client choosing. It also evaluates
honestly whether the feature is worth building, because the answer is "it
depends" and the dependencies are worth being explicit about.

## 1. Two different "automatic" decisions

Do not conflate these. They are separate features with separate risk profiles.

- **Automatic swap of the default** (offline, all traffic, slow): "is the cheap
  model good enough to be everyone's default?" This is the gated-swap built on
  shadow eval. One config value changes for everyone, gradually, with a human or
  a gate in the loop.
- **Automatic per-request routing** (online, this request, real time): "is this
  specific request hard enough to need frontier?" A decision on every request.

This doc is the second one. The Coinbase and Carson posts ("cheap model as
default, frontier only for harder tasks") are describing exactly this: the value
is in the word *automatically* deciding which requests are the "harder tasks."

## 2. Relationship to shadow eval

These compose, and the dependency runs one direction:

```
shadow eval  -->  per-request labels: did the cheap model match frontier?
                        |
                        v
              train / calibrate a router
                        |
                        v
   online: new request -> router predicts cheap vs frontier
                        |
                        v
        shadow eval again -> monitor the router for drift
```

The eval pipeline is both the **training signal** for a good router and the
**ongoing monitor** that catches the router degrading. A router without eval data
is either hand-tuned heuristics or a generic off-the-shelf model not calibrated
to your traffic. This is the core reason to build eval first even if routing is
the real goal.

## 3. Goals and non-goals

### Goals

- Add an opt-in `auto` routing mode that picks a tier per request from a small
  set of configured tiers (for example `cheap`, `frontier`).
- Make the routing strategy pluggable behind one interface, so heuristic,
  model-based, and cascade strategies are swappable without touching the proxy.
- Fail open: any classifier error, timeout, or missing data falls back to a
  configured default tier. A routing failure must never fail a request.
- Keep the live response unaffected in shape and streaming behavior; routing only
  changes which upstream serves it.

### Non-goals (this increment)

- No change to the shadow-eval design. This consumes its output; it does not
  modify it.
- No automatic *training* pipeline in the first cut. The model-based classifier,
  if built, is trained offline from exported eval data, not online.
- No per-token or mid-stream rerouting. The decision is made once, before the
  upstream call.

## 4. The decision being automated

Given an inbound request and a configured tier set ordered cheap to frontier,
choose one tier. The chosen tier resolves through the existing router to a
concrete provider and model. Everything downstream (translation, streaming,
caching, attribution, eval) is unchanged.

The decision has an asymmetric cost structure that drives the whole design:

- Routing a hard task to the cheap model produces a bad answer. This is a quality
  incident, it is hard to detect automatically, and it erodes trust.
- Routing an easy task to the frontier model wastes money. This is visible,
  benign, and self-correcting.

So under uncertainty the router should bias toward frontier. The tunable is how
much money you are willing to waste to avoid a quality miss.

## 5. Approaches

### 5.1 Heuristic classifier (free, crude)

Route on signals computable without a model: estimated token count, message
count, presence of code fences, presence of certain keywords, requested output
length, tool-call presence. Long, code-heavy, or multi-turn goes frontier; short
and simple goes cheap.

- Pro: zero added latency or spend, deterministic, trivially testable, no new
  failure dependency, no data egress.
- Con: length and surface features are weak proxies for difficulty. Short-hard
  ("prove this reduction") and long-easy ("summarize this log") are mis-routed.
  Captures some of the savings, not the precise frontier.

### 5.2 Model / classifier routing (the calibrated answer)

A small fast model, a fine-tuned classifier, or an embedding plus logistic model
predicts whether the cheap model will match the frontier for this request. Train
it on shadow-eval labels (section 2). This is what RouteLLM (open source),
NotDiamond, Martian, Unify, and OpenRouter "auto" do.

- Pro: calibrated to your actual traffic; meaningfully better accuracy than
  heuristics; the precise version of the Carson pattern.
- Con: a model call (or at least an embedding call) on the hot path of every
  request: added latency, added spend, a new dependency that can fail or slow.
  Needs training data you do not have until eval has run. Needs retraining as
  models and traffic drift.

### 5.3 Cascade (quality floor, latency cost)

Run the cheap model first, then a fast verifier (or the judge) checks whether the
answer is good enough; escalate to frontier only on failure. FrugalGPT-style.

- Pro: strong quality guarantee; you pay frontier only when the cheap answer was
  actually inadequate, measured rather than predicted.
- Con: the verifier sits on the hot path of every request, and on escalation you
  pay for two model calls plus the verifier. Poor fit for streaming and for
  latency-sensitive paths. Best for batch or latency-tolerant workloads.

### 5.4 Comparison

| | Hot-path latency | Added spend per request | Routing accuracy | New failure dependency | Data egress | Needs eval data |
| --- | --- | --- | --- | --- | --- | --- |
| Heuristic | none | none | low | no | no | no |
| Model classifier | one small call | small | high | yes | yes (prompt to router) | yes |
| Cascade | cheap call + verify | cheap call always, frontier on miss | highest (measured) | yes | within your providers | no |

## 6. Seam design

The router resolves deterministically today. `Router.Resolve(reqModel, tier,
keyDefault)` consults tier header, request-model-as-alias, key default, then the
config default. The classifier slots in as the source of an *effective tier* when
the resolved alias is the special `auto` alias.

Proposed interface, in `internal/router`:

```
type ClassifyInput struct {
    Shape     provider.Shape
    Body      []byte          // inbound request, read only
    Prompt    string          // extracted prompt text, a convenience
    EstTokens int             // cheap token estimate
    Tiers     []string        // candidate tier aliases, cheap..frontier
}

type Decision struct {
    Tier   string             // chosen tier alias
    Reason string             // for logging and the metrics label
}

type Classifier interface {
    Classify(ctx context.Context, in ClassifyInput) (Decision, error)
}
```

Integration: the proxy builds `ClassifyInput` only when the resolved alias is
`auto`, calls the classifier with a short timeout, and re-resolves with the
returned tier. On any error or timeout it resolves with the configured fallback
tier. The router stays a pure function of strings; the classifier call and its
fallback live in the proxy, next to the existing `Resolve` call. This keeps the
hot-path control flow explicit and testable, and keeps a model call out of the
router package.

The `auto` alias is just another entry in `routing.aliases`, plus a new
`routing.auto` config block:

```
routing:
  default_alias: cheap
  aliases:
    cheap:    { provider: glm,       model: glm-5.2 }
    frontier: { provider: anthropic, model: claude-opus-4-8 }
  auto:
    enabled: true
    strategy: heuristic        # heuristic | model | cascade
    tiers: [cheap, frontier]   # ordered cheap..frontier
    fallback_tier: frontier    # used on any classifier failure
    timeout: 150ms
    model:                     # only for strategy: model
      provider: glm
      model: glm-router-mini
```

Clients opt in by sending `model: auto` (or `x-llm-tier: auto`). Nothing routes
automatically unless asked, so existing behavior is untouched.

## 7. How eval output trains the router

The shadow-eval `eval_results` rows are (request, baseline model, candidate
model, score, costs). Exported with a feature view of each request (token count,
code presence, etc.), they become a labeled set: features plus "did the cheap
model match frontier" (score at or above a quality bar). That trains the
section 5.2 classifier offline. The same `eval_results` stream, kept running
after the router ships, monitors the router: if the cheap model's win rate on
auto-routed-cheap traffic drops, the router has drifted and needs retraining.
This is why eval is a permanent fixture, not a one-time training step.

## 8. Pros and cons of building this at all

The explicit ask. Treated as a real decision, not a foregone one.

### Reasons to build it

- It is where the spend actually is. The Coinbase chart and Carson's "20k down to
  under 5k" are this pattern. At meaningful spend with mixed task difficulty, a
  router captures savings that neither caching nor a static default can.
- It composes with work already planned. The eval pipeline produces the training
  and monitoring signal for free; the router is the action that turns eval
  insight into money saved.
- The cheap path is genuinely cheap. Heuristic `auto` routing is a small,
  self-contained, free-on-the-hot-path feature that captures real savings before
  any model-based work.
- For a portfolio and learning project, a calibrated router trained on your own
  eval data is a strong, current, end-to-end story.

### Reasons to be cautious or not build it

- A model-based router adds a model call to the hot path of every request. That
  is latency on the critical path, new per-request spend, and a new dependency
  that can fail or slow. The router must be faster and cheaper than the savings
  it produces, or it is net negative. Heuristics avoid this; the calibrated
  version does not.
- The marginal value depends entirely on traffic mix. If most requests genuinely
  need frontier, routing saves little. If most are easy, just changing the
  default to cheap (the swap) plus letting clients escalate already captures most
  of the savings, with no router. Routing earns its keep specifically when
  per-request difficulty is high, mixed, and not predictable ahead of time.
  Measure the mix before assuming a router pays off.
- Mis-routing is asymmetric and the bad direction is the hard-to-detect one
  (section 4). A router optimized naively for cost will quietly cause quality
  incidents. Guarding against that means biasing toward frontier and continuously
  monitoring, which eats into the savings and adds operational load.
- It needs data you do not have yet. A good model-based router requires eval
  labels from real traffic. Building the router before eval has produced data
  means shipping heuristics or a generic model, then redoing it. Sequencing
  matters.
- Drift is silent. Models change, prompts change, the router degrades without
  obvious failure. Operating it responsibly means a standing eval and retraining
  loop, which is real ongoing cost, not a one-time build.
- Build versus buy is real here. RouteLLM (open source), OpenRouter "auto",
  NotDiamond, Martian, and Unify already do model-based routing. For production,
  integrating one of these may dominate building your own. Building it yourself
  is justified by the learning and portfolio goal, or by needing routing
  calibrated on private traffic that cannot leave your boundary, not by it being
  the fastest path to lower spend.
- Security surface grows. A model-based router sends prompts to a router model,
  another egress of request content. Heuristics keep the decision local.

### When each approach is the right call

- Low volume or homogeneous task difficulty: do not build a router. Change the
  default and let clients escalate.
- Meaningful spend, mixed difficulty, latency tolerant: heuristic `auto` now, a
  model-based router later once eval data exists, cascade for the subset that
  needs a hard quality floor.
- Latency critical: heuristics only, or no router. Do not put a model call in
  front of a latency-sensitive request.

## 9. Failure modes and safety

| Failure | Behavior |
| --- | --- |
| Classifier errors or times out | Resolve with `fallback_tier` (default frontier). Never fail the request. |
| Classifier returns an unknown tier | Treat as failure, use fallback, log. |
| Router model is down or slow | Bounded by `timeout`; fall back. Optionally trip a breaker to skip the classifier while it is unhealthy. |
| `auto` requested but `routing.auto.enabled` is false | Resolve `auto` as a normal alias miss, surface a clear error, or fall back to default; pick one at config validation time. |
| Cheap model mis-serves a hard task | Detected after the fact by ongoing shadow eval, not in real time. This is the residual risk the bias-to-frontier and monitoring exist to bound. |

Fallback bias is deliberately toward the expensive, safe tier, matching the
asymmetric cost of mis-routing.

## 10. Observability

- `llmgw_route_decision_total{tier,reason,strategy}`: where auto traffic landed
  and why.
- `llmgw_route_classifier_latency_seconds`: hot-path tax of the classifier.
- `llmgw_route_fallback_total{cause}`: how often routing fell back, a health
  signal for the classifier.
- Projected and realized savings: compare auto-routed spend against a
  counterfactual all-frontier and all-cheap baseline, chartable from attribution
  plus the decision metric.
- The standing shadow-eval dashboard doubles as the router quality monitor.

## 11. Testing strategy

- Heuristic classifier: table-driven over crafted inputs (short, long, code,
  multi-turn, keyworded) asserting the chosen tier.
- Proxy integration: `auto` requested routes to the classifier's tier; classifier
  error falls back to `fallback_tier`; non-auto requests bypass the classifier
  entirely; the live response is byte-identical regardless of routing path.
- Model classifier: against an httptest mock returning canned decisions; parsing,
  timeout, and fallback paths. Real router-model quality is validated by hand
  with a key, like the judge, and is not a CI claim.
- Config validation: `auto` block requires a tier set, a valid fallback tier in
  the alias map, and (for `strategy: model`) a configured router provider.

## 12. Rollout and defaults

- `routing.auto.enabled` defaults false. `model: auto` does nothing until an
  operator turns it on and clients opt in.
- First shippable strategy is `heuristic`: free, safe, no data dependency.
- `model` strategy ships only after shadow eval has produced enough data to train
  and a manual validation pass confirms the router beats the heuristic.
- `cascade` is an opt-in mode for latency-tolerant, quality-critical routes.

## 13. Recommendation

1. Build shadow eval first (the companion doc). It is the data engine and the
   monitor.
2. Ship heuristic `auto` routing as an independent, free, low-risk win, and to
   lay down the `Classifier` seam in `internal/router`.
3. Add the model-based classifier only after eval data exists and shows the
   heuristic leaving savings on the table, and only if a per-request model call
   is acceptable on the hot path. Seriously weigh buying (RouteLLM, OpenRouter
   auto, NotDiamond) against building, unless the learning goal or a data-privacy
   constraint makes building the point.
4. Treat cascade as a niche mode, not the default.

The honest summary: automatic per-request routing is real and is where the
savings are, but the *good* version depends on eval data and puts a model on the
hot path, so the free heuristic version is the right first step and the
calibrated version is an earned second step, not a day-one build.

## 14. Open questions

1. Tier granularity: two tiers (cheap, frontier) or more (cheap, mid, frontier)?
   Start with two; more tiers multiply calibration and testing cost.
2. Does `auto` interact with the per-key default alias, or override it? Proposed:
   a key may set `auto` as its default, so unmodified clients get routing.
3. Heuristic feature set v1: which signals, and what thresholds? Needs a short
   calibration against real traffic samples before fixing defaults.
4. Build versus buy for the model strategy: revisit once eval data quantifies the
   heuristic's gap.
