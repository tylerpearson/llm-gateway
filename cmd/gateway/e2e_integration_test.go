//go:build integration

// Full-stack end-to-end test: boots the real gateway binary entrypoint (run)
// against real MySQL, ClickHouse, and Redis, and exercises auth, caching,
// streaming, and attribution through the actual HTTP wiring. Runs only under
// the integration build tag and skips unless MYSQL_DSN, CLICKHOUSE_DSN, and
// REDIS_ADDR are all set, matching the convention in
// internal/cache/cache_integration_test.go.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/store/clickhouse"
	"github.com/tylerpearson/llm-gateway/internal/store/mysql"
)

// anthropicSSE is the mock upstream body: a minimal Anthropic Messages stream
// reporting 10 input tokens and 25 output tokens, matching the fixture shape
// used by internal/proxy/proxy_test.go.
const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}

`

// TestE2EFullStack drives one request through the assembled gateway: virtual
// key auth against real MySQL, a streaming relay to a mock Anthropic-shaped
// upstream, the Redis response cache, and cost attribution flushed to real
// ClickHouse on graceful shutdown.
func TestE2EFullStack(t *testing.T) {
	mysqlDSN := os.Getenv("MYSQL_DSN")
	clickhouseDSN := os.Getenv("CLICKHOUSE_DSN")
	redisAddr := os.Getenv("REDIS_ADDR")
	if mysqlDSN == "" || clickhouseDSN == "" || redisAddr == "" {
		t.Skip("MYSQL_DSN, CLICKHOUSE_DSN, and REDIS_ADDR must all be set; skipping full-stack e2e test")
	}

	ctx := context.Background()

	// Apply schema. Both Migrate helpers are idempotent, so reruns are safe.
	if err := mysql.Migrate(mysqlDSN); err != nil {
		t.Fatalf("mysql migrate: %v", err)
	}
	if err := clickhouse.Migrate(clickhouseDSN); err != nil {
		t.Fatalf("clickhouse migrate: %v", err)
	}

	// Seed a team and virtual key. The team name carries a nanosecond
	// timestamp so reruns against a persistent CI database do not collide.
	st, err := mysql.Open(mysqlDSN)
	if err != nil {
		t.Fatalf("open mysql store: %v", err)
	}
	defer func() { _ = st.Close() }()

	teamName := fmt.Sprintf("e2e-team-%d", time.Now().UnixNano())
	team, err := st.CreateTeam(ctx, teamName)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	plaintextKey := randomHex(t, 32)
	keyHash := auth.HashKey(plaintextKey)
	vk, err := st.CreateKey(ctx, team.ID, "e2e-key", keyHash, "default")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Mock upstream: always returns the same short SSE stream regardless of
	// request body, reporting 10 input and 25 output tokens.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()
	t.Setenv("E2E_MOCK_KEY", "test-key")

	port := freePort(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf(`server:
  addr: "127.0.0.1:%s"
  shutdown_timeout: 5s
logging:
  level: error
  format: json
providers:
  mock:
    type: anthropic
    base_url: %s
    api_key_env: E2E_MOCK_KEY
routing:
  default_alias: default
  aliases:
    default:
      provider: mock
      model: mock-model
storage:
  mysql_dsn_env: MYSQL_DSN
  clickhouse_dsn_env: CLICKHOUSE_DSN
  redis_addr_env: REDIS_ADDR
security:
  redact_prompts: true
  auth_cache_ttl: 5s
`, port, upstream.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- run(cfgPath) }()

	baseURL := "http://127.0.0.1:" + port
	if !waitForReady(baseURL+"/readyz", 5*time.Second) {
		t.Fatal("gateway did not become ready in time")
	}

	reqBody := `{"model":"default","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"e2e"}]}`

	// First request: cache miss, streamed relay from the mock upstream.
	missResp := doMessagesRequest(t, baseURL, plaintextKey, reqBody)
	if missResp.status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body: %s", missResp.status, missResp.body)
	}
	if !strings.Contains(missResp.body, "message_stop") {
		t.Fatalf("first response body missing message_stop: %s", missResp.body)
	}
	if got := missResp.headers.Get("x-llm-cache"); got != "miss" {
		t.Errorf("first request x-llm-cache = %q, want %q", got, "miss")
	}

	// Second, identical request: should be served from the Redis cache.
	hitResp := doMessagesRequest(t, baseURL, plaintextKey, reqBody)
	if hitResp.status != http.StatusOK {
		t.Fatalf("second request status = %d, want 200; body: %s", hitResp.status, hitResp.body)
	}
	if got := hitResp.headers.Get("x-llm-cache"); got != "hit" {
		t.Errorf("second request x-llm-cache = %q, want %q", got, "hit")
	}

	// Wrong key: rejected before routing or caching.
	badResp := doMessagesRequest(t, baseURL, "not-the-right-key", reqBody)
	if badResp.status != http.StatusUnauthorized {
		t.Fatalf("wrong-key request status = %d, want 401; body: %s", badResp.status, badResp.body)
	}

	// Graceful shutdown flushes the attribution writer.
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

	// Verify the attribution rows landed in ClickHouse: one miss, one hit,
	// and the miss row carries the token counts from the SSE fixture.
	assertRequestLogs(t, clickhouseDSN, vk.ID)
}

type messagesResponse struct {
	status  int
	body    string
	headers http.Header
}

func doMessagesRequest(t *testing.T, baseURL, apiKey, body string) messagesResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return messagesResponse{status: resp.StatusCode, body: string(b), headers: resp.Header}
}

// assertRequestLogs opens ClickHouse directly (the store package only
// exposes Close and InsertRequestLogs, not an ad hoc query) and checks that
// this run produced a miss row and a hit row for keyID, with the miss row
// carrying the fixture's token counts.
func assertRequestLogs(t *testing.T, clickhouseDSN, keyID string) {
	t.Helper()
	opts, err := chdriver.ParseDSN(clickhouseDSN)
	if err != nil {
		t.Fatalf("parse clickhouse dsn: %v", err)
	}
	conn, err := chdriver.Open(opts)
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var total uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM request_logs WHERE key_id = ?", keyID,
	).Scan(&total); err != nil {
		t.Fatalf("count request_logs: %v", err)
	}
	if total < 2 {
		t.Fatalf("request_logs rows for key = %d, want at least 2 (one miss, one hit)", total)
	}

	var missInput, missOutput uint32
	if err := conn.QueryRow(ctx,
		"SELECT input_tokens, output_tokens FROM request_logs WHERE key_id = ? AND cache_hit = 0 ORDER BY ts ASC LIMIT 1",
		keyID,
	).Scan(&missInput, &missOutput); err != nil {
		t.Fatalf("query miss row: %v", err)
	}
	if missInput != 10 {
		t.Errorf("miss row input_tokens = %d, want 10", missInput)
	}
	if missOutput != 25 {
		t.Errorf("miss row output_tokens = %d, want 25", missOutput)
	}

	var hitCount uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM request_logs WHERE key_id = ? AND cache_hit = 1", keyID,
	).Scan(&hitCount); err != nil {
		t.Fatalf("count hit rows: %v", err)
	}
	if hitCount < 1 {
		t.Fatalf("no cache-hit rows found for key %s", keyID)
	}
}

// randomHex returns a random hex string of the given byte length, used as an
// e2e plaintext virtual key. It is never a real credential.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(b)
}
