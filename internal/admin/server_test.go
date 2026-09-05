package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"ipv6-proxy-pool/internal/config"
	"ipv6-proxy-pool/internal/lease"
	"ipv6-proxy-pool/internal/listener"
)

// stubServeFunc satisfies the SOCKS5 server interface without serving traffic.
// Serve blocks until the listener or context closes, mirroring real servers.
type stubServeFunc struct{}

func (stubServeFunc) Serve(ctx context.Context, ln net.Listener, _ string) error {
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		close(done)
	}()
	_, _ = ln.Accept()
	<-done
	return nil
}

func newTestHandler(t *testing.T, maxLeases int) http.Handler {
	t.Helper()
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: maxLeases,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return Handler(pool)
}

func TestLeaseLifecycle(t *testing.T) {
	handler := newTestHandler(t, 2)

	createBody := []byte(`{"id":"client-one","persistent":true}`)
	create := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewReader(createBody))
	create.Header.Set("Content-Type", "application/json")
	createResult := httptest.NewRecorder()
	handler.ServeHTTP(createResult, create)
	if createResult.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createResult.Code, createResult.Body.String())
	}

	var created lease.Lease
	if err := json.NewDecoder(createResult.Body).Decode(&created); err != nil {
		t.Fatalf("decode created lease: %v", err)
	}
	if created.ID != "client-one" || !created.Persistent || created.IPv6 == "" {
		t.Fatalf("created lease = %+v", created)
	}

	get := httptest.NewRequest(http.MethodGet, "/v1/leases/client-one", nil)
	getResult := httptest.NewRecorder()
	handler.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getResult.Code, getResult.Body.String())
	}

	update := httptest.NewRequest(http.MethodPatch, "/v1/leases/client-one", bytes.NewBufferString(`{"persistent":false}`))
	updateResult := httptest.NewRecorder()
	handler.ServeHTTP(updateResult, update)
	if updateResult.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", updateResult.Code, updateResult.Body.String())
	}

	var updated lease.Lease
	if err := json.NewDecoder(updateResult.Body).Decode(&updated); err != nil {
		t.Fatalf("decode updated lease: %v", err)
	}
	if updated.Persistent {
		t.Fatal("updated lease remains persistent")
	}

	list := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	listResult := httptest.NewRecorder()
	handler.ServeHTTP(listResult, list)
	if listResult.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listResult.Code, listResult.Body.String())
	}

	var leases []lease.Lease
	if err := json.NewDecoder(listResult.Body).Decode(&leases); err != nil {
		t.Fatalf("decode lease list: %v", err)
	}
	if len(leases) != 1 || leases[0].ID != "client-one" {
		t.Fatalf("lease list = %+v", leases)
	}

	remove := httptest.NewRequest(http.MethodDelete, "/v1/leases/client-one", nil)
	removeResult := httptest.NewRecorder()
	handler.ServeHTTP(removeResult, remove)
	if removeResult.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", removeResult.Code, removeResult.Body.String())
	}

	missing := httptest.NewRequest(http.MethodGet, "/v1/leases/client-one", nil)
	missingResult := httptest.NewRecorder()
	handler.ServeHTTP(missingResult, missing)
	if missingResult.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want %d", missingResult.Code, http.StatusNotFound)
	}
}

func TestCapacityAndRequestValidation(t *testing.T) {
	handler := newTestHandler(t, 1)

	first := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewBufferString(`{"id":"first"}`))
	firstResult := httptest.NewRecorder()
	handler.ServeHTTP(firstResult, first)
	if firstResult.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", firstResult.Code, firstResult.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewBufferString(`{"id":"second"}`))
	secondResult := httptest.NewRecorder()
	handler.ServeHTTP(secondResult, second)
	if secondResult.Code != http.StatusConflict {
		t.Fatalf("capacity status = %d, body = %s", secondResult.Code, secondResult.Body.String())
	}

	invalid := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewBufferString(`{"id":"x","unknown":true}`))
	invalidResult := httptest.NewRecorder()
	handler.ServeHTTP(invalidResult, invalid)
	if invalidResult.Code != http.StatusBadRequest {
		t.Fatalf("invalid request status = %d, body = %s", invalidResult.Code, invalidResult.Body.String())
	}
}

