# VLESS Standalone 独立版匿名代理节点

本项目是一个基于 `sing-box` 内核（版本 `v1.13.11`）扁平化构建的独立高性能 VLESS + Reality 代理节点。去除了所有外部面板上报及复杂的集群用户系统，采用纯静态配置驱动，专为匿名公开使用或高并发、极限制约（如 1C1G 内存）的边缘 VPS 部署而设计。

---

## 🌟 核心特性

1. **零外部依赖（解耦）**：无需对接任何 Web 面板或远程 API，纯单机控制。
2. **高并发分片锁与滑动窗口限流 (CPS)**：限制器采用 64 分片锁（Lock Sharding）降低高并发下的锁竞争损耗，同时内置高精度滑动窗口日志算法限制单源 IP 的最大并发连接数和每分钟新建连接速率，有效防御端口滥用和网络爬虫轰炸。
3. **安全防灾热重载**：
   * 支持通过发送 `SIGHUP` 信号热更新配置而不中断现有进程。
   * **灾备回滚**：如果重载时新配置发生格式错误或新内核启动失败（例如端口冲突），将自动回滚拉起原配置运行，确保节点绝对高可用。
4. **动态内存优化**：自动检测 VPS 物理内存限制（兼容 Cgroup v1/v2 以及物理内存 `/proc/meminfo`），以可用物理内存的 **80%** 动态设定 Golang 垃圾回收软阈值 (`GOMEMLIMIT`)，在规避 OOM 的同时最大化利用空闲内存。
5. **UDP 稳定性与空闲主动回收**：
   * 安全透传 Mux 协议底层的 Headroom 数据，彻底规避 UDP 数据流转发时的 `panic: buffer overflow`（缓冲区溢出）隐患。
   * **空闲清理 (Janitor)**：在控制面实时追踪 UDP 报文活跃时间戳，对于闲置 30 秒以上的 UDP 连接进行主动释放和注销，避免连接数额度被死连接占满。
6. **内置密钥推导工具**：程序自身携带 Reality 密钥生成和公钥推导命令行工具，降低外部命令依赖。

---

## 🛠️ 快速开始

### 1. 一键安装部署（推荐，适用于 Linux 境外 VPS）

