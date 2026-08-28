package models

import "database/sql"

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
