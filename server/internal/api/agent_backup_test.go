package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backup-server/internal/models"
)

// This is what makes a version's size instant in the versions panel from
// the moment it exists, not just fast on a second look: handleAgentPlan
// already walks the whole tree to create the new version's hard links
// (see filestore.SnapshotCurrent) - it must record that size in the
// database right there, so nothing ever has to walk that tree again just
// to answer "how big is this version".
func TestPlanCachesTheNewVersionsSizeImmediately(t *testing.T) {
	a, deviceID := testAPI(t)
	device, err := models.GetDevice(a.DB, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	store := a.Store.Get()
	deviceDir := store.DeviceDir(device.ID, device.Name)

	content := "contenu existant avant cette sauvegarde"
	if _, err := store.WriteFile(deviceDir, "Bureau/rapport.docx", strings.NewReader(content), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	snap, err := models.CreateSnapshot(a.DB, deviceID, models.SnapshotKindManual)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	body := `{"files":[{"path":"Bureau/rapport.docx","size":10,"mtime":1}]}`
	req := httpReqJSON(t, http.MethodPost, "/api/agent/snapshots/"+snap.ID+"/plan", body)
	w := httptest.NewRecorder()
	a.handleAgentPlan(w, req, deviceID, snap.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("handleAgentPlan: statut %d, corps %s", w.Code, w.Body.String())
	}

	versions := store.ListVersions(deviceDir)
	if len(versions) != 1 {
		t.Fatalf("versions = %v, attendu 1", versions)
	}

	bytes, at, ok, err := models.GetCachedSize(a.DB, deviceID, versions[0])
	if err != nil {
		t.Fatalf("GetCachedSize: %v", err)
	}
	if !ok {
		t.Fatal("handleAgentPlan n'a pas mis en cache la taille de la nouvelle version")
	}
	if bytes != int64(len(content)) {
		t.Fatalf("taille mise en cache = %d, attendu %d", bytes, len(content))
	}
	if time.Since(at) > 5*time.Second {
		t.Fatalf("computed_at = %v, attendu proche de maintenant", at)
	}
}
