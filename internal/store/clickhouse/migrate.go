package clickhouse

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tylerpearson/llm-gateway/migrations"
)

// Migrate applies the embedded ClickHouse up migrations. The DDL uses CREATE
// TABLE IF NOT EXISTS, so applying it repeatedly is a no-op. Statements are
// executed in filename order.
func Migrate(dsn string) error {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse clickhouse dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return fmt.Errorf("open clickhouse: %w", err)
	}
	defer func() { _ = conn.Close() }()

	entries, err := fs.ReadDir(migrations.ClickHouse, "clickhouse")
	if err != nil {
		return fmt.Errorf("read clickhouse migrations: %w", err)
	}
	var ups []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	sort.Strings(ups)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range ups {
		data, err := fs.ReadFile(migrations.ClickHouse, "clickhouse/"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		stmt := strings.TrimRight(strings.TrimSpace(string(data)), ";")
		if stmt == "" {
			continue
		}
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}
