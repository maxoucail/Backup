package api

import (
	"net/http"

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
