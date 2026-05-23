//go:build !linux

package main

import "go.uber.org/zap"

func checkSystemSettings(logger *zap.Logger) {
	// 非 Linux 平台不执行系统级 BBR 和文件句柄限制检测
}
