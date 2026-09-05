package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// session records a signed-in console browser. Only the expiry is kept; the
// credential check happens once at login time.
type session struct {
	expiresAt time.Time
}

// sessionStore keeps in-memory web console sessions. Entries are removed as
// they are found expired, so no background sweeper is needed; restarting the
// service clears every session.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]session)}
}

// create mints a random 256-bit cookie value for a fresh session.
func (s *sessionStore) create() (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := hex.EncodeToString(raw)
	expiresAt := time.Now().Add(sessionTTL)
	s.mu.Lock()
	s.removeExpiredLocked()
	s.sessions[token] = session{expiresAt: expiresAt}
	s.mu.Unlock()
	return token, expiresAt, nil
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.sessions[token]
	if !exists {
		return false
	}
	if now.After(entry.expiresAt) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func (s *sessionStore) removeExpiredLocked() {
	now := time.Now()
	for token, entry := range s.sessions {
		if now.After(entry.expiresAt) {
			delete(s.sessions, token)
		}
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// login validates the console credentials and issues a session cookie. Both
// fields are compared via their SHA-256 digests so the comparison runs in
// constant time regardless of input length.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var request loginRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	username, password := s.options.RuntimeConfig.Admin.WebUICredentials()
	if !constantTimeEqual(request.Username, username) || !constantTimeEqual(request.Password, password) {
		writeError(w, http.StatusUnauthorized, errInvalidCredentials)
		return
	}

	token, expiresAt, err := s.sessions.create()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": username})
}

// sessionInfo lets the console decide whether to show the login screen or the
// panel. It always answers 200 so the frontend can branch on the payload.
func (s *server) sessionInfo(w http.ResponseWriter, r *http.Request) {
	username, _ := s.options.RuntimeConfig.Admin.WebUICredentials()
	authenticated := s.validSession(r)
	if !authenticated {
		username = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": authenticated, "username": username})
}

// logout drops the caller's session and clears the cookie. It succeeds even
// without a valid session so a stale console can always sign out.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.destroy(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (s *server) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.sessions.valid(cookie.Value)
}

// constantTimeEqual compares two strings without leaking length or content.
func constantTimeEqual(input, expected string) bool {
	inputHash := sha256.Sum256([]byte(input))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(inputHash[:], expectedHash[:]) == 1
}
