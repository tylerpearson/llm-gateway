// Package proxy implements the inbound HTTP handlers that relay client requests
// to an upstream provider. It serves POST /v1/messages (Anthropic shape) and
// POST /v1/chat/completions (OpenAI shape). For each request it routes to a
// provider, forwards same-shape bodies verbatim or runs cross-shape translation,
// streams the response back, captures token usage, and records cost.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/eval"
	"github.com/tylerpearson/llm-gateway/internal/guard"
	"github.com/tylerpearson/llm-gateway/internal/metrics"
	"github.com/tylerpearson/llm-gateway/internal/pricing"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/translate"
	"github.com/tylerpearson/llm-gateway/internal/ratelimit"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

// ResponseCache is the response cache the proxy consults. cache.Cache satisfies
// it; tests provide a fake.
type ResponseCache interface {
	Get(ctx context.Context, key string) (*cache.Entry, bool)
	Set(ctx context.Context, key string, e *cache.Entry, ttl time.Duration)
	MaxBytes() int
}

// RateLimiter enforces budgets and rate limits. ratelimit.Limiter satisfies it.
type RateLimiter interface {
	Check(ctx context.Context, id ratelimit.Identity) ratelimit.Decision
	RecordUsage(ctx context.Context, id ratelimit.Identity, tokens int, costUSD float64)
}

// maxRequestBytes bounds the inbound body size. Long context requests are large
// but not unbounded; this guards against memory exhaustion.
const maxRequestBytes = 32 << 20 // 32 MiB

// Handler routes inbound requests to upstream providers, translating across wire
// shapes when needed.
type Handler struct {
	registry provider.Registry
	router   *router.Router
	log      *slog.Logger
	pricing  pricing.Table
	recorder attribution.Recorder
	cache         ResponseCache
	limiter       RateLimiter
	metrics       *metrics.Metrics
	redactPrompts bool
	mirror        eval.MirrorHook
	breaker       Breaker
	policy        ResiliencePolicy
	ctxCheck      contextCheck
	guard         guard.Guard
}

// Option customizes a Handler.
type Option func(*Handler)

// WithAttribution enables cost attribution: each request's cost is computed from
// table and recorded via rec (asynchronously).
func WithAttribution(rec attribution.Recorder, table pricing.Table) Option {
	return func(h *Handler) {
		h.recorder = rec
		h.pricing = table
	}
}

// WithCache enables the exact-match response cache.
func WithCache(c ResponseCache) Option {
	return func(h *Handler) { h.cache = c }
}

// WithRateLimit enables budget and rate limit enforcement.
func WithRateLimit(l RateLimiter) Option {
	return func(h *Handler) { h.limiter = l }
}

// WithMetrics enables Prometheus instrumentation.
func WithMetrics(m *metrics.Metrics) Option {
	return func(h *Handler) { h.metrics = m }
}

func (h *Handler) observe(prov, model string, status int, start time.Time, usage provider.Usage, cost float64, cacheResult string) {
	if h.metrics != nil {
		h.metrics.ObserveRequest(prov, model, status, time.Since(start), usage.InputTokens, usage.OutputTokens, cost, cacheResult)
	}
}

func (h *Handler) costOf(model string, usage provider.Usage) float64 {
	cost, _ := h.pricing.Cost(model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)
	return cost
}

// liveCacheLabel is the cache result for a request that reached the upstream:
// "miss" when the cache is enabled, "" when it is off.
func (h *Handler) liveCacheLabel() string {
	if h.cache != nil {
		return "miss"
	}
	return ""
}

// WithPromptRedaction controls whether prompt and response content stays out of
// logs. Redaction is on by default; passing false enables debug prompt previews.
func WithPromptRedaction(redact bool) Option {
	return func(h *Handler) { h.redactPrompts = redact }
}

// WithMirrorHook installs the post-routing mirror hook (a v2 eval seam). The
// hook is invoked after routing with a snapshot of the request.
func WithMirrorHook(hook eval.MirrorHook) Option {
	return func(h *Handler) { h.mirror = hook }
}

