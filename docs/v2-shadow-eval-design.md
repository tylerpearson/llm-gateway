# Design: v2 shadow evaluation and eval-gated model swaps

Status: reviewed; the decisions in section 17 are confirmed. No code has been
written against this document yet.

This document specifies the first v2 increment for llm-gateway: real shadow
evaluation of a candidate model against the served baseline, scored by an LLM
judge, recorded to ClickHouse. It also describes the seams that let a later
increment add eval-gated promotion without rework. It does not specify the
promotion increment in full; that is sketched in the "Forward seam" section and
left for a follow-up doc once we have looked at real scoring data.

## 1. Context and prior art

The gateway already proxies every LLM call (P0 through P8) and ships an inert
`MirrorHook` seam plus unused ClickHouse `eval_runs` and `eval_results` tables
(P9). v2 makes that seam real.

The pattern is established. "Catalyst by Inference.net" describes the same
workflow as a product:

1. Install the gateway, keep sending traffic to the current provider.
2. The gateway studies live traffic and builds an eval set for the app.
3. It mirrors live traffic to a candidate model to run evals. Traffic is only
   mirrored; production still uses the original provider.
4. When evals look healthy, the operator gets a notification that it is safe to
   switch.
5. The operator switches the model identifier in their code.

Two things in that workflow shape this design:

- Promotion is human in the loop. The system notifies; a person makes the
  switch. We adopt the same posture for the first gated increment rather than
  auto-swapping live traffic.
- Their scoring derives a reusable eval set from traffic before comparing
  candidates. That is a stronger signal than isolated per-request judgments. We
  start with per-request pairwise judging for simplicity and record the
  traffic-derived eval set as a future scoring strategy (section 12).

## 2. Goals and non-goals

### Goals

- For a sampled fraction of live requests, replay the same prompt against a
  configured candidate model, off the request path.
- Score the candidate against the served baseline response with an LLM judge,
  and capture cheap offline signals (cost, latency, validity) for every
  comparison regardless of the judge.
- Persist one `eval_results` row per comparison and one `eval_runs` row per
  baseline/candidate pairing, so Grafana can answer "is the cheaper model good
  enough, and how much would it save."
- Never affect the response the client receives. Shadow work is asynchronous,
  bounded, and best effort.
- Be fully testable with the existing mock-provider setup. The judge is mocked
  in CI; real judge behavior is validated once by hand with a provider key.

### Non-goals (this increment)

- No change to live routing. No promotion, automatic or manual. That is the
  next increment (section 11).
- No traffic-derived eval suite. Per-request pairwise judging only.
- No multi-judge ensembling, no bias-correction beyond response-order control.
- No new provider types. Candidates and judges are existing configured
  providers (anthropic, openai, glm).

## 3. Where it sits in the request lifecycle

The proxy's `serve` method is the integration point. Today it routes, checks
cache and limits, calls the upstream, relays the response while teeing usage,
and records attribution. The mirror seam currently fires before the upstream
call with a request-only snapshot.

Shadow eval needs the baseline response to compare against, so the hook must
fire after the response completes. The proxy also must decide, before the
upstream call, whether this request is sampled, so it knows whether to capture
the baseline response bytes.

```
serve(w, r, clientShape):
  read body, parse meta (model, stream)
  resolve target (provider + model + shape)
  cache lookup            -> hit: replay, return (not evaluated, see 8.5)
  limit check             -> hard breach: 429, return
  build mirror snapshot (request-only)
  sampled = mirror.ShouldMirror(snapshot)   // false unless a real Evaluator
  translate/buildBody
  upstream Complete
  relayResponse(..., cacheCapture, evalCapture)   // evalCapture only if sampled
  record attribution, metrics
  fireMirror(snapshot, baseline=evalCapture)      // post-response
```

The candidate call, judge call, scoring, and ClickHouse writes all happen on the
Evaluator's background workers, never in `serve`.

## 4. Component inventory

