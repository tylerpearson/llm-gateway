// Package clickhouse is the ClickHouse sink for request attribution logs. It
// implements attribution.Sink and applies the request_logs schema from the
// embedded migration files.
package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tylerpearson/llm-gateway/internal/attribution"
)

// Store writes request attribution rows to ClickHouse.
type Store struct {
	conn driver.Conn
}

// Open connects to ClickHouse using a DSN (clickhouse://host:9000/db) and
// verifies the connection.
func Open(dsn string) (*Store, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse clickhouse dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	return &Store{conn: conn}, nil
}

// Close releases the connection.
func (s *Store) Close() error { return s.conn.Close() }

// InsertRequestLogs writes a batch of attribution records.
func (s *Store) InsertRequestLogs(ctx context.Context, recs []attribution.Record) error {
	if len(recs) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO request_logs")
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, r := range recs {
		// tags is an Array(String) column; a nil slice must be sent as an empty
		// slice so the driver writes an empty array rather than rejecting a nil.
		tags := r.Tags
		if tags == nil {
			tags = []string{}
		}
		// The trailing user_agent, end_user, and tags columns were appended by a
		// later migration, so they are bound last to match table column order.
		if err := batch.Append(
			r.Timestamp,
			r.RequestID,
			r.KeyID,
			r.TeamID,
			r.RequestedModel,
			r.ServedModel,
			r.Provider,
			uint32(r.InputTokens),
			uint32(r.OutputTokens),
			uint32(r.CacheReadTokens),
			uint32(r.CacheWriteTokens),
			r.CostUSD,
			uint32(r.LatencyMS),
			boolToUint8(r.CacheHit),
			uint16(r.Status),
			r.UserAgent,
			r.EndUser,
			tags,
		); err != nil {
			return fmt.Errorf("append row: %w", err)
		}
	}
	return batch.Send()
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

var _ attribution.Sink = (*Store)(nil)
