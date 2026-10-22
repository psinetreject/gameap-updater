#!/usr/bin/env bash
# Build the GameAP Updater plugin (.wasm) in containers only.
# Nothing needs to be installed on the host except git and Docker or podman.
#
# Usage:
#   ./build.sh              build the plugin (default)
#   ./build.sh test         run the Go unit tests (needs the SDK checkout from a build)
#   ./build.sh clean        remove build artifacts, keep the SDK checkout
#   ./build.sh distclean    also drop the SDK checkout and caches
set -euo pipefail
cd "$(dirname "$0")"

SDK_REF="${SDK_REF:-v4.5.8}"     # tag or branch of github.com/gameap/gameap to build against
SDK_URL="${SDK_URL:-https://github.com/gameap/gameap.git}"
# Fully qualified so podman resolves them without short-name aliases.
NODE_IMAGE="${NODE_IMAGE:-docker.io/library/node:24-bookworm}"
TINYGO_IMAGE="${TINYGO_IMAGE:-docker.io/tinygo/tinygo:0.42.0}"  # must support Go 1.27 (the v4.5.8 SDK requires it)
OUT="${OUT:-GameAP-Updater.wasm}"

# Docker if present, else podman. Either can be forced with CONTAINER=...
CONTAINER="${CONTAINER:-$(command -v docker >/dev/null && echo docker || echo podman)}"
command -v "$CONTAINER" >/dev/null || { echo "!! neither docker nor podman found" >&2; exit 1; }

# Files written into the bind mounts must end up owned by the invoking user.
# Docker does that with -u; rootless podman maps the container user through a
# subuid range, so the equivalent there is keep-id.
if [ "$CONTAINER" = podman ]; then
  RUN_AS=(--userns=keep-id)
else
  RUN_AS=(-u "$(id -u):$(id -g)")
fi

# Go build/module caches, outside the source tree so a fresh checkout or CI
# workspace does not start cold.
CACHE_DIR="${CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/gameap-updater-build}"

# Go marks its module cache read-only, so it has to be made writable before rm.
rm_cache() {
  [ -e "$1" ] || return 0
  chmod -R u+w "$1" 2>/dev/null || true
  rm -rf "$1"
}

