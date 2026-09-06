package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ipv6-proxy-pool/internal/config"
	"ipv6-proxy-pool/internal/lease"
	"ipv6-proxy-pool/internal/listener"
)

func TestSaveConfigReassignsPoolOnPrefixChange(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 1,
		MaxLeases: 8,
		PortStart: 20000,
		PortEnd:   20007,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.Acquire("client-a", false); err != nil {
		t.Fatalf("Acquire client-a: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MinLeases:  1,
			MaxLeases:  8,
			SOCKS: config.SOCKSConfig{
				Mode:          config.ModeMultiplex,
				ListenAddress: "[::]:10080",
				PortStart:     20000,
				PortEnd:       20007,
			},
		},
		ConfigPath: path,
	})

	save := httptest.NewRequest(http.MethodPut, "/v1/config", strings.NewReader(`{
		"ipv6_prefix": "2001:db8:1::/64",
		"min_leases": 2,
		"max_leases": 8,
		"socks": {"mode": "multiplex", "listen_address": "[::]:10080", "port_start": 20000, "port_end": 20007},
		"admin": {"listen_address": "[::]:10070"}
	}`))
	save.Header.Set("Content-Type", "application/json")
	saveResult := httptest.NewRecorder()
	handler.ServeHTTP(saveResult, save)
	if saveResult.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", saveResult.Code, saveResult.Body.String())
	}
	var payload struct {
		PrefixReassigned bool `json:"prefix_reassigned"`
	}
	if err := json.NewDecoder(saveResult.Body).Decode(&payload); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if !payload.PrefixReassigned {
		t.Fatal("prefix_reassigned = false, want true after changing the prefix")
	}

	for _, item := range pool.ListAll() {
		if !strings.HasPrefix(item.IPv6, "2001:db8:1:") {
			t.Fatalf("lease %q address %s is not under the new prefix", item.ID, item.IPv6)
		}
	}

	// GET /v1/config 应立即反映新前缀。
	get := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	getResult := httptest.NewRecorder()
	handler.ServeHTTP(getResult, get)
	var current config.Config
	if err := json.NewDecoder(getResult.Body).Decode(&current); err != nil {
		t.Fatalf("decode get config: %v", err)
	}
	if current.IPv6Prefix != "2001:db8:1::/64" {
		t.Fatalf("runtime ipv6_prefix = %q, want the reassigned prefix", current.IPv6Prefix)
	}
}

func TestSaveConfigSamePrefixKeepsLeasesUntouched(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 1,
		MaxLeases: 8,
		PortStart: 20000,
		PortEnd:   20007,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	acquired, err := pool.Acquire("client-a", false)
	if err != nil {
		t.Fatalf("Acquire client-a: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MinLeases:  1,
			MaxLeases:  8,
			SOCKS: config.SOCKSConfig{
				Mode:          config.ModeMultiplex,
				ListenAddress: "[::]:10080",
				PortStart:     20000,
				PortEnd:       20007,
			},
		},
		ConfigPath: path,
	})

	save := httptest.NewRequest(http.MethodPut, "/v1/config", strings.NewReader(`{
		"ipv6_prefix": "2001:db8::/64",
		"min_leases": 1,
		"max_leases": 8,
		"socks": {"mode": "multiplex", "listen_address": "[::]:10080", "port_start": 20000, "port_end": 20007},
		"admin": {"listen_address": "[::]:10070"}
	}`))
	save.Header.Set("Content-Type", "application/json")
	saveResult := httptest.NewRecorder()
	handler.ServeHTTP(saveResult, save)
	if saveResult.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", saveResult.Code, saveResult.Body.String())
	}
	var payload struct {
		PrefixReassigned bool `json:"prefix_reassigned"`
	}
	if err := json.NewDecoder(saveResult.Body).Decode(&payload); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if payload.PrefixReassigned {
		t.Fatal("prefix_reassigned = true with an unchanged prefix")
	}
	after, ok := pool.Get("client-a")
	if !ok || after.IPv6 != acquired.IPv6 {
		t.Fatalf("lease address changed without a prefix change: %s -> %s", acquired.IPv6, after.IPv6)
	}
}

func TestConfigDefaultsEndpoint(t *testing.T) {
	handler := newTestHandler(t, 4)
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/v1/config/defaults", nil))
	if result.Code != http.StatusOK {
		t.Fatalf("defaults status = %d, body = %s", result.Code, result.Body.String())
	}
	var defaults config.Config
	if err := json.NewDecoder(result.Body).Decode(&defaults); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	expected := config.Default()
	if defaults.MinLeases != expected.MinLeases || defaults.MaxLeases != expected.MaxLeases {
		t.Fatalf("defaults min/max = %d/%d, want %d/%d", defaults.MinLeases, defaults.MaxLeases, expected.MinLeases, expected.MaxLeases)
	}
	if defaults.Admin.WebUI != nil {
		t.Fatalf("defaults must leave webui unset, got %+v", *defaults.Admin.WebUI)
	}
}

