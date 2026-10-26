#!/usr/bin/env bash
# build.sh — construit build/rootfs.ext4 depuis image/Dockerfile
# Prérequis : docker, CGO_ENABLED=0 go build disponible
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build"
VERSIONS_ENV="$REPO_ROOT/deploy/versions.env"

# Charger les versions épinglées
# shellcheck disable=SC1090
source "$VERSIONS_ENV"

ROOTFS_SIZE="${ROOTFS_SIZE:-2G}"   # taille du ext4 — ajustable
IMAGE_TAG="wsd-rootfs:${CODE_SERVER_VERSION}-${OPENCODE_VERSION}"

mkdir -p "$BUILD_DIR"

echo "==> Construction de l'image Docker ($IMAGE_TAG)..."
# Construire wsagent d'abord (binaire statique pour l'invité)
echo "--> Build wsagent (GOARCH=amd64, CGO_ENABLED=0)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -o "$REPO_ROOT/image/wsagent" \
    "$REPO_ROOT/cmd/wsagent"

docker build \
    --build-arg CODE_SERVER_VERSION="$CODE_SERVER_VERSION" \
    --build-arg OPENCODE_VERSION="$OPENCODE_VERSION" \
    --build-arg GO_VERSION="$GO_VERSION" \
    -t "$IMAGE_TAG" \
    "$REPO_ROOT/image"

rm -f "$REPO_ROOT/image/wsagent"  # nettoyage du contexte Docker

echo "==> Export du filesystem..."
TMP_ROOTFS="$BUILD_DIR/rootfs.dir"
rm -rf "$TMP_ROOTFS" && mkdir -p "$TMP_ROOTFS"
CID=$(docker create "$IMAGE_TAG")
docker export "$CID" | tar -x -C "$TMP_ROOTFS"
docker rm "$CID"

echo "==> Création de build/rootfs.ext4 (${ROOTFS_SIZE})..."
# mkfs.ext4 -d : pas besoin de loop device (e2fsprogs ≥ 1.45)
mkfs.ext4 \
    -L rootfs \
    -d "$TMP_ROOTFS" \
    -m 1 \
    "$BUILD_DIR/rootfs.ext4" \
    "$ROOTFS_SIZE"

rm -rf "$TMP_ROOTFS"

HASH=$(sha256sum "$BUILD_DIR/rootfs.ext4" | cut -c1-16)
echo "==> rootfs.ext4 prêt — hash=$HASH"
echo "$HASH" > "$BUILD_DIR/rootfs.ext4.sha256prefix"