| Component | Package | Responsibility |
| --- | --- | --- |
| `MirrorRequest`, `Outcome`, `Sampler` | `internal/eval` | Seam types; extend the existing `MirrorHook` |
| `Evaluator` | `internal/eval` | The real `MirrorHook`: sample, queue, run candidate, score, persist |
| `Scorer`, `LLMJudge`, `FixedScorer` | `internal/eval` | Judge abstraction and implementations |
| offline signals | `internal/eval` | Cost/latency/validity deltas captured per comparison |
| text extraction | `internal/eval` | Pull assistant text and prompt text from either wire shape |
| `ResultSink`, `Run`, `Result` | `internal/eval` | Persistence abstraction and row types |
| ClickHouse sink methods | `internal/store/clickhouse` | Implement `ResultSink` |
| eval config | `internal/config` | `eval:` section and validation |
| wiring | `cmd/gateway` | Construct the Evaluator when enabled |
| proxy integration | `internal/proxy` | Sampling decision, baseline capture, post-response firing |

## 5. Seam design (internal/eval)

The existing seam is request-only and fires pre-response. Two additive changes.

### 5.1 Extend `MirrorRequest` with the baseline outcome

```
type Outcome struct {
    Provider  string
    Model     string
    Shape     provider.Shape   // wire shape of Body
    Stream    bool             // Body is SSE when true, else single JSON doc
    Body      []byte           // raw response bytes; text extracted on demand
    Usage     provider.Usage
    CostUSD   float64
    LatencyMS int64
    Status    int
}

type MirrorRequest struct {
    RequestID, KeyID, TeamID       string
    RequestedModel, ServedModel    string
    Provider                       string
    ClientShape                    provider.Shape   // shape of Body and Baseline
    Body                           []byte           // inbound request, read only
    Baseline                       Outcome          // served response, sampled only
}
```

`Baseline` is populated only for sampled requests. `NopHook` ignores it, so the
inert default is unaffected.

### 5.2 Add an optional `Sampler` capability

```
type Sampler interface {
    ShouldMirror(req MirrorRequest) bool
}
```

The proxy calls `ShouldMirror` before the upstream call to decide whether to
capture the baseline. The decision tree the proxy uses:

- Hook is nil: nothing happens.
- Hook does not implement `Sampler` (for example `NopHook`): the hook's `Mirror`
  is still invoked post-response with an empty `Baseline`, preserving the inert
  seam's behavior, and no baseline is captured (no overhead).
- Hook implements `Sampler` (the `Evaluator`): capture the baseline only when
  `ShouldMirror` returns true, and invoke `Mirror` only for those sampled
  requests.

This keeps the no-op path free and the real path explicit. It also means the
sampling roll happens exactly once, in `ShouldMirror`, not again in `Mirror`.

### 5.3 Why capture in the client shape

The proxy already buffers the client-facing bytes for the cache (`boundedBuffer`
and `flushWriter.capture`). Those bytes are always in the client's wire shape
(cross-shape responses are translated before they reach the buffer). The
Evaluator therefore extracts baseline text using `ClientShape`. The candidate
response is in the candidate provider's shape; the Evaluator extracts that
separately. No new translation paths are introduced for capture.

## 6. The Evaluator

A bounded, asynchronous worker pool implementing `MirrorHook` and `Sampler`.

### 6.1 Configuration

```
type Candidate struct { Provider, Model string }

type Options struct {
    SampleRate float64               // fraction of eligible requests, [0,1]
    Candidates map[string]Candidate  // keyed by served (baseline) model
    Workers    int                   // background goroutines (default 2)
    QueueSize  int                   // pending-job buffer (default 64)
    Timeout    time.Duration         // per evaluation budget (default 30s)
}
```

A request is eligible only if its served model has a candidate. Eligibility is
checked in `ShouldMirror` before the sampling roll, so requests without a
candidate are never sampled and never capture a baseline.

### 6.2 Mirror and the worker

`Mirror` copies the bytes it needs (the request body and baseline body are owned
by the proxy and must not be retained by reference), then non-blockingly enqueues
a job. If the queue is full the job is dropped and counted, exactly like the
attribution Writer's overflow behavior. Losing a shadow evaluation is always
preferable to slowing a user request.

Workers pull jobs and run each evaluation under a fresh `context.WithTimeout`
derived from `context.Background()`. The request context is not used: it is
cancelled as soon as `serve` returns, which is before the candidate call
finishes.

### 6.3 One evaluation

1. Look up the candidate for the served model.
2. Build the candidate request: translate the inbound body from `ClientShape` to
   the candidate shape if they differ (reusing `internal/provider/translate`),
   set the candidate model, force `stream=false` so the response is a single
   document that is simple to read and score.
