#!/usr/bin/env bash
set -eu

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SERVICE_NAME="vless-standalone"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/${SERVICE_NAME}"
CONFIG_FILE="${CONFIG_DIR}/config.json"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
RELEASE_REPO="miliyao/singbox-bridge"
RELEASE_VERSION="latest"

usage() {
    cat <<EOF
一键部署独立版 VLESS 节点 (VLESS + XTLS-Vision + Reality)

使用方法:
  bash install.sh [参数]

可选参数:
  --port=8443               监听端口 (默认: 8443)
  --domain=www.amd.com      Reality 伪装目标域名 (默认: www.amd.com)
  --uuid=xxxxxx             指定 VLESS UUID (默认: 自动生成)
  --private-key=xxxxxx      指定 Reality 私钥 (默认: 自动生成)
  --version=latest          发布包版本 (默认: latest)
  --help | -h               查看此帮助信息
EOF
}

log_info() {
    echo -e "${GREEN}[INFO] $1${NC}"
}

log_warn() {
    echo -e "${YELLOW}[WARN] $1${NC}"
}

log_error() {
    echo -e "${RED}[ERROR] $1${NC}" >&2
}

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        log_error "此安装脚本必须以 root 权限运行 (sudo)。"
        exit 1
    fi
}

install_packages() {
    log_info "正在检测并安装系统依赖包..."
    local missing=""
    for cmd in curl jq openssl; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            missing="$missing $cmd"
        fi
    done

    if [ -n "$missing" ]; then
        if command -v apt-get >/dev/null 2>&1; then
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -y && apt-get install -y ca-certificates $missing
        elif command -v dnf >/dev/null 2>&1; then
            dnf install -y ca-certificates $missing
        elif command -v yum >/dev/null 2>&1; then
            yum install -y ca-certificates $missing
        elif command -v apk >/dev/null 2>&1; then
            apk add --no-cache ca-certificates $missing
        else
            log_error "未找到支持的包管理器，请先手动安装: $missing"
            exit 1
        fi
    fi
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)
            log_error "不支持的系统架构: $(uname -m)"
            exit 1
            ;;
    esac
}

generate_secrets() {
    if [ -z "${USER_UUID:-}" ]; then
        if command -v uuidgen >/dev/null 2>&1; then
            USER_UUID=$(uuidgen)
        else
            USER_UUID=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || od -x -N 16 /dev/urandom | head -n 1 | awk '{print $2$3"-"$4"-"$5"-"$6"-"$7$8$9}')
        fi
    fi

    if [ -z "${PRIVATE_KEY:-}" ]; then
        log_info "正在生成 Reality 随机私钥..."
        if openssl genpkey -algorithm x25519 -outform DER 2>/dev/null | tail -c 32 | base64 > /dev/null 2>&1; then
            PRIVATE_KEY=$(openssl genpkey -algorithm x25519 -outform DER | tail -c 32 | base64 | tr -d '\n\r=')
        else
            PRIVATE_KEY=$(openssl rand -base64 32 | tr -d '\n\r=')
        fi
    fi
}

download_binary() {
    local asset_name="vless-standalone-linux-${ARCH}"
    local download_url=""

    if [ "$RELEASE_VERSION" = "latest" ]; then
        download_url="https://github.com/${RELEASE_REPO}/releases/latest/download/${asset_name}"
    else
        download_url="https://github.com/${RELEASE_REPO}/releases/download/${RELEASE_VERSION}/${asset_name}"
    fi

    log_info "正在从 GitHub 下载二进制程序..."
    log_info "地址: ${download_url}"

    local tmp_bin="${INSTALL_DIR}/${SERVICE_NAME}.tmp"
    if ! curl -fL --retry 3 --connect-timeout 15 -o "$tmp_bin" "$download_url"; then
        log_error "从 GitHub 下载预编译包失败，请检查网络或版本号。"
        exit 1
    fi

    chmod +x "$tmp_bin"
    mv "$tmp_bin" "${INSTALL_DIR}/${SERVICE_NAME}"
    log_info "二进制程序安装成功: ${INSTALL_DIR}/${SERVICE_NAME}"
}

