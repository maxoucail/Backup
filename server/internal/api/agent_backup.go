package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"backup-server/internal/filestore"
	"backup-server/internal/models"
	"backup-server/internal/scheduler"
)

// effectivePolicy resolves per-device overrides against the global
// defaults, used both by GET /api/agent/config (agent polling) and by the
// config push sent over the WebSocket hub when an operator changes a
// device's settings in the panel.
func effectivePolicy(device *models.Device, settings *models.Settings) (intervalMinutes, retentionCount int, backupPaths []string) {
	intervalMinutes = settings.DefaultIntervalMinutes
	if device.IntervalMinutes != nil {
		intervalMinutes = *device.IntervalMinutes
	}
	retentionCount = settings.DefaultRetentionCount
	if device.RetentionCount != nil {
		retentionCount = *device.RetentionCount
	}
	backupPaths = nil
	if device.BackupPaths != "" {
		_ = json.Unmarshal([]byte(device.BackupPaths), &backupPaths)
	}
	return
}

func (a *API) handleAgentGetConfig(w http.ResponseWriter, r *http.Request, deviceID string) {
	device, err := models.GetDevice(a.DB, deviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	settings, err := models.GetSettings(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	interval, retention, paths := effectivePolicy(device, settings)
	writeJSON(w, http.StatusOK, map[string]any{
		"interval_minutes": interval,
		"retention_count":  retention,
		"backup_paths":     paths,
	})
}

type createSnapshotRequest struct {
	Kind string `json:"kind"`
}

func (a *API) handleAgentCreateSnapshot(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req createSnapshotRequest
	if err := decodeJSONLenient(r, &req); err != nil || (req.Kind != models.SnapshotKindManual && req.Kind != models.SnapshotKindScheduled) {
		writeError(w, http.StatusBadRequest, "type de sauvegarde invalide")
		return
	}

	// Backups are serialized across the fleet (see internal/queue): the
	// agent doesn't get a snapshot to write into until a slot is free.
	// Being queued is a normal outcome, not an error - no snapshot row is
	// created, so a waiting device leaves no failed backup behind.
	if granted, position := a.Queue.Acquire(deviceID); !granted {
		_ = models.AddEvent(a.DB, &deviceID, models.EventLevelInfo,
			fmt.Sprintf("Sauvegarde mise en file d'attente (position %d) : une autre machine sauvegarde déjà.", position))
		writeJSON(w, http.StatusOK, map[string]any{"queued": true, "position": position})
		return
	}

	snap, err := models.CreateSnapshot(a.DB, deviceID, req.Kind)
	if err != nil {
		a.Queue.Release(deviceID) // don't strand the slot on a failed insert
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"queued": false, "snapshot_id": snap.ID})
}

type planRequest struct {
	Files []filestore.FileInfo `json:"files"`
}

// handleAgentPlan is the heart of the incremental backup.
//
// The agent announces everything it currently holds; the server preserves
// the machine's current folder as a dated version, brings that folder in
// line with what the machine no longer has, and replies with just the
// files it doesn't already have an identical copy of. Only those cross the
// network - an unchanged 4 GB photo library is never re-sent, and never
// re-stored.
func (a *API) handleAgentPlan(w http.ResponseWriter, r *http.Request, deviceID, snapshotID string) {
	snap, err := models.GetSnapshot(a.DB, snapshotID)
	if err != nil || snap.DeviceID != deviceID {
		writeError(w, http.StatusNotFound, "sauvegarde introuvable")
		return
	}
	device, err := models.GetDevice(a.DB, deviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}

	var req planRequest
	r.Body = http.MaxBytesReader(w, r.Body, 512*1024*1024)
	if err := decodeJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "liste de fichiers invalide")
		return
	}
	if len(req.Files) == 0 {
		writeError(w, http.StatusBadRequest, "aucun fichier à sauvegarder")
		return
	}

	store := a.Store.Get()
	deviceDir := store.DeviceDir(device.ID, device.Name)

	// Preserve the current state before touching it, so a complete
	// previous version always exists even if this backup is interrupted
	// halfway through.
	versionDir, err := store.SnapshotCurrent(deviceDir, time.Now())
	if err != nil {
		log.Printf("plan: création de la version précédente pour %s: %v", device.Name, err)
		writeError(w, http.StatusInternalServerError, "impossible de préparer la nouvelle version")
		return
	}
	if versionDir != "" {
		log.Printf("sauvegarde %s: version précédente conservée dans %s", device.Name, versionDir)
	}

	if removed := store.PruneRemoved(deviceDir, req.Files); removed > 0 {
		log.Printf("sauvegarde %s: %d fichier(s) retiré(s) du miroir (absents de la machine)", device.Name, removed)
	}

	needed := store.NeededFiles(deviceDir, req.Files)

	var logicalBytes int64
	for _, f := range req.Files {
		logicalBytes += f.Size
	}
	_ = models.UpdateSnapshotProgress(a.DB, snapshotID, len(req.Files), logicalBytes, 0, 0)

	writeJSON(w, http.StatusOK, map[string]any{
		"needed":      needed,
		"destination": deviceDir,
	})
}

