//go:build !linux

package main

import (
	"os"
	"runtime/debug"

	"go.uber.org/zap"
)

func setDynamicMemoryLimit(logger *zap.Logger) {
	if os.Getenv("GOMEMLIMIT") == "" {
		// 非 Linux 开发/测试平台默认指定 1GiB 内存软限制，防止本地调试时突发内存暴涨
		debug.SetMemoryLimit(1024 * 1024 * 1024)
		logger.Info("非 Linux 平台垃圾回收内存阈值设定完成 (默认 1GiB)")
	}
}

func checkSystemSettings(logger *zap.Logger) {
	// 非 Linux 平台不执行系统级 BBR 和文件句柄限制检测
}
