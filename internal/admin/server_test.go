package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ipv6-proxy-pool/internal/config"
	"ipv6-proxy-pool/internal/lease"
)

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
