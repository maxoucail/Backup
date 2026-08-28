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

const snapshotCols = `id, device_id, kind, status, started_at, finished_at, file_count, logical_bytes, uploaded_bytes, progress_percent, error_message`

func scanSnapshot(row interface{ Scan(dest ...any) error }) (*Snapshot, error) {
	var s Snapshot
	var started string
	var finished sql.NullString
	if err := row.Scan(&s.ID, &s.DeviceID, &s.Kind, &s.Status, &started, &finished,
		&s.FileCount, &s.LogicalBytes, &s.UploadedBytes, &s.ProgressPercent, &s.ErrorMessage); err != nil {
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

// CountSuccessfulSnapshotsForDevice is how many successful backups a
// device has behind it, shown on the devices list and dashboard.
//
// A plain COUNT rather than fetching every row and taking len() of the
// slice: those handlers call this once per device on every load, and
// pulling every column of every historical snapshot just to discard it
// and keep a number was real, needless work on any device with more than
// a handful of runs behind it.
func CountSuccessfulSnapshotsForDevice(db *sql.DB, deviceID string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE device_id = ? AND status = 'success'`, deviceID).Scan(&n)
	return n, err
}

func UpdateSnapshotProgress(db *sql.DB, id string, fileCount int, logicalBytes, uploadedBytes int64, percent float64) error {
	_, err := db.Exec(`UPDATE snapshots SET file_count=?, logical_bytes=?, uploaded_bytes=?, progress_percent=? WHERE id=?`,
		fileCount, logicalBytes, uploadedBytes, percent, id)
	return err
}

func FinishSnapshot(db *sql.DB, id, status, errMsg string) error {
	_, err := db.Exec(`UPDATE snapshots SET status=?, finished_at=?, error_message=?, progress_percent=100 WHERE id=?`,
		status, toDB(time.Now()), errMsg, id)
	return err
}

// LastSuccessfulSnapshotStart returns when this device's most recent
// successful backup began, or the zero time if it has never had one.
//
// Used as the cutoff for "this file may have changed after we read it"
// (see filestore.NeededFiles). The *start* is deliberate, not the finish:
// a file could have been modified at any point between the backup starting
// and that file being read, so anything from the start onwards is suspect.
func LastSuccessfulSnapshotStart(db *sql.DB, deviceID string) (time.Time, error) {
	var started string
	err := db.QueryRow(
		`SELECT started_at FROM snapshots WHERE device_id = ? AND status = 'success'
		 ORDER BY started_at DESC LIMIT 1`, deviceID).Scan(&started)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return fromDB(started), nil
}

func DeleteSnapshot(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM snapshots WHERE id = ?`, id)
	return err
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
