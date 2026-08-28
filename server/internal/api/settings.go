package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"backup-server/internal/filestore"
	"backup-server/internal/models"
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
	// Two is the floor, not one: with a single state kept, the only copy
	// is the mirror being overwritten, so a file corrupted on the PC
	// propagates to the NAS with nothing left to fall back on.
	if req.DefaultRetentionCount < 2 {
		writeError(w, http.StatusBadRequest, "le nombre de sauvegardes à conserver doit être au moins 2")
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
	if req.MaxConcurrentBackups < 1 {
		writeError(w, http.StatusBadRequest, "le nombre de sauvegardes simultanées doit être au moins 1")
		return
	}

	current, err := models.GetSettings(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	if req.StorageRoot != current.StorageRoot {
		newStore, err := filestore.New(req.StorageRoot)
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

	// The whole incremental scheme rests on this storage giving back the
	// modification time it was told to store: that's how the server knows
	// a file hasn't changed. A mount that rounds or drops it makes every
	// file look different on every run - the machine re-uploads its entire
	// disk each time, forever, with nothing in the logs to say why. Some
	// SMB/CIFS and FAT-backed shares do exactly that, so it is checked
	// here rather than discovered from a monthly bandwidth bill.
	timestampWarning := ""
	stamp := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(marker, stamp, stamp); err != nil {
		timestampWarning = "impossible d'écrire la date de modification des fichiers (" + err.Error() + ")"
	} else if info, err := os.Stat(marker); err != nil {
		timestampWarning = "impossible de relire la date de modification des fichiers (" + err.Error() + ")"
	} else if !info.ModTime().UTC().Equal(stamp) {
		timestampWarning = fmt.Sprintf(
			"ce stockage ne conserve pas exactement la date de modification des fichiers (écrit %s, relu %s). "+
				"Les sauvegardes fonctionneront, mais chaque fichier sera considéré comme modifié à chaque passage : "+
				"tout sera retransféré à chaque sauvegarde.",
			stamp.Format(time.RFC3339), info.ModTime().UTC().Format(time.RFC3339))
	}

	if err := os.Remove(marker); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "échec de la suppression du fichier de test: " + err.Error()})
		return
	}

	// Reported as a warning, not a failure: the path genuinely works, and
	// refusing it would leave the operator with no storage at all over
	// something that costs bandwidth rather than data.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": timestampWarning})
}
