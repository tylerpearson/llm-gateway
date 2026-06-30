// Package openai adapts the gateway's unified request to the OpenAI Chat
// Completions API. It also serves GLM, which exposes an OpenAI compatible
// endpoint: only the base URL and credentials differ.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

const chatCompletionsPath = "/v1/chat/completions"

// Provider forwards requests to an OpenAI compatible Chat Completions endpoint.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// Option customizes a Provider.
type Option func(*Provider)

// WithHTTPClient overrides the default HTTP client (used in tests).
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.client = c } }

// New builds an OpenAI compatible provider. Use it for both openai and glm
// config types; pass the GLM base URL for GLM.
func New(name, baseURL, apiKey string, opts ...Option) *Provider {
	p := &Provider{
		name:    name,
		baseURL: baseURL,
		apiKey:  apiKey,
		// No client timeout: streaming responses can run long, and cancellation
		// is driven by the request context instead. The transport below bounds
		// only connection establishment and a fully hung response, never the
		// duration of an in-progress response body.
		client: &http.Client{Transport: defaultTransport()},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// defaultTransport returns a Transport with connection-level timeouts so a
// hung or slow upstream cannot pin connections indefinitely. There is
// deliberately no overall client Timeout: streaming bodies can legitimately
// run for minutes and must not be cut off mid-stream. DialContext and
// TLSHandshakeTimeout bound connection establishment, which is always safe to
// time out since no response data is in flight yet. ResponseHeaderTimeout is
// set generously to 120 seconds because a non-streaming completion may not
// send response headers until generation is well underway; a short value
// would wrongly kill long completions. It exists only to bound a connection
// that hangs forever without ever sending headers, not to bound normal
// generation latency.
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		ForceAttemptHTTP2:     true,
	}
}

// Name returns the configured provider name.
func (p *Provider) Name() string { return p.name }

// Shape reports that this provider speaks the OpenAI wire format.
func (p *Provider) Shape() provider.Shape { return provider.ShapeOpenAI }

// Complete forwards the request to the Chat Completions endpoint. For streaming
// requests it ensures stream_options.include_usage is set so the upstream
// reports token usage in the final chunk.
func (p *Provider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	body := req.Raw
	if req.Stream {
		body = ensureIncludeUsage(body)
	}

	url := p.baseURL + chatCompletionsPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: upstream request: %w", err)
	}
	return &provider.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Stream:     req.Stream,
	}, nil
}

// NewUsageScanner returns a scanner for OpenAI usage in streamed or single
// document form.
func (p *Provider) NewUsageScanner(stream bool) provider.UsageScanner {
	if stream {
		return newSSEUsageScanner()
	}
	return newJSONUsageScanner()
}

// ensureIncludeUsage sets stream_options.include_usage=true so a streamed
// response carries a final usage chunk. On any parse error it returns the body
// unchanged rather than risk corrupting it.
func ensureIncludeUsage(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	so, _ := m["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	m["stream_options"] = so
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

var _ provider.Provider = (*Provider)(nil)