3. Call the candidate via the existing `Provider.Complete`. Read the body
   bounded, teeing into the provider's non-stream usage scanner.
4. Compute candidate cost from usage via `pricing.Table.Cost`.
5. Extract prompt text (from the request), baseline text (from
   `Baseline.Body`), and candidate text (from the candidate body).
6. Compute offline signals (section 7).
7. Score with the judge (section 8).
8. Ensure an `eval_runs` row exists for this pairing (lazily, on first use),
   then insert one `eval_results` row. Run creation must be idempotent across
   workers: two workers handling the first comparisons for a pairing at the
   same time must not create duplicate runs. Guard it in the Evaluator with a
   per-pairing once (a mutex-protected map), not with a database constraint,
   since ClickHouse does not enforce uniqueness.

Every step after the candidate call is best effort: on any error the worker logs
and drops the evaluation. Nothing here can surface to the client.

### 6.4 Streaming note

Forcing the candidate to non-streaming is a deliberate simplification: the
candidate response is never sent to a client, so streaming buys nothing and
single-document parsing is simpler and less error prone. The baseline may have
been streamed (the client asked for it); the text extractor handles both SSE and
single-document baselines.

## 7. Offline signals, always captured

Independent of the judge, every comparison records:

- baseline cost and candidate cost (USD), already first-class columns.
- baseline latency and candidate latency (ms). Both measure the same thing,
  the full upstream round trip: time from sending the request to reading the
  last byte of the response body. For a streamed baseline that is total stream
  duration, not time to first token. This keeps the two columns comparable,
  with one residual skew to keep in mind: the baseline may pay streaming
  overhead while the candidate is always non-streaming (section 6.4), so small
  latency deltas are noise. The definition is documented on the dashboard
  panel so nobody reads the delta as time to first token.
- a validity flag for the candidate: non-empty text, valid JSON envelope,
  upstream status 2xx.

These give the cost and latency half of the FinOps question for free, and a
cheap safety floor: a candidate that errored or returned empty is never treated
as a win regardless of what the judge says. The judge answers quality; the
offline signals answer cost, speed, and "did it even work."

This implies a schema decision (section 9): store latency and validity as
columns so Grafana can chart them, or keep the v1 schema and derive nothing.
Recommendation: extend the schema additively.

## 8. Scoring

### 8.1 The interface

```
type Scorer interface {
    // Score returns candidate quality relative to baseline in [0,1].
    // 0.5 is parity; above 0.5 prefers the candidate.
    Score(ctx context.Context, prompt, baseline, candidate string) (float64, error)
}
```

Implementations: `LLMJudge` (primary), `FixedScorer` (a constant, for tests and
as a safe fallback). The interface is the seam that lets a traffic-derived eval
suite (section 12) drop in later.

### 8.2 The LLM judge

`LLMJudge` wraps a configured provider and model. It renders a single grading
prompt containing the task and both answers, calls the judge provider
non-streaming, and extracts the text. The prompt demands a strict output
format: the final line of the reply must be exactly one number in [0,1], and
the prompt shows the expected shape. Parsing is strict to match: take the
final line, require it to parse as a single float already in [0,1], and treat
anything else (a "8/10" scale, prose containing several numbers, an empty
reply) as a judge error, which drops the evaluation per section 11. The
asymmetry justifies the strictness: a wrong score silently poisons the
dataset, while a dropped evaluation costs one sample. The judge prompt is
built in the judge provider's wire shape.

### 8.3 Judge bias and the mitigations we take now

LLM judges have known biases: position (favoring the first or second answer),
verbosity (favoring longer answers), and self-preference (favoring the judge's
own model family). For this increment:

- We take a minimal mitigation: the labels are fixed ("Response A (baseline)"
  versus "Response B (candidate)") and the score direction is explicit. A future
  improvement is to randomize which answer is A versus B per call and average,
  which controls position bias at double the judge cost. Recorded as an open
  item, not built now.
- Self-preference is avoided operationally by not using a candidate's own family
  as its judge. This is a config guideline, not enforced in code yet.

The point of shipping the record-only increment first is precisely to look at
the score distributions before trusting them for anything consequential.

### 8.4 CI honesty

