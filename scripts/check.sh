#!/usr/bin/env bash
# Pre-deploy gate: vet, test (with -race when gcc is available), and build for
# Windows and Linux into dist/. Runs in MSYS2 (UCRT64) on Windows or on Linux.
# Usage: scripts/check.sh
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '==> %s\n' "$*"; }

# Find a working go. MSYS2's /ucrt64/bin/go is a trimmed launcher that needs
# GOROOT; the real toolchain binary works anywhere.
GO=""
for cand in "$(command -v go 2>/dev/null || true)" /ucrt64/lib/go/bin/go /c/msys2/ucrt64/lib/go/bin/go; do
    if [ -n "$cand" ] && "$cand" env GOROOT >/dev/null 2>&1; then GO="$cand"; break; fi
done
[ -n "$GO" ] || { echo "go not found (MSYS2: pacman -S mingw-w64-ucrt-x86_64-go)" >&2; exit 1; }

# MSYS2 login shells don't pass Windows' USERPROFILE/LOCALAPPDATA, which Go
# uses to find its module and build caches. Point it at the usual Windows
# locations (Go wants Windows-style paths there).
if command -v cygpath >/dev/null 2>&1; then
    if [ -z "$("$GO" env GOMODCACHE 2>/dev/null)" ]; then
        export GOPATH="$(cygpath -w "$HOME/go")"
    fi
    case "$("$GO" env GOCACHE 2>/dev/null)" in
        "" | off) export GOCACHE="$(cygpath -w "$HOME/AppData/Local/go-build")" ;;
    esac
fi

export CGO_ENABLED=0
step "go vet"
"$GO" vet ./...
# Integration tests need the network, so they only run on demand
# (go test -tags integration ./internal/cache/), but they must keep compiling.
step "go vet (integration)"
"$GO" vet -tags integration ./...

# The race detector needs cgo and a C compiler; it only affects tests, never
# the shipped binaries. gcc's own directory goes first on PATH so its DLLs win
# over same-named ones elsewhere (e.g. PostgreSQL's bin on Windows).
if gcc_path="$(command -v gcc 2>/dev/null)"; then
    step "go test -race"
    PATH="$(dirname "$gcc_path"):$PATH" CGO_ENABLED=1 "$GO" test -race ./...
else
    echo "WARNING: gcc not found: running tests without -race" >&2
    step "go test"
    "$GO" test ./...
fi

mkdir -p dist
step "build windows/amd64"
GOOS=windows GOARCH=amd64 "$GO" build -ldflags '-s -w' -o dist/vox-engine.exe ./cmd/vox-engine
step "build linux/amd64"
GOOS=linux GOARCH=amd64 "$GO" build -ldflags '-s -w' -o dist/vox-engine ./cmd/vox-engine

echo "All checks passed."
