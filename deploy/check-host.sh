#!/usr/bin/env bash
# check-host.sh — vérifie les prérequis host pour wsd
set -euo pipefail

PASS=0
FAIL=0

ok()  { echo "  [OK] $1"; PASS=$((PASS+1)); }
ko()  { echo "  [KO] $1 — $2"; FAIL=$((FAIL+1)); }

# /dev/kvm
if [ -c /dev/kvm ]; then
    ok "/dev/kvm présent et caractère"
else
    ko "/dev/kvm" "absent ou non caractère"
fi

# cgroup v2
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    ok "cgroup v2 actif"
else
    ko "cgroup v2" "/sys/fs/cgroup/cgroup.controllers absent (monter cgroup2 ou activer unified_cgroup_hierarchy)"
fi

# ip_forward
ip_fwd=$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo "0")
if [ "$ip_fwd" = "1" ]; then
    ok "net.ipv4.ip_forward = 1"
else
    ko "net.ipv4.ip_forward" "désactivé (ajouter net.ipv4.ip_forward=1 dans /etc/sysctl.d/)"
fi

# Binaires requis
for bin in ip nft sysctl firecracker jailer; do
    if command -v "$bin" >/dev/null 2>&1; then
        ok "binaire $bin trouvé : $(command -v "$bin")"
    else
        ko "binaire $bin" "introuvable dans PATH"
    fi
done

# Version Firecracker (informatif)
if command -v firecracker >/dev/null 2>&1; then
    fc_ver=$(firecracker --version 2>/dev/null | head -1 || echo "inconnu")
    echo "         → $fc_ver"
fi

# Filesystem reflink (/var/lib/wsd et /srv/jailer)
# Le seul test qui compte est un FICLONE réel : XFS n'expose pas reflink dans
# les options de montage (c'est un flag de mkfs), et d'autres filesystems que
# XFS et btrfs peuvent le supporter.
check_reflink() {
    local dir="$1"
    local target="$dir"
    while [ ! -e "$target" ]; do
        target=$(dirname "$target")
        [ "$target" = "/" ] && { echo "KO:aucun ancêtre trouvé pour $dir"; return; }
    done
    local fstype
    fstype=$(stat -f -c %T "$target" 2>/dev/null || echo unknown)

    # Test direct : cloner un fichier dans le répertoire cible.
    if [ -w "$target" ] && cp --help 2>/dev/null | grep -q -- --reflink; then
        local probe
        if probe=$(mktemp -p "$target" .wsd-reflink-XXXXXX 2>/dev/null); then
            head -c 4096 /dev/urandom > "$probe" 2>/dev/null || true
            if cp --reflink=always "$probe" "$probe.clone" 2>/dev/null; then
                rm -f "$probe" "$probe.clone"
                echo "ok (FICLONE testé sur $fstype, $target)"
            else
                rm -f "$probe" "$probe.clone"
                echo "KO:FICLONE refusé par $fstype sur $target (besoin xfs -m reflink=1 ou btrfs — voir deploy/setup-storage.sh)"
            fi
            return
        fi
    fi

    # Repli quand le répertoire n'est pas inscriptible (script lancé sans droits).
    case "$fstype" in
        xfs)
            # xfs_info demande le point de montage, pas un sous-répertoire.
            local mp
            mp=$(findmnt -n -o TARGET --target "$target" 2>/dev/null || echo "")
            if [ -n "$mp" ] && command -v xfs_info >/dev/null 2>&1 \
               && xfs_info "$mp" 2>/dev/null | grep -q "reflink=1"; then
                echo "ok (xfs reflink=1 d'après xfs_info sur $mp, non testé : $target non inscriptible)"
            else
                echo "KO:xfs sans reflink=1, ou xfs_info indisponible (reformatter avec mkfs.xfs -m reflink=1)"
            fi
            ;;
        btrfs)
            echo "ok (btrfs, non testé : $target non inscriptible)"
            ;;
        *)
            echo "KO:filesystem '$fstype' sans support FICLONE (besoin xfs+reflink=1 ou btrfs)"
            ;;
    esac
}

r1=$(check_reflink /var/lib/wsd)
if echo "$r1" | grep -q "^ok"; then
    ok "/var/lib/wsd filesystem reflink : $r1"
else
    ko "/var/lib/wsd filesystem reflink" "$r1"
fi

r2=$(check_reflink /srv/jailer)
if echo "$r2" | grep -q "^ok"; then
    ok "/srv/jailer filesystem reflink : $r2"
else
    ko "/srv/jailer filesystem reflink" "$r2"
fi

# /var/lib/wsd et /srv/jailer sur le même device
check_same_device() {
    local d1="$1" d2="$2"
    local t1="$d1" t2="$d2"
    while [ ! -e "$t1" ]; do t1=$(dirname "$t1"); done
    while [ ! -e "$t2" ]; do t2=$(dirname "$t2"); done
    local dev1 dev2
    dev1=$(stat -c %d "$t1" 2>/dev/null)
    dev2=$(stat -c %d "$t2" 2>/dev/null)
    if [ "$dev1" = "$dev2" ]; then
        echo "ok (device $dev1)"
    else
        echo "KO:devices différents ($dev1 vs $dev2) — les hardlinks et reflinks ne fonctionneront pas entre les deux chemins"
    fi
}

r3=$(check_same_device /var/lib/wsd /srv/jailer)
if echo "$r3" | grep -q "^ok"; then
    ok "/var/lib/wsd et /srv/jailer même device : $r3"
else
    ko "/var/lib/wsd et /srv/jailer même device" "$r3"
fi

echo
echo "──────────────────────────────────────"
printf "Résultat : %d OK, %d KO\n" "$PASS" "$FAIL"
echo "──────────────────────────────────────"
[ "$FAIL" -eq 0 ] && exit 0 || exit 1
