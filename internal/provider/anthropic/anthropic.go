// Package anthropic adapts the gateway's unified request to the Anthropic
// Messages API. In P1 the gateway speaks the Anthropic shape on both sides, so
// Complete forwards the client body verbatim and the adapter's job is to attach
// the gateway-held credentials and normalize usage from the response.
package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

const (
	// defaultVersion is sent as the anthropic-version header when the client
	// does not supply one.
	defaultVersion = "2023-06-01"
	messagesPath   = "/v1/messages"
)

// Provider forwards requests to the Anthropic Messages API.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	version string
	client  *http.Client
}

// Option customizes a Provider.
type Option func(*Provider)

// WithHTTPClient overrides the default HTTP client (used in tests).
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.client = c } }

// WithVersion overrides the anthropic-version header default.
func WithVersion(v string) Option { return func(p *Provider) { p.version = v } }

// New builds an Anthropic provider. baseURL is the API root (no trailing path),
// apiKey is the gateway-held provider credential.
func New(name, baseURL, apiKey string, opts ...Option) *Provider {
	p := &Provider{
		name:    name,
		baseURL: baseURL,
		apiKey:  apiKey,
		version: defaultVersion,
		// No client timeout: streaming responses can run long. Cancellation is
		// driven by the request context instead.
		client: &http.Client{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns the configured provider name.
func (p *Provider) Name() string { return p.name }

// Shape reports that this provider speaks the Anthropic wire format.
func (p *Provider) Shape() provider.Shape { return provider.ShapeAnthropic }

// Complete forwards the request body to Anthropic and returns the upstream
// response for the proxy to relay.
func (p *Provider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	url := p.baseURL + messagesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Raw))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", p.version)
	httpReq.Header.Set("Accept", acceptFor(req.Stream))
	// Pass through opt-in beta features the client requested.
	if beta := req.Header.Get("anthropic-beta"); beta != "" {
		httpReq.Header.Set("anthropic-beta", beta)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream request: %w", err)
	}

	return &provider.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Stream:     req.Stream,
	}, nil
}

// NewUsageScanner returns a scanner that extracts token usage from an Anthropic
// response in either streamed (SSE) or single-document (JSON) form.
func (p *Provider) NewUsageScanner(stream bool) provider.UsageScanner {
	if stream {
		return newSSEUsageScanner()
	}
	return newJSONUsageScanner()
}

func acceptFor(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

// compile time check that Provider satisfies the interface.
var _ provider.Provider = (*Provider)(nil)