func TestHealthAndReleaseIdleEndpoints(t *testing.T) {
	handler := newTestHandler(t, 1)

	health := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResult := httptest.NewRecorder()
	handler.ServeHTTP(healthResult, health)
	if healthResult.Code != http.StatusOK {
		t.Fatalf("health status = %d", healthResult.Code)
	}

	release := httptest.NewRequest(http.MethodPost, "/v1/leases:release-idle", nil)
	releaseResult := httptest.NewRecorder()
	handler.ServeHTTP(releaseResult, release)
	if releaseResult.Code != http.StatusOK {
		t.Fatalf("release-idle status = %d, body = %s", releaseResult.Code, releaseResult.Body.String())
	}
}

func TestRotateEndpoint(t *testing.T) {
	handler := newTestHandler(t, 4)

	create := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewBufferString(`{"id":"client-a"}`))
	createResult := httptest.NewRecorder()
	handler.ServeHTTP(createResult, create)
	var created lease.Lease
	if err := json.NewDecoder(createResult.Body).Decode(&created); err != nil {
		t.Fatalf("decode created lease: %v", err)
	}

	rotate := httptest.NewRequest(http.MethodPost, "/v1/leases/client-a/rotate", nil)
	rotateResult := httptest.NewRecorder()
	handler.ServeHTTP(rotateResult, rotate)
	if rotateResult.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", rotateResult.Code, rotateResult.Body.String())
	}
	var rotated lease.Lease
	if err := json.NewDecoder(rotateResult.Body).Decode(&rotated); err != nil {
		t.Fatalf("decode rotated lease: %v", err)
	}
	if rotated.IPv6 == created.IPv6 {
		t.Fatal("rotate did not change the IPv6 address")
	}
	if rotated.Port != created.Port {
		t.Fatalf("rotate changed port %d -> %d", created.Port, rotated.Port)
	}

	missing := httptest.NewRequest(http.MethodPost, "/v1/leases/nope/rotate", nil)
	missingResult := httptest.NewRecorder()
	handler.ServeHTTP(missingResult, missing)
	if missingResult.Code != http.StatusNotFound {
		t.Fatalf("rotate missing status = %d, want %d", missingResult.Code, http.StatusNotFound)
	}
}

func TestRecycleEndpoint(t *testing.T) {
	handler := newTestHandler(t, 4)

	create := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewBufferString(`{"id":"client-a"}`))
	createResult := httptest.NewRecorder()
	handler.ServeHTTP(createResult, create)
	var created lease.Lease
	if err := json.NewDecoder(createResult.Body).Decode(&created); err != nil {
		t.Fatalf("decode created lease: %v", err)
	}

	recycle := httptest.NewRequest(http.MethodPost, "/v1/leases/client-a/recycle", nil)
	recycleResult := httptest.NewRecorder()
	handler.ServeHTTP(recycleResult, recycle)
	if recycleResult.Code != http.StatusOK {
		t.Fatalf("recycle status = %d, body = %s", recycleResult.Code, recycleResult.Body.String())
	}
	var recycled lease.Lease
	if err := json.NewDecoder(recycleResult.Body).Decode(&recycled); err != nil {
		t.Fatalf("decode recycled lease: %v", err)
	}
	if recycled.ID != "client-a" {
		t.Fatalf("recycled id = %q, want client-a", recycled.ID)
	}
	if recycled.IPv6 == created.IPv6 {
		t.Fatal("recycle did not change the IPv6 address")
	}

	missing := httptest.NewRequest(http.MethodPost, "/v1/leases/nope/recycle", nil)
	missingResult := httptest.NewRecorder()
	handler.ServeHTTP(missingResult, missing)
	if missingResult.Code != http.StatusNotFound {
		t.Fatalf("recycle missing status = %d, want %d", missingResult.Code, http.StatusNotFound)
	}

	// Persistent leases cannot be recycled.
	persistent := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewBufferString(`{"id":"pinned","persistent":true}`))
	persistentResult := httptest.NewRecorder()
	handler.ServeHTTP(persistentResult, persistent)
	if persistentResult.Code != http.StatusCreated {
		t.Fatalf("persistent create status = %d", persistentResult.Code)
	}
	denied := httptest.NewRequest(http.MethodPost, "/v1/leases/pinned/recycle", nil)
	deniedResult := httptest.NewRecorder()
	handler.ServeHTTP(deniedResult, denied)
	if deniedResult.Code != http.StatusBadRequest {
		t.Fatalf("recycle persistent status = %d, want %d", deniedResult.Code, http.StatusBadRequest)
	}
}

