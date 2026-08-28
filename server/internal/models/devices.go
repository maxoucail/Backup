package models

import (
	"database/sql"
	"fmt"
	"time"

	"backup-server/internal/idgen"
)

func CreateDevice(db *sql.DB, name, hostname, osName, osVersion, agentVersion, secretHash, ip string) (*Device, error) {
	d := &Device{
		ID:           idgen.New(),
		Name:         name,
		Hostname:     hostname,
		OSName:       osName,
		OSVersion:    osVersion,
		AgentVersion: agentVersion,
		SecretHash:   secretHash,
		IPAddress:    ip,
		Status:       "offline",
		CreatedAt:    time.Now(),
	}
	_, err := db.Exec(
		`INSERT INTO devices (id, name, hostname, os_name, os_version, agent_version, secret_hash, ip_address, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'offline', ?)`,
		d.ID, d.Name, d.Hostname, d.OSName, d.OSVersion, d.AgentVersion, d.SecretHash, d.IPAddress, toDB(d.CreatedAt),
	)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func scanDevice(row interface {
	Scan(dest ...any) error
}) (*Device, error) {
	var d Device
	var created string
	var lastSeen sql.NullString
	var interval, retention sql.NullInt64
	if err := row.Scan(&d.ID, &d.Name, &d.Hostname, &d.OSName, &d.OSVersion, &d.AgentVersion,
		&d.SecretHash, &d.IPAddress, &d.Status, &lastSeen, &created, &interval, &retention, &d.BackupPaths); err != nil {
		return nil, err
	}
	d.CreatedAt = fromDB(created)
	if lastSeen.Valid {
		d.LastSeen = fromDBPtr(&lastSeen.String)
	}
	if interval.Valid {
		v := int(interval.Int64)
		d.IntervalMinutes = &v
	}
	if retention.Valid {
		v := int(retention.Int64)
		d.RetentionCount = &v
	}
	return &d, nil
}

const deviceCols = `id, name, hostname, os_name, os_version, agent_version, secret_hash, ip_address, status, last_seen, created_at, interval_minutes, retention_count, backup_paths`

func GetDevice(db *sql.DB, id string) (*Device, error) {
	row := db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE id = ?`, id)
	return scanDevice(row)
}

func ListDevices(db *sql.DB) ([]Device, error) {
	rows, err := db.Query(`SELECT ` + deviceCols + ` FROM devices ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func UpdateDeviceSeen(db *sql.DB, id, status, ip string) error {
	_, err := db.Exec(`UPDATE devices SET status = ?, ip_address = ?, last_seen = ? WHERE id = ?`,
		status, ip, toDB(time.Now()), id)
	return err
}

func SetDeviceStatus(db *sql.DB, id, status string) error {
	_, err := db.Exec(`UPDATE devices SET status = ? WHERE id = ?`, status, id)
	return err
}

// MarkStaleDevicesOffline flips any device whose agent hasn't checked in
// within `after` to offline. Called periodically by the scheduler so the
// dashboard reflects reality even if an agent disconnects uncleanly.
func MarkStaleDevicesOffline(db *sql.DB, after time.Duration) (int64, error) {
	cutoff := toDB(time.Now().Add(-after))
	res, err := db.Exec(`UPDATE devices SET status = 'offline' WHERE status = 'online' AND (last_seen IS NULL OR last_seen < ?)`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func RenameDevice(db *sql.DB, id, name string) error {
	_, err := db.Exec(`UPDATE devices SET name = ? WHERE id = ?`, name, id)
	return err
}

// devicePolicyColumns are the per-device policy fields the panel may
// update. Whitelisted rather than derived, since the caller passes column
// names.
var devicePolicyColumns = map[string]bool{
	"interval_minutes": true,
	"retention_count":  true,
	"backup_paths":     true,
}

// UpdateDevicePolicy writes only the policy fields the caller actually
// asked to change.
//
// Partial by design: the panel's PATCH sends one field at a time (a
// rename, say), and an update that rewrote every column would clear the
// operator's configured folder list every time they renamed a machine -
// silently sending that PC back to backing up the default folders.
func UpdateDevicePolicy(db *sql.DB, id string, set map[string]any) error {
	if len(set) == 0 {
		return nil
	}
	query := "UPDATE devices SET "
	args := make([]any, 0, len(set)+1)
	first := true
	for col, v := range set {
		if !devicePolicyColumns[col] {
			return fmt.Errorf("colonne de politique inconnue: %s", col)
		}
		if !first {
			query += ", "
		}
		query += col + " = ?"
		args = append(args, v)
		first = false
	}
	query += " WHERE id = ?"
	args = append(args, id)
	_, err := db.Exec(query, args...)
	return err
}

func DeleteDevice(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	return err
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
