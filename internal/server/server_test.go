package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tylerpearson/llm-gateway/internal/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	return New(config.Server{Addr: ":0"}, log, reg)
}

func do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s.Router(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %q, want status ok", rec.Body.String())
	}
}

func TestReadyzLifecycle(t *testing.T) {
	s := newTestServer(t)

	rec := do(t, s.Router(), "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before ready: status = %d, want 503", rec.Code)
	}

	s.SetReady(true)
	rec = do(t, s.Router(), "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("after ready: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
		t.Errorf("body = %q, want status ready", rec.Body.String())
	}
}

func TestMetricsEndpoint(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_total", Help: "test"})
	reg.MustRegister(counter)
	counter.Inc()

	s := New(config.Server{Addr: ":0"}, log, reg)
	rec := do(t, s.Router(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "test_total") {
		t.Errorf("metrics body missing registered counter: %q", rec.Body.String())
	}
}
