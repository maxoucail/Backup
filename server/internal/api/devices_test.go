package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"backup-server/internal/auth"
	"backup-server/internal/db"
	"backup-server/internal/filestore"
	"backup-server/internal/models"
	"backup-server/internal/queue"
	"backup-server/internal/ws"
)

func testAPI(t *testing.T) (*API, string) {
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
	a := New(sqlDB, filestore.NewHolder(store), ws.NewHub(sqlDB), auth.NewSessionSigner("secret-de-test"),
		queue.New(func() int { return 1 }, func(string) bool { return false }), dir, "8420")

	device, err := models.CreateDevice(sqlDB, "PC-Max", "pc-max", "Windows", "11", "1.0.0", "hash", "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return a, device.ID
}

func patchDevice(t *testing.T, a *API, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPatch, "/api/devices/"+id, strings.NewReader(body))
	w := httptest.NewRecorder()
	a.handleUpdateDevice(w, r, id)
	return w
}

// The exact folder list an operator configured is the single most
// important setting on a device - it's what "sauvegarde uniquement ce que
// je te donne" means. A PATCH that only carries a new name must leave it
// alone; rewriting every column on every PATCH sent the machine silently
// back to the default folders.
func TestRenamingADeviceKeepsItsPolicy(t *testing.T) {
	a, id := testAPI(t)

	if w := patchDevice(t, a, id, `{"interval_minutes":120,"retention_count":4,"backup_paths":["Documents","E:\\Projets"]}`); w.Code != http.StatusOK {
		t.Fatalf("réglage de la politique: %d %s", w.Code, w.Body.String())
	}
	if w := patchDevice(t, a, id, `{"name":"PC-Compta"}`); w.Code != http.StatusOK {
		t.Fatalf("renommage: %d %s", w.Code, w.Body.String())
	}

	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if device.Name != "PC-Compta" {
		t.Fatalf("nom = %q, attendu PC-Compta", device.Name)
	}
	if device.IntervalMinutes == nil || *device.IntervalMinutes != 120 {
		t.Fatalf("intervalle = %v, effacé par le renommage", device.IntervalMinutes)
	}
	if device.RetentionCount == nil || *device.RetentionCount != 4 {
		t.Fatalf("rétention = %v, effacée par le renommage", device.RetentionCount)
	}
	var paths []string
	_ = json.Unmarshal([]byte(device.BackupPaths), &paths)
	if len(paths) != 2 || paths[0] != "Documents" {
		t.Fatalf("dossiers sauvegardés = %v, effacés par le renommage", paths)
	}
}