func TestProxyTestEndpointMultiplex(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 4,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.Acquire("client-a", false); err != nil {
		t.Fatalf("Acquire client-a: %v", err)
	}
	var gotProxy, gotLeaseID string
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MinLeases:  1,
			MaxLeases:  4,
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080"},
			Admin:      config.AdminConfig{ListenAddress: "[::]:10070"},
		},
		Probe: func(_ context.Context, proxyAddr, _ string, leaseID string) (string, error) {
			gotProxy = proxyAddr
			gotLeaseID = leaseID
			// 模拟真实探测：返回该租约当前分配的地址（随机分配，不能写死）。
			entry, ok := pool.Get(leaseID)
			if !ok {
				return "", errors.New("lease not found")
			}
			return entry.IPv6, nil
		},
	})

	// multiplex 模式按 id 测试：监听地址归一化为回环地址。
	save := httptest.NewRequest(http.MethodPost, "/v1/proxies:test", strings.NewReader(`{"id":"client-a"}`))
	save.Header.Set("Content-Type", "application/json")
	saveResult := httptest.NewRecorder()
	handler.ServeHTTP(saveResult, save)
	if saveResult.Code != http.StatusOK {
		t.Fatalf("test status = %d, body = %s", saveResult.Code, saveResult.Body.String())
	}
	var payload struct {
		OK           bool   `json:"ok"`
		ExitIPv6     string `json:"exit_ipv6"`
		ExpectedIPv6 string `json:"expected_ipv6"`
		IPv6Match    bool   `json:"ipv6_match"`
		Proxy        string `json:"proxy"`
	}
	if err := json.NewDecoder(saveResult.Body).Decode(&payload); err != nil {
		t.Fatalf("decode test response: %v", err)
	}
	if !payload.OK || !payload.IPv6Match || payload.ExitIPv6 != payload.ExpectedIPv6 || payload.ExitIPv6 == "" {
		t.Fatalf("test payload = %+v", payload)
	}
	if gotProxy != "127.0.0.1:10080" || gotLeaseID != "client-a" {
		t.Fatalf("probe called with proxy=%q lease=%q, want 127.0.0.1:10080 / client-a", gotProxy, gotLeaseID)
	}

	// 探测失败：HTTP 200 + ok:false。
	failing := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MinLeases:  1,
			MaxLeases:  4,
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080"},
		},
		Probe: func(_ context.Context, _, _, _ string) (string, error) {
			return "", errors.New("egress connect failed (reply 0x05)")
		},
	})
	failResult := httptest.NewRecorder()
	failing.ServeHTTP(failResult, httptest.NewRequest(http.MethodPost, "/v1/proxies:test", strings.NewReader(`{"id":"client-a"}`)))
	var failed struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(failResult.Body).Decode(&failed); err != nil {
		t.Fatalf("decode failed test response: %v", err)
	}
	if failResult.Code != http.StatusOK || failed.OK || !strings.Contains(failed.Error, "0x05") {
		t.Fatalf("failed probe response = %d %+v", failResult.Code, failed)
	}
}

func TestProxyTestEndpointUnknownLeaseAndDisabled(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{Prefix: "2001:db8::/64", MaxLeases: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080"},
		},
		Probe: func(_ context.Context, _, _, _ string) (string, error) { return "", nil },
	})

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/v1/proxies:test", strings.NewReader(`{"id":"nope"}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown lease status = %d, want 404", missing.Code)
	}

	// 未接线 Probe 时端点明确报不可用。
	disabled := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080"},
		},
	})
	disabledResult := httptest.NewRecorder()
	disabled.ServeHTTP(disabledResult, httptest.NewRequest(http.MethodPost, "/v1/proxies:test", strings.NewReader(`{"id":"client-a"}`)))
	if disabledResult.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled probe status = %d, want 503", disabledResult.Code)
	}

	// 既无 id 也无 port：400。
	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, httptest.NewRequest(http.MethodPost, "/v1/proxies:test", strings.NewReader(`{}`)))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty target status = %d, want 400", empty.Code)
	}
}

func TestProxyTestEndpointByPortPerIPv6(t *testing.T) {
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 4,
		PortStart: 20000,
		PortEnd:   20003,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.AcquirePort("client-a", 20000, false); err != nil {
		t.Fatalf("AcquirePort client-a: %v", err)
	}
	manager := listener.NewManager(context.Background(), stubServeFunc{})
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Start("client-a", net.JoinHostPort("127.0.0.1", "20000")); err != nil {
		t.Fatalf("Start listener: %v", err)
	}

	var gotProxy, gotLeaseID string
	handler := HandlerWithOptions(pool, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MaxLeases:  4,
			SOCKS:      config.SOCKSConfig{Mode: config.ModePerIPv6, ListenAddress: "[::]:10080"},
		},
		ListenerManager: manager,
		Probe: func(_ context.Context, proxyAddr, _, leaseID string) (string, error) {
			gotProxy = proxyAddr
			gotLeaseID = leaseID
			return "2001:db8::1", nil
		},
	})

	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/proxies:test", strings.NewReader(`{"port":20000}`)))
	if result.Code != http.StatusOK {
		t.Fatalf("test by port status = %d, body = %s", result.Code, result.Body.String())
	}
	if gotProxy != "127.0.0.1:20000" || gotLeaseID != "client-a" {
		t.Fatalf("probe called with proxy=%q lease=%q, want 127.0.0.1:20000 / client-a", gotProxy, gotLeaseID)
	}
}

func TestHealthAndStatusReportVersion(t *testing.T) {
	handler := tokenTestHandler(t, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MaxLeases:  4,
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080"},
		},
		Version: "abc1234",
	})

	healthResult := httptest.NewRecorder()
	handler.ServeHTTP(healthResult, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(healthResult.Body).Decode(&health); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if health.Status != "ok" || health.Version != "abc1234" {
		t.Fatalf("healthz payload = %+v", health)
	}

	statusResult := httptest.NewRecorder()
	handler.ServeHTTP(statusResult, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var status struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(statusResult.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Version != "abc1234" {
		t.Fatalf("status version = %q, want abc1234", status.Version)
	}
}
