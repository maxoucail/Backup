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
	"net"
	"net/http"

	"backup-server/internal/auth"
	"backup-server/internal/storage"
	"backup-server/internal/ws"
)

type API struct {
	DB           *sql.DB
	Store        *storage.Holder
	Hub          *ws.Hub
	Sessions     *auth.SessionSigner
	DownloadsDir string
	// AgentPort is the port agents must be told to connect to. The panel
	// and the agent listener are different ports (see RegisterPanel vs
	// RegisterAgent), so the enrollment response can't just echo back
	// whatever host:port the admin's browser happened to use to reach the
	// panel - it has to swap in this port explicitly.
	AgentPort string
}

func New(db *sql.DB, store *storage.Holder, hub *ws.Hub, sessions *auth.SessionSigner, downloadsDir, agentPort string) *API {
	return &API{DB: db, Store: store, Hub: hub, Sessions: sessions, DownloadsDir: downloadsDir, AgentPort: agentPort}
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

// agentHost returns the host:port an agent should be told to connect to:
// whatever hostname/IP the admin's browser used to reach the panel (so it
// still works whether that's a DNS name, a LAN IP, or "localhost"), with
// the port swapped for the agent listener's port instead of the panel's.
func (a *API) agentHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	return net.JoinHostPort(host, a.AgentPort)
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}
