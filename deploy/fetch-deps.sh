#!/usr/bin/env bash
# fetch-deps.sh — installe firecracker et jailer aux versions épinglées dans
# deploy/versions.env, depuis les releases GitHub, avec vérification SHA256.
#
# Usage : fetch-deps.sh [--dest <dir>] [--doc-dir <dir>] [--arch <arch>]
#   --dest     répertoire d'installation des binaires (défaut /usr/local/bin)
#   --doc-dir  où déposer le swagger de la version (défaut /usr/share/doc/wsd)
#   --arch     x86_64 (défaut) ou aarch64
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck disable=SC1091
source "$REPO_ROOT/deploy/versions.env"

DEST="/usr/local/bin"
DOC_DIR="/usr/share/doc/wsd"
ARCH="$(uname -m)"

while [ $# -gt 0 ]; do
    case "$1" in
        --dest)    DEST="$2"; shift 2 ;;
        --doc-dir) DOC_DIR="$2"; shift 2 ;;
        --arch)    ARCH="$2"; shift 2 ;;
        -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
        *) echo "argument inconnu : $1" >&2; exit 1 ;;
    esac
done

if [ "$FIRECRACKER_VERSION" != "$JAILER_VERSION" ]; then
    echo "versions.env : FIRECRACKER_VERSION et JAILER_VERSION doivent être identiques" >&2
    echo "(les deux binaires viennent de la même release)" >&2
    exit 1
fi

VER="$FIRECRACKER_VERSION"
BASE="https://github.com/firecracker-microvm/firecracker/releases/download/${VER}"
TGZ="firecracker-${VER}-${ARCH}.tgz"
# Arborescence dans l'archive : release-<ver>-<arch>/{firecracker,jailer}-<ver>-<arch>
SUBDIR="release-${VER}-${ARCH}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "==> Téléchargement de $TGZ..."
curl -fL --progress-bar -o "$WORK/$TGZ"             "$BASE/$TGZ"
curl -fsSL              -o "$WORK/$TGZ.sha256.txt"  "$BASE/$TGZ.sha256.txt"

echo "==> Vérification SHA256..."
( cd "$WORK" && sha256sum -c "$TGZ.sha256.txt" )

echo "==> Extraction..."
tar -xzf "$WORK/$TGZ" -C "$WORK"

echo "==> Installation dans $DEST..."
install -d -m 0755 "$DEST"
install -m 0755 "$WORK/$SUBDIR/firecracker-${VER}-${ARCH}" "$DEST/firecracker"
install -m 0755 "$WORK/$SUBDIR/jailer-${VER}-${ARCH}"      "$DEST/jailer"

# Le swagger de la version exacte sert de référence pour l'API Firecracker.
SPEC="$WORK/$SUBDIR/firecracker_spec-${VER}.yaml"
if [ -f "$SPEC" ]; then
    install -d -m 0755 "$DOC_DIR"
    install -m 0644 "$SPEC" "$DOC_DIR/firecracker_spec-${VER}.yaml"
    echo "    swagger : $DOC_DIR/firecracker_spec-${VER}.yaml"
fi

echo
"$DEST/firecracker" --version | head -1
"$DEST/jailer" --version 2>&1 | head -1
