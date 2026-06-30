//go:build integration

// Needs a real ClickHouse instance. Runs only under the integration build tag
// and skips unless CLICKHOUSE_DSN points at a reachable server.
package clickhouse

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
	"github.com/tylerpearson/llm-gateway/internal/store"
)

func TestClickHouseInsertAndRead(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_DSN not set; skipping ClickHouse integration test")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	teamID := "team-" + store.NewID()
	rec := attribution.Record{
		Timestamp:      time.Now().UTC(),
		RequestID:      store.NewID(),
		KeyID:          "k1",
		TeamID:         teamID,
		RequestedModel: "claude-haiku-4-5-20251001",
		ServedModel:    "claude-haiku-4-5-20251001",
		Provider:       "anthropic",
		InputTokens:    10,
		OutputTokens:   25,
		CostUSD:        0.000135,
		LatencyMS:      42,
		Status:         200,
	}
	if err := s.InsertRequestLogs(ctx, []attribution.Record{rec}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var count uint64
	var sumCost float64
	row := s.conn.QueryRow(ctx,
		"SELECT count(), sum(cost_usd) FROM request_logs WHERE team_id = ?", teamID)
	if err := row.Scan(&count, &sumCost); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if math.Abs(sumCost-rec.CostUSD) > 1e-9 {
		t.Errorf("sum cost = %v, want %v", sumCost, rec.CostUSD)
	}
}
