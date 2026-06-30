package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/provider/openai"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}

`

const openAISSE = `data: {"choices":[{"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":" there"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3}}

data: [DONE]

`

func newHandler(t *testing.T, providers provider.Registry, routing config.Routing) (*Handler, *bytes.Buffer) {
	t.Helper()
	shapes := map[string]provider.Shape{}
	for n, p := range providers {
		shapes[n] = p.Shape()
	}
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	return New(providers, router.New(routing, shapes), log), &logBuf
}

func routingTo(providerName, model string) config.Routing {
	return config.Routing{
		DefaultAlias: "default",
		Aliases:      map[string]config.Route{"default": {Provider: providerName, Model: model}},
	}
}

func post(h *Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	if path == "/v1/messages" {
		h.Messages(rec, req)
	} else {
		h.ChatCompletions(rec, req)
	}
	return rec
}

func logFields(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	sc := bufio.NewScanner(buf)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m["msg"] == "proxy request" {
			return m
		}
	}
	t.Fatal("no proxy request log line found")
	return nil
}

func TestMessages_SameShapeStreamsAndCapturesUsage(t *testing.T) {
	var gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "test-key")}
	h, logBuf := newHandler(t, reg, routingTo("anthropic", "claude-haiku-4-5-20251001"))
	rec := post(h, "/v1/messages", `{"model":"claude-haiku-4-5-20251001","stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != anthropicSSE {
		t.Errorf("body not relayed verbatim: got %q", rec.Body.String())
	}
	if gotAPIKey != "test-key" {
		t.Errorf("upstream x-api-key = %q, want test-key", gotAPIKey)
	}
	f := logFields(t, logBuf)
	if f["input_tokens"] != float64(10) || f["output_tokens"] != float64(25) {
		t.Errorf("usage = in %v out %v, want 10/25", f["input_tokens"], f["output_tokens"])
	}
	if f["translated"] != false {
		t.Errorf("translated = %v, want false", f["translated"])
	}
}

func TestChatCompletions_SameShapeOpenAI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oai-key" {
			t.Errorf("upstream auth = %q, want Bearer oai-key", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openAISSE)
	}))
	defer upstream.Close()

	reg := provider.Registry{"openai": openai.New("openai", upstream.URL, "oai-key")}
	h, logBuf := newHandler(t, reg, routingTo("openai", "gpt-4o-mini"))
	rec := post(h, "/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Errorf("body missing relayed content: %q", rec.Body.String())
	}
	f := logFields(t, logBuf)
	if f["input_tokens"] != float64(8) || f["output_tokens"] != float64(3) {
		t.Errorf("usage = in %v out %v, want 8/3", f["input_tokens"], f["output_tokens"])
	}
}

// TestMessages_CrossShapeTranslation routes an Anthropic /v1/messages request to
// an OpenAI shaped provider and verifies the response is translated back to
// Anthropic SSE with usage captured.
func TestMessages_CrossShapeTranslation(t *testing.T) {
	var sawOpenAIModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		sawOpenAIModel = req.Model
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openAISSE)
	}))
	defer upstream.Close()

	reg := provider.Registry{"glm": openai.New("glm", upstream.URL, "glm-key")}
	h, logBuf := newHandler(t, reg, routingTo("glm", "glm-4.5"))
	// Client speaks Anthropic shape; alias "default" routes to the OpenAI shaped glm.
	rec := post(h, "/v1/messages", `{"model":"default","stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if sawOpenAIModel != "glm-4.5" {
		t.Errorf("upstream model = %q, want glm-4.5 (request translated)", sawOpenAIModel)
	}
	out := rec.Body.String()
	// The client must receive Anthropic shaped SSE, not the raw OpenAI stream.
	for _, want := range []string{"event: message_start", "content_block_delta", "text_delta", "hi", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("translated output missing %q\n got: %q", want, out)
		}
	}
	f := logFields(t, logBuf)
	if f["translated"] != true {
		t.Errorf("translated = %v, want true", f["translated"])
	}
	if f["input_tokens"] != float64(8) || f["output_tokens"] != float64(3) {
		t.Errorf("usage = in %v out %v, want 8/3", f["input_tokens"], f["output_tokens"])
	}
}

func TestServe_InvalidJSON(t *testing.T) {
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", "http://127.0.0.1:0", "k")}
	h, _ := newHandler(t, reg, routingTo("anthropic", "m"))
	rec := post(h, "/v1/messages", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestServe_UpstreamErrorPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error"}`)
	}))
	defer upstream.Close()
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	h, _ := newHandler(t, reg, routingTo("anthropic", "m"))
	rec := post(h, "/v1/messages", `{"model":"m","stream":false}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
}

func TestServe_UpstreamUnreachable(t *testing.T) {
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", "http://127.0.0.1:1", "k")}
	h, _ := newHandler(t, reg, routingTo("anthropic", "m"))
	rec := post(h, "/v1/messages", `{"model":"m","stream":false}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}
