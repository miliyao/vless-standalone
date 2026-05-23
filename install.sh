#!/usr/bin/env bash
set -eu

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

usage() {
    cat <<EOF
一键部署独立版 VLESS 节点 (VLESS + XTLS-Vision + Reality)

使用方法:
  bash install.sh [参数]

可选参数:
  --port=443                监听端口 (默认: 443)
  --domain=www.amd.com      Reality 伪装目标域名 (默认: www.amd.com)
  --uuid=xxxxxx             指定 VLESS UUID (默认: 自动生成)
  --private-key=xxxxxx      指定 Reality 私钥 (默认: 自动生成)
  --public-key=xxxxxx       指定 Reality 公钥 (默认: 自动生成或推导)
  --version=latest          发布包版本 (默认: latest)
  --show-secrets            控制台输出中显示 Reality 私钥等敏感信息 (默认: 隐藏)
  --local                   使用本地编译的二进制文件，跳过从 GitHub 下载 (默认: 停用)
  --local-bin=./file        指定本地二进制文件的路径 (默认: ./vless-standalone)
  --max-conn=100            单源 IP 最大并发连接数限制 (默认: 100, 0 表示无限制)
  --max-cps=60              单源 IP 每分钟新建连接速率限制 (默认: 60, 0 表示无限制)
  --google-ipv6=true|false  是否将 Google 流量强制通过本地 IPv6 路由直连 (默认: false)
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

get_public_ip() {
    log_info "正在检测服务器公网 IP 地址..."
    SERVER_IP=$(curl -sS --max-time 5 https://api.ipify.org 2>/dev/null || \
                curl -sS --max-time 5 https://ifconfig.me 2>/dev/null || \
                curl -sS --max-time 5 https://ipinfo.io/ip 2>/dev/null || \
                curl -sS --max-time 5 https://icanhazip.com 2>/dev/null || \
                echo "YOUR_VPS_IP")
}

generate_secrets() {
    if [ -z "${USER_UUID:-}" ]; then
        if command -v uuidgen >/dev/null 2>&1; then
            USER_UUID=$(uuidgen)
        else
            USER_UUID=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || od -x -N 16 /dev/urandom | head -n 1 | awk '{print $2$3"-"$4"-"$5"-"$6"-"$7$8$9}')
        fi
    fi

    local binary_path="${INSTALL_DIR}/${SERVICE_NAME}"
    if [ -z "${PRIVATE_KEY:-}" ]; then
        log_info "正在通过二进制程序生成 Reality 随机私钥和公钥..."
        local keys_json=""
        if keys_json=$("$binary_path" -gen-key 2>/dev/null); then
            PRIVATE_KEY=$(echo "$keys_json" | jq -r '.private_key')
            PUBLIC_KEY=$(echo "$keys_json" | jq -r '.public_key')
        else
            log_error "通过二进制程序生成密钥对失败，使用系统备份逻辑仅生成私钥 (无法自动生成分享链接！)"
            PRIVATE_KEY=$(openssl genpkey -algorithm x25519 -outform DER 2>/dev/null | tail -c 32 | base64 | tr -d '\n\r=')
        fi
    fi

    if [ -n "${PRIVATE_KEY:-}" ] && [ -z "${PUBLIC_KEY:-}" ]; then
        log_info "已提供或生成私钥，正在推导对应的 Reality 公钥..."
        if ! PUBLIC_KEY=$("$binary_path" -derive-pub "$PRIVATE_KEY" 2>/dev/null); then
            log_warn "推导公钥失败，客户端链接可能需要手动指定公钥。"
            PUBLIC_KEY=""
        fi
    fi

    if [ -z "${SHORT_ID:-}" ]; then
        log_info "正在生成 Reality 随机 short_id..."
        if command -v openssl >/dev/null 2>&1; then
            SHORT_ID=$(openssl rand -hex 8)
        else
            SHORT_ID=$(head -n 2 /dev/urandom | tr -dc 'a-f0-9' | head -c 16)
        fi
    fi
}

