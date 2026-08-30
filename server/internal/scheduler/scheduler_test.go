package scheduler

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"backup-server/internal/auth"
	"backup-server/internal/db"
	"backup-server/internal/filestore"
	"backup-server/internal/models"
)

func testStore(t *testing.T) (*sql.DB, *filestore.Holder) {
	t.Helper()
	dir := t.TempDir()
	sqlDB, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	nas := filepath.Join(dir, "nas")
	if err := db.Bootstrap(sqlDB, dir, nas, auth.HashPassword); err != nil {
		t.Fatalf("db.Bootstrap: %v", err)
	}
	store, err := filestore.New(nas)
	if err != nil {
		t.Fatalf("filestore.New: %v", err)
	}
	return sqlDB, filestore.NewHolder(store)
}

// refreshStorageUsage is what stands between the dashboard and a full NAS
// walk on every load: it does the walk once, in the background, and
// records the result so GET /api/dashboard/storage is a plain read. A
// fresh database has no figure at all until the first refresh runs.
func TestRefreshStorageUsageRecordsWhatItComputes(t *testing.T) {
	sqlDB, holder := testStore(t)
	store := holder.Get()

	if _, err := store.WriteFile(store.DeviceDir("dev1", "PC"), "Bureau/a.txt", strings.NewReader("1234567"), 0); err != nil {
		t.Fatal(err)
	}

	if _, _, err := models.GetStorageUsage(sqlDB); err != nil {
		t.Fatalf("GetStorageUsage avant tout calcul: %v", err)
	}

	refreshStorageUsage(sqlDB, holder)

	usedBytes, at, err := models.GetStorageUsage(sqlDB)
	if err != nil {
		t.Fatalf("GetStorageUsage: %v", err)
	}
	if usedBytes != 7 {
		t.Fatalf("stockage enregistré = %d, attendu 7", usedBytes)
	}
	if at.IsZero() {
		t.Fatal("la date de calcul n'a pas été enregistrée")
	}
}

// A dashboard reading a figure that's never been computed (a database
// bootstrapped but the scheduler never run) must see "nothing yet", not
// an error - the API layer turns a zero time into an omitted field rather
// than failing the request.
func TestGetStorageUsageBeforeAnyRefreshIsZero(t *testing.T) {
	sqlDB, _ := testStore(t)
	usedBytes, at, err := models.GetStorageUsage(sqlDB)
	if err != nil {
		t.Fatalf("GetStorageUsage: %v", err)
	}
	if usedBytes != 0 || !at.IsZero() {
		t.Fatalf("usedBytes=%d at=%v, attendu 0 et zero avant le premier calcul", usedBytes, at)
	}
}

// refreshStorageFree is the cheap counterpart to refreshStorageUsage: a
// single statfs() rather than a walk, so it runs every tick. It must
// still round-trip through the same GetStorageFree/UpdateStorageFree pair
// correctly.
func TestRefreshStorageFreeRecordsWhatItComputes(t *testing.T) {
	sqlDB, holder := testStore(t)

	if _, at, err := models.GetStorageFree(sqlDB); err != nil {
		t.Fatalf("GetStorageFree avant tout calcul: %v", err)
	} else if !at.IsZero() {
		t.Fatal("une date de calcul existe avant le premier calcul")
	}

	refreshStorageFree(sqlDB, holder)

	freeBytes, at, err := models.GetStorageFree(sqlDB)
	if err != nil {
		t.Fatalf("GetStorageFree: %v", err)
	}
	if freeBytes <= 0 {
		t.Fatalf("espace disponible enregistré = %d, attendu un chiffre réel (>0) pour un vrai système de fichiers", freeBytes)
	}
	if at.IsZero() {
		t.Fatal("la date de calcul n'a pas été enregistrée")
	}
}
