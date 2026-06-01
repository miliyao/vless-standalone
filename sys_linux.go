//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"go.uber.org/zap"
)

// setDynamicMemoryLimit sets GOMEMLIMIT from cgroup or physical memory capacity.
func setDynamicMemoryLimit(logger *zap.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" {
		logger.Info("GOMEMLIMIT is already set; skipping dynamic memory limit tuning")
		return
	}

	var limitBytes uint64
	var source string

	if val, err := readUintFromFile("/sys/fs/cgroup/memory.max"); err == nil && val > 0 && val < 9000000000000000000 {
		limitBytes = val
		source = "cgroup v2 memory.max"
	}

	if limitBytes == 0 {
		if val, err := readUintFromFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil && val > 0 && val < 9000000000000000000 {
			limitBytes = val
			source = "cgroup v1 memory.limit_in_bytes"
		}
	}

	if limitBytes == 0 {
		if val, err := readMemTotalFromProc(); err == nil && val > 0 {
			limitBytes = val
			source = "/proc/meminfo"
		}
	}

	if limitBytes == 0 {
		limitBytes = 750 * 1024 * 1024
		source = "fallback"
	}

	softLimit := limitBytes * 80 / 100
	debug.SetMemoryLimit(int64(softLimit))

	logger.Info("dynamic GOMEMLIMIT applied",
		zap.String("memory_source", source),
		zap.String("total_available", fmt.Sprintf("%.2f MiB", float64(limitBytes)/1024/1024)),
		zap.String("applied_gomemlimit", fmt.Sprintf("%.2f MiB", float64(softLimit)/1024/1024)),
	)
}

func readUintFromFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	valStr := strings.TrimSpace(string(data))
	if valStr == "max" {
		return 0, fmt.Errorf("cgroup limit is max")
	}
	val, err := strconv.ParseUint(valStr, 10, 64)
	if err != nil {
		return 0, err
	}
	return val, nil
}

func readMemTotalFromProc() (uint64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, err := strconv.ParseUint(fields[1], 10, 64)
				if err != nil {
					return 0, err
				}
				return val * 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

func checkSystemSettings(logger *zap.Logger) {
	congestionControlPath := "/proc/sys/net/ipv4/tcp_congestion_control"
	if data, err := os.ReadFile(congestionControlPath); err == nil {
		algo := strings.TrimSpace(string(data))
		if algo != "bbr" {
			logger.Warn("TCP BBR is not enabled; cross-region throughput may be lower",
				zap.String("current_algo", algo),
				zap.String("hint", "enable fq and net.ipv4.tcp_congestion_control=bbr"),
			)
		} else {
			logger.Info("TCP BBR is enabled", zap.String("algo", algo))
		}
	}

	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err == nil {
		if rlimit.Cur < 65535 {
			targetLimit := uint64(65535)
			if rlimit.Max < targetLimit {
				targetLimit = rlimit.Max
			}
			if targetLimit > rlimit.Cur {
				newLimit := rlimit
				newLimit.Cur = targetLimit
				if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &newLimit); err == nil {
					logger.Info("raised current process nofile soft limit", zap.Uint64("new_limit", targetLimit))
					rlimit = newLimit
				}
			}
		}

		if rlimit.Cur < 32768 {
			logger.Warn("nofile soft limit is low and may affect high concurrency",
				zap.Uint64("current_soft_limit", rlimit.Cur),
				zap.Uint64("current_hard_limit", rlimit.Max),
				zap.String("hint", "set LimitNOFILE=65535 in the systemd service"),
			)
		} else {
			logger.Info("nofile limit looks healthy", zap.Uint64("soft_limit", rlimit.Cur))
		}
	}
}
