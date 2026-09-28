# Pre-deploy gate: vet, test, and build for Windows and Linux.
# Usage: powershell -ExecutionPolicy Bypass -File scripts\check.ps1
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)

function Step($name, [scriptblock]$cmd) {
    Write-Host "==> $name"
    & $cmd
    if ($LASTEXITCODE -ne 0) { throw "$name failed (exit $LASTEXITCODE)" }
}

# Resolve go once, before PATH is modified below (MSYS2's ucrt64\bin also has a trimmed go.exe).
$go = (Get-Command go -ErrorAction Stop).Source

$env:CGO_ENABLED = '0'
Step 'go vet' { & $go vet ./... }
# Integration tests need the network, so they only run on demand, but they must keep compiling:
#   go test -tags integration ./internal/cache/
Step 'go vet (integration)' { & $go vet -tags integration ./... }

# The race detector needs cgo and a C compiler. It only affects tests, never the shipped binary.
$gcc = Get-Command gcc -ErrorAction SilentlyContinue
if ($gcc) {
    # Put gcc's own directory first so its DLLs win over same-named ones earlier on PATH
    # (e.g. PostgreSQL's bin ships older libwinpthread/zlib/zstd, which breaks cc1).
    $savedPath = $env:Path
    $env:Path = (Split-Path -Parent $gcc.Source) + ';' + $env:Path
    $env:CGO_ENABLED = '1'
    Step 'go test -race' { & $go test -race ./... }
    $env:CGO_ENABLED = '0'
    $env:Path = $savedPath
} else {
    Write-Warning 'gcc not found: running tests without -race'
    Step 'go test' { & $go test ./... }
}

New-Item -ItemType Directory -Force dist | Out-Null
Step 'build windows/amd64' { $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; & $go build -ldflags '-s -w' -o dist/bot.exe ./cmd/bot }
Step 'build linux/amd64'   { $env:GOOS = 'linux';   $env:GOARCH = 'amd64'; & $go build -ldflags '-s -w' -o dist/bot ./cmd/bot }
Remove-Item Env:GOOS, Env:GOARCH

Write-Host 'All checks passed.'
