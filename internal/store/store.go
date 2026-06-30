// Package store defines the gateway's config-plane persistence: teams and the
// virtual keys clients authenticate with. The concrete MySQL implementation
// lives in the mysql subpackage; this package holds the domain types and the
// Store interface so middleware and tooling depend on the interface, not the
// driver.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// ErrNotFound is returned by lookups when no matching row exists.
var ErrNotFound = errors.New("store: not found")

// Team groups virtual keys for attribution and budgeting.
type Team struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// VirtualKey is a per team or per developer credential. The plaintext key is
// never stored; only KeyHash (sha256 hex) is persisted, and it is never logged.
type VirtualKey struct {
	ID           string
	TeamID       string
	Name         string
	KeyHash      string
	DefaultAlias string
	Disabled     bool
	CreatedAt    time.Time
}

// AuditEntry is one recorded administrative action.
type AuditEntry struct {
	ID        string
	Actor     string
	Action    string
	Target    string
	Details   string
	CreatedAt time.Time
}

// Store is the config-plane persistence interface.
type Store interface {
	// CreateTeam inserts a team and returns it.
	CreateTeam(ctx context.Context, name string) (*Team, error)
	// ListTeams returns all teams ordered by creation time.
	ListTeams(ctx context.Context) ([]Team, error)
	// CreateKey inserts a virtual key. keyHash is the sha256 hex of the
	// plaintext key; the caller holds the plaintext and shows it once.
	CreateKey(ctx context.Context, teamID, name, keyHash, defaultAlias string) (*VirtualKey, error)
	// LookupKeyByHash returns the key with the given hash or ErrNotFound.
	LookupKeyByHash(ctx context.Context, keyHash string) (*VirtualKey, error)
	// ListKeys returns the keys for a team (hashes included, never plaintext).
	ListKeys(ctx context.Context, teamID string) ([]VirtualKey, error)
	// DisableKey marks a key disabled so it can no longer authenticate.
	DisableKey(ctx context.Context, keyID string) error
	// RecordAudit appends an audit log entry for an administrative change.
	RecordAudit(ctx context.Context, actor, action, target, details string) error
	// ListAudit returns the most recent audit entries, newest first.
	ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)
	// Close releases the underlying connection pool.
	Close() error
}

// NewID returns a random 128 bit identifier as a 32 character hex string. It is
// used for team and key primary keys to avoid a UUID dependency.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable and must never be silently
		// turned into a weak or empty id.
		panic("store: cannot read random bytes for id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