install_binary() {
    if [ "$USE_LOCAL" = true ]; then
        log_info "检测到开启本地安装，正在使用本地二进制文件..."
        if [ ! -f "$LOCAL_BIN_PATH" ]; then
            log_error "未找到本地二进制文件: ${LOCAL_BIN_PATH}。请先编译或指定正确的文件路径。"
            exit 1
        fi
        chmod +x "$LOCAL_BIN_PATH"
        cp "$LOCAL_BIN_PATH" "${INSTALL_DIR}/${SERVICE_NAME}"
        log_info "本地二进制程序复制安装成功: ${INSTALL_DIR}/${SERVICE_NAME}"
        return
    fi

    local asset_name="vless-standalone-linux-${ARCH}"
    local download_url=""
    local sha256_url=""

    if [ "$RELEASE_VERSION" = "latest" ]; then
        download_url="https://raw.githubusercontent.com/${RELEASE_REPO}/main/${asset_name}?t=$(date +%s)"
        sha256_url="https://raw.githubusercontent.com/${RELEASE_REPO}/main/${asset_name}.sha256?t=$(date +%s)"
    else
        download_url="https://github.com/${RELEASE_REPO}/releases/download/${RELEASE_VERSION}/${asset_name}"
        sha256_url="https://github.com/${RELEASE_REPO}/releases/download/${RELEASE_VERSION}/${asset_name}.sha256"
    fi

    log_info "正在从 GitHub 下载二进制程序..."
    log_info "地址: ${download_url}"

    local tmp_bin="${INSTALL_DIR}/${SERVICE_NAME}.tmp"
    if ! curl -H "Cache-Control: no-cache" -fL --retry 3 --connect-timeout 15 -o "$tmp_bin" "$download_url"; then
        log_error "从 GitHub 下载预编译包失败，请检查网络或版本号。"
        exit 1
    fi

    log_info "正在下载 SHA256 校验和文件..."
    local tmp_sha="${tmp_bin}.sha256"
    if curl -H "Cache-Control: no-cache" -fL --retry 2 --connect-timeout 10 -o "$tmp_sha" "$sha256_url" 2>/dev/null; then
        log_info "开始进行二进制程序哈希校验..."
        local expected_hash
        expected_hash=$(awk '{print $1}' "$tmp_sha" | tr -d '\r\n')
        local actual_hash
        actual_hash=$(sha256sum "$tmp_bin" | awk '{print $1}' | tr -d '\r\n')

        if [ "$expected_hash" != "$actual_hash" ]; then
            log_error "哈希值校验失败！"
            log_error "预期: ${expected_hash}"
            log_error "实际: ${actual_hash}"
            rm -f "$tmp_bin" "$tmp_sha"
            exit 1
        fi
        log_info "哈希值校验成功！"
        rm -f "$tmp_sha"
    else
        log_warn "未发现 .sha256 校验文件，跳过完整性校验。"
        rm -f "$tmp_sha"
    fi

    chmod +x "$tmp_bin"
    mv "$tmp_bin" "${INSTALL_DIR}/${SERVICE_NAME}"
    log_info "二进制程序安装成功: ${INSTALL_DIR}/${SERVICE_NAME}"
}

