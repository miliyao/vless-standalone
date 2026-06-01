package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
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

func main() {
	configPath := flag.String("config", "config.json", "配置文件 config.json 的路径")
	genKey := flag.Bool("gen-key", false, "生成一对 Reality X25519 私钥/公钥对并以 JSON 输出")
	derivePub := flag.String("derive-pub", "", "根据给定的 Base64 私钥推导其对应的 Reality 公钥")
	flag.Parse()

	if *genKey {
		if err := genRealityKeys(); err != nil {
			fmt.Fprintf(os.Stderr, "生成 Reality 密钥对失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *derivePub != "" {
		if err := derivePublicKey(*derivePub); err != nil {
			fmt.Fprintf(os.Stderr, "推导公钥失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 预加载配置以获取日志等级
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置文件失败: %v\n", err)
		os.Exit(1)
	}

	logger, err := newLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志模块失败: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = logger.Sync()
	}()

	logger.Info("独立版 VLESS 节点正在启动...")

	// 执行跨平台动态 GOMEMLIMIT 内存软限制调整 (规避 OOM 并提升 GC 效率)
	setDynamicMemoryLimit(logger)

	// 执行 Linux 运行环境网络及句柄上限自动调优与检测
	checkSystemSettings(logger)

	// 创建连接与速度限制器，并初始化静态用户表
	limiter := NewLimiter(cfg)

	// 初始化内置内核引擎调度器
	engine := NewEngine(cfg, limiter, logger)

	// 启动引擎
	if err := engine.Start(cfg); err != nil {
		logger.Fatal("节点引擎启动失败", zap.Error(err))
	}
	logger.Info("节点内核成功启动并已进入监听状态", zap.Int("listen_port", cfg.ServerPort))

	// 启动本地状态监控 API 服务
	var statusSrv *http.Server
	if strings.TrimSpace(cfg.StatusAPIListenAddr) != "" {
		statusSrv = startStatusAPI(strings.TrimSpace(cfg.StatusAPIListenAddr), limiter, logger)
	}

	// 注册信号接管：SIGHUP 用于热更新配置，SIGINT/SIGTERM 用于退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			logger.Info("接收到 SIGHUP 信号，开始尝试热重载配置文件")
			newCfg, err := LoadConfig(*configPath)
			if err != nil {
				logger.Error("热重载失败，读取新配置文件出错，将继续使用原先配置运行", zap.Error(err))
				continue
			}

			// 重载 sing-box 底层内核监听
			if err := engine.Reload(newCfg); err != nil {
				logger.Error("重载内核实例失败", zap.Error(err))
			} else {
				// 仅在内核实例成功切换后更新限流阈值，避免回滚时配置不一致。
				limiter.UpdateConfig(newCfg)
				logger.Info("热重载成功，用户及核心实例配置已刷新")
			}

		case syscall.SIGINT, syscall.SIGTERM:
			logger.Info("收到终止信号，开始准备优雅关闭进程", zap.String("signal", sig.String()))

			// 限制优雅关闭的最长阻断时间
			closeCtx, closeCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer closeCancel()

			if statusSrv != nil {
				_ = statusSrv.Shutdown(closeCtx)
			}

			if err := engine.Close(); err != nil {
				logger.Error("关闭内核实例时发生错误", zap.Error(err))
			}

			logger.Info("节点守护退出，运行结束")
			return
		}
	}
}

// newLogger 构建一个高性能的 zap JSON 结构化日志输出流
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

// startStatusAPI 启动面向 localhost 的健康度与负载快照查询端口
func startStatusAPI(addr string, limiter *Limiter, logger *zap.Logger) *http.Server {
	mux := http.NewServeMux()
	startTime := time.Now()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// 核心安全校验：仅允许本地环回接口地址访问，防止公网暴露
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error": "forbidden - invalid remote address"}`))
			return
		}
		ip := net.ParseIP(host)
		if ip == nil || (!ip.IsLoopback() && host != "localhost") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error": "forbidden - local access only"}`))
			return
		}

		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)

		snapshot := limiter.Snapshot()
		res := map[string]interface{}{
			"active_ips":         snapshot["active_ips"],
			"active_connections": snapshot["active_connections"],
			"uptime_seconds":     int64(time.Since(startTime).Seconds()),
			"memory_alloc_mib":   float64(memStats.Alloc) / 1024 / 1024,
			"memory_sys_mib":     float64(memStats.Sys) / 1024 / 1024,
			"num_gc":             memStats.NumGC,
			"goroutines":         runtime.NumGoroutine(),
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
		logger.Info("本地状态监控 API 服务已启动", zap.String("listen_addr", addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("本地状态监控 API 服务运行异常终止", zap.Error(err))
		}
	}()

	return server
}

// genRealityKeys 生成一对符合 Reality 规范的 X25519 密钥对
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

// derivePublicKey 根据给定的 Base64 编码私钥推导出对应的 Reality 公钥
func derivePublicKey(privStr string) error {
	// 统一处理可能含有的等号填充以及 URL 安全 Base64 和标准 Base64 的兼容
	privStr = strings.TrimRight(privStr, "=")
	privStr = strings.ReplaceAll(privStr, "+", "-")
	privStr = strings.ReplaceAll(privStr, "/", "_")

	privBytes, err := base64.RawURLEncoding.DecodeString(privStr)
	if err != nil {
		return fmt.Errorf("Base64 解密私钥失败: %w", err)
	}

	if len(privBytes) != 32 {
		return fmt.Errorf("私钥长度必须为 32 字节，当前为 %d 字节", len(privBytes))
	}

	privKey, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		return fmt.Errorf("解析 X25519 私钥失败: %w", err)
	}

	pubKey := privKey.PublicKey()
	pubStr := base64.RawURLEncoding.EncodeToString(pubKey.Bytes())
	fmt.Println(pubStr)
	return nil
}
