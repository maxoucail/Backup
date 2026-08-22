// Package api implements the two REST surfaces the server exposes: the
// agent-facing data plane (enrollment, chunk upload/download, manifests,
// snapshot lifecycle - authenticated with a per-device secret) and the
// panel-facing control surface (login, device/settings management -
// authenticated with a signed session cookie). Remote commands
// (backup-now, restore) are dispatched to connected agents over the
// WebSocket hub in internal/ws.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"backup-server/internal/auth"
	"backup-server/internal/storage"
	"backup-server/internal/ws"
)

type API struct {
	DB       *sql.DB
	Store    *storage.Holder
	Hub      *ws.Hub
	Sessions *auth.SessionSigner
}

func New(db *sql.DB, store *storage.Holder, hub *ws.Hub, sessions *auth.SessionSigner) *API {
	return &API{DB: db, Store: store, Hub: hub, Sessions: sessions}
}

// --- small JSON helpers -----------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}
