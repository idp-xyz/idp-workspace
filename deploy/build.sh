#!/usr/bin/env bash
# Build the idp-workspace image offline: a static binary and the built web shell on the host, then
# docker build --network none. The shell needs Node (npm ci + vite build); SKIP_WEB=1 builds an
# API-only image.
# Usage: bash deploy/build.sh [TAG]     (default idp-workspace:<commit or dev>)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
[[ -z "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]] || VERSION="$VERSION-dirty"
TAG="${1:-idp-workspace:$VERSION}"
OUT=build/image
umask 022   # the context's modes become the image's: world-readable files, traversable directories
rm -rf "$OUT" && mkdir -p "$OUT/web"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/idp-workspace" ./cmd/idp-workspace
if [[ "${SKIP_WEB:-0}" != 1 ]]; then
  command -v npm >/dev/null || { echo "BLOCKED: npm is needed to build the web shell (or SKIP_WEB=1)" >&2; exit 2; }
  [[ -d node_modules ]] || npm ci --ignore-scripts --no-audit --no-fund >/dev/null
  npm run build -w apps/web >/dev/null
  cp -r apps/web/dist/. "$OUT/web/"
else
  echo '<!doctype html><title>IDP Workspace</title><p>API only (built with SKIP_WEB=1).' > "$OUT/web/index.html"
fi
chmod -R a+rX "$OUT/web"
cp /etc/ssl/certs/ca-certificates.crt "$OUT/" && chmod 644 "$OUT/ca-certificates.crt" && chmod 755 "$OUT"
cp deploy/Dockerfile "$OUT/"
docker build --network none -q -t "$TAG" "$OUT" > /dev/null
echo "OK: $TAG $(docker image inspect --format '{{.Id}}' "$TAG" | cut -c1-19)… idp-workspace sha256 $(sha256sum "$OUT/idp-workspace" | cut -c1-12)… web $(find "$OUT/web" -type f | wc -l) files"
