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

// setDynamicMemoryLimit 根据 VPS 物理内存或容器限制动态计算并设定 Golang 的 GOMEMLIMIT
func setDynamicMemoryLimit(logger *zap.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" {
		logger.Info("检测到系统已显式设置 GOMEMLIMIT 环境变量，跳过动态内存限制调整")
		return
	}

	var limitBytes uint64
	var source string

	// 1. 尝试从 Cgroup v2 中获取内存软阈值
	if val, err := readUintFromFile("/sys/fs/cgroup/memory.max"); err == nil && val > 0 && val < 9000000000000000000 {
		limitBytes = val
		source = "Cgroup v2 (memory.max)"
	}

	// 2. 尝试从 Cgroup v1 中获取内存软阈值
	if limitBytes == 0 {
		if val, err := readUintFromFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil && val > 0 && val < 9000000000000000000 {
			limitBytes = val
			source = "Cgroup v1 (memory.limit_in_bytes)"
		}
	}

	// 3. 退一步从系统的 /proc/meminfo 读取物理内存总量
	if limitBytes == 0 {
		if val, err := readMemTotalFromProc(); err == nil && val > 0 {
			limitBytes = val
			source = "System Proc (/proc/meminfo)"
		}
	}

	// 4. 若全部读取失败，则使用 750MiB 作为最后的防爆兜底值
	if limitBytes == 0 {
		limitBytes = 750 * 1024 * 1024
		source = "Fallback Hardcode"
	}

	// 动态设定 GOMEMLIMIT 的最优分配比例为 80%
	softLimit := limitBytes * 80 / 100
	debug.SetMemoryLimit(int64(softLimit))

	logger.Info("Golang 垃圾回收内存阈值 (GOMEMLIMIT) 动态设定完成",
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
		return 0, fmt.Errorf("cgroup limit is set to max")
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
				// kB 转换为 Bytes
				return val * 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("未在 /proc/meminfo 中找到 MemTotal 项")
}

func checkSystemSettings(logger *zap.Logger) {
	// 1. 检测是否启用 TCP BBR 拥塞控制算法 (不主动修改以避免非预期系统状态变更)
	congestionControlPath := "/proc/sys/net/ipv4/tcp_congestion_control"
	if data, err := os.ReadFile(congestionControlPath); err == nil {
		algo := strings.TrimSpace(string(data))
		if algo != "bbr" {
			logger.Warn("检测到系统 TCP BBR 拥塞控制算法未启用，这在跨境网络环境下可能会影响连接速度。建议运行一键安装脚本或通过以下命令手动开启:\n" +
				"  echo \"net.core.default_qdisc=fq\" >> /etc/sysctl.conf\n" +
				"  echo \"net.ipv4.tcp_congestion_control=bbr\" >> /etc/sysctl.conf\n" +
				"  sysctl -p")
		} else {
			logger.Info("系统 TCP BBR 拥塞控制算法检测: 已启用", zap.String("algo", algo))
		}
	}

	// 2. 检测并尝试自动提升最大打开文件描述符软限制 (RLIMIT_NOFILE)
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
					logger.Info("程序已自动提升当前进程的文件描述符(nofile)软限制", zap.Uint64("new_limit", targetLimit))
					rlimit = newLimit
				}
			}
		}

		if rlimit.Cur < 32768 {
			logger.Warn("当前文件描述符(nofile)限制过低，可能会在高并发下导致连接失败。建议调大限制:",
				zap.Uint64("current_soft_limit", rlimit.Cur),
				zap.Uint64("current_hard_limit", rlimit.Max),
			)
			logger.Warn("建议在 /etc/security/limits.conf 中追加以下配置，或者在 systemd 服务配置中添加 LimitNOFILE=65535:\n" +
				"  * soft nofile 65535\n" +
				"  * hard nofile 65535")
		} else {
			logger.Info("当前文件描述符限制检测: 正常", zap.Uint64("soft_limit", rlimit.Cur))
		}
	}
}
