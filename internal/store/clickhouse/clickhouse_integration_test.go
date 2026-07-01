//go:build integration

// Needs a real ClickHouse instance. Runs only under the integration build tag
// and skips unless CLICKHOUSE_DSN points at a reachable server.
package clickhouse

import (
	"context"
	"math"
	"os"
	"reflect"
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
		UserAgent:      "claude-cli/1.0",
		EndUser:        "cust-7",
		Tags:           []string{"team-a", "x-cost-center:eng"},
	}
	if err := s.InsertRequestLogs(ctx, []attribution.Record{rec}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var count uint64
	var sumCost float64
	var userAgent, endUser string
	var tags []string
	row := s.conn.QueryRow(ctx,
		"SELECT count(), sum(cost_usd), any(user_agent), any(end_user), any(tags) FROM request_logs WHERE team_id = ?", teamID)
	if err := row.Scan(&count, &sumCost, &userAgent, &endUser, &tags); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if math.Abs(sumCost-rec.CostUSD) > 1e-9 {
		t.Errorf("sum cost = %v, want %v", sumCost, rec.CostUSD)
	}
	if userAgent != rec.UserAgent || endUser != rec.EndUser {
		t.Errorf("dims = ua %q user %q, want %q / %q", userAgent, endUser, rec.UserAgent, rec.EndUser)
	}
	if !reflect.DeepEqual(tags, rec.Tags) {
		t.Errorf("tags = %v, want %v", tags, rec.Tags)
	}
}

// TestEvalTablesExist confirms the v2 eval seam schema is applied by Migrate.
// The tables are present but unused in v1.
func TestEvalTablesExist(t *testing.T) {
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
	for _, table := range []string{"eval_runs", "eval_results"} {
		var n uint64
		if err := s.conn.QueryRow(ctx, "SELECT count() FROM "+table).Scan(&n); err != nil {
			t.Errorf("query %s: %v", table, err)
		}
	}
}
