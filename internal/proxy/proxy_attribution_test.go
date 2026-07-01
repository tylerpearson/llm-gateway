package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/pricing"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

// capturingRecorder records the attribution rows enqueued by the handler.
// recordAttribution runs inline on the request goroutine, so a row is available
// as soon as the request returns.
type capturingRecorder struct {
	mu   sync.Mutex
	recs []attribution.Record
}

func (c *capturingRecorder) Record(r attribution.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
}

func (c *capturingRecorder) last(t *testing.T) attribution.Record {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recs) == 0 {
		t.Fatal("no attribution record was captured")
	}
	return c.recs[len(c.recs)-1]
}

// attribHandler builds a handler wired with a capturing recorder and the given
// spend tag headers, backed by an upstream that streams a fixed Anthropic
// response.
func attribHandler(t *testing.T, tagHeaders []string, opts ...Option) (*Handler, *capturingRecorder) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	t.Cleanup(upstream.Close)

	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &capturingRecorder{}

	all := []Option{WithAttribution(rec, pricing.DefaultTable()), WithSpendTags(tagHeaders)}
	all = append(all, opts...)
	h := New(reg, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log, all...)
	return h, rec
}

func postDims(h *Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	if path == "/v1/messages" {
		h.Messages(rr, req)
	} else {
		h.ChatCompletions(rr, req)
	}
	return rr
}

func TestAttribution_CapturesUserAgent(t *testing.T) {
	h, rec := attribHandler(t, nil)
	postDims(h, "/v1/messages", `{"model":"claude-haiku-4-5-20251001","stream":true}`,
		map[string]string{"User-Agent": "claude-cli/1.2.3"})

	if got := rec.last(t).UserAgent; got != "claude-cli/1.2.3" {
		t.Errorf("UserAgent = %q, want claude-cli/1.2.3", got)
	}
}

func TestAttribution_EndUserPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		headers map[string]string
		want    string
	}{
		{
			name:    "header wins over body",
			body:    `{"model":"claude-haiku-4-5-20251001","user":"body-user","metadata":{"user_id":"meta-user"}}`,
			headers: map[string]string{"x-llm-end-user": "header-user"},
			want:    "header-user",
		},
		{
			name: "body user when no header",
			body: `{"model":"claude-haiku-4-5-20251001","user":"body-user","metadata":{"user_id":"meta-user"}}`,
			want: "body-user",
		},
		{
			name: "metadata user_id when no header or user",
			body: `{"model":"claude-haiku-4-5-20251001","metadata":{"user_id":"meta-user"}}`,
			want: "meta-user",
		},
		{
			name: "none supplied",
			body: `{"model":"claude-haiku-4-5-20251001"}`,
			want: "",
		},
		{
			name: "oddly typed user does not panic",
			body: `{"model":"claude-haiku-4-5-20251001","user":42}`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, rec := attribHandler(t, nil)
			postDims(h, "/v1/messages", tc.body, tc.headers)
			if got := rec.last(t).EndUser; got != tc.want {
				t.Errorf("EndUser = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttribution_Tags(t *testing.T) {
	h, rec := attribHandler(t, []string{"x-cost-center"})
	postDims(h, "/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`, map[string]string{
		"x-llm-tags":    " b , a , a ,",
		"x-cost-center": "finance",
	})

	want := []string{"a", "b", "x-cost-center:finance"}
	if got := rec.last(t).Tags; !reflect.DeepEqual(got, want) {
		t.Errorf("Tags = %v, want %v", got, want)
	}
}

func TestAttribution_NoTagsIsNil(t *testing.T) {
	h, rec := attribHandler(t, []string{"x-cost-center"})
	postDims(h, "/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`, nil)

	if got := rec.last(t).Tags; got != nil {
		t.Errorf("Tags = %v, want nil", got)
	}
}

func TestAttribution_CacheHitCarriesDims(t *testing.T) {
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	h, rec := attribHandler(t, []string{"x-cost-center"}, WithCache(fc))

	headers := map[string]string{
		"User-Agent":    "claude-cli/9",
		"x-llm-tags":    "team-a",
		"x-cost-center": "eng",
	}
	body := `{"model":"claude-haiku-4-5-20251001","user":"cust-1","stream":true}`

	// First request populates the cache (a miss).
	if r := postDims(h, "/v1/messages", body, headers); r.Header().Get("x-llm-cache") != "miss" {
		t.Fatalf("first request cache = %q, want miss", r.Header().Get("x-llm-cache"))
	}
	// Second identical request is served from cache and must still record the
	// same attribution dimensions.
	if r := postDims(h, "/v1/messages", body, headers); r.Header().Get("x-llm-cache") != "hit" {
		t.Fatalf("second request cache = %q, want hit", r.Header().Get("x-llm-cache"))
	}

	got := rec.last(t)
	if !got.CacheHit {
		t.Fatalf("expected the last record to be a cache hit")
	}
	if got.UserAgent != "claude-cli/9" || got.EndUser != "cust-1" {
		t.Errorf("cache-hit dims = ua %q user %q, want claude-cli/9 / cust-1", got.UserAgent, got.EndUser)
	}
	if want := []string{"team-a", "x-cost-center:eng"}; !reflect.DeepEqual(got.Tags, want) {
		t.Errorf("cache-hit tags = %v, want %v", got.Tags, want)
	}
}

// Guard against an accidental regression where captureSpendDims blocks on the
// body reader; it must return promptly for a normal request.
func TestAttribution_CaptureIsPrompt(t *testing.T) {
	h, rec := attribHandler(t, nil)
	done := make(chan struct{})
	go func() {
		postDims(h, "/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete in time")
	}
	_ = rec.last(t)
}