// WithFailover enables upstream failover: the handler tries each routed
// candidate in order, retries per the policy, and consults the breaker to skip
// targets in cooldown. Without this option the handler makes a single attempt
// against the primary target (the pre-failover behavior).
func WithFailover(breaker Breaker, policy ResiliencePolicy) Option {
	return func(h *Handler) {
		if breaker != nil {
			h.breaker = breaker
		}
		h.policy = policy
	}
}

// WithContextCheck enables the pre-call context-window check. table supplies each
// model's context window; charsPerToken and safetyMargin tune the conservative
// token estimate. When enabled, a request estimated to exceed a candidate
// model's window skips that candidate, and a request that fits no candidate is
// rejected before any upstream call.
func WithContextCheck(table pricing.Table, charsPerToken int, safetyMargin float64) Option {
	return func(h *Handler) {
		h.ctxCheck = contextCheck{
			enabled:       true,
			table:         table,
			charsPerToken: charsPerToken,
			safetyMargin:  safetyMargin,
		}
	}
}

// WithGuard installs a pre-call request guard. The guard inspects each request
// before it is sent upstream and may mask the body or block the request. The
// default is guard.NopGuard (allow everything).
func WithGuard(g guard.Guard) Option {
	return func(h *Handler) {
		if g != nil {
			h.guard = g
		}
	}
}

// New builds a proxy handler over the provider registry and router. Prompt
// redaction is on by default; failover is off (single attempt) until
// WithFailover is supplied; the guard defaults to a no-op.
func New(registry provider.Registry, rtr *router.Router, log *slog.Logger, opts ...Option) *Handler {
	h := &Handler{registry: registry, router: rtr, log: log, redactPrompts: true, breaker: NoopBreaker{}, guard: guard.NopGuard{}}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Messages handles POST /v1/messages (Anthropic Messages shape).
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, provider.ShapeAnthropic)
}

