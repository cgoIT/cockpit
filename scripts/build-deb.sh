#!/usr/bin/env bash
# Baut sitewatch_<version>_<arch>.deb und sitewatch-agent_<version>_<arch>.deb nach dist/ (nfpm, siehe packaging/).
# Voraussetzung: bin/ von scripts/build.sh (mit linux/amd64 und linux/arm64).
#   VERSION=1.2.3 scripts/build-deb.sh
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=${VERSION:?VERSION fehlt, z. B. VERSION=1.2.3}
ARCHS=${ARCHS:-"amd64 arm64"}
NFPM=${NFPM:-"go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0"}

rm -rf dist build && mkdir -p dist build
for arch in $ARCHS; do
  # nfpm löst ${ARCH} in den Quellpfaden nicht auf: die Binaries liegen je Architektur unter festen Namen
  cp "bin/sitewatch-linux-$arch" build/sitewatch
  cp "bin/agent-linux-$arch" build/sitewatch-agent
  for pkg in sitewatch sitewatch-agent; do
    echo "→ $pkg $VERSION ($arch)"
    ARCH=$arch VERSION=$VERSION $NFPM package -f "packaging/$pkg.yaml" -p deb -t dist/
  done
done
rm -rf build