write_config() {
    mkdir -p "$CONFIG_DIR"
    
    if [ -f "$CONFIG_FILE" ]; then
        log_warn "检测到已存在的配置文件: ${CONFIG_FILE}"
        local confirm="n"
        read -p "是否覆盖已有的配置文件? [y/N]: " confirm || true
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

    for arg in "$@"; do
        case "$arg" in
            --port=*) LISTEN_PORT="${arg#*=}" ;;
            --domain=*) DEST_DOMAIN="${arg#*=}" ;;
            --uuid=*) USER_UUID="${arg#*=}" ;;
            --private-key=*) PRIVATE_KEY="${arg#*=}" ;;
            --public-key=*) PUBLIC_KEY="${arg#*=}" ;;
            --version=*) RELEASE_VERSION="${arg#*=}" ;;
            --show-secrets) SHOW_SECRETS=true ;;
            --local) USE_LOCAL=true ;;
            --local-bin=*) USE_LOCAL=true; LOCAL_BIN_PATH="${arg#*=}" ;;
            --max-conn=*) MAX_CONN="${arg#*=}" ;;
            --max-cps=*) MAX_CPS="${arg#*=}" ;;
            --google-ipv6=*)
                if [ "${arg#*=}" = "true" ]; then
                    GOOGLE_IPV6="true"
                else
                    GOOGLE_IPV6="false"
                fi
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
    detect_arch
    install_binary
    generate_secrets
    write_config
    write_service
    enable_bbr

    get_public_ip

    # 构造标准 vless 分享链接
    local vless_link=""
    if [ -n "${PUBLIC_KEY}" ]; then
        local remark="VLESS-Standalone-Reality"
        local fps=("chrome" "firefox" "safari" "edge")
        local rand_fp=${fps[$((RANDOM % 4))]}
        vless_link="vless://${USER_UUID}@${SERVER_IP}:${LISTEN_PORT}?security=reality&sni=${DEST_DOMAIN}&pbk=${PUBLIC_KEY}&fp=${rand_fp}&type=tcp&flow=xtls-rprx-vision"
        if [ -n "${SHORT_ID}" ]; then
            vless_link="${vless_link}&sid=${SHORT_ID}"
        fi
        vless_link="${vless_link}#${remark}"
    fi

    echo -e "\n${GREEN}==================================================${NC}"
    echo -e "${GREEN} VLESS Standalone Reality 部署成功！${NC}"
    echo -e "${GREEN}==================================================${NC}"
    echo -e " 监听端口: ${YELLOW}${LISTEN_PORT}${NC}"
    echo -e " 伪装域名: ${YELLOW}${DEST_DOMAIN}${NC}"
    echo -e " 用户UUID: ${YELLOW}${USER_UUID}${NC}"
    echo -e " 客户端流控: ${YELLOW}xtls-rprx-vision${NC}"
    echo -e " 单IP并发连接限制: ${YELLOW}${MAX_CONN}${NC}"
    echo -e " 单IP新建速率限制: ${YELLOW}${MAX_CPS} conn/min${NC}"
    echo -e " 谷歌 IPv6 优先分流: ${YELLOW}${GOOGLE_IPV6}${NC}"
    if [ "$SHOW_SECRETS" = true ]; then
        echo -e " Reality私钥: ${YELLOW}${PRIVATE_KEY}${NC}"
    else
        echo -e " Reality私钥: ${YELLOW}(已写入配置文件，默认隐藏，使用 --show-secrets 选项查看)${NC}"
    fi
    if [ -n "${PUBLIC_KEY}" ]; then
        echo -e " Reality公钥: ${YELLOW}${PUBLIC_KEY}${NC}"
    fi
    echo -e " Reality短ID: ${YELLOW}${SHORT_ID}${NC}"
    echo -e " 启动命令: ${YELLOW}systemctl start ${SERVICE_NAME}${NC}"
    echo -e " 重启命令: ${YELLOW}systemctl restart ${SERVICE_NAME}${NC}"
    echo -e " 运行日志: ${YELLOW}journalctl -u ${SERVICE_NAME} -f${NC}"
    echo -e " 负载监控: ${YELLOW}curl http://127.0.0.1:23333/status${NC}"
    echo -e "${GREEN}==================================================${NC}"
    if [ -n "${vless_link}" ]; then
        echo -e " 客户端分享链接 (一键导入):"
        echo -e " ${CYAN}${vless_link}${NC}"
        echo -e "${GREEN}==================================================${NC}\n"
    else
        log_warn "由于未能推导或指定 Reality 公钥，未生成分享链接。"
        echo -e "${GREEN}==================================================${NC}\n"
    fi
}

main "$@"
