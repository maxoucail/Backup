package models

import (
	"crypto/subtle"
	"database/sql"
	"time"

	"backup-server/internal/idgen"
)

func CreateEnrollmentKey(db *sql.DB, tokenHash, label string, ttl time.Duration) (*EnrollmentKey, error) {
	k := &EnrollmentKey{
		ID:        idgen.New(),
		TokenHash: tokenHash,
		Label:     label,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}
	_, err := db.Exec(
		`INSERT INTO enrollment_keys (id, token_hash, label, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		k.ID, k.TokenHash, k.Label, toDB(k.CreatedAt), toDB(k.ExpiresAt),
	)
	if err != nil {
		return nil, err
	}
	return k, nil
}

// FindUsableEnrollmentKeyByHash returns the key row if it exists, hasn't
// expired and hasn't been used yet. Errors of type sql.ErrNoRows mean "no
// such key" to the caller.
func FindUsableEnrollmentKeyByHash(db *sql.DB, tokenHash string) (*EnrollmentKey, error) {
	row := db.QueryRow(`SELECT id, token_hash, label, created_at, expires_at, used_at, used_by_device_id
		FROM enrollment_keys WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		tokenHash, toDB(time.Now()))
	var k EnrollmentKey
	var created, expires string
	var usedAt, usedBy sql.NullString
	if err := row.Scan(&k.ID, &k.TokenHash, &k.Label, &created, &expires, &usedAt, &usedBy); err != nil {
		return nil, err
	}
	k.CreatedAt = fromDB(created)
	k.ExpiresAt = fromDB(expires)
	if usedAt.Valid {
		k.UsedAt = fromDBPtr(&usedAt.String)
	}
	if usedBy.Valid {
		k.UsedByDeviceID = &usedBy.String
	}
	return &k, nil
}

func MarkEnrollmentKeyUsed(db *sql.DB, id, deviceID string) error {
	_, err := db.Exec(`UPDATE enrollment_keys SET used_at = ?, used_by_device_id = ? WHERE id = ?`,
		toDB(time.Now()), deviceID, id)
	return err
}

func ListEnrollmentKeys(db *sql.DB) ([]EnrollmentKey, error) {
	rows, err := db.Query(`SELECT id, label, created_at, expires_at, used_at, used_by_device_id
		FROM enrollment_keys ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnrollmentKey
	for rows.Next() {
		var k EnrollmentKey
		var created, expires string
		var usedAt, usedBy sql.NullString
		if err := rows.Scan(&k.ID, &k.Label, &created, &expires, &usedAt, &usedBy); err != nil {
			return nil, err
		}
		k.CreatedAt = fromDB(created)
		k.ExpiresAt = fromDB(expires)
		if usedAt.Valid {
			k.UsedAt = fromDBPtr(&usedAt.String)
		}
		if usedBy.Valid {
			k.UsedByDeviceID = &usedBy.String
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PurgeExpiredEnrollmentKeys removes unused keys whose expiry has passed,
// keeping the table small.
func PurgeExpiredEnrollmentKeys(db *sql.DB) (int64, error) {
	res, err := db.Exec(`DELETE FROM enrollment_keys WHERE used_at IS NULL AND expires_at < ?`, toDB(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetOrCreateStaticEnrollmentToken returns the server's permanent
// enrollment key, generating one on first use. Unlike a one-time
// enrollment_keys row, this key never expires and enrolling a device with
// it doesn't consume it - it's meant to sit on the Paramètres page and be
// reused for every future device, so an operator doesn't have to generate
// and copy a fresh one-time key for each machine. The existing one-time
// flow (handleCreateEnrollmentKey) is untouched and keeps working exactly
// as before for anyone who prefers a key that expires after use.
func GetOrCreateStaticEnrollmentToken(db *sql.DB) (string, error) {
	var token string
	if err := db.QueryRow(`SELECT static_enrollment_token FROM settings WHERE id = 1`).Scan(&token); err != nil {
		return "", err
	}
	if token != "" {
		return token, nil
	}
	return RegenerateStaticEnrollmentToken(db)
}

// RegenerateStaticEnrollmentToken replaces the permanent enrollment key
// with a new random one, immediately invalidating the old one (any device
// mid-enrollment with the old value will need the new one).
func RegenerateStaticEnrollmentToken(db *sql.DB) (string, error) {
	token := idgen.Token(24)
	if _, err := db.Exec(`UPDATE settings SET static_enrollment_token = ? WHERE id = 1`, token); err != nil {
		return "", err
	}
	return token, nil
}

// MatchesStaticEnrollmentToken reports whether token is the current
// permanent enrollment key. Constant-time so a mistyped guess can't be
// distinguished from a near-miss by timing, same care as a password check.
func MatchesStaticEnrollmentToken(db *sql.DB, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var stored string
	if err := db.QueryRow(`SELECT static_enrollment_token FROM settings WHERE id = 1`).Scan(&stored); err != nil {
		return false, err
	}
	if stored == "" {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(token)) == 1, nil
}
