//go:build !linux

package main

import (
	"os"
	"runtime/debug"

	"go.uber.org/zap"
)

func setDynamicMemoryLimit(logger *zap.Logger) {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(1024 * 1024 * 1024)
		logger.Info("non-Linux default GOMEMLIMIT applied", zap.String("limit", "1GiB"))
	}
}

func checkSystemSettings(logger *zap.Logger) {
	logger.Debug("system tuning checks are skipped on non-Linux platforms")
}
