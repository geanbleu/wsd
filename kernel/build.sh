#!/usr/bin/env bash
# build.sh — compile build/vmlinux avec la config invitée Firecracker + options SPEC §4.2
# Prérequis : make, gcc, libelf-dev, bison, flex, libssl-dev, bc, pahole
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build"
VERSIONS_ENV="$REPO_ROOT/deploy/versions.env"

# shellcheck disable=SC1090
source "$VERSIONS_ENV"

# Kernel LTS — télécharger depuis kernel.org
KERNEL_MAJOR="${KERNEL_VERSION%%.*}"                        # 6.18.59 → 6
KERNEL_SERIES=$(echo "$KERNEL_VERSION" | cut -d. -f1,2)     # 6.18.59 → 6.18
TARBALL="linux-${KERNEL_VERSION}.tar.xz"
TARBALL_URL="https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_MAJOR}.x/${TARBALL}"

# Config invitée de référence dans le repo Firecracker. Le nom du fichier porte
# l'architecture et la série du kernel : microvm-kernel-ci-x86_64-6.18.config.
ARCH="${ARCH:-$(uname -m)}"
FC_VERSION="$FIRECRACKER_VERSION"
FC_GUEST_CONFIGS="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs"
FC_CONFIG_NAME="microvm-kernel-ci-${ARCH}-${KERNEL_SERIES}.config"
FC_CONFIG_URL="${FC_GUEST_CONFIGS}/${FC_CONFIG_NAME}"

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
echo "==> Récupération de $FC_CONFIG_NAME (Firecracker $FC_VERSION)..."
if ! curl -fsSL -o .config "$FC_CONFIG_URL"; then
    echo >&2
    echo "ERREUR : Firecracker $FC_VERSION ne fournit pas $FC_CONFIG_NAME." >&2
    echo "Les configs invitées n'existent que pour certaines séries de kernel." >&2
    echo "Disponibles pour $ARCH :" >&2
    curl -fsSL "https://api.github.com/repos/firecracker-microvm/firecracker/contents/resources/guest_configs?ref=${FC_VERSION}" \
        | grep -o "microvm-kernel-ci-${ARCH}-[^\"]*\.config" \
        | sort -u | sed -e 's/^/  /' >&2
    echo "Aligner KERNEL_VERSION dans deploy/versions.env sur l'une de ces séries." >&2
    exit 1
fi

# ── options requises par la SPEC §4.2 ─────────────────────────────────────────
# Elles sont toutes à =y dans la config amont ; on le vérifie au lieu de le
# supposer, et on n'active que ce qui manquerait. Surtout, rien n'est passé en
# module : la cmdline invitée contient « nomodule » et le rootfs est sur
# /dev/vda sans initramfs, donc virtio compilé en module rendrait la VM
# non amorçable.
REQUIRED_Y="
CONFIG_VIRTIO CONFIG_VIRTIO_MMIO CONFIG_VIRTIO_BLK CONFIG_VIRTIO_NET
CONFIG_VIRTIO_VSOCKETS CONFIG_VIRTIO_VSOCKETS_COMMON CONFIG_VSOCKETS
CONFIG_VIRTIO_BALLOON CONFIG_HW_RANDOM_VIRTIO CONFIG_VMGENID
CONFIG_EXT4_FS CONFIG_TMPFS CONFIG_SYSFS CONFIG_PROC_FS
CONFIG_DEVTMPFS CONFIG_DEVTMPFS_MOUNT
CONFIG_CGROUPS CONFIG_MEMCG CONFIG_BLK_CGROUP CONFIG_CGROUP_SCHED
CONFIG_FAIR_GROUP_SCHED CONFIG_CGROUP_CPUACCT
CONFIG_SERIAL_8250 CONFIG_SERIAL_8250_CONSOLE
"

missing_in_base=""
for sym in $REQUIRED_Y; do
    grep -q "^${sym}=y$" .config || missing_in_base="$missing_in_base $sym"
done
if [ -n "$missing_in_base" ]; then
    echo "--> Absentes de la config amont, activées :$missing_in_base"
    # shellcheck disable=SC2086
    scripts/config $(for sym in $missing_in_base; do printf -- '--enable %s ' "$sym"; done)
else
    echo "--> Les options requises sont déjà toutes à =y dans la config amont"
fi

# Résoudre les dépendances avec les valeurs par défaut
make olddefconfig

# olddefconfig peut désactiver une option dont une dépendance manque : vérifier
# avant de lancer une compilation de plusieurs dizaines de minutes.
not_builtin=""
for sym in $REQUIRED_Y; do
    grep -q "^${sym}=y$" .config || not_builtin="$not_builtin $sym"
done
if [ -n "$not_builtin" ]; then
    echo >&2
    echo "ERREUR : options absentes ou en module après olddefconfig :$not_builtin" >&2
    echo "Elles doivent être compilées en dur (=y). Compilation interrompue." >&2
    exit 1
fi
echo "==> Config vérifiée : $(grep -c '=y$' .config) options en dur"

echo "==> Compilation ($(nproc) threads)..."
make vmlinux -j"$(nproc)" \
    LOCALVERSION="" \
    KBUILD_BUILD_USER=wsd \
    KBUILD_BUILD_HOST=build

echo "==> Copie dans $BUILD_DIR/vmlinux..."
cp vmlinux "$BUILD_DIR/vmlinux"

HASH=$(sha256sum "$BUILD_DIR/vmlinux" | cut -c1-16)
echo "==> vmlinux prêt — hash=$HASH  version=$(./scripts/setlocalversion .)"
