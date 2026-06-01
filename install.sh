#!/usr/bin/env bash
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

SERVICE_NAME="vless-standalone"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/${SERVICE_NAME}"
CONFIG_FILE="${CONFIG_DIR}/config.json"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
RELEASE_REPO="miliyao/vless-standalone"
RELEASE_VERSION="latest"
BASE_URL=""

usage() {
    cat <<EOF
一键部署 VLESS Standalone 节点 (VLESS + XTLS-Vision + Reality)

用法:
  bash install.sh [参数]

可选参数:
  --port=443                监听端口，默认 443
  --domain=www.amd.com      Reality 伪装目标域名，默认 www.amd.com
  --uuid=xxxxxx             指定 VLESS UUID，默认自动生成
  --private-key=xxxxxx      指定 Reality 私钥，默认自动生成
  --public-key=xxxxxx       指定 Reality 公钥，默认自动推导
  --version=latest          发布版本，latest 表示从源码仓库下载构建产物
  --base-url=https://host   自定义二进制下载源，文件名需为 vless-standalone-linux-ARCH
  --skip-checksum           找不到 .sha256 文件时允许跳过完整性校验
  --show-secrets            输出中显示 Reality 私钥等敏感信息
  --local                   使用本地二进制，跳过远程下载
  --local-bin=./file        指定本地二进制路径，默认 ./vless-standalone
  --max-conn=100            单源 IP 最大并发连接数，0 表示不限制
  --max-cps=60              单源 IP 每分钟新建连接数，0 表示不限制
  --google-ipv6=true|false  是否将 Google 流量优先走 IPv6，默认 false
  --help | -h               查看帮助
EOF
}

log_info() { echo -e "${GREEN}[INFO] $1${NC}"; }
log_warn() { echo -e "${YELLOW}[WARN] $1${NC}"; }
log_error() { echo -e "${RED}[ERROR] $1${NC}" >&2; }

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        log_error "请使用 root 权限运行安装脚本，例如 sudo bash install.sh"
        exit 1
    fi
}

validate_number() {
    local name="$1"
    local value="$2"
    if ! [[ "$value" =~ ^[0-9]+$ ]]; then
        log_error "${name} 必须是非负整数，当前值: ${value}"
        exit 1
    fi
}

validate_port() {
    validate_number "port" "$LISTEN_PORT"
    if [ "$LISTEN_PORT" -lt 1 ] || [ "$LISTEN_PORT" -gt 65535 ]; then
        log_error "port 必须在 1-65535 之间，当前值: ${LISTEN_PORT}"
        exit 1
    fi
}

