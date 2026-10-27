#!/usr/bin/env bash
# uninstall.sh — désinstalle wsd.
#
# Par défaut : arrête les services, retire les unités, les binaires, le fichier
# sysctl et la documentation. La configuration, les données et le compte système
# sont CONSERVÉS. Les suppressions destructrices sont explicites et confirmées.
#
# Usage : uninstall.sh [options]
#   --prefix <dir>      préfixe des binaires        (défaut /usr/local)
#   --sysconfdir <dir>  configuration               (défaut /etc/wsd)
#   --data-dir <dir>    données wsd                 (défaut /var/lib/wsd)
#   --jailer-dir <dir>  chroots jailer              (défaut /srv/jailer)
#   --unit-dir <dir>    unités systemd              (défaut /etc/systemd/system)
#   --doc-dir <dir>     documentation               (défaut /usr/share/doc/wsd)
#   --purge             supprime aussi la configuration, le compte wsproxy et
#                       le groupe wsd
#   --purge-data        supprime aussi les disques workspace, les goldens et les
#                       chroots jailer — PERTE DE DONNÉES UTILISATEUR
#   --yes               ne pas demander de confirmation
#   --dry-run           afficher les actions sans les exécuter
#
# Variable d'environnement DESTDIR : agit sous une racine décalée (staging).
# Dans ce mode, ni le compte système ni systemd ne sont touchés.
set -euo pipefail

PREFIX="/usr/local"
SYSCONFDIR="/etc/wsd"
DATA_DIR="/var/lib/wsd"
JAILER_DIR="/srv/jailer"
UNIT_DIR="/etc/systemd/system"
DOC_DIR="/usr/share/doc/wsd"
PURGE=0
PURGE_DATA=0
ASSUME_YES=0
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --prefix)     PREFIX="$2"; shift 2 ;;
        --sysconfdir) SYSCONFDIR="$2"; shift 2 ;;
        --data-dir)   DATA_DIR="$2"; shift 2 ;;
        --jailer-dir) JAILER_DIR="$2"; shift 2 ;;
        --unit-dir)   UNIT_DIR="$2"; shift 2 ;;
        --doc-dir)    DOC_DIR="$2"; shift 2 ;;
        --purge)      PURGE=1; shift ;;
        --purge-data) PURGE_DATA=1; shift ;;
        --yes)        ASSUME_YES=1; shift ;;
        --dry-run)    DRY_RUN=1; shift ;;
        -h|--help)    sed -n '2,26p' "$0"; exit 0 ;;
        *) echo "argument inconnu : $1" >&2; exit 1 ;;
    esac
done

step() { printf '\n==> %s\n' "$1"; }
info() { printf '    %s\n' "$1"; }

run() {
    if [ "$DRY_RUN" = 1 ]; then
        printf '    [dry-run] %s\n' "$*"
    else
        "$@"
    fi
}

DESTDIR="${DESTDIR:-}"
STAGED=0
if [ -n "$DESTDIR" ]; then
    STAGED=1
    PREFIX="$DESTDIR$PREFIX"
    SYSCONFDIR="$DESTDIR$SYSCONFDIR"
    DATA_DIR="$DESTDIR$DATA_DIR"
    JAILER_DIR="$DESTDIR$JAILER_DIR"
    UNIT_DIR="$DESTDIR$UNIT_DIR"
    DOC_DIR="$DESTDIR$DOC_DIR"
    SYSCTL_FILE="$DESTDIR/etc/sysctl.d/90-wsd.conf"
    echo "Désinstallation décalée sous $DESTDIR."
else
    SYSCTL_FILE="/etc/sysctl.d/90-wsd.conf"
fi

if [ "$DRY_RUN" != 1 ] && [ "$STAGED" != 1 ] && [ "$(id -u)" != 0 ]; then
    echo "uninstall.sh doit tourner en root (ou avec --dry-run, ou avec DESTDIR=)" >&2
    exit 1
fi