case "${1:-build}" in
  clean|distclean)
    # dist/.gitkeep is tracked (see .gitignore) so the directory survives a clean.
    rm -rf frontend/dist/* frontend/node_modules "${OUT}"
    touch frontend/dist/.gitkeep
    if [ "$1" = distclean ]; then
      rm -rf .sdk
      rm_cache "$CACHE_DIR"
    fi
    echo ">> cleaned (${1})"
    exit 0
    ;;
  test)
    [ -d .sdk/gameap/.git ] || { echo "!! no SDK checkout - run ./build.sh once first" >&2; exit 1; }
    mkdir -p "$CACHE_DIR"
    # Only internal/ is tested: it has no SDK imports, so plain Go runs it.
    # The TinyGo image is used because it already ships the matching Go.
    "$CONTAINER" run --rm "${RUN_AS[@]}" \
      -e HOME=/tmp \
      -e GOCACHE=/cache/go-build \
      -e GOMODCACHE=/cache/gomod \
      -e GOPATH=/cache/gopath \
      -v "$CACHE_DIR:/cache" \
      -v "$PWD:/src" -w /src "${TINYGO_IMAGE}" \
      sh -c 'cp go.mod .build.mod; trap "rm -f .build.mod .build.sum" EXIT; GOFLAGS=-modfile=.build.mod go mod tidy && GOFLAGS=-modfile=.build.mod go vet ./internal/... && GOFLAGS=-modfile=.build.mod go test ./internal/...'
    exit 0
    ;;
  build) ;;
  *)
    echo "!! unknown command '$1' - expected build, test, clean or distclean" >&2
    exit 1
    ;;
esac

# VERSION is the single source of truth: main.go embeds it, and vite.config.ts
# injects it as __PLUGIN_VERSION__.
GO_VER=$(tr -d '[:space:]' < VERSION)
[ -n "$GO_VER" ] || { echo "!! VERSION is empty" >&2; exit 1; }
echo ">> building version ${GO_VER} with ${CONTAINER}"

echo ">> [1/3] Ensure GameAP SDK checkout (./.sdk/gameap @ ${SDK_REF})"
if [ ! -d .sdk/gameap/.git ]; then
  mkdir -p .sdk
  git clone --depth 1 --branch "${SDK_REF}" "${SDK_URL}" .sdk/gameap
else
  # Reset cache-only SDK edits, then fetch the requested ref by name so tags and
  # moving branches use the same path.
  git -C .sdk/gameap reset --hard
  if [ "$(git -C .sdk/gameap remote get-url origin)" != "$SDK_URL" ]; then
    echo ">> SDK origin changed; updating to ${SDK_URL}"
    git -C .sdk/gameap remote set-url origin "$SDK_URL"
  fi
  git -C .sdk/gameap fetch --force --tags origin "$SDK_REF"
  if [ "$(git -C .sdk/gameap rev-parse HEAD)" != "$(git -C .sdk/gameap rev-parse FETCH_HEAD)" ]; then
    echo ">> SDK checkout does not match ${SDK_REF}; switching"
    git -C .sdk/gameap checkout --detach FETCH_HEAD
  fi
fi

# pkg/proto ships host-side gRPC stubs (*_grpc.pb.go) whose TLS code TinyGo
# can't compile. The guest only needs the message types, so drop them.
rm -f .sdk/gameap/pkg/proto/*_grpc.pb.go

echo ">> [2/3] Build frontend bundle (Vite) in ${NODE_IMAGE}"
# `npm ci` builds from the committed lockfile. It replaces
# frontend/node_modules wholesale, since that directory is bind-mounted.
"$CONTAINER" run --rm "${RUN_AS[@]}" -e HOME=/tmp -e PLUGIN_VERSION="$GO_VER" \
  -v "$PWD/frontend:/app" -w /app "${NODE_IMAGE}" \
  sh -c "npm ci --no-audit --no-fund && npm run build"

[ -f frontend/dist/plugin.js ] || { echo "!! frontend build did not produce plugin.js" >&2; exit 1; }
[ -f frontend/dist/plugin.css ] || { echo "!! frontend build did not produce plugin.css" >&2; exit 1; }
# Vite empties dist/ before writing, which takes the tracked placeholder with it.
touch frontend/dist/.gitkeep

echo ">> [3/3] Compile WASM (TinyGo) in ${TINYGO_IMAGE} (cache: ${CACHE_DIR})"
# -scheduler=asyncify, not none: under TinyGo 0.42 (Go 1.27) the runtime starts
# a timer goroutine that -scheduler=none refuses to compile ("attempted to start
# a goroutine without a scheduler" in time/sleep.go). It does so for the 4.4.1
# SDK too, so it comes from the toolchain, not the SDK. asyncify is also what the
# SDK's own server-logger example builds with.
mkdir -p "$CACHE_DIR"
"$CONTAINER" run --rm "${RUN_AS[@]}" \
  -e HOME=/tmp \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/gomod \
  -e GOPATH=/cache/gopath \
  -e OUT="$OUT" \
  -v "$CACHE_DIR:/cache" \
  -v "$PWD:/src" -w /src "${TINYGO_IMAGE}" \
  sh -c 'cp go.mod .build.mod; trap "rm -f .build.mod .build.sum" EXIT; GOFLAGS=-modfile=.build.mod go mod tidy && GOFLAGS=-modfile=.build.mod tinygo build -o "$OUT" -target=wasip1 -buildmode=c-shared -scheduler=asyncify .'

echo ">> Done: ${OUT}"
echo ">> Install it via the GameAP panel: Administration -> Plugins -> Upload -> ${OUT}"
