package api

import (
	"net/http"

	"backup-server/internal/models"
)

type dailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

func (a *API) handleDashboard(w http.ResponseWriter, r *http.Request) {
	devices, err := models.ListDevices(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	views := make([]deviceView, 0, len(devices))
	online := 0
	for _, d := range devices {
		v := a.toDeviceView(d)
		if v.Online {
			online++
		}
		views = append(views, v)
	}

	store := a.Store.Get()
	usedBytes, err := store.UsedBytes()
	if err != nil {
		usedBytes = 0
	}

	events, err := models.ListRecentEvents(a.DB, 30)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	rows, err := a.DB.Query(`SELECT date(started_at) d, COUNT(*) FROM snapshots
		WHERE started_at > datetime('now', '-14 days') GROUP BY d ORDER BY d`)
	var series []dailyCount
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var dc dailyCount
			if err := rows.Scan(&dc.Date, &dc.Count); err == nil {
				series = append(series, dc)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"devices_total":      len(devices),
		"devices_online":     online,
		"storage_used_bytes": usedBytes,
		"devices":            views,
		"recent_events":      events,
		"backups_per_day":    series,
	})
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