func TestStatusReportsResidentFloor(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 2,
		MaxLeases: 4,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{MinLeases: 2, MaxLeases: 4},
	})

	status := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	statusResult := httptest.NewRecorder()
	handler.ServeHTTP(statusResult, status)
	if statusResult.Code != http.StatusOK {
		t.Fatalf("status code = %d", statusResult.Code)
	}
	var payload struct {
		LeaseCount   int `json:"lease_count"`
		StandbyCount int `json:"standby_count"`
		MinLeases    int `json:"min_leases"`
		MaxLeases    int `json:"max_leases"`
	}
	if err := json.NewDecoder(statusResult.Body).Decode(&payload); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if payload.StandbyCount != 2 || payload.MinLeases != 2 || payload.MaxLeases != 4 {
		t.Fatalf("status payload = %+v, want standby 2, min 2, max 4", payload)
	}
	if payload.LeaseCount != 0 {
		t.Fatalf("client lease count = %d, want 0 (standbys excluded)", payload.LeaseCount)
	}
}

func TestStatusReportsRuntimeMetrics(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 1,
		MaxLeases: 4,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.Acquire("client-a", false); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := pool.RecordRequest("client-a"); err != nil {
			t.Fatalf("RecordRequest: %v", err)
		}
	}
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			MinLeases: 1,
			MaxLeases: 4,
			SOCKS:     config.SOCKSConfig{Mode: config.ModePerIPv6, AlwaysOnPorts: []int{20000}},
		},
	})

	status := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	statusResult := httptest.NewRecorder()
	handler.ServeHTTP(statusResult, status)
	if statusResult.Code != http.StatusOK {
		t.Fatalf("status code = %d", statusResult.Code)
	}
	var payload struct {
		UptimeSeconds  int64      `json:"uptime_seconds"`
		LeaseCount     int        `json:"lease_count"`
		StandbyCount   int        `json:"standby_count"`
		TotalRequests  uint64     `json:"total_requests"`
		ListenerCount  int        `json:"listener_count"`
		Listeners      []struct{} `json:"listeners"`
		AlwaysOnPorts  []int      `json:"always_on_ports"`
		RotateRequests uint64     `json:"rotate_requests"`
		TokenRequired  bool       `json:"token_required"`
	}
	if err := json.NewDecoder(statusResult.Body).Decode(&payload); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if payload.UptimeSeconds < 0 {
		t.Fatalf("uptime_seconds = %d, want >= 0", payload.UptimeSeconds)
	}
	if payload.LeaseCount != 1 {
		t.Fatalf("lease_count = %d, want 1", payload.LeaseCount)
	}
	if payload.StandbyCount != 0 {
		t.Fatalf("standby_count = %d, want 0 (the seeded standby was claimed)", payload.StandbyCount)
	}
	if payload.TotalRequests != 3 {
		t.Fatalf("total_requests = %d, want 3", payload.TotalRequests)
	}
	if payload.ListenerCount != 0 || len(payload.Listeners) != 0 {
		t.Fatalf("listener info = %d/%d, want 0/0 without a listener manager", payload.ListenerCount, len(payload.Listeners))
	}
	if len(payload.AlwaysOnPorts) != 1 || payload.AlwaysOnPorts[0] != 20000 {
		t.Fatalf("always_on_ports = %v, want [20000]", payload.AlwaysOnPorts)
	}
	if payload.TokenRequired {
		t.Fatal("token_required = true, want false without an admin token")
	}
}