// ChatCompletions handles POST /v1/chat/completions (OpenAI shape).
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, provider.ShapeOpenAI)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, clientShape provider.Shape) {
	start := time.Now()
	reqID := middleware.GetReqID(r.Context())

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	// Prompt content is never persisted. It is logged only when redaction is
	// explicitly disabled, and then only as a short debug preview.
	if !h.redactPrompts {
		h.log.Debug("request prompt preview", slog.String("request_id", reqID), slog.String("preview", preview(body)))
	}

	keyDefault := ""
	var ident ratelimit.Identity
	if p, ok := auth.FromContext(r.Context()); ok {
		keyDefault = p.DefaultAlias
		ident = ratelimit.Identity{KeyID: p.KeyID, TeamID: p.TeamID}
	}
	candidates, err := h.router.Resolve(meta.Model, r.Header.Get("x-llm-tier"), keyDefault)
	if err != nil {
		h.log.Warn("routing failed", slog.String("request_id", reqID), slog.Any("error", err))
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "no route for the requested model")
		return
	}
	// primary is the alias's canonical target: the cache key, limit accounting,
	// and mirror seam key off it, while the request may ultimately be served by
	// a fallback in the candidate list.
	primary := candidates[0]

	// Budget and rate limit enforcement. This runs before the cache lookup so a
	// cache hit is still subject to per-request rate limits and hard-mode
	// budget rejection; otherwise a cached response could be replayed for free
	// past a key or team's limit. A breach is reported on x-llm-limit; in hard
	// mode it rejects the request with 429.
	if h.limiter != nil && (ident.KeyID != "" || ident.TeamID != "") {
		d := h.limiter.Check(r.Context(), ident)
		if len(d.Exceeded) > 0 {
			w.Header().Set("x-llm-limit", strings.Join(d.Exceeded, ","))
			h.log.Warn("limit exceeded",
				slog.String("request_id", reqID),
				slog.String("exceeded", strings.Join(d.Exceeded, ",")),
				slog.Bool("allowed", d.Allowed))
			if !d.Allowed {
				if h.metrics != nil {
					for _, sc := range d.Exceeded {
						h.metrics.IncLimitRejection(sc)
					}
				}
				h.observe(primary.Provider, primary.Model, http.StatusTooManyRequests, start, provider.Usage{}, 0, h.liveCacheLabel())
				h.writeError(w, http.StatusTooManyRequests, "rate_limit_error", "budget or rate limit exceeded: "+strings.Join(d.Exceeded, ", "))
				return
			}
		}
	}

	// Pre-call guard. It runs before the cache key is computed so a masked body
	// caches under the same key as identical masked inputs, and a blocked request
	// never serves or stores a cache entry. On Mask the body is rewritten before
	// it is hashed, cached, and sent upstream.
	if d := h.guard.Inspect(r.Context(), guard.Request{
		RequestID: reqID,
		KeyID:     ident.KeyID,
		TeamID:    ident.TeamID,
		Provider:  primary.Provider,
		Model:     primary.Model,
		Body:      body,
	}); d.Action != guard.Allow {
		switch d.Action {
		case guard.Block:
			if h.metrics != nil {
				h.metrics.IncGuardAction("block", d.Category)
			}
			h.log.Warn("request blocked by guard",
				slog.String("request_id", reqID), slog.String("category", d.Category))
			h.observe(primary.Provider, primary.Model, http.StatusForbidden, start, provider.Usage{}, 0, h.liveCacheLabel())
			h.writeError(w, http.StatusForbidden, "permission_error", guardMessage(d))
			return
		case guard.Mask:
			if len(d.Rewrite) > 0 {
				body = d.Rewrite
				// Re-read the routing-relevant fields in case masking touched
				// them (it should not, but the body is now authoritative).
				_ = json.Unmarshal(body, &meta)
			}
			if h.metrics != nil {
				h.metrics.IncGuardAction("mask", d.Category)
			}
		}
	}

	// Cache lookup on the exact request, scoped to the caller's tenant. A hit
	// replays the stored response without touching the upstream. Per-request
	// Cache-Control directives can bypass the read (no-cache, no-store) or bound
	// how stale a hit may be (s-maxage).
	var cacheKey string
	cc := parseCacheControl(r.Header)
	if h.cache != nil {
		tenant := ident.TeamID
		if tenant == "" {
			tenant = ident.KeyID
		}
		cacheKey = cache.Key(tenant, clientShape, primary.Provider, primary.Model, body)
		if !cc.noStore && !cc.noCache {
			if entry, ok := h.cache.Get(r.Context(), cacheKey); ok && entryFresh(entry, cc) {
				h.serveCacheHit(w, r, reqID, meta.Model, meta.Stream, primary, entry, cacheKey, start)
				return
			}
		}
	}

	// Post-routing mirror seam for future shadow evaluation (no-op in v1). It
	// records the primary target, the route the request was dispatched against
	// before any failover.
	if h.mirror != nil {
		h.mirror.Mirror(r.Context(), eval.MirrorRequest{
			RequestID:      reqID,
			KeyID:          ident.KeyID,
			TeamID:         ident.TeamID,
			RequestedModel: meta.Model,
			ServedModel:    primary.Model,
			Provider:       primary.Provider,
			Body:           body,
		})
	}

	// Pre-call context-window check: drop candidates whose model cannot fit the
	// estimated request size so dispatch fails over to a larger-context model.
	// When nothing fits, reject before any upstream call rather than send a
	// request guaranteed to fail.
	if h.ctxCheck.enabled {
		est := estimateTokens(body, h.ctxCheck.charsPerToken, h.ctxCheck.safetyMargin)
		fitting, largest := h.filterByContext(candidates, est)
		if len(fitting) == 0 {
			h.log.Warn("request exceeds all candidate context windows",
				slog.String("request_id", reqID),
				slog.Int("estimated_tokens", est),
				slog.Int("largest_window", largest))
			h.observe(primary.Provider, primary.Model, http.StatusRequestEntityTooLarge, start, provider.Usage{}, 0, h.liveCacheLabel())
			h.writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("estimated %d tokens exceeds the largest available context window (%d)", est, largest))
			return
		}
		candidates = fitting
	}

	// Dispatch across the candidate list with retries and breaker gating. served
	// is the target that actually produced the relayed response. Failover happens
	// entirely inside dispatch, before any byte is relayed to the client.
	served, resp, cleanup, lastStatus, dispErr := h.dispatch(r.Context(), reqID, clientShape, meta.Stream, candidates, body, r.Header)
	if resp == nil {
		if r.Context().Err() != nil {
			return
		}
		status := http.StatusBadGateway
		if lastStatus != 0 {
			status = lastStatus
		}
		h.log.Error("all upstream candidates failed",
			slog.String("request_id", reqID),
			slog.String("primary_provider", primary.Provider),
			slog.Int("candidates", len(candidates)),
			slog.Any("error", dispErr))
		h.observe(served.Provider, served.Model, status, start, provider.Usage{}, 0, h.liveCacheLabel())
		h.writeError(w, status, "upstream_error", "upstream provider request failed")
		return
	}
	defer cleanup()
	defer func() { _ = resp.Body.Close() }()

	// Capture the response for caching unless the request opted out with
	// no-store, in which case there is nothing to buffer.
	var capture *boundedBuffer
	if h.cache != nil && !cc.noStore {
		capture = &boundedBuffer{limit: h.cache.MaxBytes()}
	}
	if h.cache != nil {
		w.Header().Set("x-llm-cache-key", cacheKey)
	}
	usage, written, relayErr := h.relayResponse(w, resp, clientShape, served, meta.Stream, capture)

	// Post-response bookkeeping uses a context derived from the request's but
	// with cancellation stripped. r.Context() is canceled as soon as the
	// client disconnects, which happens routinely for streamed responses right
	// after the last byte is relayed; using it here would silently drop cache
	// writes and budget counters for exactly the requests that just completed.
	bgCtx := context.WithoutCancel(r.Context())

	// Store a successful, fully captured response for future identical requests.
	// capture is nil when the request opted out with no-store, so that case is
	// skipped here too. cc.ttl (from a Cache-Control ttl directive) overrides the
	// default expiry when set.
	if h.cache != nil && capture != nil && !capture.truncated && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.cache.Set(bgCtx, cacheKey, &cache.Entry{
			Status:      resp.StatusCode,
			ContentType: contentType(meta.Stream),
			Body:        capture.Bytes(),
			Usage:       usage,
		}, cc.ttl)
	}

	// Feed the request's tokens and cost into the limiter counters so later
	// requests see updated usage.
	if h.limiter != nil && (ident.KeyID != "" || ident.TeamID != "") && resp.StatusCode < 400 {
		cost, _ := h.pricing.Cost(served.Model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)
		h.limiter.RecordUsage(bgCtx, ident, usage.InputTokens+usage.OutputTokens, cost)
	}

	h.log.Info("proxy request",
		slog.String("request_id", reqID),
		slog.String("provider", served.Provider),
		slog.String("requested_model", meta.Model),
		slog.String("served_model", served.Model),
		slog.Bool("translated", clientShape != served.Shape),
		slog.Bool("failover", served.Provider != primary.Provider || served.Model != primary.Model),
		slog.Bool("stream", meta.Stream),
		slog.Bool("cache_hit", false),
		slog.Int("status", resp.StatusCode),
		slog.Int64("response_bytes", written),
		slog.Int("input_tokens", usage.InputTokens),
		slog.Int("output_tokens", usage.OutputTokens),
		slog.Duration("duration", time.Since(start)),
	)
	if h.recorder != nil {
		h.recordAttribution(r, reqID, meta.Model, served, resp.StatusCode, usage, start, false)
	}
	cost := h.costOf(served.Model, usage)
	// Usage is now known, so fill in the x-llm-cost-usd trailer declared by
	// relayResponse. Unknown models cost zero, matching the attribution record.
	w.Header().Set("x-llm-cost-usd", strconv.FormatFloat(cost, 'f', 6, 64))
	h.observe(served.Provider, served.Model, resp.StatusCode, start, usage, cost, h.liveCacheLabel())
	if relayErr != nil {
		h.log.Warn("response relay interrupted", slog.String("request_id", reqID), slog.Any("error", relayErr))
	}
}

