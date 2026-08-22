package models

import (
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