// Sending an explicit null is how the panel says "back to the server
// default" - that has to stay possible, and must not be confused with
// omitting the field.
func TestExplicitNullClearsAPolicyField(t *testing.T) {
	a, id := testAPI(t)
	patchDevice(t, a, id, `{"interval_minutes":120,"retention_count":4}`)

	if w := patchDevice(t, a, id, `{"interval_minutes":null}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	device, _ := models.GetDevice(a.DB, id)
	if device.IntervalMinutes != nil {
		t.Fatalf("intervalle = %v, attendu nul (valeur par défaut du serveur)", *device.IntervalMinutes)
	}
	if device.RetentionCount == nil || *device.RetentionCount != 4 {
		t.Fatalf("rétention = %v, ne devait pas bouger", device.RetentionCount)
	}
}

// One state is not a backup: the only copy would be the mirror being
// overwritten, so a file corrupted on the PC propagates to the NAS with
// nothing to fall back on.
func TestRetentionBelowTwoIsRefused(t *testing.T) {
	a, id := testAPI(t)
	if w := patchDevice(t, a, id, `{"retention_count":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, attendu 400", w.Code)
	}
	device, _ := models.GetDevice(a.DB, id)
	if device.RetentionCount != nil {
		t.Fatalf("rétention enregistrée = %v malgré le refus", *device.RetentionCount)
	}
}

// Renaming has to move the machine's folder on the NAS, since the folder
// is named after the device. Otherwise the existing backup is stranded
// under the old name and the next run re-uploads the whole disk.
func TestRenamingADeviceMovesItsFolderOnTheNAS(t *testing.T) {
	a, id := testAPI(t)
	store := a.Store.Get()
	device, _ := models.GetDevice(a.DB, id)
	oldDir := store.DeviceDir(device.ID, device.Name)
	if _, err := store.WriteFile(oldDir, "Bureau/rapport.docx", strings.NewReader("contenu"), 1700000000); err != nil {
		t.Fatal(err)
	}

	if w := patchDevice(t, a, id, `{"name":"PC-Compta"}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	updated, _ := models.GetDevice(a.DB, id)
	newDir := store.DeviceDir(updated.ID, updated.Name)
	if b, err := os.ReadFile(filepath.Join(newDir, "Bureau", "rapport.docx")); err != nil || string(b) != "contenu" {
		t.Fatalf("fichier introuvable dans le nouveau dossier %s: %v", newDir, err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("l'ancien dossier existe encore : deux dossiers pour une seule machine")
	}
}

// Deleting a device must take its files with it - an operator who
// decommissions a machine expects the NAS space back, not an orphan folder
// nothing references any more.
func TestDeletingADeviceRemovesItsFolder(t *testing.T) {
	a, id := testAPI(t)
	store := a.Store.Get()
	device, _ := models.GetDevice(a.DB, id)
	dir := store.DeviceDir(device.ID, device.Name)
	if _, err := store.WriteFile(dir, "Bureau/rapport.docx", strings.NewReader("contenu"), 1700000000); err != nil {
		t.Fatal(err)
	}

	if err := a.deleteDeviceAndReclaim(id); err != nil {
		t.Fatalf("deleteDeviceAndReclaim: %v", err)
	}
	if _, err := models.GetDevice(a.DB, id); err == nil {
		t.Fatal("l'appareil existe encore en base")
	}
	waitGone(t, dir)
}

// waitGone polls for the folder to disappear: the delete runs in the
// background so the panel doesn't hang on a slow network mount.
func waitGone(t *testing.T, dir string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("le dossier %s n'a pas été supprimé", dir)
}

// The incremental backup rests entirely on the storage giving back the
// modification time it was told to store. A share that rounds or drops it
// makes every file look modified on every run, so the machine re-uploads
// its whole disk each time - the storage test has to say so rather than
// let an operator find out from their bandwidth.
func TestStorageTestReportsWhetherModificationTimesSurvive(t *testing.T) {
	a, _ := testAPI(t)
	dir := t.TempDir()

	r := httptest.NewRequest(http.MethodPost, "/api/settings/test-storage",
		strings.NewReader(`{"path":`+strconv.Quote(dir)+`}`))
	w := httptest.NewRecorder()
	a.handleTestStorage(w, r)

	var res struct {
		OK      bool   `json:"ok"`
		Warning string `json:"warning"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("réponse illisible: %v", err)
	}
	if !res.OK {
		t.Fatalf("le test a échoué sur un dossier local sain: %s", res.Error)
	}
	// A local temp dir preserves timestamps, so there must be no warning -
	// which also proves the check runs and isn't warning unconditionally.
	if res.Warning != "" {
		t.Fatalf("réserve inattendue sur un disque local: %s", res.Warning)
	}
}

func httpReqJSON(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, target, strings.NewReader(body))
}

