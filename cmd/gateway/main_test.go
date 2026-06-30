package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/ratelimit"
)

func TestBuildProviders(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pcs := map[string]config.Provider{
		"anthropic": {Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "a"},
		"openai":    {Type: "openai", BaseURL: "https://api.openai.com", APIKey: "o"},
		"glm":       {Type: "glm", BaseURL: "https://glm.example", APIKey: "g"},
		"mystery":   {Type: "unknown-type", BaseURL: "https://x", APIKey: "x"},
	}

	reg := buildProviders(pcs, log)

	if _, ok := reg["mystery"]; ok {
		t.Error("provider with unknown type should be skipped")
	}
	want := map[string]provider.Shape{
		"anthropic": provider.ShapeAnthropic,
		"openai":    provider.ShapeOpenAI,
		"glm":       provider.ShapeOpenAI, // GLM uses the OpenAI adapter
	}
	if len(reg) != len(want) {
		t.Fatalf("registry has %d providers, want %d", len(reg), len(want))
	}
	for name, shape := range want {
		p, ok := reg[name]
		if !ok {
			t.Errorf("provider %q missing from registry", name)
			continue
		}
		if p.Shape() != shape {
			t.Errorf("provider %q shape = %q, want %q", name, p.Shape(), shape)
		}
		if p.Name() != name {
			t.Errorf("provider %q Name() = %q", name, p.Name())
		}
	}
}

func TestBuildProviders_Empty(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if reg := buildProviders(map[string]config.Provider{}, log); len(reg) != 0 {
		t.Errorf("empty config should yield empty registry, got %d", len(reg))
	}
}

func TestAuthCacheTTL(t *testing.T) {
	if got := authCacheTTL(0); got != auth.DefaultCacheTTL {
		t.Errorf("authCacheTTL(0) = %v, want default %v", got, auth.DefaultCacheTTL)
	}
	if got := authCacheTTL(5 * time.Minute); got != 5*time.Minute {
		t.Errorf("authCacheTTL(5m) = %v, want 5m", got)
	}
}

func TestLimitSettings(t *testing.T) {
	c := config.Limits{
		Mode:    "hard",
		PerKey:  config.LimitSet{RequestsPerMin: 10, TokensPerMin: 1000, MonthlyUSD: 50},
		PerTeam: config.LimitSet{RequestsPerMin: 100, TokensPerMin: 9000, MonthlyUSD: 500},
	}
	got := limitSettings(c)

	if got.Mode != ratelimit.Mode("hard") {
		t.Errorf("Mode = %q, want hard", got.Mode)
	}
	wantKey := ratelimit.Limits{RequestsPerMin: 10, TokensPerMin: 1000, MonthlyUSD: 50}
	if got.PerKey != wantKey {
		t.Errorf("PerKey = %+v, want %+v", got.PerKey, wantKey)
	}
	wantTeam := ratelimit.Limits{RequestsPerMin: 100, TokensPerMin: 9000, MonthlyUSD: 500}
	if got.PerTeam != wantTeam {
		t.Errorf("PerTeam = %+v, want %+v", got.PerTeam, wantTeam)
	}
}

func TestNewLogger_LevelsAndFormats(t *testing.T) {
	levels := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"unknown": slog.LevelInfo, // unknown falls back to info
	}
	for name, want := range levels {
		log := newLogger(config.Logging{Level: name, Format: "json"})
		if log.Enabled(context.Background(), want) == false {
			t.Errorf("level %q: logger should be enabled at %v", name, want)
		}
		// One level below the threshold must be disabled (except for debug, the
		// lowest level we configure).
		if want > slog.LevelDebug && log.Enabled(context.Background(), want-1) {
			t.Errorf("level %q: logger should be disabled below %v", name, want)
		}
	}

	// Both formats must produce a usable logger.
	for _, format := range []string{"json", "text", "other"} {
		if newLogger(config.Logging{Level: "info", Format: format}) == nil {
			t.Errorf("format %q produced nil logger", format)
		}
	}
}

func TestRun_ConfigLoadError(t *testing.T) {
	if err := run(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("run with missing config should return an error")
	}
}

// TestRun_StartAndGracefulShutdown wires the full server from a minimal config
// (no storage, no providers), waits for it to serve, then delivers SIGTERM and
// confirms run returns cleanly through the graceful-shutdown path.
func TestRun_StartAndGracefulShutdown(t *testing.T) {
	port := freePort(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "server:\n  addr: \"127.0.0.1:" + port + "\"\n  shutdown_timeout: 5s\nlogging:\n  level: error\n  format: json\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- run(cfgPath) }()

	// Wait for the server to accept connections on /readyz.
	url := "http://127.0.0.1:" + port + "/readyz"
	if !waitForReady(url, 3*time.Second) {
		t.Fatal("server did not become ready in time")
	}

	// Deliver SIGTERM to ourselves to trigger the graceful-shutdown branch.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	return port
}

func waitForReady(url string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // short-lived readiness poll in test
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