const maxFileBodyBytes = 16 * 1024 * 1024 * 1024 // 16 GiB ceiling per file

// handleAgentUploadFile stores one file, in clear, at its own path under
// the machine's folder.
func (a *API) handleAgentUploadFile(w http.ResponseWriter, r *http.Request, deviceID string) {
	device, err := models.GetDevice(a.DB, deviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeError(w, http.StatusBadRequest, "chemin manquant")
		return
	}
	var modTime int64
	fmt.Sscanf(r.URL.Query().Get("mtime"), "%d", &modTime)

	r.Body = http.MaxBytesReader(w, r.Body, maxFileBodyBytes)
	store := a.Store.Get()
	n, err := store.WriteFile(store.DeviceDir(device.ID, device.Name), relPath, r.Body, modTime)
	if err != nil {
		writeError(w, http.StatusBadRequest, "échec de l'écriture: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes_written": n})
}

type finishSnapshotRequest struct {
	Status        string `json:"status"`
	ErrorMessage  string `json:"error_message"`
	UploadedBytes int64  `json:"uploaded_bytes"`
}

func (a *API) handleAgentFinishSnapshot(w http.ResponseWriter, r *http.Request, deviceID, snapshotID string) {
	snap, err := models.GetSnapshot(a.DB, snapshotID)
	if err != nil || snap.DeviceID != deviceID {
		writeError(w, http.StatusNotFound, "sauvegarde introuvable")
		return
	}
	var req finishSnapshotRequest
	if err := decodeJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if req.Status != models.SnapshotStatusSuccess && req.Status != models.SnapshotStatusFailed && req.Status != models.SnapshotStatusCancelled {
		writeError(w, http.StatusBadRequest, "statut invalide")
		return
	}

	_, _ = a.DB.Exec(`UPDATE snapshots SET uploaded_bytes = ? WHERE id = ?`, req.UploadedBytes, snapshotID)
	if err := models.FinishSnapshot(a.DB, snapshotID, req.Status, req.ErrorMessage); err != nil {
		a.Queue.Release(deviceID) // slot must not outlive the backup, even on a failed write
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	// Whatever the outcome, this device is done occupying its slot: hand
	// it to whoever is waiting.
	a.Queue.Release(deviceID)

	level := models.EventLevelInfo
	msg := "Sauvegarde terminée avec succès."
	if req.Status != models.SnapshotStatusSuccess {
		level = models.EventLevelError
		msg = "Sauvegarde en échec: " + req.ErrorMessage
	}
	_ = models.AddEvent(a.DB, &deviceID, level, msg)

	if req.Status == models.SnapshotStatusSuccess {
		// Retention runs only now, once the new backup is complete: an
		// old version is never dropped while the current one is still
		// being written.
		go func(deviceID string) {
			if _, err := scheduler.RotateRetention(a.DB, a.Store, deviceID); err != nil {
				log.Printf("api: rotation de rétention pour %s: %v", deviceID, err)
			}
		}(deviceID)
	}

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}
