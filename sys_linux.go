//go:build linux

package main

import (
	"os"
	"strings"
	"syscall"

	"go.uber.org/zap"
)

func checkSystemSettings(logger *zap.Logger) {
	// 1. 尝试自动启用 TCP BBR 拥塞控制算法
	congestionControlPath := "/proc/sys/net/ipv4/tcp_congestion_control"
	if data, err := os.ReadFile(congestionControlPath); err == nil {
		algo := strings.TrimSpace(string(data))
		if algo != "bbr" {
			// 尝试以 root 权限直接动态修改系统变量
			if err := os.WriteFile(congestionControlPath, []byte("bbr"), 0644); err == nil {
				logger.Info("系统 TCP BBR 拥塞控制算法未启用，程序已自动将其动态切换为 BBR")
			} else {
				logger.Warn("系统未启用 TCP BBR 拥塞控制算法，这在跨境网络环境下可能会影响连接速度。建议通过以下命令开启:\n" +
					"  echo \"net.core.default_qdisc=fq\" >> /etc/sysctl.conf\n" +
					"  echo \"net.ipv4.tcp_congestion_control=bbr\" >> /etc/sysctl.conf\n" +
					"  sysctl -p")
			}
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

		// 如果提升后依然过低，发出警告指引
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
