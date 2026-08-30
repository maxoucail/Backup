package api

import (
	"net/http"
	"time"

	"backup-server/internal/models"
)

type dailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// The dashboard used to be one endpoint that gathered everything -
// devices, storage usage, events, the daily chart - before answering at
// all. That meant the single slowest piece decided how long the whole
// page stayed blank, and the slowest piece by far is storage usage: it
// walks the entire NAS tree (see filestore.UsedBytes), which on a real
// network mount is a stat() per file over the network, not a local disk
// operation. A handful of devices with real history was enough to make
// that walk take significantly longer than every other piece of this
// page combined.
//
// Split into one endpoint per section instead, so the panel can fire them
// side by side and render each card the moment its own data is back,
// rather than the page staying blank until the slowest of them finishes.

// handleDashboardDevices returns the device table: same view model as
// GET /api/devices, reused here so the dashboard's device list and
// storage-by-device chart don't need a second representation to stay in
// sync with. Purely local, indexed SQLite lookups - the fast part of the
// dashboard, and the part that actually changes on every backup.
func (a *API) handleDashboardDevices(w http.ResponseWriter, r *http.Request) {
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

// handleDashboardStorage reports total space used and free across the
// whole NAS.
//
// Both are plain database reads, never live filesystem work: used space
// means walking every device's entire folder (see filestore.UsedBytes),
// which on a real network mount is a stat() per file over the network -
// far too slow to redo on every dashboard load. Free space is cheap (a
// single statfs() - see filestore.Store.FreeBytes) but is still served
// from the same cached figure for consistency, refreshed on its own,
// faster cadence by the scheduler (see scheduler.refreshStorageFree vs
// refreshStorageUsage). Either way this handler only ever reads what the
// scheduler last wrote, so it's always fast.
func (a *API) handleDashboardStorage(w http.ResponseWriter, r *http.Request) {
	usedBytes, usedAt, err := models.GetStorageUsage(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	freeBytes, freeAt, err := models.GetStorageFree(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	resp := map[string]any{"storage_used_bytes": usedBytes, "storage_free_bytes": freeBytes}
	if !usedAt.IsZero() {
		resp["used_computed_at"] = usedAt.UTC().Format(time.RFC3339)
	}
	if !freeAt.IsZero() {
		resp["free_computed_at"] = freeAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDashboardBackupsPerDay feeds the "backups per day" chart.
func (a *API) handleDashboardBackupsPerDay(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(`SELECT date(started_at) d, COUNT(*) FROM snapshots
		WHERE started_at > datetime('now', '-14 days') GROUP BY d ORDER BY d`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	defer rows.Close()
	series := make([]dailyCount, 0)
	for rows.Next() {
		var dc dailyCount
		if err := rows.Scan(&dc.Date, &dc.Count); err == nil {
			series = append(series, dc)
		}
	}
	writeJSON(w, http.StatusOK, series)
}

func (a *API) handleListEvents(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	var (
		events []models.Event
		err    error
	)
	if deviceID != "" {
		events, err = models.ListEventsForDevice(a.DB, deviceID, 300)
	} else {
		events, err = models.ListRecentEvents(a.DB, 300)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, events)
}
