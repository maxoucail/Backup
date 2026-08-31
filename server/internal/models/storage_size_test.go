package models

import (
	"database/sql"
	"testing"
	"time"

	"backup-server/internal/db"
)

func newSizeCacheTestDB(t *testing.T) (*sql.DB, *Device) {
	t.Helper()
	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	dev, err := CreateDevice(sqlDB, "PC", "pc.local", "windows", "11", "1.0.0", "hash", "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return sqlDB, dev
}

// This is the exact operator complaint: on a real NAS, computing a
// version's size is a full recursive stat() of every file - fine once,
// ruinous when done fresh on every single load of the panel's versions
// tab. A previous version's folder is written once and never touched
// again short of deletion, so a cached size must survive as-is across
// calls - this lives in the database rather than in memory precisely so
// it also survives a server restart, unlike a plain in-process map.
func TestGetSetCachedSizeRoundTrips(t *testing.T) {
	sqlDB, dev := newSizeCacheTestDB(t)

	if _, _, ok, err := GetCachedSize(sqlDB, dev.ID, "2026-01-01_10-00"); err != nil {
		t.Fatalf("GetCachedSize: %v", err)
	} else if ok {
		t.Fatal("ok = true avant tout calcul, attendu false")
	}

	if err := SetCachedSize(sqlDB, dev.ID, "2026-01-01_10-00", 12345); err != nil {
		t.Fatalf("SetCachedSize: %v", err)
	}

	bytes, at, ok, err := GetCachedSize(sqlDB, dev.ID, "2026-01-01_10-00")
	if err != nil {
		t.Fatalf("GetCachedSize: %v", err)
	}
	if !ok || bytes != 12345 {
		t.Fatalf("GetCachedSize = (%d, %v), attendu (12345, true)", bytes, ok)
	}
	if time.Since(at) > 5*time.Second {
		t.Fatalf("computed_at = %v, attendu proche de maintenant", at)
	}
}

// A second SetCachedSize for the same device+scope must replace the row,
// not add a conflicting one - a version's size can legitimately be
// recomputed (e.g. after an eviction) and the cache must reflect the new
// number, not the old one alongside it.
func TestSetCachedSizeOverwritesThePreviousValue(t *testing.T) {
	sqlDB, dev := newSizeCacheTestDB(t)

	if err := SetCachedSize(sqlDB, dev.ID, CurrentSizeScope, 100); err != nil {
		t.Fatal(err)
	}
	if err := SetCachedSize(sqlDB, dev.ID, CurrentSizeScope, 200); err != nil {
		t.Fatal(err)
	}

	bytes, _, ok, err := GetCachedSize(sqlDB, dev.ID, CurrentSizeScope)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || bytes != 200 {
		t.Fatalf("GetCachedSize = (%d, %v), attendu (200, true)", bytes, ok)
	}
}

// DeleteCachedSize is what handleDeleteVersion/handleDeleteCurrent call so
// a later read never hands back a number for a folder that no longer
// exists.
func TestDeleteCachedSizeEvictsOnlyThatScope(t *testing.T) {
	sqlDB, dev := newSizeCacheTestDB(t)

	if err := SetCachedSize(sqlDB, dev.ID, "v1", 10); err != nil {
		t.Fatal(err)
	}
	if err := SetCachedSize(sqlDB, dev.ID, "v2", 20); err != nil {
		t.Fatal(err)
	}

	if err := DeleteCachedSize(sqlDB, dev.ID, "v1"); err != nil {
		t.Fatal(err)
	}

	if _, _, ok, err := GetCachedSize(sqlDB, dev.ID, "v1"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("v1 aurait dû être évincé")
	}
	if _, _, ok, err := GetCachedSize(sqlDB, dev.ID, "v2"); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("v2 n'aurait pas dû être touché par l'éviction de v1")
	}
}

// Deleting a device must take every one of its cached sizes with it - the
// storage_size_cache table declares device_id with ON DELETE CASCADE
// specifically so callers never have to enumerate every version scope by
// hand just to clean up after DeleteDevice.
func TestDeletingADeviceCascadesItsCachedSizes(t *testing.T) {
	sqlDB, dev := newSizeCacheTestDB(t)

	if err := SetCachedSize(sqlDB, dev.ID, CurrentSizeScope, 100); err != nil {
		t.Fatal(err)
	}
	if err := SetCachedSize(sqlDB, dev.ID, "v1", 10); err != nil {
		t.Fatal(err)
	}

	if err := DeleteDevice(sqlDB, dev.ID); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}

	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM storage_size_cache WHERE device_id = ?`, dev.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("lignes de cache restantes après suppression de l'appareil = %d, attendu 0 (ON DELETE CASCADE)", n)
	}
}
