// Package provider defines the gateway's unified upstream interface and the
// shared request/response/usage types. Each concrete provider (anthropic,
// openai, glm) lives in its own subpackage and adapts the unified request to
// that provider's wire format while normalizing token usage on the way back.
//
// In P1 only the anthropic adapter exists and requests flow through in the
// native Anthropic Messages shape. Cross-shape translation is added in P4.
package provider

import (
	"context"
	"io"
	"net/http"
)

// Request is the gateway's normalized view of an inbound completion request.
// Raw carries the original client JSON body so a same-shape provider can
// forward it verbatim; the typed fields let the router and adapters make
// decisions without reparsing the whole payload.
type Request struct {
	// Model is the model named by the client (may be a concrete model or an
	// alias the router has already resolved).
	Model string
	// Stream reports whether the client asked for a streamed (SSE) response.
	Stream bool
	// Raw is the original request body, forwarded as is for same-shape routing.
	Raw []byte
	// Header carries selected inbound headers an adapter may need to pass
	// through (for example anthropic-beta). It never carries client auth.
	Header http.Header
}

// Usage is normalized token accounting captured from an upstream response.
// Cache fields are Anthropic specific and stay zero for providers that do not
// report them.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Response wraps the upstream HTTP response. Body is streamed through to the
// client while the proxy tees it to capture Usage. The caller must close Body.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	// Stream reports whether Body is an SSE event stream (true) or a single
	// JSON document (false).
	Stream bool
}

// UsageScanner accumulates token usage from upstream response bytes as they are
// relayed to the client. The proxy tees the response stream into it and reads
// Usage once the stream is fully drained. Implementations are provider specific
// because each provider reports usage in its own wire shape.
type UsageScanner interface {
	io.Writer
	// Usage returns the accounting captured so far. It is meaningful once the
	// full response body has been written.
	Usage() Usage
}

// Provider is the single interface every upstream adapter implements.
type Provider interface {
	// Name is the configured provider name (the key under providers: in config).
	Name() string
	// Complete forwards req to the upstream and returns the response. The proxy
	// is responsible for relaying Body to the client and capturing usage.
	Complete(ctx context.Context, req *Request) (*Response, error)
	// NewUsageScanner returns a scanner for the given response mode (streamed
	// SSE when stream is true, otherwise a single JSON document).
	NewUsageScanner(stream bool) UsageScanner
}

// Registry is a set of constructed providers keyed by configured name.
type Registry map[string]Provider

// Get returns the named provider and whether it was found.
func (r Registry) Get(name string) (Provider, bool) {
	p, ok := r[name]
	return p, ok
}
