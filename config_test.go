package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func validTestConfig() Config {
	return Config{
		LogLevel:              "info",
		ServerPort:            443,
		ListenIP:              "0.0.0.0",
		Flow:                  "xtls-rprx-vision",
		GoogleIPv6:            true,
		ClashAPIListenAddr:    "127.0.0.1:9090",
		StatusAPIListenAddr:   "127.0.0.1:23333",
		MaxConnPerIP:          100,
		MaxNewConnPerIPPerMin: 60,
		TLSSettings: TLSSettings{
			ServerName: "www.example.com",
			ServerPort: "443",
			PrivateKey: base64.RawURLEncoding.EncodeToString([]byte{
				0, 1, 2, 3, 4, 5, 6, 7,
				8, 9, 10, 11, 12, 13, 14, 15,
				16, 17, 18, 19, 20, 21, 22, 23,
				24, 25, 26, 27, 28, 29, 30, 31,
			}),
			ShortID: []string{"0123456789abcdef"},
		},
		UUIDs: []string{"de305d54-75b4-431b-adb2-eb6b9e546013"},
	}
}

func writeConfigFile(t *testing.T, cfg Config) string {
	t.Helper()

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigValid(t *testing.T) {
	path := writeConfigFile(t, validTestConfig())

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.ServerPort != 443 {
		t.Fatalf("expected server port 443, got %d", cfg.ServerPort)
	}
}

func TestLoadConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Config)
		wantSubstr string
	}{
		{
			name: "invalid server port",
			mutate: func(cfg *Config) {
				cfg.ServerPort = 70000
			},
			wantSubstr: "server_port",
		},
		{
			name: "invalid listen ip",
			mutate: func(cfg *Config) {
				cfg.ListenIP = "not-an-ip"
			},
			wantSubstr: "listen_ip",
		},
		{
			name: "invalid log level",
			mutate: func(cfg *Config) {
				cfg.LogLevel = "verbose"
			},
			wantSubstr: "log_level",
		},
		{
			name: "missing reality server name",
			mutate: func(cfg *Config) {
				cfg.TLSSettings.ServerName = ""
			},
			wantSubstr: "server_name",
		},
		{
			name: "invalid reality private key",
			mutate: func(cfg *Config) {
				cfg.TLSSettings.PrivateKey = base64.RawURLEncoding.EncodeToString([]byte("too-short"))
			},
			wantSubstr: "private_key",
		},
		{
			name: "invalid short id",
			mutate: func(cfg *Config) {
				cfg.TLSSettings.ShortID = []string{"abc"}
			},
			wantSubstr: "short_id",
		},
		{
			name: "missing uuids",
			mutate: func(cfg *Config) {
				cfg.UUIDs = nil
			},
			wantSubstr: "uuids",
		},
		{
			name: "invalid uuid",
			mutate: func(cfg *Config) {
				cfg.UUIDs = []string{"not-a-uuid"}
			},
			wantSubstr: "UUID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validTestConfig()
			tt.mutate(&cfg)
			path := writeConfigFile(t, cfg)

			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantSubstr, err)
			}
		})
	}
}
