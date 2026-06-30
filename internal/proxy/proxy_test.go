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

	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
)

const upstreamSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":2,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}

`

// newProxy builds a proxy handler wired to an Anthropic provider that points at
// the given mock upstream, and a buffer capturing the handler's structured log.
func newProxy(t *testing.T, upstreamURL string) (*Handler, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	prov := anthropic.New("anthropic", upstreamURL, "test-key")
	return New(prov, log), &logBuf
}

func postMessages(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Messages(rec, req)
	return rec
}

// logFields returns the fields of the "proxy request" log line.
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

func TestMessages_StreamsAndCapturesUsage(t *testing.T) {
	var gotAPIKey, gotVersion, gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamSSE)
	}))
	defer upstream.Close()

	h, logBuf := newProxy(t, upstream.URL)
	rec := postMessages(h, `{"model":"claude-haiku-4-5-20251001","stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if rec.Body.String() != upstreamSSE {
		t.Errorf("body not relayed verbatim:\n got %q\nwant %q", rec.Body.String(), upstreamSSE)
	}

	// The gateway must attach its own credentials and stream-appropriate Accept.
	if gotAPIKey != "test-key" {
		t.Errorf("upstream x-api-key = %q, want test-key", gotAPIKey)
	}
	if gotVersion == "" {
		t.Error("upstream anthropic-version not set")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("upstream Accept = %q, want text/event-stream", gotAccept)
	}

	fields := logFields(t, logBuf)
	if got := fields["input_tokens"]; got != float64(10) {
		t.Errorf("input_tokens = %v, want 10", got)
	}
	if got := fields["output_tokens"]; got != float64(25) {
		t.Errorf("output_tokens = %v, want 25", got)
	}
	if got := fields["cache_read_tokens"]; got != float64(2) {
		t.Errorf("cache_read_tokens = %v, want 2", got)
	}
	if got := fields["cache_write_tokens"]; got != float64(3) {
		t.Errorf("cache_write_tokens = %v, want 3", got)
	}
}

func TestMessages_NonStreamingJSON(t *testing.T) {
	const doc = `{"id":"msg_1","type":"message","usage":{"input_tokens":5,"output_tokens":7}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, doc)
	}))
	defer upstream.Close()

	h, logBuf := newProxy(t, upstream.URL)
	rec := postMessages(h, `{"model":"claude-haiku-4-5-20251001","stream":false}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != doc {
		t.Errorf("body = %q, want %q", rec.Body.String(), doc)
	}
	fields := logFields(t, logBuf)
	if got := fields["input_tokens"]; got != float64(5) {
		t.Errorf("input_tokens = %v, want 5", got)
	}
	if got := fields["output_tokens"]; got != float64(7) {
		t.Errorf("output_tokens = %v, want 7", got)
	}
}

func TestMessages_InvalidJSON(t *testing.T) {
	h, _ := newProxy(t, "http://127.0.0.1:0")
	rec := postMessages(h, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Errorf("body = %q, want invalid_request_error", rec.Body.String())
	}
}

func TestMessages_UpstreamErrorStatusPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error"}}`)
	}))
	defer upstream.Close()

	h, _ := newProxy(t, upstream.URL)
	rec := postMessages(h, `{"model":"x","stream":false}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passed through", rec.Code)
	}
}

func TestMessages_UpstreamUnreachable(t *testing.T) {
	// Port 0 on a closed address forces a dial failure inside Complete.
	h, _ := newProxy(t, "http://127.0.0.1:1")
	rec := postMessages(h, `{"model":"x","stream":false}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}
