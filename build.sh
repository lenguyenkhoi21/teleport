#!/bin/bash
# =============================================================================
# build.sh — Build Teleport v18.10.3 (Keycloak OIDC Patch)
# =============================================================================
#
# Script này build Teleport binaries bằng Docker container.
# Không cần cài Go, Rust, Node.js trên máy — chỉ cần Docker.
#
# Usage:
#   ./build.sh                    # Build tất cả (teleport, tctl, tsh, tbot)
#   ./build.sh teleport tctl      # Build binary cụ thể
#   ./build.sh --fast             # Build nhanh (skip web UI + RDP)
#   ./build.sh --image-only       # Chỉ build Docker image, không build binary
#
# Output: ./output/
# =============================================================================

set -euo pipefail

# --- Config ---
BUILDER_IMAGE="teleport-builder"
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${SRC_DIR}/output"
DOCKERFILE="${SRC_DIR}/Dockerfile.build"

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${CYAN}>>>${NC} $*"; }
ok()   { echo -e "${GREEN}✓${NC} $*"; }
warn() { echo -e "${YELLOW}⚠${NC} $*"; }
err()  { echo -e "${RED}✗${NC} $*" >&2; }

# --- Parse args ---
FAST_MODE=0
IMAGE_ONLY=0
TARGETS=()

for arg in "$@"; do
    case "$arg" in
        --fast)      FAST_MODE=1 ;;
        --image-only) IMAGE_ONLY=1 ;;
        --help|-h)
            echo "Usage: $0 [OPTIONS] [TARGETS...]"
            echo ""
            echo "Options:"
            echo "  --fast         Skip web UI and RDP client build (much faster)"
            echo "  --image-only   Only build the Docker builder image"
            echo "  -h, --help     Show this help"
            echo ""
            echo "Targets (default: all):"
            echo "  teleport       Auth/Proxy/Node server"
            echo "  tctl           Admin CLI"
            echo "  tsh            User login CLI"
            echo "  tbot           Machine identity bot"
            echo "  teleport-update Auto-updater"
            echo ""
            echo "Examples:"
            echo "  $0                       # Build all binaries with web UI"
            echo "  $0 --fast                # Build all binaries, skip web UI"
            echo "  $0 --fast teleport tctl  # Build only server + admin CLI"
            echo "  $0 tsh                   # Build only tsh client"
            exit 0
            ;;
        *)           TARGETS+=("$arg") ;;
    esac
done

# --- Check Docker ---
if ! command -v docker &>/dev/null; then
    err "Docker is not installed. Please install Docker first."
    exit 1
fi

# --- Step 1: Build Docker image ---
log "Building Docker builder image: ${BUILDER_IMAGE}..."
docker buildx build --progress=plain -t "${BUILDER_IMAGE}" -f "${DOCKERFILE}" "${SRC_DIR}"
ok "Builder image ready"

if [ "$IMAGE_ONLY" -eq 1 ]; then
    ok "Image-only mode, done."
    exit 0
fi

# --- Step 2: Build binaries ---
echo ""
log "Starting build..."
echo "    Source:  ${SRC_DIR}"
echo "    Output:  ${OUT_DIR}"
echo "    Mode:    $([ $FAST_MODE -eq 1 ] && echo 'FAST (skip web UI + RDP)' || echo 'FULL')"
echo "    Targets: ${TARGETS[*]:-all}"
echo ""

mkdir -p "${OUT_DIR}"

# Build env vars
DOCKER_ENV=()
if [ "$FAST_MODE" -eq 1 ]; then
    DOCKER_ENV+=(-e WEBASSETS_SKIP_BUILD=1 -e RDPCLIENT_SKIP_BUILD=1)
fi

docker run --rm \
    -v "${SRC_DIR}:/src" \
    -v "${OUT_DIR}:/output" \
    "${DOCKER_ENV[@]}" \
    "${BUILDER_IMAGE}" \
    "${TARGETS[@]}"

# --- Done ---
echo ""
log "Build output:"
for bin in "${OUT_DIR}"/*; do
    if [ -f "$bin" ] && [ -x "$bin" ]; then
        name=$(basename "$bin")
        size=$(du -h "$bin" | cut -f1)
        ok "${name}  (${size})"
    fi
done

echo ""
ok "Done! Binaries are in: ${OUT_DIR}/"
