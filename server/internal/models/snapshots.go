package models

import (
	"database/sql"
	"time"

	"backup-server/internal/idgen"
)

func CreateSnapshot(db *sql.DB, deviceID, kind string) (*Snapshot, error) {
	s := &Snapshot{
		ID:        idgen.New(),
		DeviceID:  deviceID,
		Kind:      kind,
		Status:    SnapshotStatusRunning,
		StartedAt: time.Now(),
	}
	_, err := db.Exec(
		`INSERT INTO snapshots (id, device_id, kind, status, started_at) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.DeviceID, s.Kind, s.Status, toDB(s.StartedAt),
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

const snapshotCols = `id, device_id, kind, status, started_at, finished_at, file_count, logical_bytes, uploaded_bytes, progress_percent, error_message, manifest_path`

func scanSnapshot(row interface{ Scan(dest ...any) error }) (*Snapshot, error) {
	var s Snapshot
	var started string
	var finished sql.NullString
	if err := row.Scan(&s.ID, &s.DeviceID, &s.Kind, &s.Status, &started, &finished,
		&s.FileCount, &s.LogicalBytes, &s.UploadedBytes, &s.ProgressPercent, &s.ErrorMessage, &s.ManifestPath); err != nil {
		return nil, err
	}
	s.StartedAt = fromDB(started)
	if finished.Valid {
		s.FinishedAt = fromDBPtr(&finished.String)
	}
	return &s, nil
}

func GetSnapshot(db *sql.DB, id string) (*Snapshot, error) {
	row := db.QueryRow(`SELECT `+snapshotCols+` FROM snapshots WHERE id = ?`, id)
	return scanSnapshot(row)
}

func ListSnapshotsForDevice(db *sql.DB, deviceID string, limit int) ([]Snapshot, error) {
	rows, err := db.Query(`SELECT `+snapshotCols+` FROM snapshots WHERE device_id = ? ORDER BY started_at DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListSuccessfulSnapshotsForDevice returns completed snapshots oldest-first,
// used by the retention rotator to decide what to delete.
func ListSuccessfulSnapshotsForDevice(db *sql.DB, deviceID string) ([]Snapshot, error) {
	rows, err := db.Query(`SELECT `+snapshotCols+` FROM snapshots WHERE device_id = ? AND status = 'success' ORDER BY started_at ASC`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func UpdateSnapshotProgress(db *sql.DB, id string, fileCount int, logicalBytes, uploadedBytes int64, percent float64) error {
	_, err := db.Exec(`UPDATE snapshots SET file_count=?, logical_bytes=?, uploaded_bytes=?, progress_percent=? WHERE id=?`,
		fileCount, logicalBytes, uploadedBytes, percent, id)
	return err
}

func FinishSnapshot(db *sql.DB, id, status, errMsg, manifestPath string) error {
	_, err := db.Exec(`UPDATE snapshots SET status=?, finished_at=?, error_message=?, manifest_path=?, progress_percent=100 WHERE id=?`,
		status, toDB(time.Now()), errMsg, manifestPath, id)
	return err
}

func DeleteSnapshot(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM snapshots WHERE id = ?`, id)
	return err
}

// AllManifestPaths returns manifest paths for every snapshot that still
// exists (any status) across all devices - used by the chunk garbage
// collector to know what's still referenced.
func AllManifestPaths(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT manifest_path FROM snapshots WHERE manifest_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// StorageUsedByDevice sums logical_bytes of the latest successful snapshot
// per device (rough "GB used" figure for the dashboard).
func LatestSnapshotForDevice(db *sql.DB, deviceID string) (*Snapshot, error) {
	row := db.QueryRow(`SELECT `+snapshotCols+` FROM snapshots WHERE device_id = ? ORDER BY started_at DESC LIMIT 1`, deviceID)
	s, err := scanSnapshot(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}
