#!/usr/bin/env bash
# Baut Agent und Cockpit als statische Binaries nach bin/ und schreibt bin/SHA256SUMS.
#   scripts/build.sh                          Standardziele
#   TARGETS="linux/amd64" scripts/build.sh    nur bestimmte Ziele (os/arch, durch Leerzeichen getrennt)
#   VERSION=1.2.3 scripts/build.sh            Version erzwingen (sonst aus git describe)
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)}
TARGETS=${TARGETS:-"linux/amd64 linux/arm64 darwin/arm64"}

rm -rf bin && mkdir -p bin
for t in $TARGETS; do
  os=${t%/*} arch=${t#*/}
  for cmd in agent cockpit; do
    out="bin/$cmd-$os-$arch"
    echo "→ $out ($VERSION)"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" "./cmd/$cmd"
  done
done
(cd bin && { sha256sum agent-* cockpit-* 2>/dev/null || shasum -a 256 agent-* cockpit-*; } > SHA256SUMS)
