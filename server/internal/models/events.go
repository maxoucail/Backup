package models

import (
	"database/sql"
	"time"
)

func AddEvent(db *sql.DB, deviceID *string, level, message string) error {
	_, err := db.Exec(`INSERT INTO events (device_id, ts, level, message) VALUES (?, ?, ?, ?)`,
		deviceID, toDB(time.Now()), level, message)
	return err
}

func ListEventsForDevice(db *sql.DB, deviceID string, limit int) ([]Event, error) {
	rows, err := db.Query(`SELECT id, device_id, ts, level, message FROM events WHERE device_id = ? ORDER BY ts DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func ListRecentEvents(db *sql.DB, limit int) ([]Event, error) {
	rows, err := db.Query(`SELECT id, device_id, ts, level, message FROM events ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var e Event
		var ts string
		var deviceID sql.NullString
		if err := rows.Scan(&e.ID, &deviceID, &ts, &e.Level, &e.Message); err != nil {
			return nil, err
		}
		e.TS = fromDB(ts)
		if deviceID.Valid {
			e.DeviceID = &deviceID.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeOldEvents keeps the events table bounded: rows older than
// `olderThan` are deleted, and if the table still has more than `maxRows`
// rows afterwards, the oldest excess rows are trimmed too. This is what
// keeps the database from growing forever while still retaining a useful
// recent history per device.
func PurgeOldEvents(db *sql.DB, olderThan time.Duration, maxRows int) (int64, error) {
	cutoff := toDB(time.Now().Add(-olderThan))
	res, err := db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&total); err != nil {
		return deleted, err
	}
	if total > maxRows {
		excess := total - maxRows
		res2, err := db.Exec(`DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY ts ASC LIMIT ?)`, excess)
		if err != nil {
			return deleted, err
		}
		n2, _ := res2.RowsAffected()
		deleted += n2
	}
	return deleted, nil
}
