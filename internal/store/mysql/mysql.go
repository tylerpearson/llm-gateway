// Package mysql is the MySQL implementation of the gateway config-plane store.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// Register the MySQL driver with database/sql.
	_ "github.com/go-sql-driver/mysql"

	"github.com/tylerpearson/llm-gateway/internal/store"
)

// Store is a MySQL backed store.Store.
type Store struct {
	db *sql.DB
}

// Open connects to MySQL and verifies the connection. The DSN is the
// go-sql-driver form (user:pass@tcp(host:port)/db); parseTime=true is added if
// absent so TIMESTAMP columns scan into time.Time.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", ensureParam(dsn, "parseTime", "true"))
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// CreateTeam inserts a team with a generated id.
func (s *Store) CreateTeam(ctx context.Context, name string) (*store.Team, error) {
	t := &store.Team{ID: store.NewID(), Name: name, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO teams (id, name, created_at) VALUES (?, ?, ?)",
		t.ID, t.Name, t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create team: %w", err)
	}
	return t, nil
}

// ListTeams returns all teams ordered by creation time.
func (s *Store) ListTeams(ctx context.Context) ([]store.Team, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, created_at FROM teams ORDER BY created_at")
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var teams []store.Team
	for rows.Next() {
		var t store.Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan team: %w", err)
		}
		teams = append(teams, t)
	}
	return teams, rows.Err()
}

// CreateKey inserts a virtual key. keyHash is the sha256 hex of the plaintext.
func (s *Store) CreateKey(ctx context.Context, teamID, name, keyHash, defaultAlias string) (*store.VirtualKey, error) {
	vk := &store.VirtualKey{
		ID:           store.NewID(),
		TeamID:       teamID,
		Name:         name,
		KeyHash:      keyHash,
		DefaultAlias: defaultAlias,
		CreatedAt:    time.Now().UTC(),
	}
	var alias any
	if defaultAlias != "" {
		alias = defaultAlias
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO virtual_keys (id, team_id, name, key_hash, default_alias, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		vk.ID, vk.TeamID, vk.Name, vk.KeyHash, alias, vk.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}
	return vk, nil
}

const keyColumns = "id, team_id, name, key_hash, COALESCE(default_alias, ''), disabled, created_at"

func scanKey(row interface{ Scan(...any) error }) (*store.VirtualKey, error) {
	var vk store.VirtualKey
	var disabled int
	if err := row.Scan(&vk.ID, &vk.TeamID, &vk.Name, &vk.KeyHash, &vk.DefaultAlias, &disabled, &vk.CreatedAt); err != nil {
		return nil, err
	}
	vk.Disabled = disabled != 0
	return &vk, nil
}

// LookupKeyByHash returns the key with the given hash or store.ErrNotFound.
func (s *Store) LookupKeyByHash(ctx context.Context, keyHash string) (*store.VirtualKey, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+keyColumns+" FROM virtual_keys WHERE key_hash = ?", keyHash)
	vk, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup key: %w", err)
	}
	return vk, nil
}

// ListKeys returns the keys for a team.
func (s *Store) ListKeys(ctx context.Context, teamID string) ([]store.VirtualKey, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+keyColumns+" FROM virtual_keys WHERE team_id = ? ORDER BY created_at", teamID)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []store.VirtualKey
	for rows.Next() {
		vk, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan key: %w", err)
		}
		keys = append(keys, *vk)
	}
	return keys, rows.Err()
}

// DisableKey marks a key disabled.
func (s *Store) DisableKey(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE virtual_keys SET disabled = 1 WHERE id = ?", keyID)
	if err != nil {
		return fmt.Errorf("disable key: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// RecordAudit appends an audit entry.
func (s *Store) RecordAudit(ctx context.Context, actor, action, target, details string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO audit_log (id, actor, action, target, details, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		store.NewID(), actor, action, target, nullable(details), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// ListAudit returns the most recent audit entries, newest first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]store.AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, actor, action, target, COALESCE(details, ''), created_at FROM audit_log ORDER BY created_at DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []store.AuditEntry
	for rows.Next() {
		var e store.AuditEntry
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Details, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ensureParam appends key=value to a go-sql-driver DSN query string if the key
// is not already present.
func ensureParam(dsn, key, value string) string {
	if strings.Contains(dsn, key+"=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + key + "=" + value
}

var _ store.Store = (*Store)(nil)
