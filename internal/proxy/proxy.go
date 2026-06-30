// Package proxy implements the inbound HTTP handlers that relay client requests
// to an upstream provider. In P1 it serves POST /v1/messages by forwarding to
// the Anthropic provider, streaming the response back to the client while
// teeing it to capture token usage.
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
)

// maxRequestBytes bounds the inbound body size. Long context requests are large
// but not unbounded; this guards against memory exhaustion.
const maxRequestBytes = 32 << 20 // 32 MiB

// Handler relays inbound requests to a single upstream provider.
type Handler struct {
	provider provider.Provider
	log      *slog.Logger
	pricing  pricing.Table
	recorder attribution.Recorder
}

// Option customizes a Handler.
type Option func(*Handler)

// WithAttribution enables cost attribution: each request's cost is computed
// from table and recorded via rec (asynchronously).
func WithAttribution(rec attribution.Recorder, table pricing.Table) Option {
	return func(h *Handler) {
		h.recorder = rec
		h.pricing = table
	}
}

// New builds a proxy handler bound to the given upstream provider.
func New(p provider.Provider, log *slog.Logger, opts ...Option) *Handler {
	h := &Handler{provider: p, log: log}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// hopByHop are response headers that must not be forwarded to the client.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// Messages handles POST /v1/messages (Anthropic Messages shape).
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
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

	preq := &provider.Request{
		Model:  meta.Model,
		Stream: meta.Stream,
		Raw:    body,
		Header: passthroughHeaders(r.Header),
	}

	resp, err := h.provider.Complete(r.Context(), preq)
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			// Client went away; nothing to write.
			return
		}
		h.log.Error("upstream request failed",
			slog.String("request_id", reqID),
			slog.String("provider", h.provider.Name()),
			slog.Any("error", err),
		)
		h.writeError(w, http.StatusBadGateway, "upstream_error", "upstream provider request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vals := range resp.Header {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	scanner := h.provider.NewUsageScanner(resp.Stream)
	written, relayErr := relay(w, resp.Body, scanner)
	usage := scanner.Usage()

	h.log.Info("proxy request",
		slog.String("request_id", reqID),
		slog.String("provider", h.provider.Name()),
		slog.String("model", meta.Model),
		slog.Bool("stream", meta.Stream),
		slog.Int("status", resp.StatusCode),
		slog.Int64("response_bytes", written),
		slog.Int("input_tokens", usage.InputTokens),
		slog.Int("output_tokens", usage.OutputTokens),
		slog.Int("cache_read_tokens", usage.CacheReadTokens),
		slog.Int("cache_write_tokens", usage.CacheWriteTokens),
		slog.Duration("duration", time.Since(start)),
	)

	if h.recorder != nil {
		h.recordAttribution(r, reqID, meta.Model, resp.StatusCode, usage, start)
	}

	if relayErr != nil {
		h.log.Warn("response relay interrupted",
			slog.String("request_id", reqID),
			slog.Any("error", relayErr),
		)
	}
}

// recordAttribution computes the request cost and enqueues an attribution
// record. In P1 pass-through the served model equals the requested model and
// cache_hit is always false; both change once routing (P4) and caching (P5)
// land.
func (h *Handler) recordAttribution(r *http.Request, reqID, model string, status int, usage provider.Usage, start time.Time) {
	cost, _ := h.pricing.Cost(model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)

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
		RequestedModel:   model,
		ServedModel:      model,
		Provider:         h.provider.Name(),
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

// passthroughHeaders selects inbound headers that upstream adapters may need.
// Client auth is never forwarded; the gateway attaches its own credentials.
func passthroughHeaders(in http.Header) http.Header {
	out := http.Header{}
	if v := in.Get("anthropic-beta"); v != "" {
		out.Set("anthropic-beta", v)
	}
	return out
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
