param(
    [string]$Version = $(if ($env:VERSION) { $env:VERSION } else { "dev" }),
    [string]$Commit = $(if ($env:COMMIT) { $env:COMMIT } else { "" }),
    [string]$BuildTime = $(if ($env:BUILD_TIME) { $env:BUILD_TIME } else { "" }),
    [string]$DistDir = $(if ($env:DIST_DIR) { $env:DIST_DIR } else { "dist" })
)

$ErrorActionPreference = "Stop"
$AppName = "vless-standalone"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "go is required"
}

if (-not $Commit) {
    try {
        $Commit = (git rev-parse --short HEAD 2>$null).Trim()
    } catch {
        $Commit = "unknown"
    }
    if (-not $Commit) {
        $Commit = "unknown"
    }
}

if (-not $BuildTime) {
    $BuildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
}

New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

if (-not $env:GOCACHE) {
    $env:GOCACHE = Join-Path (Get-Location) ".gocache"
    New-Item -ItemType Directory -Force -Path $env:GOCACHE | Out-Null
}
if (-not $env:GOMODCACHE) {
    $env:GOMODCACHE = Join-Path (Get-Location) ".gomodcache"
    New-Item -ItemType Directory -Force -Path $env:GOMODCACHE | Out-Null
}

$ldflags = "-s -w -X main.version=$Version -X main.commit=$Commit -X main.buildTime=$BuildTime"

function Build-One {
    param([string]$Arch)

    $output = Join-Path $DistDir "$AppName-linux-$Arch"
    Write-Host "Building $output"

    $env:GOOS = "linux"
    $env:GOARCH = $Arch
    go build -tags with_utls -ldflags $ldflags -o $output .
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed for linux/$Arch"
    }

    $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $output).Hash.ToLowerInvariant()
    $fileName = Split-Path -Leaf $output
    "$hash  $fileName" | Set-Content -NoNewline -Encoding ascii -LiteralPath "$output.sha256"
}

try {
    Build-One "amd64"
    Build-One "arm64"
} finally {
    Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "Artifacts written to ${DistDir}:"
Write-Host "  $AppName-linux-amd64"
Write-Host "  $AppName-linux-amd64.sha256"
Write-Host "  $AppName-linux-arm64"
Write-Host "  $AppName-linux-arm64.sha256"