write_config() {
    mkdir -p "$CONFIG_DIR"
    
    if [ -f "$CONFIG_FILE" ]; then
        log_warn "检测到已存在的配置文件: ${CONFIG_FILE}"
        read -p "是否覆盖已有的配置文件? [y/N]: " confirm
        if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
            log_info "已跳过配置文件生成，将使用原有配置。"
            return
        fi
    fi

    log_info "写入配置文件: ${CONFIG_FILE}..."
    cat > "$CONFIG_FILE" <<EOF
{
  "listen_ip": "::",
  "server_port": ${LISTEN_PORT},
  "flow": "xtls-rprx-vision",
  "log_level": "info",
  "clash_api_listen_addr": "",
  "google_ipv6": true,
  "tls_settings": {
    "server_name": "${DEST_DOMAIN}",
    "server_port": "443",
    "private_key": "${PRIVATE_KEY}",
    "short_id": [
      "12345678",
      "abcdef00"
    ]
  },
  "limits": {
    "max_conn_per_user": 128,
    "max_conn_per_ip": 0,
    "max_new_conn_per_user_per_min": 600,
    "max_new_conn_per_ip_per_min": 0
  },
  "users": [
    {
      "name": "default-user",
      "uuid": "${USER_UUID}",
      "speed_limit": 0,
      "device_limit": 0
    }
  ]
}
EOF
    chmod 600 "$CONFIG_FILE"
}

write_service() {
    log_info "正在配置 systemd 服务守护进程..."
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Standalone VLESS (Reality) Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${SERVICE_NAME} -config=${CONFIG_FILE}
Restart=always
RestartSec=5
LimitNOFILE=65535
WorkingDirectory=${CONFIG_DIR}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME"
    systemctl restart "$SERVICE_NAME"
    
    if ! systemctl is-active --quiet "$SERVICE_NAME"; then
        log_error "服务启动失败。请运行 'journalctl -u ${SERVICE_NAME} -n 50' 调试错误日志。"
        exit 1
    fi
    log_info "服务已成功安装并运行！"
}

enable_bbr() {
    log_info "正在检测并尝试启用系统 BBR 加速..."
    modprobe tcp_bbr 2>/dev/null || true
    
    local cc=""
    cc=$(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null || true)
    if [ "$cc" = "bbr" ]; then
        log_info "BBR 加速检测: 已处于启用状态。"
        return
    fi

    local available=""
    available=$(sysctl -n net.ipv4.tcp_available_congestion_control 2>/dev/null || true)
    if echo "$available" | grep -q bbr; then
        cat > /etc/sysctl.d/99-vless-standalone.conf <<EOF
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
EOF
        sysctl -p /etc/sysctl.d/99-vless-standalone.conf >/dev/null 2>&1 || true
        log_info "系统 BBR 调优开启成功。"
    else
        log_warn "该内核似乎不支持 BBR，已跳过。"
    fi
}

main() {
    require_root
    
    LISTEN_PORT=8443
    DEST_DOMAIN="www.amd.com"
    USER_UUID=""
    PRIVATE_KEY=""

    for arg in "$@"; do
        case "$arg" in
            --port=*) LISTEN_PORT="${arg#*=}" ;;
            --domain=*) DEST_DOMAIN="${arg#*=}" ;;
            --uuid=*) USER_UUID="${arg#*=}" ;;
            --private-key=*) PRIVATE_KEY="${arg#*=}" ;;
            --version=*) RELEASE_VERSION="${arg#*=}" ;;
            --help|-h)
                usage
                exit 0
                ;;
            *)
                log_error "未知参数: $arg"
                usage
                exit 1
                ;;
        esac
    done

    install_packages
    detect_arch
    generate_secrets
    download_binary
    write_config
    write_service
    enable_bbr

    echo -e "\n${GREEN}==================================================${NC}"
    echo -e "${GREEN} VLESS Standalone Reality 部署成功！${NC}"
    echo -e "${GREEN}==================================================${NC}"
    echo -e " 监听端口: ${YELLOW}${LISTEN_PORT}${NC}"
    echo -e " 伪装域名: ${YELLOW}${DEST_DOMAIN}${NC}"
    echo -e " 用户UUID: ${YELLOW}${USER_UUID}${NC}"
    echo -e " 客户端流控: ${YELLOW}xtls-rprx-vision${NC}"
    echo -e " Reality私钥: ${YELLOW}${PRIVATE_KEY}${NC}"
    echo -e " 启动命令: ${YELLOW}systemctl start ${SERVICE_NAME}${NC}"
    echo -e " 重启命令: ${YELLOW}systemctl restart ${SERVICE_NAME}${NC}"
    echo -e " 运行日志: ${YELLOW}journalctl -u ${SERVICE_NAME} -f${NC}"
    echo -e "${GREEN}==================================================${NC}\n"
}

main "$@"
EOF
