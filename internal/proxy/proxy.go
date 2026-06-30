// Package proxy implements the inbound HTTP handlers that relay client requests
// to an upstream provider. It serves POST /v1/messages (Anthropic shape) and
// POST /v1/chat/completions (OpenAI shape). For each request it routes to a
// provider, forwards same-shape bodies verbatim or runs cross-shape translation,
// streams the response back, captures token usage, and records cost.
package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/pricing"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/translate"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

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
	if p, ok := auth.FromContext(r.Context()); ok {
		keyDefault = p.DefaultAlias
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
		h.writeError(w, http.StatusBadGateway, "upstream_error", "upstream provider request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	usage, written, relayErr := h.relayResponse(w, resp, clientShape, target, meta.Stream)

	h.log.Info("proxy request",
		slog.String("request_id", reqID),
		slog.String("provider", target.Provider),
		slog.String("requested_model", meta.Model),
		slog.String("served_model", target.Model),
		slog.Bool("translated", clientShape != target.Shape),
		slog.Bool("stream", meta.Stream),
		slog.Int("status", resp.StatusCode),
		slog.Int64("response_bytes", written),
		slog.Int("input_tokens", usage.InputTokens),
		slog.Int("output_tokens", usage.OutputTokens),
		slog.Duration("duration", time.Since(start)),
	)
	if h.recorder != nil {
		h.recordAttribution(r, reqID, meta.Model, target, resp.StatusCode, usage, start)
	}
	if relayErr != nil {
		h.log.Warn("response relay interrupted", slog.String("request_id", reqID), slog.Any("error", relayErr))
	}
}

// relayResponse writes the upstream response to the client, translating the body
// across shapes when needed, and returns the captured usage and bytes written.
// Error responses and same-shape responses are relayed verbatim; only
// successful cross-shape responses are translated.
func (h *Handler) relayResponse(w http.ResponseWriter, resp *provider.Response, clientShape provider.Shape, target router.Target, stream bool) (provider.Usage, int64, error) {
	w.Header().Set("Content-Type", contentType(stream))
	w.WriteHeader(resp.StatusCode)

	sameShape := clientShape == target.Shape
	if sameShape || resp.StatusCode >= 400 {
		prov, _ := h.registry.Get(target.Provider)
		scanner := prov.NewUsageScanner(resp.Stream)
		written, err := relay(w, resp.Body, scanner)
		var usage provider.Usage
		if resp.StatusCode < 400 {
			usage = scanner.Usage()
		}
		return usage, written, err
	}

	// Cross-shape success: translate the body to the client's shape.
	fw := &flushWriter{w: w}
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
// translated stream events reach the client promptly. It counts bytes written.
type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
	written int64
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.written += int64(n)
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

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
// record attributed to the authenticated key and team.
func (h *Handler) recordAttribution(r *http.Request, reqID, requestedModel string, target router.Target, status int, usage provider.Usage, start time.Time) {
	cost, _ := h.pricing.Cost(target.Model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)

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
		CacheHit:         false,
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
