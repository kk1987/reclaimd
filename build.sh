#!/bin/sh
# Cross-compile reclaimd for the router (aarch64, OpenWrt/musl), the laptop
# (x86_64, Arch/glibc), and FreeBSD on both architectures.
#
# CGO_ENABLED=0 is what lets one source tree serve all of them: with no libc
# linkage at all, the musl/glibc split simply does not exist, and the binary
# needs nothing on the target but a kernel.
set -eu

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${DATE}"

mkdir -p out
for target in linux/amd64 linux/arm64 freebsd/amd64 freebsd/arm64; do
    goos=${target%/*}
    goarch=${target#*/}
    out="out/reclaimd-${goos}-${goarch}"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags="$LDFLAGS" -o "$out" ./cmd/reclaimd
    printf '%-32s %s\n' "$out" "$(du -h "$out" | cut -f1)"
done