// Deleting an old version deletes real files with no undo, so the wrong
// (or missing) confirmation must be refused server-side - not just by the
// panel's own dialog, which a direct API call bypasses entirely.
func TestDeleteVersionRequiresRetypingTheExactVersionName(t *testing.T) {
	a, id := testAPI(t)
	store := a.Store.Get()
	device, _ := models.GetDevice(a.DB, id)
	dir := store.DeviceDir(device.ID, device.Name)
	if _, err := store.WriteFile(dir, "Bureau/a.txt", strings.NewReader("v1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SnapshotCurrent(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	name := store.ListVersions(dir)[0]

	for _, body := range []string{`{}`, `{"confirm_name":"pas le bon nom"}`, `{"confirm_name":""}`} {
		w := httptest.NewRecorder()
		a.handleDeleteVersion(w, httpReqJSON(t, http.MethodDelete, "/api/devices/"+id+"/versions/"+name, body), id, name)
		if w.Code == http.StatusOK {
			t.Fatalf("suppression acceptée sans la bonne confirmation (body=%s)", body)
		}
	}
	if len(store.ListVersions(dir)) != 1 {
		t.Fatal("la version a été supprimée malgré une confirmation absente ou incorrecte")
	}

	w := httptest.NewRecorder()
	a.handleDeleteVersion(w, httpReqJSON(t, http.MethodDelete, "/api/devices/"+id+"/versions/"+name, `{"confirm_name":"`+name+`"}`), id, name)
	if w.Code != http.StatusOK {
		t.Fatalf("suppression refusée avec la bonne confirmation: %d %s", w.Code, w.Body.String())
	}
	if len(store.ListVersions(dir)) != 0 {
		t.Fatal("la version existe encore après une suppression confirmée")
	}
}

// Deleting the live mirror is the most destructive single action this
// panel offers short of decommissioning a device outright - it must
// require the exact device name, same as decommissioning does, and must
// never touch previous versions.
func TestDeleteCurrentRequiresRetypingTheDeviceNameAndKeepsVersions(t *testing.T) {
	a, id := testAPI(t)
	store := a.Store.Get()
	device, _ := models.GetDevice(a.DB, id)
	dir := store.DeviceDir(device.ID, device.Name)
	if _, err := store.WriteFile(dir, "Bureau/a.txt", strings.NewReader("v1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SnapshotCurrent(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteFile(dir, "Bureau/a.txt", strings.NewReader("v2"), 0); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	a.handleDeleteCurrent(w, httpReqJSON(t, http.MethodDelete, "/api/devices/"+id+"/current", `{"confirm_name":"mauvais nom"}`), id)
	if w.Code == http.StatusOK {
		t.Fatal("suppression de la sauvegarde actuelle acceptée sans la bonne confirmation")
	}
	if _, err := os.Stat(filepath.Join(dir, "Bureau", "a.txt")); err != nil {
		t.Fatal("le fichier actuel a été supprimé malgré une confirmation incorrecte")
	}

	w = httptest.NewRecorder()
	a.handleDeleteCurrent(w, httpReqJSON(t, http.MethodDelete, "/api/devices/"+id+"/current", `{"confirm_name":"`+device.Name+`"}`), id)
	if w.Code != http.StatusOK {
		t.Fatalf("suppression refusée avec la bonne confirmation: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "Bureau", "a.txt")); !os.IsNotExist(err) {
		t.Fatal("le fichier actuel existe encore après une suppression confirmée")
	}
	if len(store.ListVersions(dir)) != 1 {
		t.Fatal("les versions précédentes ont été touchées par la suppression de la sauvegarde actuelle")
	}
}

// EffectiveIntervalMinutes is what the dashboard uses to estimate when a
// device's next backup should happen (see dashboard.html). It must
// reflect a device's own override when it has one, and the server
// default otherwise - the exact same resolution effectivePolicy already
// does for the agent's own config endpoint, reused here for consistency.
func TestEffectiveIntervalMinutesReflectsOverrideOrDefault(t *testing.T) {
	a, id := testAPI(t)

	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	view := a.toDeviceView(*device)
	settings, err := models.GetSettings(a.DB)
	if err != nil {
		t.Fatal(err)
	}
	if view.EffectiveIntervalMinutes != settings.DefaultIntervalMinutes {
		t.Fatalf("intervalle effectif = %d, attendu la valeur par défaut du serveur (%d)",
			view.EffectiveIntervalMinutes, settings.DefaultIntervalMinutes)
	}

	custom := settings.DefaultIntervalMinutes + 42
	if err := models.UpdateDevicePolicy(a.DB, id, map[string]any{"interval_minutes": custom}); err != nil {
		t.Fatal(err)
	}
	device, _ = models.GetDevice(a.DB, id)
	view = a.toDeviceView(*device)
	if view.EffectiveIntervalMinutes != custom {
		t.Fatalf("intervalle effectif = %d, attendu la valeur propre à l'appareil (%d)", view.EffectiveIntervalMinutes, custom)
	}
}
