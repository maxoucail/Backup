package api

import (
	"encoding/json"
	"net/http"

	"backup-server/internal/models"
	"backup-server/internal/storage"
	"backup-server/internal/ws"
)

type deviceView struct {
	models.Device
	Online         bool             `json:"online"`
	LatestSnapshot *models.Snapshot `json:"latest_snapshot,omitempty"`
	SnapshotCount  int              `json:"snapshot_count"`
}

func (a *API) toDeviceView(d models.Device) deviceView {
	latest, _ := models.LatestSnapshotForDevice(a.DB, d.ID)
	snaps, _ := models.ListSuccessfulSnapshotsForDevice(a.DB, d.ID)
	return deviceView{
		Device:         d,
		Online:         a.Hub.IsOnline(d.ID),
		LatestSnapshot: latest,
		SnapshotCount:  len(snaps),
	}
}

func (a *API) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := models.ListDevices(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	views := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		views = append(views, a.toDeviceView(d))
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *API) handleGetDevice(w http.ResponseWriter, r *http.Request, id string) {
	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	snapshots, err := models.ListSnapshotsForDevice(a.DB, id, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	events, err := models.ListEventsForDevice(a.DB, id, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device":    a.toDeviceView(*device),
		"snapshots": snapshots,
		"events":    events,
	})
}

type updateDeviceRequest struct {
	Name            *string  `json:"name"`
	IntervalMinutes *int     `json:"interval_minutes"`
	RetentionCount  *int     `json:"retention_count"`
	BackupPaths     []string `json:"backup_paths"`
}

func (a *API) handleUpdateDevice(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := models.GetDevice(a.DB, id); err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	var req updateDeviceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if req.Name != nil && *req.Name != "" {
		if err := models.RenameDevice(a.DB, id, *req.Name); err != nil {
			writeError(w, http.StatusInternalServerError, "erreur serveur")
			return
		}
	}

	pathsJSON := ""
	if req.BackupPaths != nil {
		b, _ := json.Marshal(req.BackupPaths)
		pathsJSON = string(b)
	}
	if err := models.UpdateDevicePolicy(a.DB, id, req.IntervalMinutes, req.RetentionCount, pathsJSON); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	device, _ := models.GetDevice(a.DB, id)
	settings, _ := models.GetSettings(a.DB)
	interval, retention, paths := effectivePolicy(device, settings)
	a.Hub.SendCommand(id, ws.Envelope{
		Type:            ws.TypeConfig,
		IntervalMinutes: &interval,
		RetentionCount:  &retention,
		BackupPaths:     paths,
	})

	writeJSON(w, http.StatusOK, a.toDeviceView(*device))
}

func (a *API) handleDeleteDevice(w http.ResponseWriter, r *http.Request, id string) {
	snapshots, err := models.ListSnapshotsForDevice(a.DB, id, 100000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	store := a.Store.Get()
	for _, s := range snapshots {
		_ = storage.DeleteManifest(s.ManifestPath)
	}
	if err := models.DeleteDevice(a.DB, id); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	go func() {
		remaining, err := models.AllManifestPaths(a.DB)
		if err == nil {
			_, _, _ = store.GarbageCollect(remaining)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (a *API) handleBackupNow(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := models.GetDevice(a.DB, id); err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	if !a.Hub.SendCommand(id, ws.Envelope{Type: ws.TypeBackupNow}) {
		writeError(w, http.StatusConflict, "l'appareil n'est pas connecté")
		return
	}
	_ = models.AddEvent(a.DB, &id, models.EventLevelInfo, "Sauvegarde manuelle demandée depuis le panneau.")
	writeJSON(w, http.StatusAccepted, map[string]string{"ok": "true"})
}

type restoreRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

func (a *API) handleRestore(w http.ResponseWriter, r *http.Request, id string) {
	var req restoreRequest
	if err := decodeJSON(r, &req); err != nil || req.SnapshotID == "" {
		writeError(w, http.StatusBadRequest, "sauvegarde cible manquante")
		return
	}
	snap, err := models.GetSnapshot(a.DB, req.SnapshotID)
	if err != nil || snap.DeviceID != id {
		writeError(w, http.StatusNotFound, "sauvegarde introuvable")
		return
	}
	if snap.Status != models.SnapshotStatusSuccess {
		writeError(w, http.StatusBadRequest, "cette sauvegarde n'est pas utilisable pour une restauration")
		return
	}
	if !a.Hub.SendCommand(id, ws.Envelope{Type: ws.TypeRestore, SnapshotID: req.SnapshotID}) {
		writeError(w, http.StatusConflict, "l'appareil n'est pas connecté")
		return
	}
	_ = models.AddEvent(a.DB, &id, models.EventLevelInfo, "Restauration demandée depuis le panneau.")
	writeJSON(w, http.StatusAccepted, map[string]string{"ok": "true"})
}

func (a *API) handleCancelJob(w http.ResponseWriter, r *http.Request, id string) {
	if !a.Hub.SendCommand(id, ws.Envelope{Type: ws.TypeCancel}) {
		writeError(w, http.StatusConflict, "l'appareil n'est pas connecté")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"ok": "true"})
}
