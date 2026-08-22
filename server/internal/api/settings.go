package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"backup-server/internal/models"
	"backup-server/internal/storage"
)

func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := models.GetSettings(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (a *API) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req models.Settings
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if req.DefaultRetentionCount < 1 {
		writeError(w, http.StatusBadRequest, "le nombre de sauvegardes à conserver doit être au moins 1")
		return
	}
	if req.DefaultIntervalMinutes < 5 {
		writeError(w, http.StatusBadRequest, "l'intervalle minimum est de 5 minutes")
		return
	}
	if req.StorageRoot == "" {
		writeError(w, http.StatusBadRequest, "le chemin de stockage est requis")
		return
	}

	current, err := models.GetSettings(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	if req.StorageRoot != current.StorageRoot {
		newStore, err := storage.New(req.StorageRoot)
		if err != nil {
			writeError(w, http.StatusBadRequest, "chemin de stockage inaccessible: "+err.Error())
			return
		}
		a.Store.Set(newStore)
		_ = models.AddEvent(a.DB, nil, models.EventLevelWarning,
			"Emplacement de stockage changé vers "+req.StorageRoot+". Les anciennes données ne sont pas migrées automatiquement.")
	}

	if err := models.UpdateSettings(a.DB, &req); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, req)
}

type testStorageRequest struct {
	Path string `json:"path"`
}

const storageTestPersistDelay = 6 * time.Second

// handleTestStorage verifies a candidate storage path is actually usable
// as a NAS-backed destination, not just instantly writable to some local
// cache: it writes a marker file, waits a few seconds, then confirms the
// file is *still* there and readable before cleaning up. A flaky mount
// that accepts a write but silently drops it (or disconnects moments
// later) fails this check instead of only surfacing once real backups
// start landing on it.
func (a *API) handleTestStorage(w http.ResponseWriter, r *http.Request) {
	var req testStorageRequest
	if err := decodeJSON(r, &req); err != nil || req.Path == "" {
		writeError(w, http.StatusBadRequest, "chemin manquant")
		return
	}

	if err := os.MkdirAll(req.Path, 0o750); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "impossible de créer le dossier: " + err.Error()})
		return
	}

	marker := filepath.Join(req.Path, ".backup-center-test-"+fmt.Sprint(time.Now().UnixNano()))
	content := []byte("backup-center connectivity test\n")
	if err := os.WriteFile(marker, content, 0o640); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "échec de l'écriture: " + err.Error()})
		return
	}

	time.Sleep(storageTestPersistDelay)

	data, err := os.ReadFile(marker)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "le fichier de test a disparu (montage instable ?): " + err.Error()})
		return
	}
	if string(data) != string(content) {
		_ = os.Remove(marker)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "le contenu relu ne correspond pas (écriture non fiable)"})
		return
	}

	if err := os.Remove(marker); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "échec de la suppression du fichier de test: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
