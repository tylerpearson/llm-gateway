// Package migrations embeds the SQL migration files so they ship inside the
// gateway and gatewayctl binaries and can be applied with golang-migrate at
// runtime without shipping the .sql files separately.
package migrations

import "embed"

// MySQL holds the MySQL migration files under the mysql/ subdirectory.
//
//go:embed mysql/*.sql
var MySQL embed.FS

// ClickHouse holds the ClickHouse migration files under the clickhouse/
// subdirectory. ClickHouse DDL is applied directly (CREATE TABLE IF NOT EXISTS
// is idempotent) rather than through golang-migrate.
//
//go:embed clickhouse/*.sql
var ClickHouse embed.FS

