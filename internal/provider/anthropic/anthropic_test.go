package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

// newTestProvider builds a Provider pointed at srv with the server's own client
// so requests stay in-process.
func newTestProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	return New("anthropic", srv.URL, "secret-key", WithHTTPClient(srv.Client()))
}

func TestComplete_AttachesCredentialsAndForwardsBody(t *testing.T) {
	var (
		gotPath    string
		gotMethod  string
		gotHeaders http.Header
		gotBody    []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	body := []byte(`{"model":"claude","messages":[]}`)
	req := &provider.Request{Model: "claude", Raw: body, Header: http.Header{}}

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != messagesPath {
		t.Errorf("path = %q, want %q", gotPath, messagesPath)
	}
	if string(gotBody) != string(body) {
		t.Errorf("forwarded body = %q, want %q", gotBody, body)
	}
	if got := gotHeaders.Get("x-api-key"); got != "secret-key" {
		t.Errorf("x-api-key = %q, want secret-key", got)
	}
	if got := gotHeaders.Get("anthropic-version"); got != defaultVersion {
		t.Errorf("anthropic-version = %q, want %q", got, defaultVersion)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := gotHeaders.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json for non-stream", got)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Stream {
		t.Error("response Stream = true, want false")
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != `{"id":"msg_1"}` {
		t.Errorf("relayed body = %q", got)
	}
}

func TestComplete_StreamSetsEventStreamAccept(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	req := &provider.Request{Model: "claude", Stream: true, Raw: []byte(`{}`), Header: http.Header{}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if accept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", accept)
	}
	if !resp.Stream {
		t.Error("response Stream = false, want true for streaming request")
	}
}

func TestComplete_PassesThroughAnthropicBeta(t *testing.T) {
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	h := http.Header{}
	h.Set("anthropic-beta", "prompt-caching-2024-07-31")
	req := &provider.Request{Model: "claude", Raw: []byte(`{}`), Header: h}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotBeta != "prompt-caching-2024-07-31" {
		t.Errorf("anthropic-beta = %q, want passthrough", gotBeta)
	}
}

func TestComplete_OmitsBetaWhenAbsent(t *testing.T) {
	var hasBeta bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasBeta = r.Header["Anthropic-Beta"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	req := &provider.Request{Model: "claude", Raw: []byte(`{}`), Header: http.Header{}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if hasBeta {
		t.Error("anthropic-beta header should be absent when client did not send one")
	}
}

func TestComplete_WithVersionOverride(t *testing.T) {
	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New("anthropic", srv.URL, "k", WithHTTPClient(srv.Client()), WithVersion("2099-01-01"))
	req := &provider.Request{Model: "claude", Raw: []byte(`{}`), Header: http.Header{}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotVersion != "2099-01-01" {
		t.Errorf("anthropic-version = %q, want override", gotVersion)
	}
}

func TestComplete_PropagatesUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	req := &provider.Request{Model: "claude", Raw: []byte(`{}`), Header: http.Header{}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429; upstream errors must be relayed, not swallowed", resp.StatusCode)
	}
}

func TestComplete_TransportErrorIsReturned(t *testing.T) {
	// Point at a closed server so the round trip fails at the transport layer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := srv.Client()
	url := srv.URL
	srv.Close()

	p := New("anthropic", url, "k", WithHTTPClient(client))
	req := &provider.Request{Model: "claude", Raw: []byte(`{}`), Header: http.Header{}}
	_, err := p.Complete(context.Background(), req)
	if err == nil {
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
	req := &provider.Request{Model: "claude", Raw: []byte(`{}`), Header: http.Header{}}
	_, err := p.Complete(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestProviderMetadata(t *testing.T) {
	p := New("my-anthropic", "http://x", "k")
	if p.Name() != "my-anthropic" {
		t.Errorf("Name = %q, want my-anthropic", p.Name())
	}
	if p.Shape() != provider.ShapeAnthropic {
		t.Errorf("Shape = %q, want anthropic", p.Shape())
	}
	if _, ok := p.NewUsageScanner(true).(*sseUsageScanner); !ok {
		t.Error("NewUsageScanner(true) should return the SSE scanner")
	}
	if _, ok := p.NewUsageScanner(false).(*jsonUsageScanner); !ok {
		t.Error("NewUsageScanner(false) should return the JSON scanner")
	}
}

func TestAcceptFor(t *testing.T) {
	if got := acceptFor(true); got != "text/event-stream" {
		t.Errorf("acceptFor(true) = %q", got)
	}
	if got := acceptFor(false); got != "application/json" {
		t.Errorf("acceptFor(false) = %q", got)
	}
}
