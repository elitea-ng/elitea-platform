#!/usr/bin/env bash
# build-native-bin.sh <out-dir> — the build directory a native E2E stack runs
# from (E2E_NATIVE_BIN; layout in deploy/native/e2e-standalone.mjs): the Go
# binaries and the web bundles the e2e-standalone images carry, built the way
# their Containerfiles build them, without an image.
#
#   elitea-main, elitea-migrate      services/elitea-main/Containerfile `builder`
#   elitea-subapp-host               services/elitea-subapp-host/Containerfile
#   web/app/                         apps/elitea-web/Containerfile `builder` (build:app)
#   admin_ui/static/dist/            services/elitea-main/Containerfile `admin-builder` (build:admin)
#   admin_ui/static/brand-preview/   the same stage (build:brand-preview, empty when absent)
#
# Needs go (go.work's version) and node with apps/elitea-web's dependencies
# installed (`npm ci` there). GOWORK=off inside each module, as the images
# build them: a module's replace directives, not go.work, decide its graph.
# VERSION (default dev) is baked in as elitea-main's buildinfo.Version and
# the SPA's VITE_APP_VERSION, as publish.yml passes it to the images.
#
# Used by ci-web-e2e-native.yml's `native-build` job; runnable locally.
set -euo pipefail

out="${1:?usage: build-native-bin.sh <out-dir>}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VERSION="${VERSION:-dev}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"

export CGO_ENABLED=0 GOWORK=off
# The Containerfiles retry `go mod download` because a single fetch of the
# whole graph resets now and then; the same bounded retry here.
mod_download() {
  for attempt in 1 2 3 4 5; do
    go mod download && return 0
    [ "$attempt" = 5 ] && { echo "go mod download failed after 5 attempts" >&2; return 1; }
    echo "go mod download failed (attempt ${attempt}/5)" >&2
    sleep $((attempt * 5))
  done
}

echo "→ elitea-main, elitea-migrate"
(
  cd "$REPO_ROOT/services/elitea-main"
  mod_download
  go build -trimpath -ldflags="-s -w -X github.com/elitea-ng/elitea-platform/services/elitea-main/internal/buildinfo.Version=${VERSION}" \
    -o "$out/elitea-main" ./cmd/elitea-main
  go build -trimpath -ldflags="-s -w" -o "$out/elitea-migrate" ./cmd/elitea-migrate
)
echo "→ elitea-subapp-host"
(
  cd "$REPO_ROOT/services/elitea-subapp-host"
  mod_download
  go build -trimpath -ldflags="-s -w" -o "$out/elitea-subapp-host" ./cmd/elitea-subapp-host
)

echo "→ web bundles (app, admin, brand-preview)"
(
  cd "$REPO_ROOT/apps/elitea-web"
  export NODE_OPTIONS="--max-old-space-size=4096"
  VITE_APP_VERSION="$VERSION" npm run build:app
  npm run build:admin
  npm run build:brand-preview --if-present
  [ -f dist/brand-preview/index.html ] || mkdir -p dist/brand-preview
  rm -rf "$out/web/app" "$out/admin_ui/static"
  mkdir -p "$out/web" "$out/admin_ui/static"
  cp -R dist/app "$out/web/app"
  cp -R dist/admin "$out/admin_ui/static/dist"
  cp -R dist/brand-preview "$out/admin_ui/static/brand-preview"
)

# What the native table starts must be here, and executable: a build that
# silently produced less fails here, not as a service that never comes up.
for f in elitea-main elitea-migrate elitea-subapp-host; do
  [ -x "$out/$f" ] || { echo "ERROR: $out/$f was not built" >&2; exit 1; }
done
for f in web/app/index.html admin_ui/static/dist/index.html; do
  [ -f "$out/$f" ] || { echo "ERROR: $out/$f was not built" >&2; exit 1; }
done
echo "✓ native build directory: $out"
