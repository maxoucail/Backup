package models

import "database/sql"

func GetSettings(db *sql.DB) (*Settings, error) {
	var s Settings
	err := db.QueryRow(`SELECT storage_root, default_interval_minutes, default_retention_count,
		event_retention_days, event_retention_max_rows, chunk_size_bytes FROM settings WHERE id = 1`).
		Scan(&s.StorageRoot, &s.DefaultIntervalMinutes, &s.DefaultRetentionCount,
			&s.EventRetentionDays, &s.EventRetentionMaxRows, &s.ChunkSizeBytes)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func UpdateSettings(db *sql.DB, s *Settings) error {
	_, err := db.Exec(`UPDATE settings SET storage_root=?, default_interval_minutes=?, default_retention_count=?,
		event_retention_days=?, event_retention_max_rows=?, chunk_size_bytes=? WHERE id=1`,
		s.StorageRoot, s.DefaultIntervalMinutes, s.DefaultRetentionCount,
		s.EventRetentionDays, s.EventRetentionMaxRows, s.ChunkSizeBytes)
	return err
}
