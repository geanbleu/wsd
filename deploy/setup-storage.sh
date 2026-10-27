#!/usr/bin/env bash
# setup-storage.sh — prépare le stockage de wsd : un filesystem XFS avec
# reflink=1, qui porte à la fois les données wsd et les chroots jailer.
#
# wsd a besoin de FICLONE (copies reflink) pour cloner le disque workspace et le
# rootfs du golden sans recopier les données, et de hardlinks entre les données
# et les chroots — donc d'un seul et même filesystem pour les deux.
# Seuls XFS (reflink=1) et btrfs implémentent FICLONE ; ext4 ne le fait pas.
#
# Usage : setup-storage.sh (--device <dev> | --image <fichier> --size <taille>) [options]
#   --device <dev>     formate ce périphérique en XFS reflink=1. DESTRUCTIF.
#   --image <fichier>  crée à la place un fichier XFS monté en loop. Pour un host
#                      dont le filesystem racine n'a pas reflink, ou pour une
#                      maquette. Les performances sont celles du FS sous-jacent.
#   --size <taille>    taille de l'image, ex. 200G (requis avec --image)
#   --mount <dir>      point de montage (défaut /var/lib/wsd)
#   --jailer <dir>     monte en bind <mount>/jailer sur <dir>, pour garder
#                      jailer_dir=/srv/jailer. Sans cette option, utiliser
#                      jailer_dir=<mount>/jailer dans wsd.json.
#   --fstab            écrit les entrées dans /etc/fstab (sinon elles sont
#                      seulement affichées)
#   --force            formate même si un filesystem est déjà présent
#   --yes              ne pas demander de confirmation
#   --dry-run          afficher les actions sans les exécuter
set -euo pipefail

DEVICE=""
IMAGE=""
SIZE=""
MOUNT="/var/lib/wsd"
JAILER=""
WRITE_FSTAB=0
FORCE=0
ASSUME_YES=0
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --device)  DEVICE="$2"; shift 2 ;;
        --image)   IMAGE="$2"; shift 2 ;;
        --size)    SIZE="$2"; shift 2 ;;
        --mount)   MOUNT="$2"; shift 2 ;;
        --jailer)  JAILER="$2"; shift 2 ;;
        --fstab)   WRITE_FSTAB=1; shift ;;
        --force)   FORCE=1; shift ;;
        --yes)     ASSUME_YES=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h|--help) sed -n '2,27p' "$0"; exit 0 ;;
        *) echo "argument inconnu : $1" >&2; exit 1 ;;
    esac
done

step() { printf '\n==> %s\n' "$1"; }
info() { printf '    %s\n' "$1"; }
die()  { echo "erreur : $1" >&2; exit 1; }

run() {
    if [ "$DRY_RUN" = 1 ]; then
        printf '    [dry-run] %s\n' "$*"
    else
        "$@"
    fi
}

confirm() {
    [ "$ASSUME_YES" = 1 ] && return 0
    [ "$DRY_RUN" = 1 ] && return 0
    printf '%s [taper « oui » pour confirmer] ' "$1"
    local answer
    read -r answer
    [ "$answer" = "oui" ]
}

