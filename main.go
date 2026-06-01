package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const shutdownTimeout = 30 * time.Second

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config.json")
	checkConfig := flag.Bool("check-config", false, "validate config and exit without starting the proxy")
	showVersion := flag.Bool("version", false, "print version information and exit")
	genKey := flag.Bool("gen-key", false, "generate a Reality X25519 private/public key pair as JSON")
	derivePub := flag.String("derive-pub", "", "derive a Reality public key from a Base64 private key")
	flag.Parse()

	if *showVersion {
		printVersion()
		return
	}

	if *genKey {
		if err := genRealityKeys(); err != nil {
			fmt.Fprintf(os.Stderr, "generate Reality key pair failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *derivePub != "" {
		if err := derivePublicKey(*derivePub); err != nil {
			fmt.Fprintf(os.Stderr, "derive public key failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed: %v\n", err)
		os.Exit(1)
	}
	if *checkConfig {
		fmt.Printf("config validation passed: %s\n", *configPath)
		return
	}

	logger, err := newLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize logger failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = logger.Sync()
	}()

	logger.Info("VLESS standalone node is starting")

	setDynamicMemoryLimit(logger)
	checkSystemSettings(logger)

	limiter := NewLimiter(cfg)
	engine := NewEngine(cfg, limiter, logger)

	if err := engine.Start(cfg); err != nil {
		logger.Fatal("node engine failed to start", zap.Error(err))
	}
	logger.Info("node core started and listening", zap.Int("listen_port", cfg.ServerPort))

	var statusSrv *http.Server
	if strings.TrimSpace(cfg.StatusAPIListenAddr) != "" {
		statusSrv = startStatusAPI(strings.TrimSpace(cfg.StatusAPIListenAddr), limiter, func() *Config {
			return cfg
		}, *configPath, logger)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			logger.Info("received SIGHUP, starting config reload")
			newCfg, err := LoadConfig(*configPath)
			if err != nil {
				logger.Error("config reload failed; continuing with current config", zap.Error(err))
				continue
			}

			if err := engine.Reload(newCfg); err != nil {
				logger.Error("reload core instance failed", zap.Error(err))
			} else {
				limiter.UpdateConfig(newCfg)
				cfg = newCfg
				logger.Info("config reload completed")
			}

		case syscall.SIGINT, syscall.SIGTERM:
			logger.Info("received termination signal, shutting down", zap.String("signal", sig.String()))

			closeCtx, closeCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer closeCancel()

			if statusSrv != nil {
				_ = statusSrv.Shutdown(closeCtx)
			}

			if err := engine.Close(); err != nil {
				logger.Error("core instance close failed", zap.Error(err))
			}

			logger.Info("node stopped")
			return
		}
	}
}

func printVersion() {
	res := map[string]string{
		"version":    version,
		"commit":     commit,
		"build_time": buildTime,
		"go_version": runtime.Version(),
		"goos":       runtime.GOOS,
		"goarch":     runtime.GOARCH,
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Printf("version=%s commit=%s build_time=%s go=%s\n", version, commit, buildTime, runtime.Version())
		return
	}
	fmt.Println(string(data))
}

func newLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zap.DebugLevel
	case "info":
		zapLevel = zap.InfoLevel
	case "warn":
		zapLevel = zap.WarnLevel
	case "error":
		zapLevel = zap.ErrorLevel
	default:
		zapLevel = zap.InfoLevel
	}

	cfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(zapLevel),
		Development: false,
		Encoding:    "json",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "ts",
			LevelKey:       "level",
			MessageKey:     "msg",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.SecondsDurationEncoder,
		},
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	return cfg.Build()
}

func startStatusAPI(addr string, limiter *Limiter, currentConfig func() *Config, configPath string, logger *zap.Logger) *http.Server {
	mux := http.NewServeMux()
	startTime := time.Now()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error": "forbidden - invalid remote address"}`))
			return
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error": "forbidden - local access only"}`))
			return
		}

		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)

		snapshot := limiter.Snapshot()
		cfg := currentConfig()
		res := map[string]interface{}{
			"version":                version,
			"commit":                 commit,
			"build_time":             buildTime,
			"go_version":             runtime.Version(),
			"goos":                   runtime.GOOS,
			"goarch":                 runtime.GOARCH,
			"config_hash":            hashConfigFile(configPath),
			"listen_port":            cfg.ServerPort,
			"active_ips":             snapshot["active_ips"],
			"active_connections":     snapshot["active_connections"],
			"active_udp_connections": snapshot["active_udp_connections"],
			"limit_settings":         limiter.Limits(),
			"uptime_seconds":         int64(time.Since(startTime).Seconds()),
			"memory_alloc_mib":       float64(memStats.Alloc) / 1024 / 1024,
			"memory_sys_mib":         float64(memStats.Sys) / 1024 / 1024,
			"num_gc":                 memStats.NumGC,
			"goroutines":             runtime.NumGoroutine(),
		}

		data, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": "internal server error"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		logger.Info("local status API started", zap.String("listen_addr", addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("local status API stopped unexpectedly", zap.Error(err))
		}
	}()

	return server
}

func hashConfigFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func genRealityKeys() error {
	privKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pubKey := privKey.PublicKey()

	privStr := base64.RawURLEncoding.EncodeToString(privKey.Bytes())
	pubStr := base64.RawURLEncoding.EncodeToString(pubKey.Bytes())

	res := map[string]string{
		"private_key": privStr,
		"public_key":  pubStr,
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func derivePublicKey(privStr string) error {
	privStr = strings.TrimRight(privStr, "=")
	privStr = strings.ReplaceAll(privStr, "+", "-")
	privStr = strings.ReplaceAll(privStr, "/", "_")

	privBytes, err := base64.RawURLEncoding.DecodeString(privStr)
	if err != nil {
		return fmt.Errorf("Base64 decode private key failed: %w", err)
	}

	if len(privBytes) != 32 {
		return fmt.Errorf("private key length must be 32 bytes, got %d bytes", len(privBytes))
	}

	privKey, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		return fmt.Errorf("parse X25519 private key failed: %w", err)
	}

	pubKey := privKey.PublicKey()
	pubStr := base64.RawURLEncoding.EncodeToString(pubKey.Bytes())
	fmt.Println(pubStr)
	return nil
}
