#!/usr/bin/env bash
# boot.sh — test de boot à froid d'une microVM Firecracker, sans jailer.
# Lance la VM dans un netns dédié, configure le réseau, vérifie code-server et vsock.
# Prérequis : root, /dev/kvm, build/vmlinux, build/rootfs.ext4, firecracker dans PATH.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BUILD_DIR="$REPO_ROOT/build"

source "$REPO_ROOT/deploy/versions.env"

# ── paramètres ─────────────────────────────────────────────────────────────────
NS=wsd-boot-test
NETNS_PATH="/var/run/netns/$NS"
TAP_IP="172.16.0.1"        # côté host dans le netns
VM_IP="172.16.0.2"         # côté invité
FC_SOCKET="/tmp/fc-boot-test.sock"
VSOCK_PATH="/tmp/fc-vsock-test.sock"
WS_DISK="/tmp/wsd-boot-workspace.ext4"
FC_LOG="/tmp/fc-boot-test.log"
FC_PID=""

cleanup() {
    echo "==> Nettoyage..."
    [ -n "$FC_PID" ] && kill "$FC_PID" 2>/dev/null && wait "$FC_PID" 2>/dev/null || true
    ip netns del "$NS" 2>/dev/null || true
    rm -f "$FC_SOCKET" "$VSOCK_PATH" "$WS_DISK"
    echo "==> Nettoyage terminé"
}
trap cleanup EXIT

# ── pré-requis ──────────────────────────────────────────────────────────────────
[ "$(id -u)" -eq 0 ] || { echo "ERREUR : root requis"; exit 1; }
[ -f "$BUILD_DIR/vmlinux" ]    || { echo "ERREUR : build/vmlinux absent (lancez make kernel)"; exit 1; }
[ -f "$BUILD_DIR/rootfs.ext4" ] || { echo "ERREUR : build/rootfs.ext4 absent (lancez make image)"; exit 1; }
command -v firecracker >/dev/null || { echo "ERREUR : firecracker introuvable dans PATH"; exit 1; }

# ── disque workspace (1 GiB sparse, vierge) ────────────────────────────────────
echo "==> Création du disque workspace de test (1 GiB)..."
truncate -s 1G "$WS_DISK"

# ── netns + tap ────────────────────────────────────────────────────────────────
echo "==> Création du netns $NS..."
ip netns add "$NS"

echo "==> Configuration tap0 dans le netns..."
ip netns exec "$NS" ip tuntap add name tap0 mode tap
ip netns exec "$NS" ip addr add "${TAP_IP}/30" dev tap0
ip netns exec "$NS" ip link set tap0 up
ip netns exec "$NS" ip link set lo up

# ── démarrage de Firecracker dans le netns ──────────────────────────────────────
echo "==> Démarrage de Firecracker..."
rm -f "$FC_SOCKET" "$VSOCK_PATH"
ip netns exec "$NS" firecracker \
    --api-sock "$FC_SOCKET" \
    --log-path "$FC_LOG" \
    --level Info \
    &
FC_PID=$!

# Attendre que le socket soit disponible
for i in $(seq 1 20); do
    [ -S "$FC_SOCKET" ] && break
    sleep 0.1
done
[ -S "$FC_SOCKET" ] || { echo "ERREUR : socket Firecracker non créé"; exit 1; }

FC_API() {
    local method="$1" path="$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -s -X "$method" \
            --unix-socket "$FC_SOCKET" \
            -H 'Content-Type: application/json' \
            -d "$body" \
            "http://localhost${path}"
    else
        curl -s -X "$method" \
            --unix-socket "$FC_SOCKET" \
            "http://localhost${path}"
    fi
}

# ── configuration de la VM ─────────────────────────────────────────────────────
echo "==> Configuration machine..."
FC_API PUT /machine-config \
    '{"vcpu_count":2,"mem_size_mib":512}'

echo "==> Boot source..."
FC_API PUT /boot-source "$(printf '{
  "kernel_image_path": "%s",
  "boot_args": "console=ttyS0 reboot=k panic=1 pci=off nomodule ro"
}' "$BUILD_DIR/vmlinux")"

echo "==> Drive rootfs..."
FC_API PUT /drives/rootfs "$(printf '{
  "drive_id": "rootfs",
  "path_on_host": "%s",
  "is_root_device": true,
  "is_read_only": false
}' "$BUILD_DIR/rootfs.ext4")"

echo "==> Drive workspace..."
FC_API PUT /drives/workspace "$(printf '{
  "drive_id": "workspace",
  "path_on_host": "%s",
  "is_root_device": false,
  "is_read_only": false
}' "$WS_DISK")"

echo "==> Interface réseau..."
FC_API PUT /network-interfaces/eth0 \
    '{"iface_id":"eth0","host_dev_name":"tap0","guest_mac":"06:00:AC:10:00:02"}'

echo "==> Vsock..."
FC_API PUT /vsock "$(printf '{"guest_cid":3,"uds_path":"%s"}' "$VSOCK_PATH")"

# ── démarrage ──────────────────────────────────────────────────────────────────
echo "==> InstanceStart..."
BOOT_TS=$(date +%s%3N)
FC_API PUT /actions '{"action_type":"InstanceStart"}'

# ── attente : code-server prêt ─────────────────────────────────────────────────
echo "==> Attente de code-server (max 60 s)..."
for i in $(seq 1 120); do
    HTTP_CODE=$(ip netns exec "$NS" \
        curl -s -o /dev/null -w "%{http_code}" \
        --max-time 1 "http://${VM_IP}:8080" 2>/dev/null || echo "000")
    if [ "$HTTP_CODE" != "000" ]; then
        READY_TS=$(date +%s%3N)
        BOOT_MS=$((READY_TS - BOOT_TS))
        echo "  code-server répond : HTTP $HTTP_CODE"
        echo "  ★ Temps de boot à froid jusqu'à code-server prêt : ${BOOT_MS} ms"
        break
    fi
    sleep 0.5
done

if [ "$HTTP_CODE" = "000" ]; then
    echo "ERREUR : code-server n'a pas répondu dans les temps"
    echo "Log Firecracker :"
    cat "$FC_LOG" || true
    exit 1
fi

# ── test vsock ping ────────────────────────────────────────────────────────────
echo "==> Test vsock ping..."
# Le handshake Firecracker : CONNECT 1024 → OK 1024 → JSON
VSOCK_RESP=$(printf 'CONNECT 1024\n{"op":"ping"}\n' | \
    socat - "UNIX-CONNECT:$VSOCK_PATH" 2>/dev/null | tail -1 || echo "")

if echo "$VSOCK_RESP" | grep -q '"ok":true'; then
    echo "  vsock ping OK : $VSOCK_RESP"
else
    echo "AVERTISSEMENT : vsock ping inattendu : $VSOCK_RESP"
fi

# ── résumé ─────────────────────────────────────────────────────────────────────
echo ""
echo "══════════════════════════════════════════════════════"
echo "  Phase 1 — critères d'acceptation"
echo "══════════════════════════════════════════════════════"
echo "  code-server : HTTP $HTTP_CODE  (${BOOT_MS} ms depuis InstanceStart)"
echo "  vsock ping  : $VSOCK_RESP"
echo "  → Inscrire le temps de boot dans docs/PLAN.md (tableau Mesures)"
echo "══════════════════════════════════════════════════════"
