package admin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ipv6-proxy-pool/internal/config"
	"ipv6-proxy-pool/internal/lease"
	"ipv6-proxy-pool/internal/listener"
)

// errInvalidCredentials is returned for a wrong console login. The message is
// deliberately vague so it does not reveal whether the username matched.
var errInvalidCredentials = errors.New("invalid username or password")

// Pool describes the lease operations exposed by the management API.
type Pool interface {
	Acquire(id string, persistent bool) (lease.Lease, error)
	AcquirePort(id string, port int, persistent bool) (lease.Lease, error)
	Get(id string) (lease.Lease, bool)
	List() []lease.Lease
	ListAll() []lease.Lease
	StandbyCount() int
	Release(id string) bool
	ReleaseIdle() int
	Rotate(id string) (lease.Lease, error)
	Recycle(id string) (lease.Lease, error)
	SetPersistent(id string, persistent bool) (lease.Lease, error)
	Reassign(prefix string) error
}

// ProbeFunc verifies a live proxy endpoint: proxyAddr is the SOCKS5 listener,
// leaseID targets one specific lease (multiplex mode), and a non-empty
// egressURL additionally checks public egress and returns the observed exit
// IPv6 address. Tests inject a stub instead of dialing real networks.
type ProbeFunc func(ctx context.Context, proxyAddr, egressURL, leaseID string) (exitIPv6 string, err error)

// sessionTTL is how long a successful web console login stays valid. Sessions
// live in memory only, so restarting the service signs every console out.
const (
	sessionCookie = "ipv6_proxy_pool_session"
	sessionTTL    = 24 * time.Hour
)

// Options configures management endpoints, per-IPv6 listeners, and optional
// same-origin web assets.
type Options struct {
	ConfigPath      string
	RuntimeConfig   config.Config
	AdminToken      string
	Web             fs.FS
	ListenerManager *listener.Manager
	// Probe, when set, backs POST /v1/proxies:test. A nil value disables the
	// endpoint so deployments that do not wire the probe get a clear error.
	Probe ProbeFunc
}

// Handler preserves the original lease-only API constructor.
func Handler(pool Pool) http.Handler {
	return HandlerWithOptions(pool, Options{})
}

