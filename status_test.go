package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newStatusTestServer(t *testing.T, cfg *Config, limiter *Limiter, configPath string) *httptest.Server {
	t.Helper()

	srv := startStatusAPI("127.0.0.1:0", limiter, func() *Config {
		return cfg
	}, configPath, zap.NewNop())
	t.Cleanup(func() {
		_ = srv.Close()
	})

	return httptest.NewServer(srv.Handler)
}

func decodeStatusResponse(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	return body
}

func TestStatusAPIResponseIncludesDiagnostics(t *testing.T) {
	cfg := validTestConfig()
	cfg.ServerPort = 8443
	cfg.MaxConnPerIP = 7
	cfg.MaxNewConnPerIPPerMin = 3

	configPath := writeConfigFile(t, cfg)
	limiter := NewLimiter(&cfg)
	ip := netip.MustParseAddr("192.0.2.10")
	lastActive := time.Now().UnixNano()
	limiter.Register(ConnMeta{
		ConnID:     "udp-1",
		SourceIP:   ip,
		StartedAt:  time.Now(),
		IsUDP:      true,
		LastActive: &lastActive,
	})

	server := newStatusTestServer(t, &cfg, limiter, configPath)
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body := decodeStatusResponse(t, resp)
	expectedHash := sha256.Sum256(mustReadFile(t, configPath))

	if body["version"] != version {
		t.Fatalf("expected version %q, got %v", version, body["version"])
	}
	if body["config_hash"] != hex.EncodeToString(expectedHash[:]) {
		t.Fatalf("unexpected config_hash: %v", body["config_hash"])
	}
	if body["listen_port"] != float64(8443) {
		t.Fatalf("expected listen_port 8443, got %v", body["listen_port"])
	}
	if body["active_ips"] != float64(1) {
		t.Fatalf("expected active_ips 1, got %v", body["active_ips"])
	}
	if body["active_connections"] != float64(1) {
		t.Fatalf("expected active_connections 1, got %v", body["active_connections"])
	}
	if body["active_udp_connections"] != float64(1) {
		t.Fatalf("expected active_udp_connections 1, got %v", body["active_udp_connections"])
	}

	limits, ok := body["limit_settings"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected limit_settings object, got %T", body["limit_settings"])
	}
	if limits["max_conn_per_ip"] != float64(7) {
		t.Fatalf("expected max_conn_per_ip 7, got %v", limits["max_conn_per_ip"])
	}
	if limits["max_new_conn_per_ip_per_min"] != float64(3) {
		t.Fatalf("expected max_new_conn_per_ip_per_min 3, got %v", limits["max_new_conn_per_ip_per_min"])
	}
	if limits["window_seconds"] != float64(60) {
		t.Fatalf("expected window_seconds 60, got %v", limits["window_seconds"])
	}
}

func TestStatusAPIRejectsNonLoopbackRemoteAddr(t *testing.T) {
	cfg := validTestConfig()
	configPath := writeConfigFile(t, cfg)
	limiter := NewLimiter(&cfg)
	server := newStatusTestServer(t, &cfg, limiter, configPath)
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "203.0.113.9:45678"
	rec := httptest.NewRecorder()

	server.Config.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", rec.Code)
	}
}

func TestStatusAPIConfigHashReflectsFileChanges(t *testing.T) {
	cfg := validTestConfig()
	configPath := writeConfigFile(t, cfg)
	limiter := NewLimiter(&cfg)
	server := newStatusTestServer(t, &cfg, limiter, configPath)
	defer server.Close()

	firstResp, err := server.Client().Get(server.URL + "/status")
	if err != nil {
		t.Fatalf("GET first /status: %v", err)
	}
	first := decodeStatusResponse(t, firstResp)

	if err := os.WriteFile(configPath, []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatalf("write changed config: %v", err)
	}

	secondResp, err := server.Client().Get(server.URL + "/status")
	if err != nil {
		t.Fatalf("GET second /status: %v", err)
	}
	second := decodeStatusResponse(t, secondResp)

	if first["config_hash"] == second["config_hash"] {
		t.Fatalf("expected config_hash to change, got %v", second["config_hash"])
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	return data
}
