package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ipv6-proxy-pool/internal/config"
	"ipv6-proxy-pool/internal/lease"
)

func TestTokenStoreAcceptsLegacyAndNamed(t *testing.T) {
	store := newTokenStore("legacy-token-123", []config.NamedToken{
		{Name: "ops", Token: "named-token-456"},
		{Name: "blank", Token: "   "},
	})
	if !store.any() {
		t.Fatal("any() = false with tokens configured")
	}
	for _, valid := range []string{"legacy-token-123", "named-token-456"} {
		if !store.accepts(valid) {
			t.Fatalf("accepts(%q) = false, want true", valid)
		}
	}
	if store.accepts("wrong-token") {
		t.Fatal("accepts(wrong) = true, want false")
	}
	// 空白的命名令牌条目必须在加载时被忽略。
	listed := store.list()
	if len(listed) != 1 || listed[0].Name != "ops" {
		t.Fatalf("named tokens = %+v, want only ops", listed)
	}
}

func TestTokenStoreAddRotateRemove(t *testing.T) {
	store := newTokenStore("", nil)
	if err := store.add("ops", "0123456789abcdef"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.add("ops", "another-token-123"); err == nil {
		t.Fatal("add accepted a duplicate name")
	}
	if err := store.add("  ", "0123456789abcdef"); err == nil {
		t.Fatal("add accepted a blank name")
	}
	if err := store.replace("ops", "fedcba9876543210"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if store.accepts("0123456789abcdef") {
		t.Fatal("old value still accepted after rotate")
	}
	if !store.accepts("fedcba9876543210") {
		t.Fatal("new value not accepted after rotate")
	}
	if err := store.replace("missing", "fedcba9876543210"); err == nil {
		t.Fatal("replace accepted a missing name")
	}
	if !store.remove("ops") {
		t.Fatal("remove(ops) = false")
	}
	if store.remove("ops") {
		t.Fatal("remove(ops) twice = true")
	}
	if store.accepts("fedcba9876543210") {
		t.Fatal("removed token still accepted")
	}
}

func tokenTestHandler(t *testing.T, options Options) http.Handler {
	t.Helper()
	pool, err := lease.NewPool(lease.Options{Prefix: "2001:db8::/64", MaxLeases: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return HandlerWithOptions(pool, options)
}

func TestTokensAPILifecycle(t *testing.T) {
	handler := tokenTestHandler(t, Options{
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MinLeases:  1,
			MaxLeases:  4,
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080", PortStart: 10000, PortEnd: 10003},
			Admin:      config.AdminConfig{ListenAddress: "[::]:10070"},
		},
		AdminToken: "legacy-token-123",
	})

	// 旧令牌仍可访问受保护接口。
	legacy := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	legacy.Header.Set("Authorization", "Bearer legacy-token-123")
	legacyResult := httptest.NewRecorder()
	handler.ServeHTTP(legacyResult, legacy)
	if legacyResult.Code != http.StatusOK {
		t.Fatalf("legacy token status = %d", legacyResult.Code)
	}

	// 新建命名令牌。
	create := httptest.NewRequest(http.MethodPost, "/v1/tokens", strings.NewReader(`{"name":"ops"}`))
	create.Header.Set("Authorization", "Bearer legacy-token-123")
	createResult := httptest.NewRecorder()
	handler.ServeHTTP(createResult, create)
	if createResult.Code != http.StatusCreated {
		t.Fatalf("create token status = %d, body = %s", createResult.Code, createResult.Body.String())
	}
	var created config.NamedToken
	if err := json.NewDecoder(createResult.Body).Decode(&created); err != nil {
		t.Fatalf("decode created token: %v", err)
	}
	if created.Name != "ops" || len(created.Token) != 32 {
		t.Fatalf("created token = %+v", created)
	}

	// 新令牌立即可用于访问受保护接口。
	named := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	named.Header.Set("Authorization", "Bearer "+created.Token)
	namedResult := httptest.NewRecorder()
	handler.ServeHTTP(namedResult, named)
	if namedResult.Code != http.StatusOK {
		t.Fatalf("named token status = %d", namedResult.Code)
	}

	// 列表包含新令牌。
	list := httptest.NewRequest(http.MethodGet, "/v1/tokens", nil)
	list.Header.Set("Authorization", "Bearer legacy-token-123")
	listResult := httptest.NewRecorder()
	handler.ServeHTTP(listResult, list)
	var payload struct {
		Tokens []config.NamedToken `json:"tokens"`
	}
	if err := json.NewDecoder(listResult.Body).Decode(&payload); err != nil {
		t.Fatalf("decode token list: %v", err)
	}
	if len(payload.Tokens) != 1 || payload.Tokens[0].Name != "ops" {
		t.Fatalf("token list = %+v", payload.Tokens)
	}

	// 轮换后旧值失效、新值可用。
	rotate := httptest.NewRequest(http.MethodPost, "/v1/tokens/ops/rotate", strings.NewReader(`{}`))
	rotate.Header.Set("Authorization", "Bearer legacy-token-123")
	rotateResult := httptest.NewRecorder()
	handler.ServeHTTP(rotateResult, rotate)
	var rotated config.NamedToken
	if err := json.NewDecoder(rotateResult.Body).Decode(&rotated); err != nil {
		t.Fatalf("decode rotated token: %v", err)
	}
	stale := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	stale.Header.Set("Authorization", "Bearer "+created.Token)
	staleResult := httptest.NewRecorder()
	handler.ServeHTTP(staleResult, stale)
	if staleResult.Code != http.StatusUnauthorized {
		t.Fatalf("stale token status = %d, want 401", staleResult.Code)
	}
	fresh := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	fresh.Header.Set("Authorization", "Bearer "+rotated.Token)
	freshResult := httptest.NewRecorder()
	handler.ServeHTTP(freshResult, fresh)
	if freshResult.Code != http.StatusOK {
		t.Fatalf("rotated token status = %d", freshResult.Code)
	}

	// 删除后失效；重复删除 404。
	del := httptest.NewRequest(http.MethodDelete, "/v1/tokens/ops", nil)
	del.Header.Set("Authorization", "Bearer legacy-token-123")
	delResult := httptest.NewRecorder()
	handler.ServeHTTP(delResult, del)
	if delResult.Code != http.StatusOK {
		t.Fatalf("delete status = %d", delResult.Code)
	}
	gone := httptest.NewRequest(http.MethodGet, "/v1/leases", nil)
	gone.Header.Set("Authorization", "Bearer "+rotated.Token)
	goneResult := httptest.NewRecorder()
	handler.ServeHTTP(goneResult, gone)
	if goneResult.Code != http.StatusUnauthorized {
		t.Fatalf("deleted token status = %d, want 401", goneResult.Code)
	}
	delAgain := httptest.NewRequest(http.MethodDelete, "/v1/tokens/ops", nil)
	delAgain.Header.Set("Authorization", "Bearer legacy-token-123")
	delAgainResult := httptest.NewRecorder()
	handler.ServeHTTP(delAgainResult, delAgain)
	if delAgainResult.Code != http.StatusNotFound {
		t.Fatalf("repeated delete status = %d, want 404", delAgainResult.Code)
	}
}

func TestTokensPersistToConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	handler := tokenTestHandler(t, Options{
		ConfigPath: path,
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			MinLeases:  1,
			MaxLeases:  4,
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080", PortStart: 10000, PortEnd: 10003},
			Admin:      config.AdminConfig{ListenAddress: "[::]:10070"},
		},
		AdminToken: "legacy-token-123",
	})
	create := httptest.NewRequest(http.MethodPost, "/v1/tokens", strings.NewReader(`{"name":"ops"}`))
	create.Header.Set("Authorization", "Bearer legacy-token-123")
	createResult := httptest.NewRecorder()
	handler.ServeHTTP(createResult, create)
	if createResult.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createResult.Code, createResult.Body.String())
	}

	// 令牌已写回配置文件：模拟重启后 Load 仍能看到。
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load saved config: %v", err)
	}
	if len(saved.Admin.Tokens) != 1 || saved.Admin.Tokens[0].Name != "ops" {
		t.Fatalf("persisted tokens = %+v", saved.Admin.Tokens)
	}
}