confirm() {
    [ "$ASSUME_YES" = 1 ] && return 0
    [ "$DRY_RUN" = 1 ] && return 0
    printf '%s [taper « oui » pour confirmer] ' "$1"
    local answer
    read -r answer
    [ "$answer" = "oui" ]
}

# ── workspaces encore présents ? ──────────────────────────────────────────────
step "Workspaces restants"
if [ "$STAGED" = 1 ]; then
    info "ignoré (désinstallation décalée)"
fi
LEFTOVER_NS=$(ip netns list 2>/dev/null | grep -c '^ws-' || true)
LEFTOVER_FC=$(pgrep -c -x firecracker 2>/dev/null || true)
if [ "$STAGED" != 1 ] && { [ "${LEFTOVER_NS:-0}" != 0 ] || [ "${LEFTOVER_FC:-0}" != 0 ]; }; then
    info "$LEFTOVER_NS netns ws-*, $LEFTOVER_FC process firecracker"
    info "Supprimer les workspaces avec 'wsctl rm <id>' avant de désinstaller"
    info "laisse le host propre (netns, veth, nftables, chroots)."
    if ! confirm "Continuer malgré des workspaces restants ?"; then
        echo "abandon" >&2
        exit 1
    fi
elif [ "$STAGED" != 1 ]; then
    info "aucun netns ws-* ni process firecracker"
fi

# ── services ──────────────────────────────────────────────────────────────────
step "Services"
if [ "$STAGED" = 1 ]; then
    info "ignoré (désinstallation décalée)"
elif command -v systemctl >/dev/null 2>&1; then
    for unit in wsproxy wsd; do
        if systemctl list-unit-files "$unit.service" >/dev/null 2>&1; then
            run systemctl disable --now "$unit.service" || true
            info "$unit.service arrêté et désactivé"
        fi
    done
fi

# ── unités, binaires, sysctl, doc ─────────────────────────────────────────────
step "Fichiers installés"
run rm -f "$UNIT_DIR/wsd.service" "$UNIT_DIR/wsproxy.service"
if [ "$STAGED" != 1 ] && command -v systemctl >/dev/null 2>&1; then
    run systemctl daemon-reload
fi
run rm -f "$PREFIX/bin/wsd" "$PREFIX/bin/wsproxy" "$PREFIX/bin/wsctl"
run rm -f "$SYSCTL_FILE"
run rm -rf "$DOC_DIR"
info "unités, binaires, $SYSCTL_FILE, $DOC_DIR"
info "firecracker et jailer ne sont pas touchés (installés hors de wsd)"

# ── purge de la configuration et du compte ────────────────────────────────────
if [ "$PURGE" = 1 ]; then
    step "Configuration et compte système (--purge)"
    run rm -rf "$SYSCONFDIR"
    info "$SYSCONFDIR supprimé (y compris les secrets)"
    if [ "$STAGED" = 1 ]; then
        info "compte wsproxy et groupe wsd conservés (désinstallation décalée)"
    elif getent passwd wsproxy >/dev/null; then
        run userdel wsproxy || true
        info "utilisateur wsproxy supprimé"
    fi
    if getent group wsd >/dev/null; then
        run groupdel wsd || true
        info "groupe wsd supprimé"
    fi
else
    info "$SYSCONFDIR, le compte wsproxy et le groupe wsd sont conservés (--purge pour les retirer)"
fi

# ── purge des données ─────────────────────────────────────────────────────────
if [ "$PURGE_DATA" = 1 ]; then
    step "Données (--purge-data)"
    echo "    Vont être supprimés définitivement :"
    echo "      $DATA_DIR  (disques workspace, goldens, images, wsd.db)"
    echo "      $JAILER_DIR"
    if confirm "    Supprimer les données utilisateur ?"; then
        run rm -rf "$DATA_DIR" "$JAILER_DIR"
        info "données supprimées"
    else
        info "données conservées"
    fi
else
    info "$DATA_DIR et $JAILER_DIR sont conservés (--purge-data pour les retirer)"
fi

echo
echo "Désinstallation terminée."
