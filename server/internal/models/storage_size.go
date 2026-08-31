package models

import (
	"database/sql"
	"time"
)

// CurrentSizeScope is the storage_size_cache scope name for a machine's
// live mirror, as opposed to one of its previous versions (scoped by that
// version's own folder name).
const CurrentSizeScope = "current"

// GetCachedSize looks up a memoized size computed by
// filestore.Store.DeviceUsedBytes/CurrentUsedBytes for one device+scope
// pair. ok is false if nothing has been computed yet.
func GetCachedSize(db *sql.DB, deviceID, scope string) (bytes int64, computedAt time.Time, ok bool, err error) {
	var at string
	err = db.QueryRow(
		`SELECT bytes, computed_at FROM storage_size_cache WHERE device_id = ? AND scope = ?`,
		deviceID, scope,
	).Scan(&bytes, &at)
	if err == sql.ErrNoRows {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return bytes, fromDB(at), true, nil
}

// SetCachedSize records a freshly computed size, replacing whatever was
// cached for this device+scope before.
func SetCachedSize(db *sql.DB, deviceID, scope string, bytes int64) error {
	_, err := db.Exec(
		`INSERT INTO storage_size_cache (device_id, scope, bytes, computed_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (device_id, scope) DO UPDATE SET bytes = excluded.bytes, computed_at = excluded.computed_at`,
		deviceID, scope, bytes, toDB(time.Now()),
	)
	return err
}

// DeleteCachedSize evicts one device+scope entry - called wherever the
// folder it describes stops existing (a previous version removed, or the
// live mirror wiped), so a later read never hands back a number for
// something that's gone. Deleting a whole device relies on the
// storage_size_cache table's ON DELETE CASCADE instead of this, since that
// covers every version scope at once without having to enumerate them.
func DeleteCachedSize(db *sql.DB, deviceID, scope string) error {
	_, err := db.Exec(`DELETE FROM storage_size_cache WHERE device_id = ? AND scope = ?`, deviceID, scope)
	return err
}
