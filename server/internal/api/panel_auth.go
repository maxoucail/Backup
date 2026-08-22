package api

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"backup-server/internal/auth"
	"backup-server/internal/models"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	req.Username = strings.TrimSpace(req.Username)

	user, err := models.GetUserByUsername(a.DB, req.Username)
	if err == sql.ErrNoRows {
		// Run a bcrypt comparison anyway against a dummy hash so a
		// nonexistent-username response takes the same time as a
		// wrong-password one (no username enumeration via timing).
		auth.VerifyPassword(req.Password, "$2a$10$C6UzMDM.H6dfI/f/IKcEeO7pEStwUq3B7qc9J8dU7lqXKQnAo9Rxi")
		writeError(w, http.StatusUnauthorized, "identifiants incorrects")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}

	if !auth.VerifyPassword(req.Password, user.PasswordHash) {
		writeError(w, http.StatusUnauthorized, "identifiants incorrects")
		return
	}

	cookieVal, err := a.Sessions.MakeCookie(user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "erreur serveur")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    cookieVal,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		Expires:  time.Now().Add(7 * 24 * time.Hour),
	})
	writeJSON(w, http.StatusOK, map[string]string{"username": user.Username})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := models.GetUserByID(a.DB, userIDFromContext(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "session invalide")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": user.Username})
}

type changeCredentialsRequest struct {
	CurrentPassword string `json:"current_password"`
	NewUsername     string `json:"new_username"`
	NewPassword     string `json:"new_password"`
}

func (a *API) handleChangeCredentials(w http.ResponseWriter, r *http.Request) {
	var req changeCredentialsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}

	user, err := models.GetUserByID(a.DB, userIDFromContext(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "session invalide")
		return
	}
	if !auth.VerifyPassword(req.CurrentPassword, user.PasswordHash) {
		writeError(w, http.StatusForbidden, "mot de passe actuel incorrect")
		return
	}

	if req.NewUsername != "" && req.NewUsername != user.Username {
		if err := models.UpdateUsername(a.DB, user.ID, strings.TrimSpace(req.NewUsername)); err != nil {
			writeError(w, http.StatusConflict, "ce nom d'utilisateur est déjà pris")
			return
		}
	}

	if req.NewPassword != "" {
		if len(req.NewPassword) < 8 {
			writeError(w, http.StatusBadRequest, "le mot de passe doit contenir au moins 8 caractères")
			return
		}
		hash, err := auth.HashPassword(req.NewPassword)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "erreur serveur")
			return
		}
		if err := models.UpdateUserPassword(a.DB, user.ID, hash); err != nil {
			writeError(w, http.StatusInternalServerError, "erreur serveur")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}
