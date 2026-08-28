package models

import (
	"database/sql"
	"time"
)

func GetSettings(db *sql.DB) (*Settings, error) {
	var s Settings
	err := db.QueryRow(`SELECT storage_root, default_interval_minutes, default_retention_count,
		event_retention_days, event_retention_max_rows, max_concurrent_backups
		FROM settings WHERE id = 1`).
		Scan(&s.StorageRoot, &s.DefaultIntervalMinutes, &s.DefaultRetentionCount,
			&s.EventRetentionDays, &s.EventRetentionMaxRows, &s.MaxConcurrentBackups)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func UpdateSettings(db *sql.DB, s *Settings) error {
	_, err := db.Exec(`UPDATE settings SET storage_root=?, default_interval_minutes=?, default_retention_count=?,
		event_retention_days=?, event_retention_max_rows=?, max_concurrent_backups=? WHERE id=1`,
		s.StorageRoot, s.DefaultIntervalMinutes, s.DefaultRetentionCount,
		s.EventRetentionDays, s.EventRetentionMaxRows, s.MaxConcurrentBackups)
	return err
}

// GetStorageUsage returns the fleet-wide storage figure last computed by
// the scheduler's periodic refresh (see scheduler.refreshStorageUsage),
// and when that refresh last ran. At is the zero time before the very
// first refresh has completed.
//
// Deliberately not part of Settings/UpdateSettings: that struct is
// blindly overwritten on every settings save from the panel, and the
// panel's own form has no field for this figure - folding it in there
// would silently zero it out the next time an operator saves any other
// setting.
func GetStorageUsage(db *sql.DB) (usedBytes int64, at time.Time, err error) {
	var atStr sql.NullString
	err = db.QueryRow(`SELECT storage_used_bytes, storage_used_at FROM settings WHERE id = 1`).Scan(&usedBytes, &atStr)
	if err != nil {
		return 0, time.Time{}, err
	}
	if atStr.Valid {
		at = fromDB(atStr.String)
	}
	return usedBytes, at, nil
}

// UpdateStorageUsage records a freshly computed storage figure. See
// GetStorageUsage for why this stays independent of Settings.
func UpdateStorageUsage(db *sql.DB, usedBytes int64) error {
	_, err := db.Exec(`UPDATE settings SET storage_used_bytes = ?, storage_used_at = ? WHERE id = 1`,
		usedBytes, toDB(time.Now()))
	return err
}
