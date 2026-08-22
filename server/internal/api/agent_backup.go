package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"backup-server/internal/models"
	"backup-server/internal/scheduler"
	"backup-server/internal/storage"
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
		"chunk_size_bytes": settings.ChunkSizeBytes,
	})
}

type createSnapshotRequest struct {
	Kind string `json:"kind"`
}

func (a *API) handleAgentCreateSnapshot(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req createSnapshotRequest
	if err := decodeJSON(r, &req); err != nil || (req.Kind != models.SnapshotKindManual && req.Kind != models.SnapshotKindScheduled) {
		writeError(w, http.StatusBadRequest, "type de sauvegarde invalide")
		return
	}
	snap, err := models.CreateSnapshot(a.DB, deviceID, req.Kind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"snapshot_id": snap.ID})
}

type checkChunksRequest struct {
	Hashes []string `json:"hashes"`
}

// handleAgentCheckChunks lets the agent skip re-uploading any chunk the
// store already has - whether from this device's own previous backup or,
// thanks to content-addressing, from any other device that happened to
// have identical file content. This is the core of the incremental
// behaviour: only genuinely new or changed data crosses the network.
func (a *API) handleAgentCheckChunks(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req checkChunksRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	store := a.Store.Get()
	missing := make([]string, 0, len(req.Hashes))
	for _, h := range req.Hashes {
		if !isValidHash(h) {
			writeError(w, http.StatusBadRequest, "hash invalide: "+h)
			return
		}
		if !store.HasChunk(h) {
			missing = append(missing, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

const maxChunkBodyBytes = 64 * 1024 * 1024 // hard ceiling, well above the configured chunk size

func (a *API) handleAgentUploadChunk(w http.ResponseWriter, r *http.Request, deviceID string, hash string) {
	if !isValidHash(hash) {
		writeError(w, http.StatusBadRequest, "hash invalide")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChunkBodyBytes)
	store := a.Store.Get()
	n, err := store.WriteChunk(hash, r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "échec de l'écriture du chunk: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes_written": n})
}

func (a *API) handleAgentDownloadChunk(w http.ResponseWriter, r *http.Request, deviceID string, hash string) {
	if !isValidHash(hash) {
		writeError(w, http.StatusBadRequest, "hash invalide")
		return
	}
	store := a.Store.Get()
	f, err := store.ChunkReader(hash)
	if err != nil {
		writeError(w, http.StatusNotFound, "chunk introuvable")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

func (a *API) handleAgentSubmitManifest(w http.ResponseWriter, r *http.Request, deviceID, snapshotID string) {
	snap, err := models.GetSnapshot(a.DB, snapshotID)
	if err != nil || snap.DeviceID != deviceID {
		writeError(w, http.StatusNotFound, "sauvegarde introuvable")
		return
	}

	var manifest storage.Manifest
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024*1024)
	if err := decodeJSON(r, &manifest); err != nil {
		writeError(w, http.StatusBadRequest, "manifeste invalide")
		return
	}
	manifest.DeviceID = deviceID
	manifest.SnapshotID = snapshotID

	store := a.Store.Get()
	var logicalBytes int64
	for _, f := range manifest.Files {
		logicalBytes += f.Size
		for _, c := range f.Chunks {
			if !isValidHash(c) {
				writeError(w, http.StatusBadRequest, "hash de chunk invalide dans le manifeste")
				return
			}
			if !store.HasChunk(c) {
				writeError(w, http.StatusConflict, "chunk manquant sur le serveur: "+c)
				return
			}
		}
	}

	path, err := store.WriteManifest(&manifest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	if err := models.UpdateSnapshotProgress(a.DB, snapshotID, len(manifest.Files), logicalBytes, snap.UploadedBytes, snap.ProgressPercent); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	_, _ = a.DB.Exec(`UPDATE snapshots SET manifest_path = ? WHERE id = ?`, path, snapshotID)

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (a *API) handleAgentGetManifest(w http.ResponseWriter, r *http.Request, deviceID, snapshotID string) {
	snap, err := models.GetSnapshot(a.DB, snapshotID)
	if err != nil || snap.DeviceID != deviceID {
		writeError(w, http.StatusNotFound, "sauvegarde introuvable")
		return
	}
	if snap.ManifestPath == "" {
		writeError(w, http.StatusNotFound, "manifeste introuvable")
		return
	}
	manifest, err := storage.ReadManifest(snap.ManifestPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "manifeste introuvable")
		return
	}
	writeJSON(w, http.StatusOK, manifest)
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
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if req.Status != models.SnapshotStatusSuccess && req.Status != models.SnapshotStatusFailed && req.Status != models.SnapshotStatusCancelled {
		writeError(w, http.StatusBadRequest, "statut invalide")
		return
	}

	_, _ = a.DB.Exec(`UPDATE snapshots SET uploaded_bytes = ? WHERE id = ?`, req.UploadedBytes, snapshotID)
	if err := models.FinishSnapshot(a.DB, snapshotID, req.Status, req.ErrorMessage, snap.ManifestPath); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	level := models.EventLevelInfo
	msg := "Sauvegarde terminée avec succès."
	if req.Status != models.SnapshotStatusSuccess {
		level = models.EventLevelError
		msg = "Sauvegarde en échec: " + req.ErrorMessage
	}
	_ = models.AddEvent(a.DB, &deviceID, level, msg)

	if req.Status == models.SnapshotStatusSuccess {
		// Runs after the response is sent: retention rotation walks the
		// whole chunk store during GC, which could otherwise make the
		// agent's finish request wait a long time on a large repository.
		go func(deviceID string) {
			if _, err := scheduler.RotateRetention(a.DB, a.Store, deviceID); err != nil {
				log.Printf("api: retention rotation for device %s failed: %v", deviceID, err)
			}
		}(deviceID)
	}

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}
