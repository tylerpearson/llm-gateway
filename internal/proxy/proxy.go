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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/cache"
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
	Set(ctx context.Context, key string, e *cache.Entry)
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
	cache    ResponseCache
	limiter  RateLimiter
	metrics  *metrics.Metrics
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

// New builds a proxy handler over the provider registry and router.
func New(registry provider.Registry, rtr *router.Router, log *slog.Logger, opts ...Option) *Handler {
	h := &Handler{registry: registry, router: rtr, log: log}
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

	keyDefault := ""
	var ident ratelimit.Identity
	if p, ok := auth.FromContext(r.Context()); ok {
		keyDefault = p.DefaultAlias
		ident = ratelimit.Identity{KeyID: p.KeyID, TeamID: p.TeamID}
	}
	target, err := h.router.Resolve(meta.Model, r.Header.Get("x-llm-tier"), keyDefault)
	if err != nil {
		h.log.Warn("routing failed", slog.String("request_id", reqID), slog.Any("error", err))
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "no route for the requested model")
		return
	}
	prov, ok := h.registry.Get(target.Provider)
	if !ok {
		h.writeError(w, http.StatusBadGateway, "upstream_error", "resolved provider is not available")
		return
	}

	// Cache lookup on the exact request. A hit replays the stored response
	// without touching the upstream.
	var cacheKey string
	if h.cache != nil {
		cacheKey = cache.Key(clientShape, target.Provider, target.Model, body)
		if entry, ok := h.cache.Get(r.Context(), cacheKey); ok {
			h.serveCacheHit(w, r, reqID, meta.Model, meta.Stream, target, entry, start)
			return
		}
	}

	// Budget and rate limit enforcement. A breach is reported on x-llm-limit;
	// in hard mode it rejects the request with 429.
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
				h.observe(target.Provider, target.Model, http.StatusTooManyRequests, start, provider.Usage{}, 0, h.liveCacheLabel())
				h.writeError(w, http.StatusTooManyRequests, "rate_limit_error", "budget or rate limit exceeded: "+strings.Join(d.Exceeded, ", "))
				return
			}
		}
	}

	sendBody, err := buildBody(body, clientShape, target)
	if err != nil {
		h.log.Error("request translation failed", slog.String("request_id", reqID), slog.Any("error", err))
		h.writeError(w, http.StatusBadRequest, "invalid_request_error", "could not translate request to the upstream format")
		return
	}

	resp, err := prov.Complete(r.Context(), &provider.Request{
		Model:  target.Model,
		Stream: meta.Stream,
		Raw:    sendBody,
		Header: passthroughHeaders(r.Header),
	})
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			return
		}
		h.log.Error("upstream request failed",
			slog.String("request_id", reqID), slog.String("provider", prov.Name()), slog.Any("error", err))
		if h.metrics != nil {
			h.metrics.IncUpstreamError(prov.Name())
		}
		h.observe(target.Provider, target.Model, http.StatusBadGateway, start, provider.Usage{}, 0, h.liveCacheLabel())
		h.writeError(w, http.StatusBadGateway, "upstream_error", "upstream provider request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var capture *boundedBuffer
	if h.cache != nil {
		capture = &boundedBuffer{limit: h.cache.MaxBytes()}
	}
	usage, written, relayErr := h.relayResponse(w, resp, clientShape, target, meta.Stream, capture)

	// Store a successful, fully captured response for future identical requests.
	if h.cache != nil && capture != nil && !capture.truncated && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.cache.Set(r.Context(), cacheKey, &cache.Entry{
			Status:      resp.StatusCode,
			ContentType: contentType(meta.Stream),
			Body:        capture.Bytes(),
			Usage:       usage,
		})
	}

	// Feed the request's tokens and cost into the limiter counters so later
	// requests see updated usage.
	if h.limiter != nil && (ident.KeyID != "" || ident.TeamID != "") && resp.StatusCode < 400 {
		cost, _ := h.pricing.Cost(target.Model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)
		h.limiter.RecordUsage(r.Context(), ident, usage.InputTokens+usage.OutputTokens, cost)
	}

	h.log.Info("proxy request",
		slog.String("request_id", reqID),
		slog.String("provider", target.Provider),
		slog.String("requested_model", meta.Model),
		slog.String("served_model", target.Model),
		slog.Bool("translated", clientShape != target.Shape),
		slog.Bool("stream", meta.Stream),
		slog.Bool("cache_hit", false),
		slog.Int("status", resp.StatusCode),
		slog.Int64("response_bytes", written),
		slog.Int("input_tokens", usage.InputTokens),
		slog.Int("output_tokens", usage.OutputTokens),
		slog.Duration("duration", time.Since(start)),
	)
	if h.recorder != nil {
		h.recordAttribution(r, reqID, meta.Model, target, resp.StatusCode, usage, start, false)
	}
	h.observe(target.Provider, target.Model, resp.StatusCode, start, usage, h.costOf(target.Model, usage), h.liveCacheLabel())
	if relayErr != nil {
		h.log.Warn("response relay interrupted", slog.String("request_id", reqID), slog.Any("error", relayErr))
	}
}

// serveCacheHit replays a cached response without calling the upstream.
func (h *Handler) serveCacheHit(w http.ResponseWriter, r *http.Request, reqID, requestedModel string, stream bool, target router.Target, entry *cache.Entry, start time.Time) {
	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("x-llm-cache", "hit")
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
