# Builds the Leonardo image-generation REST API binary.
#
# Usage:  ./build.ps1            # native build -> puppet-leonardo.exe
#         ./build.ps1 -Cross     # also cross-compile Linux release binaries into dist\

$ErrorActionPreference = "Stop"

Write-Host "==> Building Go binary (puppet-leonardo.exe)..." -ForegroundColor Cyan
go build -o puppet-leonardo.exe .

# Cross-compile Linux release binaries into dist\<os>-<arch>\ (opt in with -Cross).
if ($args -contains '-Cross') {
    Write-Host "==> Cross-compiling release binaries into dist\ ..." -ForegroundColor Cyan
    $platforms = @(
        @{ Dir = 'linux-amd64'; GOOS = 'linux'; GOARCH = 'amd64'; GOARM = '' },
        @{ Dir = 'linux-arm64'; GOOS = 'linux'; GOARCH = 'arm64'; GOARM = '' }
    )
    foreach ($p in $platforms) {
        $outDir = Join-Path 'dist' $p.Dir
        New-Item -ItemType Directory -Force -Path $outDir | Out-Null
        Write-Host "    $($p.GOOS)/$($p.GOARCH): puppet-leonardo" -ForegroundColor DarkGray
        $env:CGO_ENABLED = '0'; $env:GOOS = $p.GOOS; $env:GOARCH = $p.GOARCH; $env:GOARM = $p.GOARM
        go build -trimpath -ldflags '-s -w' -o (Join-Path $outDir 'puppet-leonardo') .
    }
    # Restore host build environment so later commands build natively.
    Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH, Env:GOARM -ErrorAction SilentlyContinue
}

Write-Host "==> Done. Run with: ./puppet-leonardo.exe" -ForegroundColor Green
Write-Host "    Web UI:  http://localhost:8080/" -ForegroundColor Green
Write-Host "    API:     POST http://localhost:8080/api/generate  {`"prompt`":`"...`",`"size`":`"16:9`"}" -ForegroundColor Green
