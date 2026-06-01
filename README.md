# VLESS Standalone

VLESS Standalone 是一个基于 `sing-box` 内核的独立 VLESS + Reality 代理节点。项目去除了外部面板、远程 API 和复杂用户系统，采用单机静态配置，适合边缘 VPS、公开匿名节点或资源受限环境。

## 核心特性

- **单文件配置**：通过 `config.json` 管理端口、UUID、Reality 密钥、限流和状态 API。
- **Reality 支持**：内置 Reality X25519 密钥生成和公钥推导工具。
- **高并发限流**：基于 64 分片锁和滑动窗口，支持单 IP 并发连接数与每分钟新建连接数限制。
- **热重载**：发送 `SIGHUP` 后重新加载配置；新配置启动失败时回滚到旧配置。
- **UDP 空闲清理**：主动回收长时间空闲的 UDP 连接，避免额度被占满。
- **动态内存限制**：Linux 下根据 cgroup 或物理内存自动设置 `GOMEMLIMIT`。
- **本地状态 API**：默认只允许 loopback 访问，可查看运行负载、内存、版本和限流状态。

## 快速安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/miliyao/vless-standalone/main/install.sh)
```

常用示例：

```bash
bash install.sh --port=8443 --max-conn=50 --max-cps=30
```

如果你不使用 GitHub Actions，也可以从自己的下载源安装：

```bash
bash install.sh --base-url=https://example.com/releases --version=latest
```

下载源需要提供下列文件：

```text
vless-standalone-linux-amd64
vless-standalone-linux-amd64.sha256
vless-standalone-linux-arm64
vless-standalone-linux-arm64.sha256
```

## 安装参数

| 参数 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `--port=443` | `443` | 代理服务监听端口 |
| `--domain=www.amd.com` | `www.amd.com` | Reality 伪装 SNI 域名 |
| `--uuid=...` | 自动生成 | 指定 VLESS UUID |
| `--private-key=...` | 自动生成 | 指定 Reality 私钥 |
| `--public-key=...` | 自动推导 | 指定 Reality 公钥，用于输出分享链接 |
| `--version=latest` | `latest` | 发布版本；非 latest 时从 GitHub Release 下载 |
| `--base-url=...` | 空 | 自定义二进制下载源 |
| `--skip-checksum` | 关闭 | 找不到 `.sha256` 时允许跳过校验 |
| `--local` | 关闭 | 使用本地二进制安装 |
| `--local-bin=./file` | `./vless-standalone` | 指定本地二进制路径 |
| `--max-conn=100` | `100` | 单源 IP 最大并发连接数，`0` 表示不限制 |
| `--max-cps=60` | `60` | 单源 IP 每分钟新建连接数，`0` 表示不限制 |
| `--google-ipv6=true\|false` | `false` | Google 相关域名优先通过 IPv6 直连 |
| `--show-secrets` | 关闭 | 安装完成时显示 Reality 私钥 |

## 本地发布

需要 Go `1.24.7+`。

Linux/macOS：

```bash
VERSION=v1.0.0 ./build.sh
```

Windows PowerShell：

```powershell
.\build.ps1 -Version v1.0.0
```

脚本会在 `dist/` 目录生成：

```text
vless-standalone-linux-amd64
vless-standalone-linux-amd64.sha256
vless-standalone-linux-arm64
vless-standalone-linux-arm64.sha256
```

将 `dist/` 里的 4 个文件上传到你的静态下载源后，即可通过 `--base-url` 安装：

```bash
bash install.sh --base-url=https://example.com/releases
```

本地单架构开发构建：

```bash
go build -tags with_utls -o vless-standalone
./vless-standalone -version
```

## 配置文件

`config.json` 示例：

```json
{
  "log_level": "info",
  "server_port": 443,
  "listen_ip": "0.0.0.0",
  "flow": "xtls-rprx-vision",
  "google_ipv6": true,
  "clash_api_listen_addr": "",
  "status_api_listen_addr": "127.0.0.1:23333",
  "max_conn_per_ip": 80,
  "max_new_conn_per_ip_per_min": 240,
  "tls_settings": {
    "server_name": "www.amd.com",
    "server_port": "443",
    "private_key": "YOUR_REALITY_PRIVATE_KEY_HERE",
    "short_id": [
      "0123456789abcdef"
    ]
  },
  "uuids": [
    "de305d54-75b4-431b-adb2-eb6b9e546013"
  ]
}
```

配置检查：

```bash
./vless-standalone -config config.json -check-config
```

版本信息：

```bash
./vless-standalone -version
```

## Reality 密钥

生成密钥对：

```bash
./vless-standalone -gen-key
```

根据私钥推导公钥：

```bash
./vless-standalone -derive-pub "YOUR_PRIVATE_KEY"
```

## 运行与热重载

启动：

```bash
./vless-standalone -config config.json
```

热重载：

```bash
kill -s SIGHUP $(pgrep vless-standalone)
```

注意：底层监听端口需要重新绑定，热重载期间可能出现短暂连接中断，建议在低峰时操作。

## 状态 API

启用 `status_api_listen_addr` 后，可在本机访问：

```bash
curl http://127.0.0.1:23333/status
```

返回示例：

```json
{
  "version": "dev",
  "commit": "unknown",
  "build_time": "unknown",
  "go_version": "go1.24.7",
  "goos": "linux",
  "goarch": "amd64",
  "config_hash": "012345...",
  "listen_port": 443,
  "active_ips": 12,
  "active_connections": 87,
  "active_udp_connections": 4,
  "limit_settings": {
    "max_conn_per_ip": 80,
    "max_new_conn_per_ip_per_min": 240,
    "window_seconds": 60
  },
  "uptime_seconds": 12345,
  "memory_alloc_mib": 14.52,
  "memory_sys_mib": 32.1,
  "num_gc": 45,
  "goroutines": 120
}
```

状态 API 会校验远端地址，只允许本地回环访问。

## systemd

安装脚本会自动写入 systemd 服务。手动服务示例：

```ini
[Unit]
Description=VLESS Standalone Reality Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/vless-standalone -config=/etc/vless-standalone/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
LimitNOFILE=65535
WorkingDirectory=/etc/vless-standalone

[Install]
WantedBy=multi-user.target
```

常用命令：

```bash
systemctl start vless-standalone
systemctl reload vless-standalone
systemctl restart vless-standalone
journalctl -u vless-standalone -n 100 -f
```

## 排查

- `Reality private_key 长度错误`：私钥不是 32 字节 Base64，请用 `-gen-key` 重新生成。
- `UUID 无效`：UUID 必须符合 `8-4-4-4-12` 格式。
- `address already in use`：端口被占用，请换端口或停止占用进程。
- 服务启动失败：运行 `journalctl -u vless-standalone -n 80 --no-pager` 查看日志。

## 维护说明

仓库不再保存预编译二进制。请在本地或 CI 外部构建产物，并通过 Release、对象存储或自己的静态下载源分发。
