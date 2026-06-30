package mysql

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	// Register the MySQL migration database driver.
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/tylerpearson/llm-gateway/migrations"
)

// Migrate applies all pending MySQL migrations from the embedded files. It is
// idempotent: running it when the schema is current is a no-op. The DSN is the
// go-sql-driver form; multiStatements=true is added so multi statement
// migration files apply in one exec.
func Migrate(dsn string) error {
	src, err := iofs.New(migrations.MySQL, "mysql")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src,
		"mysql://"+ensureParam(dsn, "multiStatements", "true"))
	if err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
