package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
	if err := a.deleteDeviceAndReclaim(id); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (a *API) deleteDeviceAndReclaim(id string) error {
	snapshots, err := models.ListSnapshotsForDevice(a.DB, id, 100000)
	if err != nil {
		return err
	}
	store := a.Store.Get()
	for _, s := range snapshots {
		_ = storage.DeleteManifest(s.ManifestPath)
	}
	if err := models.DeleteDevice(a.DB, id); err != nil {
		return err
	}
	go func() {
		remaining, err := models.AllManifestPaths(a.DB)
		if err == nil {
			_, _, _ = store.GarbageCollect(remaining, storage.GraceCutoff())
		}
	}()
	return nil
}

type decommissionRequest struct {
	ConfirmName string `json:"confirm_name"`
}

// handleDecommissionDevice is the destructive, remote "turn this device
// off for good" action: it tells the agent (if currently connected) to
// unregister itself as a service/autostart entry and erase its local
// credentials before this device's identity is deleted server-side, so a
// machine that gets re-enrolled later starts from a clean slate. Requires
// the operator to retype the device's exact name as a second, explicit
// confirmation beyond the panel's own confirm dialog.
func (a *API) handleDecommissionDevice(w http.ResponseWriter, r *http.Request, id string) {
	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	var req decommissionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if strings.TrimSpace(req.ConfirmName) != device.Name {
		writeError(w, http.StatusBadRequest, "le nom saisi ne correspond pas à celui de l'appareil")
		return
	}

	wasOnline := a.Hub.SendCommand(id, ws.Envelope{Type: ws.TypeUninstall})

	if err := a.deleteDeviceAndReclaim(id); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": "true", "agent_notified": wasOnline})
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

// handleDeleteSnapshot removes one backup on its own, outside the usual
// retention rotation - an operator deciding a particular snapshot is
// pointless (a test run, one taken right before a big cleanup) shouldn't
// have to wait for it to age out. A snapshot still running is refused: it
// has no manifest yet, and deleting its DB row out from under an agent
// mid-upload would strand the queue slot models.DeleteSnapshot alone
// doesn't know to release.
func (a *API) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request, deviceID, snapshotID string) {
	snap, err := models.GetSnapshot(a.DB, snapshotID)
	if err != nil || snap.DeviceID != deviceID {
		writeError(w, http.StatusNotFound, "sauvegarde introuvable")
		return
	}
	if snap.Status == models.SnapshotStatusRunning {
		writeError(w, http.StatusConflict, "impossible de supprimer une sauvegarde en cours")
		return
	}

	if err := storage.DeleteManifest(snap.ManifestPath); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	if err := models.DeleteSnapshot(a.DB, snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	_ = models.AddEvent(a.DB, &deviceID, models.EventLevelInfo, "Sauvegarde supprimée depuis le panneau.")

	// Runs after the response is sent for the same reason as retention
	// rotation: GC walks the whole chunk store, which can take a while on
	// a large repository and shouldn't make the delete button hang.
	go func() {
		store := a.Store.Get()
		remaining, err := models.AllManifestPaths(a.DB)
		if err == nil {
			_, _, _ = store.GarbageCollect(remaining, storage.GraceCutoff())
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

type reassignSnapshotRequest struct {
	TargetDeviceID string `json:"target_device_id"`
}

// handleReassignSnapshot moves a snapshot to a different device so it can
// be restored there - the scenario this exists for is a machine that died
// or was replaced: its last backup is still good, but a snapshot could
// previously only ever be restored on the exact device that created it.
// Chunks are content-addressed and already shared across the whole fleet,
// so there's nothing to re-upload; this just repoints the snapshot's
// ownership. Decommissioning a device deletes its snapshots outright (see
// handleDecommissionDevice), so anything worth keeping has to be
// reassigned first, while the old device record still exists.
func (a *API) handleReassignSnapshot(w http.ResponseWriter, r *http.Request, deviceID, snapshotID string) {
	snap, err := models.GetSnapshot(a.DB, snapshotID)
	if err != nil || snap.DeviceID != deviceID {
		writeError(w, http.StatusNotFound, "sauvegarde introuvable")
		return
	}
	if snap.Status == models.SnapshotStatusRunning {
		writeError(w, http.StatusConflict, "impossible de déplacer une sauvegarde en cours")
		return
	}

	var req reassignSnapshotRequest
	if err := decodeJSON(r, &req); err != nil || req.TargetDeviceID == "" {
		writeError(w, http.StatusBadRequest, "appareil cible manquant")
		return
	}
	if req.TargetDeviceID == deviceID {
		writeError(w, http.StatusBadRequest, "l'appareil cible doit être différent de l'appareil actuel")
		return
	}
	target, err := models.GetDevice(a.DB, req.TargetDeviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil cible introuvable")
		return
	}

	if err := models.ReassignSnapshot(a.DB, snapshotID, req.TargetDeviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	sourceName := deviceID
	if source, err := models.GetDevice(a.DB, deviceID); err == nil {
		sourceName = source.Name
	}
	_ = models.AddEvent(a.DB, &deviceID, models.EventLevelInfo,
		fmt.Sprintf("Sauvegarde déplacée vers l'appareil %q pour restauration.", target.Name))
	_ = models.AddEvent(a.DB, &req.TargetDeviceID, models.EventLevelInfo,
		fmt.Sprintf("Sauvegarde reçue depuis l'appareil %q, disponible pour restauration.", sourceName))

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}