# ── validation des arguments ──────────────────────────────────────────────────
[ -n "$DEVICE" ] || [ -n "$IMAGE" ] || die "--device ou --image est requis"
[ -z "$DEVICE" ] || [ -z "$IMAGE" ] || die "--device et --image s'excluent"
[ -z "$IMAGE" ] || [ -n "$SIZE" ] || die "--size est requis avec --image"
case "$MOUNT" in /*) ;; *) die "--mount doit être un chemin absolu" ;; esac
if [ -n "$JAILER" ]; then
    case "$JAILER" in /*) ;; *) die "--jailer doit être un chemin absolu" ;; esac
fi

if [ "$DRY_RUN" != 1 ] && [ "$(id -u)" != 0 ]; then
    die "setup-storage.sh doit tourner en root (ou avec --dry-run)"
fi

command -v mkfs.xfs >/dev/null 2>&1 || die "mkfs.xfs introuvable (paquet xfsprogs)"

# ── le point de montage doit être libre ───────────────────────────────────────
step "Point de montage $MOUNT"
if findmnt -n --mountpoint "$MOUNT" >/dev/null 2>&1; then
    die "$MOUNT est déjà un point de montage — le démonter ou choisir --mount"
fi
if [ -d "$MOUNT" ] && [ -n "$(ls -A "$MOUNT" 2>/dev/null || true)" ]; then
    info "$MOUNT existe et n'est pas vide : son contenu sera masqué par le montage"
    confirm "    Continuer ?" || die "abandon"
fi
run install -d -m 0755 "$MOUNT"
info "prêt"

# ── création du support ───────────────────────────────────────────────────────
if [ -n "$DEVICE" ]; then
    step "Périphérique $DEVICE"
    [ -b "$DEVICE" ] || [ "$DRY_RUN" = 1 ] || die "$DEVICE n'est pas un périphérique bloc"

    if findmnt -n --source "$DEVICE" >/dev/null 2>&1; then
        die "$DEVICE est monté — le démonter d'abord"
    fi
    existing=$(blkid -o value -s TYPE "$DEVICE" 2>/dev/null || true)
    if [ -n "$existing" ] && [ "$FORCE" != 1 ]; then
        die "$DEVICE contient déjà un filesystem « $existing » — --force pour écraser"
    fi
    size_h=$(lsblk -dn -o SIZE "$DEVICE" 2>/dev/null | tr -d ' ' || echo inconnue)
    info "taille $size_h, filesystem actuel : ${existing:-aucun}"
    confirm "    FORMATER $DEVICE en XFS ? Toutes ses données seront perdues." \
        || die "abandon"

    run mkfs.xfs -f -m reflink=1 -L wsd "$DEVICE"
    SRC="$DEVICE"
    FSTAB_SRC=""   # rempli plus bas depuis l'UUID
    FSTAB_OPTS="defaults"
else
    step "Image $IMAGE ($SIZE)"
    if [ -e "$IMAGE" ] && [ "$FORCE" != 1 ]; then
        die "$IMAGE existe déjà — --force pour l'écraser"
    fi
    run install -d -m 0755 "$(dirname "$IMAGE")"
    # Fichier sparse : l'espace n'est consommé qu'à l'écriture.
    run truncate -s "$SIZE" "$IMAGE"
    run chmod 0600 "$IMAGE"
    run mkfs.xfs -f -m reflink=1 -L wsd "$IMAGE"
    info "image XFS prête (fichier sparse, croît à l'usage)"
    info "attention : les reflinks sont internes à l'image, le FS sous-jacent"
    info "voit un seul gros fichier — la sauvegarde se fait au niveau de l'image"
    SRC="$IMAGE"
    FSTAB_SRC="$IMAGE"
    FSTAB_OPTS="loop,defaults"
fi

# ── montage ───────────────────────────────────────────────────────────────────
step "Montage sur $MOUNT"
if [ -n "$IMAGE" ]; then
    run mount -o loop "$SRC" "$MOUNT"
else
    run mount "$SRC" "$MOUNT"
fi
info "monté"

# ── vérification FICLONE : le seul test qui compte ────────────────────────────
step "Vérification des copies reflink (FICLONE)"
if [ "$DRY_RUN" = 1 ]; then
    info "[dry-run] cp --reflink=always dans $MOUNT"
else
    probe="$MOUNT/.wsd-reflink-probe"
    head -c 65536 /dev/urandom > "$probe.src"
    if cp --reflink=always "$probe.src" "$probe.dst" 2>/dev/null; then
        info "FICLONE fonctionne sur $(stat -f -c %T "$MOUNT")"
    else
        rm -f "$probe.src" "$probe.dst"
        umount "$MOUNT"
        die "FICLONE ne fonctionne pas sur le filesystem monté — wsd ne peut pas tourner dessus"
    fi
    rm -f "$probe.src" "$probe.dst"
fi

# ── arborescence ──────────────────────────────────────────────────────────────
step "Arborescence"
run install -d -m 0755 "$MOUNT/images" "$MOUNT/workspaces" "$MOUNT/goldens" "$MOUNT/jailer"
info "$MOUNT/{images,workspaces,goldens,jailer}"

# ── bind mount des chroots jailer ─────────────────────────────────────────────
if [ -n "$JAILER" ]; then
    step "Bind mount $JAILER"
    if findmnt -n --mountpoint "$JAILER" >/dev/null 2>&1; then
        info "$JAILER est déjà un point de montage, ignoré"
    else
        run install -d -m 0755 "$JAILER"
        run mount --bind "$MOUNT/jailer" "$JAILER"
        info "$MOUNT/jailer → $JAILER (même superbloc : hardlinks OK)"
    fi
fi

# ── fstab ─────────────────────────────────────────────────────────────────────
step "Montage au démarrage"
if [ -n "$DEVICE" ]; then
    if [ "$DRY_RUN" = 1 ]; then
        FSTAB_SRC="UUID=<uuid de $DEVICE>"
    else
        FSTAB_SRC="UUID=$(blkid -o value -s UUID "$DEVICE")"
    fi
fi
[ -n "$FSTAB_SRC" ] || FSTAB_SRC="$SRC"

fstab_lines="$FSTAB_SRC $MOUNT xfs $FSTAB_OPTS 0 0"
if [ -n "$JAILER" ]; then
    fstab_lines="$fstab_lines
$MOUNT/jailer $JAILER none bind 0 0"
fi

if [ "$WRITE_FSTAB" = 1 ]; then
    if [ "$DRY_RUN" = 1 ]; then
        info "[dry-run] ajout dans /etc/fstab :"
        printf '%s\n' "$fstab_lines" | sed 's/^/      /'
    elif grep -qF " $MOUNT " /etc/fstab 2>/dev/null; then
        info "/etc/fstab mentionne déjà $MOUNT — rien ajouté, à vérifier à la main"
    else
        {
            echo "# wsd — stockage XFS reflink (setup-storage.sh)"
            echo "$fstab_lines"
        } >> /etc/fstab
        info "/etc/fstab complété"
        systemctl daemon-reload 2>/dev/null || true
        findmnt --verify --fstab >/dev/null \
            && info "findmnt --verify : OK" \
            || info "findmnt --verify signale un problème, relire /etc/fstab"
    fi
else
    info "non écrit (--fstab pour le faire). À ajouter dans /etc/fstab :"
    printf '%s\n' "$fstab_lines" | sed 's/^/      /'
fi

# ── suite ─────────────────────────────────────────────────────────────────────
cat <<EOF

──────────────────────────────────────────────────────────────────────
Stockage prêt sur $MOUNT.

À régler dans /etc/wsd/wsd.json :
  "data_dir":   "$MOUNT",
EOF
if [ -n "$JAILER" ]; then
    echo "  \"jailer_dir\": \"$JAILER\","
else
    echo "  \"jailer_dir\": \"$MOUNT/jailer\","
fi
cat <<EOF

Puis vérifier, installer et démarrer :
  bash deploy/check-host.sh
  sudo bash deploy/install.sh --with-deps
──────────────────────────────────────────────────────────────────────
EOF
