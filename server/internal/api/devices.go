package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"backup-server/internal/filestore"
	"backup-server/internal/models"
	"backup-server/internal/ws"
)

type deviceView struct {
	models.Device
	Online         bool             `json:"online"`
	LatestSnapshot *models.Snapshot `json:"latest_snapshot,omitempty"`
	SnapshotCount  int              `json:"snapshot_count"`
	// StorageDir is where this machine's files actually sit on the NAS.
	// Restoring means opening this folder and copying back what's needed,
	// so the panel shows it rather than making an operator guess.
	StorageDir string `json:"storage_dir"`
	// EffectiveIntervalMinutes is this device's actual backup interval -
	// its own override if it has one, the server default otherwise (see
	// effectivePolicy). Sent so the panel can estimate when the next
	// backup should happen (latest_snapshot.started_at + this many
	// minutes) without having to separately fetch and merge the server's
	// default settings itself. An estimate only: the real schedule,
	// catch-ups included, is decided by the agent, which this server-only
	// figure knows nothing about.
	EffectiveIntervalMinutes int `json:"effective_interval_minutes"`
}

func (a *API) toDeviceView(d models.Device) deviceView {
	latest, _ := models.LatestSnapshotForDevice(a.DB, d.ID)
	count, _ := models.CountSuccessfulSnapshotsForDevice(a.DB, d.ID)
	interval := 0
	if settings, err := models.GetSettings(a.DB); err == nil {
		interval, _, _ = effectivePolicy(&d, settings)
	}
	return deviceView{
		Device:                   d,
		Online:                   a.Hub.IsOnline(d.ID),
		LatestSnapshot:           latest,
		SnapshotCount:            count,
		StorageDir:               a.Store.Get().DeviceDir(d.ID, d.Name),
		EffectiveIntervalMinutes: interval,
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
	// Decoded twice on purpose: once into the typed request, and once into
	// a bare map so we can tell "field omitted" from "field explicitly set
	// to null". A pointer field is nil in both cases, and treating the
	// first as the second is what made a simple rename wipe the machine's
	// configured folder list.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	var req updateDeviceRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(body, &present); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if req.Name != nil && *req.Name != "" {
		// The machine's folder on the NAS is named after the device, so
		// the rename has to happen on disk too - otherwise the existing
		// backup is stranded under the old name and the next run starts
		// from scratch.
		current, err2 := models.GetDevice(a.DB, id)
		if err2 != nil {
			writeError(w, http.StatusNotFound, "appareil introuvable")
			return
		}
		store := a.Store.Get()
		oldDir := store.DeviceDir(current.ID, current.Name)
		newDir := store.DeviceDir(current.ID, *req.Name)
		if err := store.RenameDevice(oldDir, newDir); err != nil {
			writeError(w, http.StatusConflict, "renommage du dossier de sauvegarde impossible: "+err.Error())
			return
		}
		if err := models.RenameDevice(a.DB, id, *req.Name); err != nil {
			// Put the folder back: the name in the database is what every
			// later path is derived from, so the two must not diverge.
			_ = store.RenameDevice(newDir, oldDir)
			writeError(w, http.StatusInternalServerError, "erreur serveur")
			return
		}
	}

	set := map[string]any{}
	if _, ok := present["interval_minutes"]; ok {
		set["interval_minutes"] = nullableInt(req.IntervalMinutes)
	}
	if _, ok := present["retention_count"]; ok {
		// Below two, a machine's only copy is the mirror being overwritten;
		// see filestore and scheduler.minRetention.
		if req.RetentionCount != nil && *req.RetentionCount < 2 {
			writeError(w, http.StatusBadRequest, "le nombre d'états à conserver doit être au moins 2")
			return
		}
		set["retention_count"] = nullableInt(req.RetentionCount)
	}
	if _, ok := present["backup_paths"]; ok {
		pathsJSON := ""
		if req.BackupPaths != nil {
			b, _ := json.Marshal(req.BackupPaths)
			pathsJSON = string(b)
		}
		set["backup_paths"] = pathsJSON
	}
	if err := models.UpdateDevicePolicy(a.DB, id, set); err != nil {
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

// deleteDeviceAndReclaim forgets a machine and deletes its folder on the
// NAS, previous versions included. Nothing is shared between machines -
// each one owns its own tree - so there is nothing left to collect
// afterwards.
func (a *API) deleteDeviceAndReclaim(id string) error {
	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		return err
	}
	store := a.Store.Get()
	deviceDir := store.DeviceDir(device.ID, device.Name)
	if err := models.DeleteDevice(a.DB, id); err != nil {
		return err
	}
	// Deleting several gigabytes over a network mount can take a while;
	// the panel shouldn't hang on it, and the database is already the
	// authority on which machines exist.
	go func() {
		if err := store.RemoveDevice(deviceDir); err != nil {
			log.Printf("suppression du dossier %s: %v", deviceDir, err)
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

func (a *API) handleCancelJob(w http.ResponseWriter, r *http.Request, id string) {
	if !a.Hub.SendCommand(id, ws.Envelope{Type: ws.TypeCancel}) {
		writeError(w, http.StatusConflict, "l'appareil n'est pas connecté")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"ok": "true"})
}

// handleDeleteSnapshot removes one line from a machine's backup history.
//
// This is a history entry, not the files: the backup itself is the
// machine's folder on the NAS, which the next backup keeps up to date. To
// free actual disk space, delete a previous version instead (see
// handleDeleteVersion). A snapshot still running is refused - deleting its
// row out from under an agent mid-upload would strand the queue slot
// models.DeleteSnapshot alone doesn't know to release.
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
	if err := models.DeleteSnapshot(a.DB, snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	_ = models.AddEvent(a.DB, &deviceID, models.EventLevelInfo, "Ligne d'historique de sauvegarde supprimée depuis le panneau.")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// versionView describes one previous version of a machine's files as the
// panel shows it: a name that reads as a date, the folder to open on the
// NAS, and what it costs on disk.
type versionView struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// handleListVersions reports what's actually on the NAS for one machine:
// the live mirror (its up-to-date copy) plus every previous version kept
// by retention. This is the panel's answer to "where do I go to get my
// files back", so it deals in folders on disk, not database rows.
func (a *API) handleListVersions(w http.ResponseWriter, r *http.Request, id string) {
	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	store := a.Store.Get()
	deviceDir := store.DeviceDir(device.ID, device.Name)

	versions := make([]versionView, 0)
	for _, name := range store.ListVersions(deviceDir) {
		dir, err := store.VersionDir(deviceDir, name)
		if err != nil {
			continue
		}
		versions = append(versions, versionView{
			Name:      name,
			Path:      dir,
			SizeBytes: store.DeviceUsedBytes(dir),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"storage_dir":     deviceDir,
		"used_bytes":      store.CurrentUsedBytes(deviceDir),
		"versions":        versions,
		"versions_folder": filestore.VersionsDirName,
	})
}

type deleteFilesRequest struct {
	ConfirmName string `json:"confirm_name"`
}

// handleDeleteVersion drops one previous version. The live mirror is never
// touched here - see handleDeleteCurrent for that, a deliberately separate
// and more heavily guarded action.
//
// Requires retyping the exact version name being deleted, checked here
// server-side rather than trusted to the panel's own confirmation dialog:
// this deletes real files on the NAS with no undo, so the guard has to
// hold even against a direct API call, not just a careless click.
func (a *API) handleDeleteVersion(w http.ResponseWriter, r *http.Request, id, name string) {
	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	var req deleteFilesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if strings.TrimSpace(req.ConfirmName) != name {
		writeError(w, http.StatusBadRequest, "le nom saisi ne correspond pas à la version")
		return
	}
	store := a.Store.Get()
	deviceDir := store.DeviceDir(device.ID, device.Name)
	if err := store.RemoveVersion(deviceDir, name); err != nil {
		writeError(w, http.StatusBadRequest, "suppression impossible: "+err.Error())
		return
	}
	_ = models.AddEvent(a.DB, &id, models.EventLevelInfo,
		fmt.Sprintf("Ancienne version %q supprimée depuis le panneau.", name))
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleDeleteCurrent wipes a machine's live mirror - its up-to-date
// backup - on explicit operator request. Unlike handleDeleteVersion, this
// does touch the current backup, on purpose: the operator wants a clean
// slate (a live copy suspected corrupted or infected, say) without losing
// the historical versions already kept in _anciennes_versions, which this
// never touches.
//
// Guarded the same way as decommissioning a device (see
// handleDecommissionDevice): retyping the exact device name. This is the
// most destructive single action this panel offers short of deleting the
// device outright, so it gets the same defense in depth.
func (a *API) handleDeleteCurrent(w http.ResponseWriter, r *http.Request, id string) {
	device, err := models.GetDevice(a.DB, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "appareil introuvable")
		return
	}
	var req deleteFilesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if strings.TrimSpace(req.ConfirmName) != device.Name {
		writeError(w, http.StatusBadRequest, "le nom saisi ne correspond pas à celui de l'appareil")
		return
	}
	store := a.Store.Get()
	if err := store.RemoveCurrent(store.DeviceDir(device.ID, device.Name)); err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	_ = models.AddEvent(a.DB, &id, models.EventLevelWarning,
		"Sauvegarde actuelle supprimée depuis le panneau : la prochaine sauvegarde repartira de zéro pour cet appareil.")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// nullableInt turns an absent value into SQL NULL, which is how a device
// says "use the server default" for a policy field.
func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
