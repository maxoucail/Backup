// Package idgen generates the random IDs and tokens used across the
// server: primary keys, enrollment keys, device secrets.
package idgen

import (
	"crypto/rand"
	"encoding/base64"

	"github.com/google/uuid"
)

// New returns a new random UUID string, used as primary key for users,
// devices, snapshots and enrollment keys.
func New() string {
	return uuid.NewString()
}

// Token returns a high-entropy URL-safe random token, used for enrollment
// keys and device secrets (never stored in plaintext, only their hash).
func Token(nbytes int) string {
	buf := make([]byte, nbytes)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}
