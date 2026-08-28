package api

import (
	"net/http"

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

// handleDashboardStorage reports total space used across the whole NAS.
// Isolated in its own endpoint on purpose: this is the one figure on the
// dashboard that can be genuinely slow to compute fresh (a full walk of
// every device's folder, cached for 30s - see filestore.UsedBytes), and
// it must never hold up the rest of the page while it works.
func (a *API) handleDashboardStorage(w http.ResponseWriter, r *http.Request) {
	usedBytes, err := a.Store.Get().UsedBytes()
	if err != nil {
		usedBytes = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{"storage_used_bytes": usedBytes})
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