// HandlerWithOptions returns the management API and optional web UI handler.
func HandlerWithOptions(pool Pool, options Options) http.Handler {
	server := &server{pool: pool, options: options, started: time.Now(), sessions: newSessionStore()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("POST /v1/auth/login", server.login)
	mux.HandleFunc("POST /v1/auth/logout", server.logout)
	mux.HandleFunc("GET /v1/auth/session", server.sessionInfo)
	mux.HandleFunc("GET /v1/status", server.status)
	mux.HandleFunc("GET /v1/config", server.getConfig)
	mux.HandleFunc("GET /v1/config/defaults", server.getConfigDefaults)
	mux.HandleFunc("PUT /v1/config", server.saveConfig)
	mux.HandleFunc("POST /v1/proxies:test", server.testProxy)
	mux.HandleFunc("GET /v1/leases", server.listLeases)
	mux.HandleFunc("POST /v1/leases", server.createLease)
	mux.HandleFunc("GET /v1/leases/{id}", server.getLease)
	mux.HandleFunc("PATCH /v1/leases/{id}", server.updateLease)
	mux.HandleFunc("DELETE /v1/leases/{id}", server.deleteLease)
	mux.HandleFunc("POST /v1/leases/{id}/rotate", server.rotateLease)
	mux.HandleFunc("POST /v1/leases/{id}/recycle", server.recycleLease)
	mux.HandleFunc("POST /v1/leases:release-idle", server.releaseIdle)
	if options.Web != nil {
		mux.Handle("/", http.FileServer(http.FS(options.Web)))
	}

	var handler http.Handler = mux
	if options.AdminToken != "" {
		handler = server.requireToken(handler)
	}
	return handler
}

// requireToken guards every /v1/* endpoint with a constant-time comparison
// against the configured admin token. Health checks, static web assets and the
// console login flow stay open so monitoring, the local UI and sign-in remain
// reachable; a valid web console session is accepted in place of the header.
func (s *server) requireToken(next http.Handler) http.Handler {
	token := "Bearer " + s.options.AdminToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(auth), []byte(token)) != 1 && !s.validSession(r) {
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid admin token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type server struct {
	pool     Pool
	options  Options
	sessions *sessionStore
	mu       sync.Mutex
	started  time.Time
}

type createLeaseRequest struct {
	ID         string `json:"id"`
	Persistent bool   `json:"persistent"`
}

type updateLeaseRequest struct {
	Persistent *bool `json:"persistent"`
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	leases := s.pool.List()
	persistent := 0
	var totalRequests uint64
	for _, item := range leases {
		if item.Persistent {
			persistent++
		}
		totalRequests += item.Requests
	}
	standby := s.pool.StandbyCount()

	var listeners []listener.Info
	if s.options.ListenerManager != nil {
		listeners = s.options.ListenerManager.List()
	} else {
		listeners = []listener.Info{}
	}

	cfg := s.options.RuntimeConfig
	writeJSON(w, http.StatusOK, map[string]any{
		"status":               "ok",
		"uptime_seconds":       int64(time.Since(s.started).Seconds()),
		"lease_count":          len(leases),
		"persistent_count":     persistent,
		"standby_count":        standby,
		"total_requests":       totalRequests,
		"listener_count":       len(listeners),
		"listeners":            listeners,
		"min_leases":           cfg.MinLeases,
		"max_leases":           cfg.MaxLeases,
		"ipv6_prefix":          cfg.IPv6Prefix,
		"socks_mode":           cfg.SOCKS.Mode,
		"socks_listen_address": cfg.SOCKS.ListenAddress,
		"port_start":           cfg.SOCKS.PortStart,
		"port_end":             cfg.SOCKS.PortEnd,
		"always_on_ports":      cfg.SOCKS.AlwaysOnPorts,
		"idle_timeout_seconds": int64(cfg.IdleTimeout.Seconds()),
		"rotate_after_seconds": int64(cfg.RotateAfter.Seconds()),
		"rotate_requests":      cfg.RotateRequests,
		"token_required":       s.options.AdminToken != "",
	})
}

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.options.RuntimeConfig)
}

func (s *server) saveConfig(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.options.ConfigPath) == "" {
		writeError(w, http.StatusServiceUnavailable, errors.New("configuration persistence is not enabled"))
		return
	}
	// Decode onto the defaults so a partial payload keeps the built-in
	// defaults for every omitted field instead of silently zeroing them
	// (mirrors how Load applies Default() before overlaying the file).
	data, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read config payload: %w", err))
		return
	}
	candidate := config.Default()
	if err := decodeJSONBytes(data, &candidate); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// A payload that never mentions webui must not turn the default
	// admin/admin into persisted credentials: like Load, treat an absent
	// webui section as "unset".
	if !config.WebUIExplicit(data) {
		candidate.Admin.WebUI = nil
	}
	// A new IPv6 prefix takes effect immediately: the whole pool moves to the
	// new prefix without restarting (listeners bind host:port, never the lease
	// address). Other setting changes still need a restart.
	prefixChanged := candidate.IPv6Prefix != s.options.RuntimeConfig.IPv6Prefix
	if prefixChanged {
		if err := s.pool.Reassign(candidate.IPv6Prefix); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("reassign pool for new prefix: %w", err))
			return
		}
	}
	if err := config.SaveAtomic(s.options.ConfigPath, candidate); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.options.RuntimeConfig.IPv6Prefix = candidate.IPv6Prefix
	writeJSON(w, http.StatusOK, map[string]any{
		"saved":             true,
		"restart_required":  true,
		"prefix_reassigned": prefixChanged,
		"config":            candidate,
	})
}

// getConfigDefaults returns the built-in defaults for the console's "restore
// defaults" action. The webui section is deliberately left unset so the
// restore keeps the blank-means-keep semantics instead of persisting
// admin/admin.
func (s *server) getConfigDefaults(w http.ResponseWriter, _ *http.Request) {
	defaults := config.Default()
	defaults.Admin.WebUI = nil
	writeJSON(w, http.StatusOK, defaults)
}

