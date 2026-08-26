package api

import "net/http"

// RegisterShared wires the routes meant to be reachable from anywhere a
// workstation might be (installer downloads): not sensitive, so it's
// registered on both the panel and the agent listener rather than forcing
// a specific one - whichever port an operator's firewall rules leave open
// to a given machine, the download page still works from it.
func (a *API) RegisterShared(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/downloads", a.handleListDownloads)
	mux.HandleFunc("GET /downloads/{name}", a.handleDownloadFile)
}

// RegisterPanel wires the admin web panel: the login page's API, every
// session-cookie-authenticated management endpoint, and the HTML/static
// serving is added separately by web.Register. Meant to be bound to a
// port an operator can firewall off to an admin-only network/VLAN,
// separate from the agent listener that every backed-up workstation must
// be able to reach.
func (a *API) RegisterPanel(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
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
	mux.HandleFunc("DELETE /api/devices/{id}/snapshots/{snapshotId}", a.requireSession(func(w http.ResponseWriter, r *http.Request) {
		a.handleDeleteSnapshot(w, r, r.PathValue("id"), r.PathValue("snapshotId"))
	}))
	mux.HandleFunc("POST /api/devices/{id}/snapshots/{snapshotId}/reassign", a.requireSession(func(w http.ResponseWriter, r *http.Request) {
		a.handleReassignSnapshot(w, r, r.PathValue("id"), r.PathValue("snapshotId"))
	}))

	mux.HandleFunc("GET /api/settings", a.requireSession(a.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", a.requireSession(a.handleUpdateSettings))
	mux.HandleFunc("POST /api/settings/test-storage", a.requireSession(a.handleTestStorage))

	mux.HandleFunc("POST /api/enrollment-keys", a.requireSession(a.handleCreateEnrollmentKey))
	mux.HandleFunc("GET /api/enrollment-keys", a.requireSession(a.handleListEnrollmentKeys))
	mux.HandleFunc("GET /api/settings/enrollment-key", a.requireSession(a.handleGetStaticEnrollmentKey))
	mux.HandleFunc("POST /api/settings/enrollment-key/regenerate", a.requireSession(a.handleRegenerateStaticEnrollmentKey))

	mux.HandleFunc("POST /api/downloads/upload", a.requireSession(a.handleUploadDownload))
	mux.HandleFunc("DELETE /api/downloads/{name}", a.requireSession(func(w http.ResponseWriter, r *http.Request) {
		a.handleDeleteDownload(w, r, r.PathValue("name"))
	}))
}

// RegisterAgent wires the device-secret-authenticated data plane
// (enrollment, chunk upload/download, manifests, snapshot lifecycle) and
// the WebSocket control channel. Meant to be bound to a port reachable
// from every VLAN/subnet a backed-up workstation might be on - unlike the
// panel, this has to be broadly reachable for the product to work at all.
func (a *API) RegisterAgent(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agent/enroll", a.handleAgentEnroll)
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