func TestStatusReportsActiveListeners(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 1,
		MaxLeases: 4,
		PortStart: 20000,
		PortEnd:   20003,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	manager := listener.NewManager(context.Background(), stubServeFunc{})
	t.Cleanup(func() { _ = manager.Close() })

	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig:   config.Config{MinLeases: 1, MaxLeases: 4, SOCKS: config.SOCKSConfig{Mode: config.ModePerIPv6}},
		ListenerManager: manager,
	})

	leaseEntry, err := pool.Acquire("client-a", false)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := manager.Start("client-a", net.JoinHostPort("127.0.0.1", strconv.Itoa(leaseEntry.Port))); err != nil {
		t.Fatalf("Start listener: %v", err)
	}

	status := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	statusResult := httptest.NewRecorder()
	handler.ServeHTTP(statusResult, status)
	if statusResult.Code != http.StatusOK {
		t.Fatalf("status code = %d", statusResult.Code)
	}
	var payload struct {
		ListenerCount int `json:"listener_count"`
		Listeners     []struct {
			ID      string `json:"id"`
			Address string `json:"address"`
		} `json:"listeners"`
	}
	if err := json.NewDecoder(statusResult.Body).Decode(&payload); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if payload.ListenerCount != 1 || len(payload.Listeners) != 1 {
		t.Fatalf("listener payload = %+v, want exactly one active listener", payload)
	}
	if payload.Listeners[0].ID != "client-a" || payload.Listeners[0].Address == "" {
		t.Fatalf("listener entry = %+v, want id client-a with an address", payload.Listeners[0])
	}
}

func TestListLeasesIncludesStandbysOnQuery(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 2,
		MaxLeases: 4,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	handler := Handler(pool)

	list := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	listResult := httptest.NewRecorder()
	handler.ServeHTTP(listResult, list)
	var clients []lease.Lease
	if err := json.NewDecoder(listResult.Body).Decode(&clients); err != nil {
		t.Fatalf("decode lease list: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("default list = %d entries, want 0", len(clients))
	}

	all := httptest.NewRequest(http.MethodGet, "/v1/leases?include_standby=true", nil)
	allResult := httptest.NewRecorder()
	handler.ServeHTTP(allResult, all)
	var entries []lease.Lease
	if err := json.NewDecoder(allResult.Body).Decode(&entries); err != nil {
		t.Fatalf("decode full lease list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("include_standby list = %d entries, want 2", len(entries))
	}
}

func TestAdminTokenProtectsAPI(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 4,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	handler := HandlerWithOptions(pool, Options{AdminToken: "super-secret-token"})

	withoutToken := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	withoutResult := httptest.NewRecorder()
	handler.ServeHTTP(withoutResult, withoutToken)
	if withoutResult.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want %d", withoutResult.Code, http.StatusUnauthorized)
	}

	wrongToken := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	wrongToken.Header.Set("Authorization", "Bearer wrong")
	wrongResult := httptest.NewRecorder()
	handler.ServeHTTP(wrongResult, wrongToken)
	if wrongResult.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want %d", wrongResult.Code, http.StatusUnauthorized)
	}

	withToken := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	withToken.Header.Set("Authorization", "Bearer super-secret-token")
	withResult := httptest.NewRecorder()
	handler.ServeHTTP(withResult, withToken)
	if withResult.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, body = %s", withResult.Code, withResult.Body.String())
	}

	health := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResult := httptest.NewRecorder()
	handler.ServeHTTP(healthResult, health)
	if healthResult.Code != http.StatusOK {
		t.Fatalf("health with token configured status = %d", healthResult.Code)
	}
}

func loginAsConsole(handler http.Handler, username, password string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}

func TestWebUILoginWithDefaultCredentials(t *testing.T) {
	handler := newTestHandler(t, 2)

	wrong := loginAsConsole(handler, "admin", "nope")
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want %d", wrong.Code, http.StatusUnauthorized)
	}

	unknownUser := loginAsConsole(handler, "root", "admin")
	if unknownUser.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user status = %d, want %d", unknownUser.Code, http.StatusUnauthorized)
	}

	right := loginAsConsole(handler, "admin", "admin")
	if right.Code != http.StatusOK {
		t.Fatalf("default login status = %d, body = %s", right.Code, right.Body.String())
	}
	cookies := right.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Value == "" {
		t.Fatalf("login did not set a session cookie: %v", cookies)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
}