// defaultEgressURL is the exit-address service used by proxy probes. It
// returns the caller's public address as plain text and prefers IPv6.
const defaultEgressURL = "http://api64.ipify.org"

type testProxyRequest struct {
	ID     string `json:"id"`
	Port   int    `json:"port"`
	Egress string `json:"egress"`
}

// probeTargetError carries an HTTP status for proxy target resolution
// failures (unknown lease, unknown port, or a malformed request).
type probeTargetError struct {
	status int
	err    error
}

func (e *probeTargetError) Error() string { return e.err.Error() }
func (e *probeTargetError) Unwrap() error { return e.err }

// testProxy runs a live network probe against one proxy lease: it dials the
// lease's SOCKS5 listener and completes the handshake (multiplex mode
// identifies as "lease:<id>" so no throwaway lease is minted), then verifies
// public egress through the proxy and reports whether the observed exit IPv6
// matches the lease address.
func (s *server) testProxy(w http.ResponseWriter, r *http.Request) {
	if s.options.Probe == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("proxy probing is not enabled"))
		return
	}
	var request testProxyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request.ID = strings.TrimSpace(request.ID)

	proxyAddr, leaseID, expectedIPv6, err := s.resolveProbeTarget(request)
	if err != nil {
		var targetErr *probeTargetError
		if errors.As(err, &targetErr) {
			writeError(w, targetErr.status, targetErr.err)
		} else {
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	egress := strings.TrimSpace(request.Egress)
	if egress == "" {
		egress = defaultEgressURL
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	exitIPv6, err := s.options.Probe(ctx, proxyAddr, egress, leaseID)
	latency := time.Since(start)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"proxy":      proxyAddr,
			"error":      err.Error(),
			"latency_ms": latency.Milliseconds(),
		})
		return
	}
	matched := expectedIPv6 != "" && strings.EqualFold(expectedIPv6, exitIPv6)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"proxy":         proxyAddr,
		"latency_ms":    latency.Milliseconds(),
		"exit_ipv6":     exitIPv6,
		"expected_ipv6": expectedIPv6,
		"ipv6_match":    matched,
	})
}

// resolveProbeTarget maps a request {id, port} to the listener address to
// dial. per_ipv6 leases use their own listener; multiplex mode shares the
// SOCKS listen address. Listen hosts are normalized to loopback because a
// probe dials, it does not bind.
func (s *server) resolveProbeTarget(request testProxyRequest) (proxyAddr, leaseID, expectedIPv6 string, err error) {
	socksListen := s.options.RuntimeConfig.SOCKS.ListenAddress
	if request.ID != "" {
		entry, ok := s.pool.Get(request.ID)
		if !ok {
			return "", "", "", &probeTargetError{http.StatusNotFound, fmt.Errorf("lease %q not found", request.ID)}
		}
		if s.options.ListenerManager != nil {
			if info, ok := s.options.ListenerManager.Get(request.ID); ok {
				return normalizeProbeAddress(info.Address), request.ID, entry.IPv6, nil
			}
		}
		if entry.Port != 0 {
			return net.JoinHostPort("127.0.0.1", strconv.Itoa(entry.Port)), request.ID, entry.IPv6, nil
		}
		return normalizeProbeAddress(socksListen), request.ID, entry.IPv6, nil
	}
	if request.Port != 0 {
		host := "127.0.0.1"
		if h, _, splitErr := net.SplitHostPort(socksListen); splitErr == nil && h != "::" && h != "0.0.0.0" && h != "" {
			host = h
		}
		for _, item := range s.pool.ListAll() {
			if item.Port == request.Port {
				return net.JoinHostPort(host, strconv.Itoa(request.Port)), item.ID, item.IPv6, nil
			}
		}
		return "", "", "", &probeTargetError{http.StatusNotFound, fmt.Errorf("lease on port %d not found", request.Port)}
	}
	return "", "", "", errors.New("id or port must be provided")
}

