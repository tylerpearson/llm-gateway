// Package migrations embeds the SQL migration files so they ship inside the
// gateway and gatewayctl binaries and can be applied with golang-migrate at
// runtime without shipping the .sql files separately.
package migrations

import "embed"

// MySQL holds the MySQL migration files under the mysql/ subdirectory.
//
//go:embed mysql/*.sql
var MySQL embed.FS
