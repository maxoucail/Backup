package api

import (
	"net/http"
	"strings"
	"time"

	"backup-server/internal/auth"
	"backup-server/internal/idgen"
	"backup-server/internal/models"
)

type createEnrollmentKeyRequest struct {
	Label      string `json:"label"`
	TTLMinutes int    `json:"ttl_minutes"`
}

// handleCreateEnrollmentKey generates a one-time key an operator types into
// a freshly installed agent (along with the server address) to pair it
// permanently. Only the hash is stored; the plaintext token is returned
// exactly once, in this response.
func (a *API) handleCreateEnrollmentKey(w http.ResponseWriter, r *http.Request) {
	var req createEnrollmentKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	ttl := time.Duration(req.TTLMinutes) * time.Minute
	if req.TTLMinutes <= 0 {
		ttl = 30 * time.Minute
	}

	token := idgen.Token(24)
	key, err := models.CreateEnrollmentKey(a.DB, auth.HashToken(token), strings.TrimSpace(req.Label), ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          key.ID,
		"token":       token,
		"expires_at":  key.ExpiresAt,
		"server_host": a.agentHost(r),
	})
}

func (a *API) handleListEnrollmentKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := models.ListEnrollmentKeys(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleGetStaticEnrollmentKey returns the server's permanent enrollment
// key, generating one the first time it's requested. Unlike the one-time
// keys above, this one is meant to be re-read from the panel indefinitely,
// so - unusually for this codebase - it's returned every time, not just
// once at creation.
func (a *API) handleGetStaticEnrollmentKey(w http.ResponseWriter, r *http.Request) {
	token, err := models.GetOrCreateStaticEnrollmentToken(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "server_host": a.agentHost(r)})
}

// handleRegenerateStaticEnrollmentKey replaces the permanent key with a
// new one, e.g. if the old one leaked or an operator just wants to rotate
// it. Devices already enrolled are unaffected; only future enrollments
// with the old value stop working.
func (a *API) handleRegenerateStaticEnrollmentKey(w http.ResponseWriter, r *http.Request) {
	token, err := models.RegenerateStaticEnrollmentToken(a.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	_ = models.AddEvent(a.DB, nil, models.EventLevelWarning, "Clé d'enrôlement permanente régénérée depuis le panneau.")
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "server_host": a.agentHost(r)})
}

type agentEnrollRequest struct {
	Token        string `json:"token"`
	Name         string `json:"name"`
	Hostname     string `json:"hostname"`
	OSName       string `json:"os_name"`
	OSVersion    string `json:"os_version"`
	AgentVersion string `json:"agent_version"`
}

// handleAgentEnroll is the only unauthenticated agent-facing endpoint: a
// fresh install exchanges an enrollment key - either a one-time key from
// the panel's "Enrôler un nouvel appareil" form, or the server's permanent
// key from Paramètres - for a permanent device identity (device_id +
// device_secret). The secret is shown to the agent exactly once here and
// never again; the server only ever stores its hash from this point on.
func (a *API) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	var req agentEnrollRequest
	if err := decodeJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "clé d'enrôlement manquante")
		return
	}

	key, keyErr := models.FindUsableEnrollmentKeyByHash(a.DB, auth.HashToken(req.Token))
	usingStaticKey := false
	if keyErr != nil {
		matched, err := models.MatchesStaticEnrollmentToken(a.DB, req.Token)
		if err != nil || !matched {
			writeError(w, http.StatusUnauthorized, "clé d'enrôlement invalide, expirée ou déjà utilisée")
			return
		}
		usingStaticKey = true
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.Hostname
	}
	secret := idgen.Token(36)
	device, err := models.CreateDevice(a.DB, name, req.Hostname, req.OSName, req.OSVersion, req.AgentVersion,
		auth.HashToken(secret), clientIP(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	if !usingStaticKey {
		_ = models.MarkEnrollmentKeyUsed(a.DB, key.ID, device.ID)
	}
	_ = models.AddEvent(a.DB, &device.ID, models.EventLevelInfo, "Appareil enrôlé.")

	writeJSON(w, http.StatusCreated, map[string]string{
		"device_id":     device.ID,
		"device_secret": secret,
	})
}