// normalizeProbeAddress replaces wildcard and empty listen hosts with the
// loopback address so the probe can actually dial the listener.
func normalizeProbeAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	switch host {
	case "", "::", "0.0.0.0":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (s *server) listLeases(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("include_standby") == "true" {
		writeJSON(w, http.StatusOK, s.pool.ListAll())
		return
	}
	writeJSON(w, http.StatusOK, s.pool.List())
}

func (s *server) createLease(w http.ResponseWriter, r *http.Request) {
	var request createLeaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	if request.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id must not be empty"))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.options.RuntimeConfig.SOCKS.Mode != config.ModePerIPv6 || s.options.ListenerManager == nil {
		created, err := s.pool.Acquire(request.ID, request.Persistent)
		if err != nil {
			writeLeaseError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
		return
	}

	created, existed := s.pool.Get(request.ID)
	if !existed {
		var err error
		created, err = s.pool.Acquire(request.ID, request.Persistent)
		if err != nil {
			writeLeaseError(w, err)
			return
		}
	}

	if _, running := s.options.ListenerManager.Get(request.ID); !running {
		host, _, err := net.SplitHostPort(s.options.RuntimeConfig.SOCKS.ListenAddress)
		if err != nil {
			if !existed {
				s.pool.Release(request.ID)
			}
			writeError(w, http.StatusInternalServerError, fmt.Errorf("parse SOCKS listen address: %w", err))
			return
		}
		address := net.JoinHostPort(host, strconv.Itoa(created.Port))
		if _, err := s.options.ListenerManager.Start(request.ID, address); err != nil {
			if !existed {
				s.pool.Release(request.ID)
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	writeJSON(w, http.StatusCreated, created)
}

func writeLeaseError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, lease.ErrCapacity), errors.Is(err, lease.ErrPortUnavailable):
		status = http.StatusConflict
	case errors.Is(err, lease.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, lease.ErrPersistent):
		status = http.StatusBadRequest
	}
	writeError(w, status, err)
}

func (s *server) getLease(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.pool.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("lease not found"))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *server) updateLease(w http.ResponseWriter, r *http.Request) {
	var request updateLeaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.Persistent == nil {
		writeError(w, http.StatusBadRequest, errors.New("persistent is required"))
		return
	}
	updated, err := s.pool.SetPersistent(r.PathValue("id"), *request.Persistent)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *server) deleteLease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.pool.Get(id); !exists {
		writeError(w, http.StatusNotFound, errors.New("lease not found"))
		return
	}
	if s.options.RuntimeConfig.SOCKS.Mode == config.ModePerIPv6 && s.options.ListenerManager != nil {
		if err := s.options.ListenerManager.Stop(id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if !s.pool.Release(id) {
		writeError(w, http.StatusNotFound, errors.New("lease not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) releaseIdle(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"released": s.pool.ReleaseIdle()})
}

// rotateLease assigns the lease a fresh IPv6 source address. The port stays
// unchanged in per_ipv6 mode, so clients only need to update any hardcoded IP
// allowlist, not their proxy configuration.
func (s *server) rotateLease(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rotated, err := s.pool.Rotate(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, rotated)
}

// recycleLease releases the lease and immediately re-acquires a new one under
// the same id: the client keeps its identity but gets a different port and a
// fresh IPv6. In per_ipv6 mode the old listener is stopped by the OnRelease
// hook and a new one is started for the replacement lease.
func (s *server) recycleLease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.mu.Lock()
	defer s.mu.Unlock()

	recycled, err := s.pool.Recycle(id)
	if err != nil {
		writeLeaseError(w, err)
		return
	}

	if s.options.RuntimeConfig.SOCKS.Mode == config.ModePerIPv6 && s.options.ListenerManager != nil {
		host, _, err := net.SplitHostPort(s.options.RuntimeConfig.SOCKS.ListenAddress)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("parse SOCKS listen address: %w", err))
			return
		}
		address := net.JoinHostPort(host, strconv.Itoa(recycled.Port))
		if _, err := s.options.ListenerManager.Start(id, address); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, recycled)
}

func decodeJSON(r *http.Request, destination any) error {
	data, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return err
	}
	return decodeJSONBytes(data, destination)
}

func decodeJSONBytes(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