The judge cannot run for real in CI without a provider key. CI tests the judge
logic (prompt construction, parsing, clamping, error handling) against an
httptest mock provider returning canned judge responses, and tests the Evaluator
end to end with a `FixedScorer`. Real judge quality is validated once by hand
with a key and noted as a manual gate. CI green proves the plumbing, not the
judgment.

### 8.5 Cache hits are not evaluated

A cache hit serves a stored response without an upstream call. The current code
returns before the mirror seam, and we keep that: re-evaluating identical cached
requests wastes candidate and judge spend for no new signal. Documented so it is
a choice, not an oversight.

## 9. Data model

The P9 schema exists and is unused:

```
eval_runs(id, created_at, name, baseline_model, candidate_model, status)
eval_results(run_id, request_id, ts, baseline_model, candidate_model,
             baseline_cost_usd, candidate_cost_usd, score)
```

A run groups all results for one baseline/candidate pairing. The Evaluator
creates a run lazily on first comparison for a pairing (status "active") and
references its id thereafter.

Proposed additive migration `0004` to support the offline signals in section 7:

```
ALTER TABLE eval_results
  ADD COLUMN baseline_latency_ms  UInt32,
  ADD COLUMN candidate_latency_ms UInt32,
  ADD COLUMN candidate_valid      UInt8
```

Additive and back compatible: existing rows default the new columns, and the
insert path sets them. Decision (confirmed in review): take the migration now,
so the first data collected is already rich enough to judge the signal. In
particular, without `candidate_valid` the early data cannot distinguish
"candidate lost on quality" from "candidate returned garbage", and that is
precisely the first question the data has to answer.

Privacy: `eval_results` stores models, costs, latency, and a score. It never
stores prompt or response text, consistent with the redaction posture. See
section 10.

## 10. Security and privacy

This is the most important non-functional section, because shadow eval moves
prompt and response content in a way the rest of the gateway is careful to avoid.

- The gateway never persists prompt or response bodies, and `redact_prompts`
  defaults to true. The eval pipeline keeps that property: no body text is
  written to ClickHouse. Only derived numbers (cost, latency, score, validity)
  are stored.
- However, evaluation necessarily sends the prompt and both responses to the
  judge provider, and sends the prompt to the candidate provider. That is real
  content egress to upstreams. It is the same trust boundary as serving the
  request in the first place (the prompt already went to the baseline provider),
  but the judge may be a different vendor than the baseline, which widens the
  set of parties that see the content.
- Mitigation and policy: eval is off by default. When enabled, the judge and
  candidate providers are operator-chosen configured providers, so the operator
  controls who sees the data. We document clearly that enabling eval sends
  sampled prompts and responses to the candidate and judge providers, and we add
  a guard so that with `redact_prompts: true` the proxy still does not log eval
  body text (eval text lives only in memory on the worker for the duration of an
  evaluation).
- The baseline body is held in a bounded in-memory buffer and discarded after
  the evaluation. Nothing is written to disk.

## 11. Failure modes and safety

| Failure | Behavior |
| --- | --- |
| Queue full | Drop the job, count it, periodic warn. Never block `serve`. |
| Candidate call errors or times out | Log, drop the evaluation. No row written. |
| Judge call errors or unparseable | Log, drop the evaluation. No row written. |
| Baseline capture truncated (response over cap) | Skip evaluation; cannot judge a partial answer. |
| ClickHouse insert fails | Log, drop. Eval data is best effort, like attribution. |
| Request context cancelled | Irrelevant; workers use a detached context. |
| Sink slow | Bounded by per-evaluation timeout and worker count. |

Cost amplification is a real operational cost, not a bug: each sampled request
triggers one candidate call plus one judge call. At sample rate s with average
request cost c, baseline candidate cost roughly c_cand, and judge cost c_judge,
the added spend is about s * (c_cand + c_judge) per request. The default sample
rate must be low (proposed 0.05) and the feature off by default. This is
surfaced in the docs and in a metric (section 13).

## 12. Alternatives considered

- Offline-only scoring (cost/latency/length, no judge). Rejected as the primary
  scorer because it cannot measure quality, which is the entire question.
  Adopted as an always-on complement (section 7).