// cacheControl holds the per-request cache directives parsed from the
// Cache-Control header. The gateway honors a small, HTTP-aligned subset:
// no-store (do not read or write the cache), no-cache (skip the read but still
// store the fresh response), s-maxage=<seconds> (only serve a hit younger than
// this), and ttl=<seconds> (override the store expiry for this response).
type cacheControl struct {
	noStore    bool
	noCache    bool
	sMaxAge    time.Duration
	hasSMaxAge bool
	ttl        time.Duration
}

// parseCacheControl reads the Cache-Control request header into directives.
// Unknown directives are ignored; malformed numeric values are dropped rather
// than failing the request.
func parseCacheControl(hdr http.Header) cacheControl {
	var cc cacheControl
	raw := hdr.Get("Cache-Control")
	if raw == "" {
		return cc
	}
	for _, part := range strings.Split(raw, ",") {
		name, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.ToLower(strings.TrimSpace(name))
		val = strings.TrimSpace(val)
		switch name {
		case "no-store":
			cc.noStore = true
		case "no-cache":
			cc.noCache = true
		case "s-maxage":
			if secs, err := strconv.Atoi(val); err == nil && secs >= 0 {
				cc.sMaxAge = time.Duration(secs) * time.Second
				cc.hasSMaxAge = true
			}
		case "ttl":
			if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
				cc.ttl = time.Duration(secs) * time.Second
			}
		}
	}
	return cc
}

