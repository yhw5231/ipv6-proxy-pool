package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"ipv6-proxy-pool/internal/config"
)

// errTokenNotFound is returned when a named token is missing.
var errTokenNotFound = errors.New("token not found")

// tokenStore keeps the live bearer tokens: the legacy single token plus any
// number of named client tokens. Mutations apply immediately (no restart
// needed) and are persisted to config.json by the server.
type tokenStore struct {
	mu     sync.Mutex
	legacy string
	named  map[string]string // name -> token
}

func newTokenStore(legacy string, initial []config.NamedToken) *tokenStore {
	store := &tokenStore{legacy: legacy, named: make(map[string]string, len(initial))}
	for _, named := range initial {
		if name := strings.TrimSpace(named.Name); name != "" && strings.TrimSpace(named.Token) != "" {
			store.named[name] = named.Token
		}
	}
	return store
}

// accepts reports whether a raw bearer token is valid: the legacy token or any
// named token.
func (s *tokenStore) accepts(bearer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.legacy != "" && bearer == s.legacy {
		return true
	}
	for _, token := range s.named {
		if token == bearer {
			return true
		}
	}
	return false
}

// any reports whether token protection is enabled at all.
func (s *tokenStore) any() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legacy != "" || len(s.named) > 0
}

// list returns the named tokens sorted by name.
func (s *tokenStore) list() []config.NamedToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]config.NamedToken, 0, len(s.named))
	for name, token := range s.named {
		result = append(result, config.NamedToken{Name: name, Token: token})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// add stores a new named token, rejecting blank names and duplicates.
func (s *tokenStore) add(name, token string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("token name must not be empty")
	}
	if len(token) < 8 {
		return errors.New("token must be at least 8 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.named[name]; exists {
		return fmt.Errorf("token %q already exists", name)
	}
	s.named[name] = token
	return nil
}

// replace swaps the token value for an existing name.
func (s *tokenStore) replace(name, token string) error {
	if len(token) < 8 {
		return errors.New("token must be at least 8 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.named[name]; !exists {
		return fmt.Errorf("%w: %q", errTokenNotFound, name)
	}
	s.named[name] = token
	return nil
}

// remove deletes a named token, reporting whether it existed.
func (s *tokenStore) remove(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.named[name]; !exists {
		return false
	}
	delete(s.named, name)
	return true
}

// randomToken mints a 128-bit hex bearer token.
func randomToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

type tokenRequest struct {
	Name string `json:"name"`
}

// listTokens exposes every named token with its full value so an operator can
// hand it to clients directly.
func (s *server) listTokens(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tokens": s.tokens.list()})
}

func (s *server) createToken(w http.ResponseWriter, r *http.Request) {
	var request tokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("name must not be empty"))
		return
	}
	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.tokens.add(request.Name, token); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.persistTokens(); err != nil {
		s.tokens.remove(request.Name)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, config.NamedToken{Name: request.Name, Token: token})
}

func (s *server) rotateToken(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.tokens.replace(name, token); err != nil {
		if errors.Is(err, errTokenNotFound) {
			writeError(w, http.StatusNotFound, err)
		} else {
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	if err := s.persistTokens(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, config.NamedToken{Name: name, Token: token})
}

func (s *server) deleteToken(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if !s.tokens.remove(name) {
		writeError(w, http.StatusNotFound, fmt.Errorf("%w: %q", errTokenNotFound, name))
		return
	}
	if err := s.persistTokens(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
}

// persistTokens writes the current named tokens back into config.json while
// keeping every other running setting untouched.
func (s *server) persistTokens() error {
	if strings.TrimSpace(s.options.ConfigPath) == "" {
		return errors.New("configuration persistence is not enabled")
	}
	s.options.RuntimeConfig.Admin.Tokens = s.tokens.list()
	return config.SaveAtomic(s.options.ConfigPath, s.options.RuntimeConfig)
}