func TestWebUISessionEndpointAndLogout(t *testing.T) {
	handler := newTestHandler(t, 2)

	anonymous := httptest.NewRequest(http.MethodGet, "/v1/auth/session", nil)
	anonymousResult := httptest.NewRecorder()
	handler.ServeHTTP(anonymousResult, anonymous)
	if anonymousResult.Code != http.StatusOK {
		t.Fatalf("anonymous session status = %d", anonymousResult.Code)
	}
	var anonymousState struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(anonymousResult.Body).Decode(&anonymousState); err != nil {
		t.Fatalf("decode anonymous session: %v", err)
	}
	if anonymousState.Authenticated {
		t.Fatal("session reports authenticated without a login")
	}

	login := loginAsConsole(handler, "admin", "admin")
	cookie := login.Result().Cookies()[0]

	authed := httptest.NewRequest(http.MethodGet, "/v1/auth/session", nil)
	authed.AddCookie(cookie)
	authedResult := httptest.NewRecorder()
	handler.ServeHTTP(authedResult, authed)
	var state struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
	}
	if err := json.NewDecoder(authedResult.Body).Decode(&state); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if !state.Authenticated || state.Username != "admin" {
		t.Fatalf("session state = %+v, want authenticated admin", state)
	}

	logout := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	logout.AddCookie(cookie)
	logoutResult := httptest.NewRecorder()
	handler.ServeHTTP(logoutResult, logout)
	if logoutResult.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %s", logoutResult.Code, logoutResult.Body.String())
	}

	replayed := httptest.NewRequest(http.MethodGet, "/v1/auth/session", nil)
	replayed.AddCookie(cookie)
	replayedResult := httptest.NewRecorder()
	handler.ServeHTTP(replayedResult, replayed)
	var replayState struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(replayedResult.Body).Decode(&replayState); err != nil {
		t.Fatalf("decode post-logout session: %v", err)
	}
	if replayState.Authenticated {
		t.Fatal("session survived logout")
	}
}

func TestAdminTokenAcceptsConsoleSession(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 4,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	handler := HandlerWithOptions(pool, Options{AdminToken: "super-secret-token"})

	// The login endpoint itself must stay reachable while a token is set.
	sessionInfo := httptest.NewRequest(http.MethodGet, "/v1/auth/session", nil)
	sessionResult := httptest.NewRecorder()
	handler.ServeHTTP(sessionResult, sessionInfo)
	if sessionResult.Code != http.StatusOK {
		t.Fatalf("session endpoint status = %d, want %d", sessionResult.Code, http.StatusOK)
	}

	login := loginAsConsole(handler, "admin", "admin")
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]

	withCookie := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	withCookie.AddCookie(cookie)
	cookieResult := httptest.NewRecorder()
	handler.ServeHTTP(cookieResult, withCookie)
	if cookieResult.Code != http.StatusOK {
		t.Fatalf("session cookie status = %d, body = %s", cookieResult.Code, cookieResult.Body.String())
	}

	withToken := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	withToken.Header.Set("Authorization", "Bearer super-secret-token")
	tokenResult := httptest.NewRecorder()
	handler.ServeHTTP(tokenResult, withToken)
	if tokenResult.Code != http.StatusOK {
		t.Fatalf("bearer token status = %d, body = %s", tokenResult.Code, tokenResult.Body.String())
	}
}

func TestConfiguredWebUICredentialsOverrideDefaults(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 4,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			Admin: config.AdminConfig{
				WebUI: config.WebUIConfig{Username: "ops", Password: "s3cret"},
			},
		},
	})

	defaultLogin := loginAsConsole(handler, "admin", "admin")
	if defaultLogin.Code != http.StatusUnauthorized {
		t.Fatalf("default credentials status = %d, want %d", defaultLogin.Code, http.StatusUnauthorized)
	}

	customLogin := loginAsConsole(handler, "ops", "s3cret")
	if customLogin.Code != http.StatusOK {
		t.Fatalf("custom credentials status = %d, body = %s", customLogin.Code, customLogin.Body.String())
	}
}
