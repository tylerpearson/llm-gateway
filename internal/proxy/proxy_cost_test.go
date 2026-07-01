package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/pricing"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

// gatewayServer wraps a handler in an httptest server so a real http.Client
// round-trip parses response trailers, which httptest.ResponseRecorder does not
// surface.
func gatewayServer(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			h.Messages(w, r)
		default:
			h.ChatCompletions(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCostHeader_CacheHitIsZero verifies a cache hit reports a zero cost as a
// normal header, since replaying a cached response incurs no upstream spend.
func TestCostHeader_CacheHitIsZero(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	fc := &fakeCache{store: map[string]*cache.Entry{}}
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log,
		WithCache(fc), WithAttribution(nopRecorder{}, pricing.DefaultTable()))

	body := `{"model":"claude-haiku-4-5-20251001","stream":true}`

	// Prime the cache with a miss, then assert the hit reports zero cost.
	_ = post(h, "/v1/messages", body)
	rec := post(h, "/v1/messages", body)
	if got := rec.Header().Get("x-llm-cache"); got != "hit" {
		t.Fatalf("x-llm-cache = %q, want hit", got)
	}
	if got := rec.Header().Get("x-llm-cost-usd"); got != "0.000000" {
		t.Errorf("x-llm-cost-usd = %q, want 0.000000", got)
	}
}

// TestCostTrailer_LiveRequest verifies a live upstream response reports its
// computed cost as a trailer, since token usage is only known after the body.
func TestCostTrailer_LiveRequest(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string // in 10, out 25 tokens from anthropicSSE
	}{
		// Haiku: 10/1e6*1.00 + 25/1e6*5.00 = 0.000135.
		{name: "known model", model: "claude-haiku-4-5-20251001", want: "0.000135"},
		// A model absent from the pricing table attributes zero cost.
		{name: "unknown model", model: "mystery-model", want: "0.000000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicSSE)
			}))
			defer upstream.Close()

			reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
			shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			h := New(reg, router.New(routingTo("anthropic", tc.model), shapes), log,
				WithAttribution(nopRecorder{}, pricing.DefaultTable()))

			gw := gatewayServer(t, h)
			body := `{"model":"` + tc.model + `","stream":true}`
			resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			// The trailer is only populated after the body is fully read.
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("drain body: %v", err)
			}
			if got := resp.Trailer.Get("x-llm-cost-usd"); got != tc.want {
				t.Errorf("x-llm-cost-usd trailer = %q, want %q", got, tc.want)
			}
		})
	}
}

// nopRecorder is an attribution.Recorder that discards records; the cost tests
// only care about the response signal, not the persisted row.
type nopRecorder struct{}

func (nopRecorder) Record(_ attribution.Record) {}
