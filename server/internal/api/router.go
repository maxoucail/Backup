package api

import "net/http"

// Register wires every route onto mux. Two trust domains share this mux:
// session-cookie-authenticated panel endpoints under /api/... and
// device-secret-authenticated agent endpoints under /api/agent/... plus
// /ws/agent. Go 1.22's method+pattern ServeMux keeps this a flat, readable
// list instead of needing a third-party router.
func (a *API) Register(mux *http.ServeMux) {
	// Public.
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/agent/enroll", a.handleAgentEnroll)
	mux.HandleFunc("GET /api/downloads", a.handleListDownloads)
	mux.HandleFunc("GET /downloads/{name}", a.handleDownloadFile)

	// Panel session.
	mux.HandleFunc("POST /api/auth/logout", a.requireSession(a.handleLogout))
	mux.HandleFunc("GET /api/auth/me", a.requireSession(a.handleMe))
	mux.HandleFunc("POST /api/account/credentials", a.requireSession(a.handleChangeCredentials))

	mux.HandleFunc("GET /api/dashboard", a.requireSession(a.handleDashboard))
	mux.HandleFunc("GET /api/events", a.requireSession(a.handleListEvents))

	mux.HandleFunc("GET /api/devices", a.requireSession(a.handleListDevices))
	mux.HandleFunc("GET /api/devices/{id}", a.requireSession(a.withPathID(a.handleGetDevice)))
	mux.HandleFunc("PATCH /api/devices/{id}", a.requireSession(a.withPathID(a.handleUpdateDevice)))
	mux.HandleFunc("DELETE /api/devices/{id}", a.requireSession(a.withPathID(a.handleDeleteDevice)))
	mux.HandleFunc("POST /api/devices/{id}/decommission", a.requireSession(a.withPathID(a.handleDecommissionDevice)))
	mux.HandleFunc("POST /api/devices/{id}/backup-now", a.requireSession(a.withPathID(a.handleBackupNow)))
	mux.HandleFunc("POST /api/devices/{id}/restore", a.requireSession(a.withPathID(a.handleRestore)))
	mux.HandleFunc("POST /api/devices/{id}/cancel", a.requireSession(a.withPathID(a.handleCancelJob)))

	mux.HandleFunc("GET /api/settings", a.requireSession(a.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", a.requireSession(a.handleUpdateSettings))
	mux.HandleFunc("POST /api/settings/test-storage", a.requireSession(a.handleTestStorage))

	mux.HandleFunc("POST /api/enrollment-keys", a.requireSession(a.handleCreateEnrollmentKey))
	mux.HandleFunc("GET /api/enrollment-keys", a.requireSession(a.handleListEnrollmentKeys))

	mux.HandleFunc("POST /api/downloads/upload", a.requireSession(a.handleUploadDownload))
	mux.HandleFunc("DELETE /api/downloads/{name}", a.requireSession(func(w http.ResponseWriter, r *http.Request) {
		a.handleDeleteDownload(w, r, r.PathValue("name"))
	}))

	// Agent data plane.
	mux.HandleFunc("GET /api/agent/config", a.requireDevice(a.handleAgentGetConfig))
	mux.HandleFunc("POST /api/agent/snapshots", a.requireDevice(a.handleAgentCreateSnapshot))
	mux.HandleFunc("POST /api/agent/snapshots/{id}/check-chunks", a.requireDevice(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		a.handleAgentCheckChunks(w, r, deviceID)
	}))
	mux.HandleFunc("PUT /api/agent/chunks/{hash}", a.requireDevice(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		a.handleAgentUploadChunk(w, r, deviceID, r.PathValue("hash"))
	}))
	mux.HandleFunc("GET /api/agent/chunks/{hash}", a.requireDevice(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		a.handleAgentDownloadChunk(w, r, deviceID, r.PathValue("hash"))
	}))
	mux.HandleFunc("POST /api/agent/snapshots/{id}/manifest", a.requireDevice(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		a.handleAgentSubmitManifest(w, r, deviceID, r.PathValue("id"))
	}))
	mux.HandleFunc("GET /api/agent/snapshots/{id}/manifest", a.requireDevice(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		a.handleAgentGetManifest(w, r, deviceID, r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/agent/snapshots/{id}/finish", a.requireDevice(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		a.handleAgentFinishSnapshot(w, r, deviceID, r.PathValue("id"))
	}))

	// Agent control-plane WebSocket.
	mux.HandleFunc("GET /ws/agent", a.requireDevice(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		a.Hub.ServeAgent(w, r, deviceID, clientIP(r))
	}))
}

// withPathID adapts a handler that takes the "id" path value as a plain
// argument, for endpoints under panel session auth (which has no
// device-style trailing-argument convention of its own).
func (a *API) withPathID(next func(w http.ResponseWriter, r *http.Request, id string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r, r.PathValue("id"))
	}
}
