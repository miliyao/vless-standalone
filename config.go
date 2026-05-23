package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

// TLSSettings 承载 Reality 所需的证书与密钥配置
type TLSSettings struct {
	ServerName string   `json:"server_name"` // 目标服务器 SNI 域名（用于 Reality 握手伪装）
	ServerPort string   `json:"server_port"` // 目标服务器端口，默认 443
	PrivateKey string   `json:"private_key"` // Reality 私钥
	ShortID    []string `json:"short_id"`    // Reality Short ID 列表
}

// Config 是主程序加载 config.json 的全局配置容器
type Config struct {
	LogLevel              string      `json:"log_level"`                   // 日志级别：debug, info, warn, error
	ServerPort            int         `json:"server_port"`                 // 代理监听端口
	ListenIP              string      `json:"listen_ip"`                   // 绑定的监听 IP，为空则监听全局（:: / 0.0.0.0）
	Flow                  string      `json:"flow"`                        // VLESS 流控，如 xtls-rprx-vision
	GoogleIPv6            bool        `json:"google_ipv6"`                 // 是否开启 Google 域名强制 IPv6 直连优化
	ClashAPIListenAddr    string      `json:"clash_api_listen_addr"`       // 内置 Clash 控制面板地址，为空则关闭
	StatusAPIListenAddr   string      `json:"status_api_listen_addr"`      // 本地状态监控 API 地址，例如 127.0.0.1:23333，留空则关闭
	UUIDs                 []string    `json:"uuids"`                       // 允许接入的 VLESS UUID 列表（匿名公开节点可只配一个通用 UUID）
	TLSSettings           TLSSettings `json:"tls_settings"`                // Reality 证书配置
	MaxConnPerIP          int         `json:"max_conn_per_ip"`             // 单源 IP 最大并发连接数（0 表示不限制）
	MaxNewConnPerIPPerMin int         `json:"max_new_conn_per_ip_per_min"` // 单源 IP 每分钟允许新建的连接数（0 表示不限制）
}

var (
	uuidRegex  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	shortIDReg = regexp.MustCompile(`^[0-9a-fA-F]{2,16}$`)
)

// LoadConfig 从指定路径加载并校验 config.json
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 JSON 失败: %w", err)
	}

	// 1. 监听端口范围校验
	if cfg.ServerPort <= 0 || cfg.ServerPort > 65535 {
		return nil, fmt.Errorf("配置项 server_port 无效: %d (必须在 1-65535 之间)", cfg.ServerPort)
	}

	// 2. 监听 IP 格式校验
	if strings.TrimSpace(cfg.ListenIP) != "" {
		if _, err := netip.ParseAddr(strings.TrimSpace(cfg.ListenIP)); err != nil {
			return nil, fmt.Errorf("配置项 listen_ip 格式错误 %q: %w", cfg.ListenIP, err)
		}
	}

	// 3. 日志级别校验
	logLvl := strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	if logLvl != "debug" && logLvl != "info" && logLvl != "warn" && logLvl != "error" {
		return nil, fmt.Errorf("配置项 log_level 无效: %q (必须是 debug/info/warn/error)", cfg.LogLevel)
	}

	// 4. Reality 伪装域名校验
	if strings.TrimSpace(cfg.TLSSettings.ServerName) == "" {
		return nil, fmt.Errorf("Reality 配置缺失: server_name 不能为空")
	}

	// 5. Reality 私钥 32 字节 Base64 强校验
	privKey := strings.TrimSpace(cfg.TLSSettings.PrivateKey)
	if privKey == "" {
		return nil, fmt.Errorf("Reality 配置缺失: private_key 不能为空")
	}
	privKeyNorm := strings.TrimRight(privKey, "=")
	privKeyNorm = strings.ReplaceAll(privKeyNorm, "+", "-")
	privKeyNorm = strings.ReplaceAll(privKeyNorm, "/", "_")
	privBytes, err := base64.RawURLEncoding.DecodeString(privKeyNorm)
	if err != nil {
		return nil, fmt.Errorf("Reality private_key 解码 Base64 失败: %w", err)
	}
	if len(privBytes) != 32 {
		return nil, fmt.Errorf("Reality private_key 长度错误: 必须是 32 字节 (Base64 解密后为 %d 字节)", len(privBytes))
	}

	// 6. Reality ShortID 格式及偶数长度校验
	for i, sid := range cfg.TLSSettings.ShortID {
		if !shortIDReg.MatchString(sid) || len(sid)%2 != 0 {
			return nil, fmt.Errorf("第 %d 个 short_id %q 无效: 必须是偶数长度 (2-16) 的十六进制字符串", i+1, sid)
		}
	}

	// 7. 用户 UUID 格式校验
	if len(cfg.UUIDs) == 0 {
		return nil, fmt.Errorf("配置项 uuids 不能为空，至少需要一个 VLESS UUID")
	}
	for i, u := range cfg.UUIDs {
		uTrimmed := strings.TrimSpace(u)
		if !uuidRegex.MatchString(uTrimmed) {
			return nil, fmt.Errorf("第 %d 个 UUID %q 无效: 必须符合 RFC 4122 标准格式", i+1, u)
		}
	}

	return &cfg, nil
}
