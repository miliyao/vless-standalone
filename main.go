package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const shutdownTimeout = 30 * time.Second

func main() {
	// 在 1C1G 极限制约的环境下，如果没有显式设置 GOMEMLIMIT，
	// 则默认强行指定 750MiB 的 GC 垃圾回收触发软限制，以有效规避 OOM-Killer 强杀。
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(750 * 1024 * 1024)
	}

	configPath := flag.String("config", "config.json", "配置文件 config.json 的路径")
	flag.Parse()

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

			// 更新限制器的静态内存表
			limiter.UpdateUsers(newCfg.Users)

			// 重载 sing-box 底层内核监听
			if err := engine.Reload(newCfg); err != nil {
				logger.Error("重载内核实例失败", zap.Error(err))
			} else {
				logger.Info("热重载成功，用户及核心实例配置已刷新")
			}

		case syscall.SIGINT, syscall.SIGTERM:
			logger.Info("收到终止信号，开始准备优雅关闭进程", zap.String("signal", sig.String()))

			// 限制优雅关闭的最长阻断时间
			closeCtx, closeCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_ = closeCtx
			closeCancel()

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
