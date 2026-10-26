#!/usr/bin/env bash
# build.sh — compile build/vmlinux avec la config invitée Firecracker + options SPEC §4.2
# Prérequis : make, gcc, libelf-dev, bison, flex, libssl-dev, bc, pahole
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build"
VERSIONS_ENV="$REPO_ROOT/deploy/versions.env"

# shellcheck disable=SC1090
source "$VERSIONS_ENV"

# Kernel LTS 6.12.x — télécharger depuis kernel.org
KERNEL_MAJOR="${KERNEL_VERSION%%.*}"   # 6
TARBALL="linux-${KERNEL_VERSION}.tar.xz"
TARBALL_URL="https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_MAJOR}.x/${TARBALL}"

# Config invitée de référence dans le repo Firecracker
FC_VERSION="$FIRECRACKER_VERSION"
FC_CONFIG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs/microvm-kernel-ci-config-x86_64"

mkdir -p "$BUILD_DIR"
WORK_DIR="$BUILD_DIR/kernel-src"

# ── téléchargement ─────────────────────────────────────────────────────────────
if [ ! -f "$BUILD_DIR/$TARBALL" ]; then
    echo "==> Téléchargement du kernel $KERNEL_VERSION..."
    curl -fL --progress-bar -o "$BUILD_DIR/$TARBALL" "$TARBALL_URL"
fi

# ── extraction ─────────────────────────────────────────────────────────────────
if [ ! -d "$WORK_DIR" ]; then
    echo "==> Extraction..."
    mkdir -p "$WORK_DIR"
    tar -xf "$BUILD_DIR/$TARBALL" --strip-components=1 -C "$WORK_DIR"
fi

cd "$WORK_DIR"

# ── config de base Firecracker ─────────────────────────────────────────────────
echo "==> Récupération de la config invitée Firecracker ($FC_VERSION)..."
curl -fsSL -o .config "$FC_CONFIG_URL"

# ── options supplémentaires requises par la SPEC §4.2 ─────────────────────────
# Activer ou vérifier chaque option via scripts/config
scripts/config \
    --enable  CONFIG_VIRTIO_VSOCKETS \
    --enable  CONFIG_VIRTIO_VSOCKETS_COMMON \
    --enable  CONFIG_VSOCKETS \
    --enable  CONFIG_VIRTIO_BALLOON \
    --enable  CONFIG_VMGENID \
    --enable  CONFIG_EXT4_FS \
    --enable  CONFIG_CGROUPS \
    --enable  CONFIG_CGROUP_V1 \
    --enable  CONFIG_CGROUP_CPUACCT \
    --enable  CONFIG_MEMCG \
    --enable  CONFIG_BLK_CGROUP \
    --enable  CONFIG_CGROUP_SCHED \
    --enable  CONFIG_FAIR_GROUP_SCHED \
    --enable  CONFIG_TMPFS \
    --enable  CONFIG_SYSFS \
    --enable  CONFIG_PROC_FS \
    --enable  CONFIG_DEVTMPFS \
    --enable  CONFIG_DEVTMPFS_MOUNT \
    --module  CONFIG_VIRTIO \
    --module  CONFIG_VIRTIO_NET \
    --module  CONFIG_VIRTIO_BLK \
    --module  CONFIG_VIRTIO_PCI \
    --enable  CONFIG_HW_RANDOM_VIRTIO

# Résoudre les dépendances automatiquement (yes '' = répondre avec la valeur par défaut)
make olddefconfig

echo "==> Compilation ($(nproc) threads)..."
make vmlinux -j"$(nproc)" \
    LOCALVERSION="" \
    KBUILD_BUILD_USER=wsd \
    KBUILD_BUILD_HOST=build

echo "==> Copie dans $BUILD_DIR/vmlinux..."
cp vmlinux "$BUILD_DIR/vmlinux"

HASH=$(sha256sum "$BUILD_DIR/vmlinux" | cut -c1-16)
echo "==> vmlinux prêt — hash=$HASH  version=$(./scripts/setlocalversion .)"
