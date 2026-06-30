package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/eval"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

type recordingHook struct {
	called bool
	got    eval.MirrorRequest
}

func (h *recordingHook) Mirror(_ context.Context, req eval.MirrorRequest) {
	h.called = true
	h.got = req
}

func TestMirrorHookInvokedPostRouting(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	hook := &recordingHook{}
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Alias "fast" resolves to model "claude-opus-4-8" so we can confirm the
	// hook sees the served (resolved) model, not just the requested alias.
	routing := config.Routing{
		DefaultAlias: "default",
		Aliases: map[string]config.Route{
			"default": {Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
			"fast":    {Provider: "anthropic", Model: "claude-opus-4-8"},
		},
	}
	h := New(reg, router.New(routing, shapes), log, WithMirrorHook(hook))

	rec := post(h, "/v1/messages", `{"model":"fast","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !hook.called {
		t.Fatal("mirror hook was not invoked")
	}
	if hook.got.RequestedModel != "fast" || hook.got.ServedModel != "claude-opus-4-8" || hook.got.Provider != "anthropic" {
		t.Errorf("mirror request = %+v, unexpected routing snapshot", hook.got)
	}
}