func TestRestartEndpoint(t *testing.T) {
	restarted := false
	handler := tokenTestHandler(t, Options{
		RuntimeConfig: config.Config{
			IPv6Prefix: "2001:db8::/64",
			SOCKS:      config.SOCKSConfig{Mode: config.ModeMultiplex, ListenAddress: "[::]:10080"},
		},
		OnRestart: func() { restarted = true },
	})

	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/restart", strings.NewReader(`{}`)))
	if result.Code != http.StatusOK {
		t.Fatalf("restart status = %d, body = %s", result.Code, result.Body.String())
	}
	if !restarted {
		t.Fatal("OnRestart was not invoked")
	}

	// 未接线 OnRestart 时明确返回 503。
	disabled := tokenTestHandler(t, Options{})
	disabledResult := httptest.NewRecorder()
	disabled.ServeHTTP(disabledResult, httptest.NewRequest(http.MethodPost, "/v1/restart", strings.NewReader(`{}`)))
	if disabledResult.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled restart status = %d, want 503", disabledResult.Code)
	}
}

func TestConfigDefaultsPerIPv6Mode(t *testing.T) {
	handler := newTestHandler(t, 4)
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/v1/config/defaults?mode=per_ipv6", nil))
	if result.Code != http.StatusOK {
		t.Fatalf("defaults status = %d", result.Code)
	}
	var defaults config.Config
	if err := json.NewDecoder(result.Body).Decode(&defaults); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	if defaults.SOCKS.Mode != config.ModePerIPv6 {
		t.Fatalf("defaults mode = %q, want per_ipv6", defaults.SOCKS.Mode)
	}
	if defaults.SOCKS.PortEnd-defaults.SOCKS.PortStart+1 < defaults.MaxLeases {
		t.Fatalf("per_ipv6 defaults port range (%d-%d) smaller than max_leases %d",
			defaults.SOCKS.PortStart, defaults.SOCKS.PortEnd, defaults.MaxLeases)
	}
	if len(defaults.SOCKS.AlwaysOnPorts) != 3 {
		t.Fatalf("per_ipv6 defaults always_on_ports = %v, want 3 entries", defaults.SOCKS.AlwaysOnPorts)
	}
	if defaults.Admin.WebUI != nil {
		t.Fatal("defaults must leave webui unset")
	}

	// 无 mode 参数时保持 multiplex 默认（无常开端口）。
	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/v1/config/defaults", nil))
	var plainDefaults config.Config
	if err := json.NewDecoder(plain.Body).Decode(&plainDefaults); err != nil {
		t.Fatalf("decode plain defaults: %v", err)
	}
	if plainDefaults.SOCKS.Mode != config.ModeMultiplex || len(plainDefaults.SOCKS.AlwaysOnPorts) != 0 {
		t.Fatalf("plain defaults = mode %q always_on_ports %v", plainDefaults.SOCKS.Mode, plainDefaults.SOCKS.AlwaysOnPorts)
	}
}
