package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tylerpearson/llm-gateway/internal/metrics"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

func TestProxyEmitsMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	providers := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(providers, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log, WithMetrics(m))

	rec := post(h, "/v1/messages", `{"model":"claude-haiku-4-5-20251001","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if n, err := testutil.GatherAndCount(reg, "llmgw_requests_total"); err != nil || n != 1 {
		t.Errorf("llmgw_requests_total series = %d (err %v), want 1", n, err)
	}
	if n, err := testutil.GatherAndCount(reg, "llmgw_tokens_total"); err != nil || n != 2 {
		t.Errorf("llmgw_tokens_total series = %d (err %v), want 2 (input+output)", n, err)
	}
}