// entryFresh reports whether a cached entry satisfies the request's s-maxage
// bound. Without s-maxage every stored entry is fresh enough.
func entryFresh(e *cache.Entry, cc cacheControl) bool {
	if !cc.hasSMaxAge {
		return true
	}
	return time.Since(time.Unix(e.CreatedAt, 0)) <= cc.sMaxAge
}

// serveCacheHit replays a cached response without calling the upstream.
func (h *Handler) serveCacheHit(w http.ResponseWriter, r *http.Request, reqID, requestedModel string, stream bool, target router.Target, entry *cache.Entry, cacheKey string, start time.Time) {
	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("x-llm-cache", "hit")
	w.Header().Set("x-llm-cache-key", cacheKey)
	// A cache hit incurs no upstream spend, so its attributed cost is zero, the
	// same value recordAttribution stores. Report it explicitly; it doubles as a
	// signal of cache savings. Unlike the live path the cost is known before the
	// body here, so it is a normal header rather than a trailer.
	w.Header().Set("x-llm-cost-usd", "0.000000")
	w.WriteHeader(entry.Status)
	_, _ = w.Write(entry.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	h.log.Info("proxy request",
		slog.String("request_id", reqID),
		slog.String("provider", target.Provider),
		slog.String("requested_model", requestedModel),
		slog.String("served_model", target.Model),
		slog.Bool("translated", false),
		slog.Bool("stream", stream),
		slog.Bool("cache_hit", true),
		slog.Int("status", entry.Status),
		slog.Int("input_tokens", entry.Usage.InputTokens),
		slog.Int("output_tokens", entry.Usage.OutputTokens),
		slog.Duration("duration", time.Since(start)),
	)
	if h.recorder != nil {
		h.recordAttribution(r, reqID, requestedModel, target, entry.Status, entry.Usage, start, true)
	}
	h.observe(target.Provider, target.Model, entry.Status, start, entry.Usage, 0, "hit")
}

// relayResponse writes the upstream response to the client, translating the body
// across shapes when needed, and returns the captured usage and bytes written.
// Error responses and same-shape responses are relayed verbatim; only
// successful cross-shape responses are translated.
func (h *Handler) relayResponse(w http.ResponseWriter, resp *provider.Response, clientShape provider.Shape, target router.Target, stream bool, capture *boundedBuffer) (provider.Usage, int64, error) {
	w.Header().Set("Content-Type", contentType(stream))
	if h.cache != nil {
		w.Header().Set("x-llm-cache", "miss")
	}
	// The per-request cost depends on token usage, which is only known after the
	// body has streamed. Announce x-llm-cost-usd as a trailer so serve can set its
	// value once usage is captured. Add, not Set, so any earlier Trailer
	// declaration is preserved.
	w.Header().Add("Trailer", "x-llm-cost-usd")
	w.WriteHeader(resp.StatusCode)

	sameShape := clientShape == target.Shape
	if sameShape || resp.StatusCode >= 400 {
		prov, _ := h.registry.Get(target.Provider)
		scanner := prov.NewUsageScanner(resp.Stream)
		// Tee the client-facing bytes into the cache capture buffer as well.
		var tee io.Writer = scanner
		if capture != nil {
			tee = io.MultiWriter(scanner, capture)
		}
		written, err := relay(w, resp.Body, tee)
		var usage provider.Usage
		if resp.StatusCode < 400 {
			usage = scanner.Usage()
		}
		return usage, written, err
	}

	// Cross-shape success: translate the body to the client's shape.
	fw := &flushWriter{w: w, capture: capture}
	if f, ok := w.(http.Flusher); ok {
		fw.flusher = f
	}
	var usage provider.Usage
	var err error
	if target.Shape == provider.ShapeOpenAI {
		usage, err = translate.OpenAIResponseToAnthropic(fw, resp.Body, resp.Stream, target.Model)
	} else {
		usage, err = translate.AnthropicResponseToOpenAI(fw, resp.Body, resp.Stream, target.Model)
	}
	return usage, fw.written, err
}

// buildBody produces the request body to send upstream: verbatim (with the
// resolved model set) for same-shape, or translated for cross-shape.
func buildBody(body []byte, clientShape provider.Shape, target router.Target) ([]byte, error) {
	if clientShape == target.Shape {
		return setModel(body, target.Model), nil
	}
	if target.Shape == provider.ShapeOpenAI {
		return translate.AnthropicRequestToOpenAI(body, target.Model)
	}
	return translate.OpenAIRequestToAnthropic(body, target.Model)
}

func contentType(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

// preview returns a short, truncated view of a body for debug logging.
func preview(body []byte) string {
	const max = 256
	if len(body) > max {
		return string(body[:max]) + "..."
	}
	return string(body)
}

// setModel rewrites the top level model field without disturbing other fields.
func setModel(body []byte, model string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return body
	}
	m["model"] = mb
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// relay copies body to the client, flushing each chunk so streamed responses
// reach the client promptly, while teeing the same bytes into scanner for usage
// capture.
func relay(w http.ResponseWriter, body io.Reader, scanner io.Writer) (int64, error) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	var total int64
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return total, writeErr
			}
			_, _ = scanner.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
			total += int64(n)
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// flushWriter flushes the underlying ResponseWriter after each write so
// translated stream events reach the client promptly. It counts bytes written
// and optionally tees them into a capture buffer for caching.
type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
	capture *boundedBuffer
	written int64
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.written += int64(n)
	if fw.capture != nil {
		_, _ = fw.capture.Write(p[:n])
	}
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

// boundedBuffer accumulates bytes up to a limit. Once the limit is exceeded it
// marks itself truncated and discards its contents, signaling that the
// response is too large to cache.
type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if !b.truncated {
		if b.buf.Len()+len(p) > b.limit {
			b.truncated = true
			b.buf.Reset()
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// passthroughHeaders selects inbound headers that upstream adapters may need.
// Client auth is never forwarded; the gateway attaches its own credentials.
func passthroughHeaders(in http.Header) http.Header {
	out := http.Header{}
	if v := in.Get("anthropic-beta"); v != "" {
		out.Set("anthropic-beta", v)
	}
	return out
}

// recordAttribution computes the request cost and enqueues an attribution
// record attributed to the authenticated key and team. A cache hit is recorded
// with zero cost since it incurred no upstream spend.
func (h *Handler) recordAttribution(r *http.Request, reqID, requestedModel string, target router.Target, status int, usage provider.Usage, start time.Time, cacheHit bool) {
	var cost float64
	if !cacheHit {
		cost, _ = h.pricing.Cost(target.Model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)
	}

	var keyID, teamID string
	if p, ok := auth.FromContext(r.Context()); ok {
		keyID = p.KeyID
		teamID = p.TeamID
	}
	h.recorder.Record(attribution.Record{
		Timestamp:        start,
		RequestID:        reqID,
		KeyID:            keyID,
		TeamID:           teamID,
		RequestedModel:   requestedModel,
		ServedModel:      target.Model,
		Provider:         target.Provider,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		CostUSD:          cost,
		LatencyMS:        time.Since(start).Milliseconds(),
		CacheHit:         cacheHit,
		Status:           status,
	})
}

// guardMessage builds the client-facing message for a blocked request, falling
// back to a generic reason when the guard did not supply one so no internal
// detail leaks by accident.
func guardMessage(d guard.Decision) string {
	if d.Reason != "" {
		return d.Reason
	}
	return "request blocked by policy"
}

func (h *Handler) writeError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}