可以直接运行以下一键脚本进行安装：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/miliyao/vless-standalone/main/install.sh)
```

> [!TIP]
> * 该脚本会自动获取对应 CPU 架构的最新编译发布包、自动创建配置文件并配置证书密钥、托管 Systemd 服务并开启 Linux 内核 BBR 优化。
> * **可配参数**：您可以在一键命令后附加参数自定义端口或限制，例如限制单 IP 并发数为 50：
>   `bash <(curl -fsSL https://raw.githubusercontent.com/miliyao/vless-standalone/main/install.sh) --port=8443 --max-conn=50`

#### 一键脚本参数说明

| 安装参数 | 示例 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `--port` | `--port=443` | `443` | 代理服务外部监听端口 |
| `--domain` | `--domain=www.amd.com` | `www.amd.com` | Reality 伪装握手 SNI 域名 |
| `--uuid` | `--uuid=xxxxxx` | 随机生成 | 指定连接 UUID，若空则自动创建 |
| `--private-key` | `--private-key=xxx` | 随机生成 | 指定 Reality 密钥对中的私钥 |
| `--max-conn` | `--max-conn=100` | `100` | 单源 IP 允许的最大并发连接数 (0 表示无限制) |
| `--max-cps` | `--max-cps=60` | `60` | 单源 IP 每分钟允许新建的连接数限制 (0 表示无限制) |
| `--google-ipv6` | `--google-ipv6=true`| `false` | 是否强制将 Google 流量通过本地 IPv6 路由直连 (可选: `true`/`false`) |
| `--local` | `--local` | 关闭 | 使用本地编译的 `./vless-standalone` 进行覆盖部署（跳过 GitHub 远程下载） |
| `--local-bin` | `--local-bin=./file`| `./vless-standalone` | 本地部署模式下指定要拷贝的本地二进制路径 |
| `--show-secrets`| `--show-secrets` | 隐藏 | 在安装成功控制台输出中，是否显式显示 Reality 私钥 |

---

### 2. 手动编译部署（开发/测试）

#### A. 编译项目

在 Go 1.23.0+ 环境下，运行以下命令编译出二进制程序：

```bash
# 整理依赖
go mod tidy

# 编译程序 (必须包含 with_utls 编译标签以支持 Reality)
go build -tags with_utls -o vless-standalone
```

#### B. 编写配置文件 `config.json`

在程序同级目录下创建 `config.json`。配置范本如下：

```json
{
  "log_level": "info",
  "server_port": 443,
  "listen_ip": "0.0.0.0",
  "flow": "xtls-rprx-vision",
  "google_ipv6": true,
  "clash_api_listen_addr": "127.0.0.1:9090",
  "status_api_listen_addr": "127.0.0.1:23333",
  "max_conn_per_ip": 100,
  "max_new_conn_per_ip_per_min": 60,
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

#### 配置参数说明

| 配置字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `log_level` | string | 日志等级，可选 `debug`, `info`, `warn`, `error` |
| `server_port` | int | 节点监听的外部代理端口 |
| `listen_ip` | string | 监听 IP，留空则默认监听全局 IPv4/IPv6 |
| `flow` | string | VLESS 流控模式，建议使用 `xtls-rprx-vision` |
| `google_ipv6` | bool | 是否将 Google 域名的流量强制通过本地 IPv6 路由直连（降低 IPv4 负载） |
| `status_api_listen_addr` | string | 本地状态监控与负载快照 API 监听地址，例如 `127.0.0.1:23333`，留空则关闭 |
| `max_conn_per_ip` | int | 单源 IP 允许的最大并发 TCP/UDP 连接数，`0` 表示不限制 |
| `max_new_conn_per_ip_per_min` | int | 单源 IP 每分钟允许新建的连接数限制 (CPS)，`0` 表示不限制 |
| `tls_settings.private_key` | string | Base64 格式的 Reality 32 字节私钥 |
| `tls_settings.short_id` | []string | 偶数长度（2-16）的十六进制 short_id 列表 |
| `uuids` | []string | 允许接入的符合 RFC 4122 规范的 VLESS 客户端 UUID 列表 |

---

## 🔑 Reality 密钥管理

本项目内置密钥生成工具，不需要借助外部 openssl 或第三方工具：

### 1. 生成随机密钥对
```bash
./vless-standalone -gen-key
```
返回格式：
```json
{
  "private_key": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx=",
  "public_key": "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy="
}
```

### 2. 通过已有的私钥推导公钥
```bash
./vless-standalone -derive-pub "YOUR_PRIVATE_KEY"
```

---

## 🚀 运维与信号管理

### 启动程序
```bash
./vless-standalone -config config.json
```

### 热重载配置
当您修改了 `config.json` 中的限制阈值或用户 UUID 时，可向进程发送 `SIGHUP` 信号刷新配置：
```bash
kill -s SIGHUP $(pgrep vless-standalone)
```
> [!IMPORTANT]
> **热重载边界说明**：由于 sing-box 底层独占监听端口以进行入站流量解密，重新绑定端口需要先释放旧的监听。因此在重载过程中会产生**约 0.5-1 秒的短暂连接中断**，在有持续的长连接流量时，建议在流量低谷时段执行。

### 本地负载监控
若开启了 `status_api_listen_addr`，您可以在本地通过 curl 快速查询当前节点运行负载及内存状态（安全校验只允许 loopback 本地回环访问）：
```bash
curl http://127.0.0.1:23333/status
```
返回 JSON 结构示例：
```json
{
  "active_ips": 12,
  "active_connections": 87,
  "uptime_seconds": 12345,
  "memory_alloc_mib": 14.52,
  "memory_sys_mib": 32.10,
  "num_gc": 45,
  "goroutines": 120
}
```

---

## 📦 Systemd 服务部署

推荐将程序作为服务托管在 Linux 系统中（一键部署脚本已自动配置）：

创建 `/etc/systemd/system/vless-standalone.service` 文件：
```ini
[Unit]
Description=VLESS Standalone Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/etc/vless-standalone
ExecStart=/usr/local/bin/vless-standalone -config /etc/vless-standalone/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5s
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

**管理命令：**
```bash
# 启动服务
systemctl start vless-standalone

# 重载配置 (平滑重载)
systemctl reload vless-standalone

# 开启自启
systemctl enable vless-standalone

# 查看最近 100 行日志
journalctl -u vless-standalone -n 100 -f
```

---

## 🔍 常见报错与排查

1. **`Reality private_key 长度错误`**
   * **原因**：填写的私钥不是合法的 32 字节 Base64 字符串。
   * **解决**：使用 `./vless-standalone -gen-key` 重新生成合法的私钥。

2. **`UUID 无效: 必须符合 RFC 4122 标准格式`**
   * **原因**：填写的 UUID 不符合 standard UUID 格式。
   * **解决**：在 Linux 下执行 `uuidgen` 生成一个新的 UUID，或使用任何符合 8-4-4-4-12 格式的随机字符串。

3. **`端口被占用: address already in use`**
   * **原因**：您配置的 `server_port` 已经被其他程序（例如 Nginx 或其它 sing-box）占用，或者该端口没有被释放。
   * **解决**：执行 `netstat -lnpt | grep 端口号` 找到占用端口的 PID，并将其关闭；或者修改 `config.json` 换用其他空闲端口。

4. **`系统 TCP BBR 拥塞控制算法未启用` 警告**
   * **原因**：宿主机操作系统未开启 BBR，在高延迟跨境传输下可能导致性能大幅衰减。
   * **解决**：程序本身作为无特权/解耦的代理服务，不会主动污染和更改操作系统的全局内核参数。您应当手动开启：
     ```bash
     echo "net.core.default_qdisc=fq" >> /etc/sysctl.conf
     echo "net.ipv4.tcp_congestion_control=bbr" >> /etc/sysctl.conf
     sysctl -p
     ```
