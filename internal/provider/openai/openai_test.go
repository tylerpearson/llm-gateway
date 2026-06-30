package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

func newTestProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	return New("openai", srv.URL, "sk-secret", WithHTTPClient(srv.Client()))
}

func TestComplete_AttachesBearerAndForwardsBody(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotAccept string
		gotCT     string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	req := &provider.Request{Model: "gpt-4o", Raw: body, Header: http.Header{}}

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotPath != chatCompletionsPath {
		t.Errorf("path = %q, want %q", gotPath, chatCompletionsPath)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-secret", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json for non-stream", gotAccept)
	}
	// A non-streaming body must be forwarded verbatim (no include_usage rewrite).
	if string(gotBody) != string(body) {
		t.Errorf("forwarded body = %q, want %q", gotBody, body)
	}
	if resp.Stream {
		t.Error("response Stream = true, want false")
	}
}

func TestComplete_StreamInjectsIncludeUsage(t *testing.T) {
	var (
		gotAccept string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	req := &provider.Request{Model: "gpt-4o", Stream: true, Raw: []byte(`{"model":"gpt-4o"}`), Header: http.Header{}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", gotAccept)
	}
	if !resp.Stream {
		t.Error("response Stream = false, want true")
	}
	var m map[string]any
	if err := json.Unmarshal(gotBody, &m); err != nil {
		t.Fatalf("upstream body not valid JSON: %v", err)
	}
	so, ok := m["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("stream_options.include_usage not injected: %v", m["stream_options"])
	}
}

func TestComplete_PropagatesUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	req := &provider.Request{Model: "gpt-4o", Raw: []byte(`{}`), Header: http.Header{}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestComplete_TransportErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := srv.Client()
	url := srv.URL
	srv.Close()

	p := New("openai", url, "k", WithHTTPClient(client))
	req := &provider.Request{Model: "gpt-4o", Raw: []byte(`{}`), Header: http.Header{}}
	if _, err := p.Complete(context.Background(), req); err == nil {
		t.Fatal("expected transport error, got nil")
	}
}

func TestComplete_HonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := &provider.Request{Model: "gpt-4o", Raw: []byte(`{}`), Header: http.Header{}}
	if _, err := p.Complete(ctx, req); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := New("glm", "http://x", "k")
	if p.Name() != "glm" {
		t.Errorf("Name = %q, want glm", p.Name())
	}
	if p.Shape() != provider.ShapeOpenAI {
		t.Errorf("Shape = %q, want openai", p.Shape())
	}
	if _, ok := p.NewUsageScanner(true).(*sseUsageScanner); !ok {
		t.Error("NewUsageScanner(true) should return the SSE scanner")
	}
	if _, ok := p.NewUsageScanner(false).(*jsonUsageScanner); !ok {
		t.Error("NewUsageScanner(false) should return the JSON scanner")
	}
}

func TestEnsureIncludeUsage_PreservesExistingStreamOptions(t *testing.T) {
	in := []byte(`{"model":"x","stream_options":{"foo":"bar"}}`)
	var m map[string]any
	if err := json.Unmarshal(ensureIncludeUsage(in), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so := m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Error("include_usage should be set")
	}
	if so["foo"] != "bar" {
		t.Error("existing stream_options keys must be preserved")
	}
}