- Traffic-derived eval suite (the "build evals from your data first" approach in
  the Catalyst workflow). Stronger signal: a fixed, reusable eval set makes
  candidate comparisons apples to apples rather than one-off pairwise judgments,
  and decouples scoring from live request volume. More work: it needs an eval
  generation step, storage for the suite, and a scheduled runner. Deferred. The
  `Scorer` seam is the place it would plug in, as a `SuiteScorer` that ignores
  the per-request baseline and scores the candidate against the stored suite.
- Auto-promotion in this increment. Rejected: promotion touches live routing and
  must follow real data and human review. See section 11 of the build plan and
  the forward seam below.

## 13. Observability

New Prometheus metrics (registered alongside the existing `llmgw_*` collectors):

- `llmgw_eval_sampled_total{baseline_model}`: requests selected for evaluation.
- `llmgw_eval_completed_total{baseline_model,candidate_model}`: comparisons
  recorded.
- `llmgw_eval_dropped_total{reason}`: queue full, candidate error, judge error,
  truncated.
- `llmgw_eval_score{baseline_model,candidate_model}`: a histogram of scores.
- `llmgw_eval_candidate_cost_usd_total` and the baseline counterpart, so the
  projected savings of a swap is chartable.

A provisioned Grafana "Model comparison" dashboard reads `eval_results`: score
distribution per pairing, candidate-versus-baseline cost, latency deltas, and
projected monthly savings if the candidate were promoted.

## 14. Forward seam: eval-gated promotion (next increment, not built here)

Recorded so this increment does not paint it into a corner.

- A `Gate` records each scored comparison (candidate won or not) into Redis
  counters per alias, with enough state to compute a win rate over a window.
- The Evaluator calls the gate after each result. The gate is optional and nil
  in this increment.
- When a candidate's win rate over at least N comparisons reaches a threshold,
  the gate marks the pairing as a recommendation, not an automatic route change.
  Following the Catalyst posture and the cross-fork risk in the scope analysis,
  promotion is human in the loop first: surface the recommendation in Grafana
  and via a `gatewayctl eval` command, and let an operator flip it.
- Only after the recommendation path is trusted would an opt-in auto-swap land,
  and only with a kill switch, per-alias blast-radius limits, and
  confidence-based gating rather than a raw count threshold.

The `Evaluator` already exposes an optional gate attachment point for this. No
gate logic is implemented in this increment.

## 15. Testing strategy

- Unit, `internal/eval`: text and prompt extraction across both shapes and both
  response modes; judge prompt construction, score parsing, clamping, and error
  handling against an httptest mock provider; Evaluator end to end with a mock
  candidate provider, a `FixedScorer`, and a fake `ResultSink`, asserting the run
  and result rows and the offline signals; sampling boundaries (rate 0 never,
  rate 1 always, no-candidate never).
- Unit, `internal/proxy`: a sampling hook stub receives a populated `Baseline`;
  a non-sampler hook (`NopHook` style) is still invoked with an empty baseline;
  truncated capture skips evaluation; the live response is byte-identical with
  and without sampling.
- Unit, `internal/config`: eval validation (sample rate bounds, judge and
  candidate providers must exist, candidates required when enabled).
- Integration (behind the `integration` tag), `internal/store/clickhouse`:
  insert and read back an `eval_runs` row and an `eval_results` row including the
  new columns.
- Manual, once, with a real key: enable eval at a high sample rate against a
  cheap real candidate and a real judge, confirm rows land with plausible
  scores. This is the only validation of real judge behavior and is explicitly
  not part of CI.

## 16. Rollout and defaults

- `eval.enabled` defaults to false. The gateway behaves exactly as today until
  an operator opts in.
- When enabled, `eval.sample_rate` defaults low (proposed 0.05) and requires a
  judge provider plus at least one candidate, validated at config load.
- Requires ClickHouse configured (for the sink). If eval is enabled without
  ClickHouse, fail fast at startup with a clear message.

## 17. Decisions (confirmed in review)

1. Take the additive `0004` migration for latency and validity columns. See
   section 9 for the rationale.
2. Default sample rate: 0.05.
3. Position-bias control (randomize A/B and average): later. Record raw
   single-pass scores first and look at them before paying double judge cost.
4. Judge selection: do not use a candidate's own model family as its judge.
   Documented now as a config guideline; consider enforcing at config
   validation later.
