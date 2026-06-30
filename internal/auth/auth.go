// Package auth handles virtual key credentials: generating them, hashing them
// for storage, and the HTTP middleware that authenticates inbound requests
// against the store. Plaintext keys and their hashes are never logged.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// KeyPrefix is the human readable prefix on every generated virtual key. It
// makes keys greppable and recognizable without revealing the secret.
const KeyPrefix = "llmgw_"

// GenerateKey returns a new random virtual key and its storage hash. The
// plaintext is shown to the operator once and never persisted; only the hash is
// stored.
func GenerateKey() (plaintext, hash string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("auth: cannot read random bytes for key: " + err.Error())
	}
	plaintext = KeyPrefix + hex.EncodeToString(b[:])
	return plaintext, HashKey(plaintext)
}

// HashKey returns the sha256 hex digest used to store and look up a key.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Principal is the authenticated identity attached to a request context.
type Principal struct {
	KeyID        string
	KeyName      string
	TeamID       string
	DefaultAlias string
}

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the authenticated principal, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok
}