install_packages() {
    log_info "检查系统依赖..."
    local missing=""
    for cmd in curl jq openssl sha256sum; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            missing="$missing $cmd"
        fi
    done

    if [ -z "$missing" ]; then
        return
    fi

    if command -v apt-get >/dev/null 2>&1; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -y
        apt-get install -y ca-certificates $missing
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y ca-certificates $missing
    elif command -v yum >/dev/null 2>&1; then
        yum install -y ca-certificates $missing
    elif command -v apk >/dev/null 2>&1; then
        apk add --no-cache ca-certificates $missing
    else
        log_error "未找到支持的包管理器，请先手动安装:${missing}"
        exit 1
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

get_public_ip() {
    log_info "检测服务器公网 IP..."
    SERVER_IP=$(curl -sS --max-time 5 https://api.ipify.org 2>/dev/null || \
                curl -sS --max-time 5 https://ifconfig.me 2>/dev/null || \
                curl -sS --max-time 5 https://ipinfo.io/ip 2>/dev/null || \
                curl -sS --max-time 5 https://icanhazip.com 2>/dev/null || \
                echo "YOUR_VPS_IP")
}

load_existing_config() {
    if [ ! -f "$CONFIG_FILE" ]; then
        return
    fi

    log_info "检测到已有配置，将复用未显式指定的参数: ${CONFIG_FILE}"
    if [ -z "$USER_UUID" ]; then
        USER_UUID=$(jq -r '.uuids[0] // empty' "$CONFIG_FILE" 2>/dev/null || true)
    fi
    if [ -z "$PRIVATE_KEY" ]; then
        PRIVATE_KEY=$(jq -r '.tls_settings.private_key // empty' "$CONFIG_FILE" 2>/dev/null || true)
    fi
    if [ -z "$SHORT_ID" ]; then
        SHORT_ID=$(jq -r '.tls_settings.short_id[0] // empty' "$CONFIG_FILE" 2>/dev/null || true)
    fi

    if [ "$PORT_SET" = false ]; then
        local port
        port=$(jq -r '.server_port // empty' "$CONFIG_FILE" 2>/dev/null || true)
        if [ -n "$port" ]; then
            LISTEN_PORT="$port"
        fi
    fi
    if [ "$DOMAIN_SET" = false ]; then
        local domain
        domain=$(jq -r '.tls_settings.server_name // empty' "$CONFIG_FILE" 2>/dev/null || true)
        if [ -n "$domain" ]; then
            DEST_DOMAIN="$domain"
        fi
    fi
    if [ "$MAX_CONN_SET" = false ]; then
        local conn
        conn=$(jq -r '.max_conn_per_ip // empty' "$CONFIG_FILE" 2>/dev/null || true)
        if [ -n "$conn" ]; then
            MAX_CONN="$conn"
        fi
    fi
    if [ "$MAX_CPS_SET" = false ]; then
        local cps
        cps=$(jq -r '.max_new_conn_per_ip_per_min // empty' "$CONFIG_FILE" 2>/dev/null || true)
        if [ -n "$cps" ]; then
            MAX_CPS="$cps"
        fi
    fi
    if [ "$GOOGLE_IPV6_SET" = false ]; then
        local ipv6
        ipv6=$(jq -r '.google_ipv6 // empty' "$CONFIG_FILE" 2>/dev/null || true)
        if [ -n "$ipv6" ]; then
            GOOGLE_IPV6="$ipv6"
        fi
    fi

    return 0
}

generate_uuid() {
    if command -v uuidgen >/dev/null 2>&1; then
        uuidgen
    elif [ -r /proc/sys/kernel/random/uuid ]; then
        cat /proc/sys/kernel/random/uuid
    else
        openssl rand -hex 16 | sed -E 's/(.{8})(.{4})(.{4})(.{4})(.{12})/\1-\2-\3-\4-\5/'
    fi
}

generate_secrets() {
    if [ -z "${USER_UUID:-}" ]; then
        USER_UUID=$(generate_uuid)
    fi

    local binary_path="${INSTALL_DIR}/${SERVICE_NAME}"
    if [ -z "${PRIVATE_KEY:-}" ]; then
        log_info "通过程序生成 Reality 密钥对..."
        local keys_json=""
        if keys_json=$("$binary_path" -gen-key 2>/dev/null); then
            PRIVATE_KEY=$(echo "$keys_json" | jq -r '.private_key')
            PUBLIC_KEY=$(echo "$keys_json" | jq -r '.public_key')
        else
            log_error "Reality 密钥生成失败，请检查二进制是否可执行"
            exit 1
        fi
    fi

    if [ -n "${PRIVATE_KEY:-}" ] && [ -z "${PUBLIC_KEY:-}" ]; then
        log_info "根据 Reality 私钥推导公钥..."
        if ! PUBLIC_KEY=$("$binary_path" -derive-pub "$PRIVATE_KEY" 2>/dev/null); then
            log_warn "公钥推导失败，客户端链接将不会自动生成"
            PUBLIC_KEY=""
        fi
    fi

    if [ -z "${SHORT_ID:-}" ]; then
        SHORT_ID=$(openssl rand -hex 8)
    fi
}

asset_urls() {
    local asset_name="$1"
    if [ -n "$BASE_URL" ]; then
        BASE_URL="${BASE_URL%/}"
        DOWNLOAD_URL="${BASE_URL}/${asset_name}"
        SHA256_URL="${BASE_URL}/${asset_name}.sha256"
    elif [ "$RELEASE_VERSION" = "latest" ]; then
        DOWNLOAD_URL="https://github.com/${RELEASE_REPO}/releases/latest/download/${asset_name}"
        SHA256_URL="https://github.com/${RELEASE_REPO}/releases/latest/download/${asset_name}.sha256"
    else
        DOWNLOAD_URL="https://github.com/${RELEASE_REPO}/releases/download/${RELEASE_VERSION}/${asset_name}"
        SHA256_URL="https://github.com/${RELEASE_REPO}/releases/download/${RELEASE_VERSION}/${asset_name}.sha256"
    fi
}

install_binary() {
    mkdir -p "$INSTALL_DIR"
    local target="${INSTALL_DIR}/${SERVICE_NAME}"

    if [ "$USE_LOCAL" = true ]; then
        log_info "使用本地二进制安装: ${LOCAL_BIN_PATH}"
        if [ ! -f "$LOCAL_BIN_PATH" ]; then
            log_error "未找到本地二进制文件: ${LOCAL_BIN_PATH}"
            exit 1
        fi
        install -m 0755 "$LOCAL_BIN_PATH" "$target"
        return
    fi

    local asset_name="vless-standalone-linux-${ARCH}"
    asset_urls "$asset_name"

    log_info "下载二进制: ${DOWNLOAD_URL}"
    local tmp_dir
    tmp_dir=$(mktemp -d)
    local tmp_bin="${tmp_dir}/${asset_name}"
    local tmp_sha="${tmp_dir}/${asset_name}.sha256"

    if ! curl -fL --retry 3 --connect-timeout 15 -o "$tmp_bin" "$DOWNLOAD_URL"; then
        rm -rf "$tmp_dir"
        log_error "下载二进制失败，请检查网络、版本号或 --base-url"
        exit 1
    fi

    if curl -fL --retry 2 --connect-timeout 10 -o "$tmp_sha" "$SHA256_URL" 2>/dev/null; then
        local expected_hash
        expected_hash=$(awk '{print $1}' "$tmp_sha" | tr -d '\r\n')
        local actual_hash
        actual_hash=$(sha256sum "$tmp_bin" | awk '{print $1}' | tr -d '\r\n')
        if [ "$expected_hash" != "$actual_hash" ]; then
            log_error "SHA256 校验失败"
            log_error "期望: ${expected_hash}"
            log_error "实际: ${actual_hash}"
            rm -rf "$tmp_dir"
            exit 1
        fi
        log_info "SHA256 校验通过"
    elif [ "$SKIP_CHECKSUM" = true ]; then
        log_warn "未找到 .sha256 文件，已按 --skip-checksum 跳过校验"
    else
        log_error "未找到 .sha256 校验文件。若确认下载源可信，可加 --skip-checksum"
        rm -rf "$tmp_dir"
        exit 1
    fi

    install -m 0755 "$tmp_bin" "$target"
    rm -rf "$tmp_dir"
    log_info "二进制安装完成: ${target}"
}

write_config() {
    mkdir -p "$CONFIG_DIR"

    local backup=""
    if [ -f "$CONFIG_FILE" ]; then
        backup="${CONFIG_FILE}.bak.$(date +%Y%m%d%H%M%S)"
        cp "$CONFIG_FILE" "$backup"
        log_info "已备份旧配置: ${backup}"
    fi

    log_info "写入配置文件: ${CONFIG_FILE}"
    cat > "$CONFIG_FILE" <<EOF
{
  "listen_ip": "::",
  "server_port": ${LISTEN_PORT},
  "flow": "xtls-rprx-vision",
  "log_level": "info",
  "clash_api_listen_addr": "",
  "status_api_listen_addr": "127.0.0.1:23333",
  "google_ipv6": ${GOOGLE_IPV6},
  "max_conn_per_ip": ${MAX_CONN},
  "max_new_conn_per_ip_per_min": ${MAX_CPS},
  "tls_settings": {
    "server_name": "${DEST_DOMAIN}",
    "server_port": "443",
    "private_key": "${PRIVATE_KEY}",
    "short_id": [
      "${SHORT_ID}"
    ]
  },
  "uuids": [
    "${USER_UUID}"
  ]
}
EOF
    chmod 600 "$CONFIG_FILE"

    if ! "${INSTALL_DIR}/${SERVICE_NAME}" -config "$CONFIG_FILE" -check-config >/dev/null; then
        log_error "生成的配置未通过校验"
        if [ -n "$backup" ]; then
            cp "$backup" "$CONFIG_FILE"
            log_warn "已恢复旧配置: ${backup}"
        fi
        exit 1
    fi
}

write_service() {
    log_info "写入 systemd 服务: ${SERVICE_FILE}"
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=VLESS Standalone Reality Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${SERVICE_NAME} -config=${CONFIG_FILE}
ExecReload=/bin/kill -HUP \$MAINPID
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
        log_error "服务启动失败，请运行 journalctl -u ${SERVICE_NAME} -n 80 --no-pager 查看日志"
        exit 1
    fi
    log_info "服务已安装并运行"
}

enable_bbr() {
    log_info "检查并尝试启用 BBR..."
    modprobe tcp_bbr 2>/dev/null || true

    local cc=""
    cc=$(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null || true)
    if [ "$cc" = "bbr" ]; then
        log_info "BBR 已启用"
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
        log_info "BBR 调优已写入系统配置"
    else
        log_warn "当前内核不支持 BBR，已跳过"
    fi
}

print_summary() {
    get_public_ip

    local vless_link=""
    if [ -n "${PUBLIC_KEY}" ]; then
        local remark="VLESS-Standalone-Reality"
        local fps=("chrome" "firefox" "safari" "edge")
        local rand_fp=${fps[$((RANDOM % 4))]}
        vless_link="vless://${USER_UUID}@${SERVER_IP}:${LISTEN_PORT}?security=reality&sni=${DEST_DOMAIN}&pbk=${PUBLIC_KEY}&fp=${rand_fp}&type=tcp&flow=xtls-rprx-vision&spx=/"
        if [ -n "${SHORT_ID}" ]; then
            vless_link="${vless_link}&sid=${SHORT_ID}"
        fi
        vless_link="${vless_link}#${remark}"
    fi

    echo -e "\n${GREEN}==================================================${NC}"
    echo -e "${GREEN} VLESS Standalone Reality 部署成功${NC}"
    echo -e "${GREEN}==================================================${NC}"
    echo -e " 监听端口: ${YELLOW}${LISTEN_PORT}${NC}"
    echo -e " 伪装域名: ${YELLOW}${DEST_DOMAIN}${NC}"
    echo -e " 用户 UUID: ${YELLOW}${USER_UUID}${NC}"
    echo -e " 客户端 flow: ${YELLOW}xtls-rprx-vision${NC}"
    echo -e " 单 IP 并发连接限制: ${YELLOW}${MAX_CONN}${NC}"
    echo -e " 单 IP 新建速率限制: ${YELLOW}${MAX_CPS} conn/min${NC}"
    echo -e " Google IPv6 分流: ${YELLOW}${GOOGLE_IPV6}${NC}"
    if [ "$SHOW_SECRETS" = true ]; then
        echo -e " Reality 私钥: ${YELLOW}${PRIVATE_KEY}${NC}"
    else
        echo -e " Reality 私钥: ${YELLOW}(已写入配置文件，默认隐藏；使用 --show-secrets 查看)${NC}"
    fi
    [ -n "${PUBLIC_KEY}" ] && echo -e " Reality 公钥: ${YELLOW}${PUBLIC_KEY}${NC}"
    echo -e " Reality short_id: ${YELLOW}${SHORT_ID}${NC}"
    echo -e " 启动命令: ${YELLOW}systemctl start ${SERVICE_NAME}${NC}"
    echo -e " 重载命令: ${YELLOW}systemctl reload ${SERVICE_NAME}${NC}"
    echo -e " 运行日志: ${YELLOW}journalctl -u ${SERVICE_NAME} -f${NC}"
    echo -e " 状态监控: ${YELLOW}curl http://127.0.0.1:23333/status${NC}"
    echo -e "${GREEN}==================================================${NC}"
    if [ -n "${vless_link}" ]; then
        echo -e " 客户端分享链接:"
        echo -e " ${CYAN}${vless_link}${NC}"
        echo -e "${GREEN}==================================================${NC}\n"
    else
        log_warn "未能得到 Reality 公钥，因此未生成分享链接"
        echo -e "${GREEN}==================================================${NC}\n"
    fi
}

main() {
    require_root

    LISTEN_PORT=443
    DEST_DOMAIN="www.amd.com"
    USER_UUID=""
    PRIVATE_KEY=""
    PUBLIC_KEY=""
    SHORT_ID=""
    SHOW_SECRETS=false
    USE_LOCAL=false
    LOCAL_BIN_PATH="./vless-standalone"
    MAX_CONN=100
    MAX_CPS=60
    GOOGLE_IPV6="false"
    SKIP_CHECKSUM=false
    PORT_SET=false
    DOMAIN_SET=false
    MAX_CONN_SET=false
    MAX_CPS_SET=false
    GOOGLE_IPV6_SET=false

    for arg in "$@"; do
        case "$arg" in
            --port=*) LISTEN_PORT="${arg#*=}"; PORT_SET=true ;;
            --domain=*) DEST_DOMAIN="${arg#*=}"; DOMAIN_SET=true ;;
            --uuid=*) USER_UUID="${arg#*=}" ;;
            --private-key=*) PRIVATE_KEY="${arg#*=}" ;;
            --public-key=*) PUBLIC_KEY="${arg#*=}" ;;
            --version=*) RELEASE_VERSION="${arg#*=}" ;;
            --base-url=*) BASE_URL="${arg#*=}" ;;
            --skip-checksum) SKIP_CHECKSUM=true ;;
            --show-secrets) SHOW_SECRETS=true ;;
            --local) USE_LOCAL=true ;;
            --local-bin=*) USE_LOCAL=true; LOCAL_BIN_PATH="${arg#*=}" ;;
            --max-conn=*) MAX_CONN="${arg#*=}"; MAX_CONN_SET=true ;;
            --max-cps=*) MAX_CPS="${arg#*=}"; MAX_CPS_SET=true ;;
            --google-ipv6=*)
                case "${arg#*=}" in
                    true|false) GOOGLE_IPV6="${arg#*=}"; GOOGLE_IPV6_SET=true ;;
                    *) log_error "--google-ipv6 只能是 true 或 false"; exit 1 ;;
                esac
                ;;
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
    load_existing_config
    validate_port
    validate_number "max-conn" "$MAX_CONN"
    validate_number "max-cps" "$MAX_CPS"

    detect_arch
    install_binary
    generate_secrets
    write_config
    write_service
    enable_bbr
    print_summary
}

main "$@"
