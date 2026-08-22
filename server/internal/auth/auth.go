// Package auth handles administrator password hashing, signed session
// cookies for the web panel, and the high-entropy token hashing used for
// device secrets and enrollment keys.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// --- Administrator passwords -------------------------------------------------

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func VerifyPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// --- Session cookies ---------------------------------------------------------

type sessionPayload struct {
	UserID string `json:"uid"`
	Exp    int64  `json:"exp"`
}

const sessionTTL = 7 * 24 * time.Hour

type SessionSigner struct {
	secret []byte
}

func NewSessionSigner(secret string) *SessionSigner {
	return &SessionSigner{secret: []byte(secret)}
}

func (s *SessionSigner) sign(data []byte) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *SessionSigner) MakeCookie(userID string) (string, error) {
	payload := sessionPayload{UserID: userID, Exp: time.Now().Add(sessionTTL).Unix()}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	sig := s.sign([]byte(encoded))
	return encoded + "." + sig, nil
}

var ErrInvalidSession = errors.New("invalid or expired session")

func (s *SessionSigner) ReadCookie(cookie string) (string, error) {
	dot := -1
	for i := len(cookie) - 1; i >= 0; i-- {
		if cookie[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return "", ErrInvalidSession
	}
	encoded, sig := cookie[:dot], cookie[dot+1:]
	expected := s.sign([]byte(encoded))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", ErrInvalidSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrInvalidSession
	}
	var payload sessionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", ErrInvalidSession
	}
	if time.Now().Unix() > payload.Exp {
		return "", ErrInvalidSession
	}
	return payload.UserID, nil
}

// --- High-entropy tokens (enrollment keys, device secrets) ------------------

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func VerifyToken(token, hash string) bool {
	return hmac.Equal([]byte(HashToken(token)), []byte(hash))
}
