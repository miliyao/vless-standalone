#!/usr/bin/env bash
set -euo pipefail

APP_NAME="vless-standalone"
DIST_DIR="${DIST_DIR:-dist}"
VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

usage() {
    cat <<EOF
Build local release artifacts.

Usage:
  VERSION=v1.0.0 ./build.sh

Environment:
  VERSION      Version string injected into the binary, default: dev
  COMMIT       Commit string, default: current git short SHA
  BUILD_TIME   UTC build time, default: now
  DIST_DIR     Output directory, default: dist
EOF
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    usage
    exit 0
fi

if ! command -v go >/dev/null 2>&1; then
    echo "go is required" >&2
    exit 1
fi

mkdir -p "$DIST_DIR"

if [ -z "${GOCACHE:-}" ]; then
    export GOCACHE="$(pwd)/.gocache"
    mkdir -p "$GOCACHE"
fi
if [ -z "${GOMODCACHE:-}" ]; then
    export GOMODCACHE="$(pwd)/.gomodcache"
    mkdir -p "$GOMODCACHE"
fi

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"

build_one() {
    local goarch="$1"
    local output="${DIST_DIR}/${APP_NAME}-linux-${goarch}"

    echo "Building ${output}"
    GOOS=linux GOARCH="$goarch" go build -tags with_utls -ldflags "$LDFLAGS" -o "$output" .

    if command -v sha256sum >/dev/null 2>&1; then
        (cd "$DIST_DIR" && sha256sum "${APP_NAME}-linux-${goarch}" > "${APP_NAME}-linux-${goarch}.sha256")
    elif command -v shasum >/dev/null 2>&1; then
        (cd "$DIST_DIR" && shasum -a 256 "${APP_NAME}-linux-${goarch}" > "${APP_NAME}-linux-${goarch}.sha256")
    else
        echo "sha256sum or shasum is required" >&2
        exit 1
    fi
}

build_one amd64
build_one arm64

cat <<EOF

Artifacts written to ${DIST_DIR}:
  ${APP_NAME}-linux-amd64
  ${APP_NAME}-linux-amd64.sha256
  ${APP_NAME}-linux-arm64
  ${APP_NAME}-linux-arm64.sha256
EOF
