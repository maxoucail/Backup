package api

import (
	"context"
	"net/http"
	"regexp"

	"backup-server/internal/auth"
	"backup-server/internal/models"
)

type ctxKey int

const (
	ctxUserID ctxKey = iota
	ctxDeviceID
)

const sessionCookieName = "backup_session"

// requireSession protects panel API endpoints. It returns 401 JSON (the
// frontend JS redirects to /login on that status) rather than an HTTP
// redirect, since these are all fetch()-consumed endpoints.
func (a *API) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentification requise")
			return
		}
		userID, err := a.Sessions.ReadCookie(cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "session invalide ou expirée")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		next(w, r.WithContext(ctx))
	}
}

func userIDFromContext(r *http.Request) string {
	v, _ := r.Context().Value(ctxUserID).(string)
	return v
}

// requireDevice protects agent-facing endpoints using the per-device
// secret issued at enrollment. Credentials travel as headers rather than
// query params so they never end up logged in URLs.
func (a *API) requireDevice(next func(w http.ResponseWriter, r *http.Request, deviceID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.Header.Get("X-Device-Id")
		secret := r.Header.Get("X-Device-Secret")
		if deviceID == "" || secret == "" {
			writeError(w, http.StatusUnauthorized, "identifiants d'appareil manquants")
			return
		}
		device, err := models.GetDevice(a.DB, deviceID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "appareil inconnu")
			return
		}
		if !auth.VerifyToken(secret, device.SecretHash) {
			writeError(w, http.StatusUnauthorized, "secret d'appareil invalide")
			return
		}
		next(w, r, deviceID)
	}
}

var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// isValidHash guards every place a client-supplied hash reaches the
// filesystem (chunk store paths are derived directly from it) so a
// malformed or malicious value can never cause a path traversal.
func isValidHash(h string) bool {
	return hashPattern.MatchString(h)
}
