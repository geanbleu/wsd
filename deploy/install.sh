#!/usr/bin/env bash
# install.sh — installe wsd sur un host (idempotent).
#
# Fonctionne depuis le dépôt (après `make build`) comme depuis l'archive
# produite par `make dist` : les deux ont la même arborescence relative.
#
# Usage : install.sh [options]
#   --prefix <dir>      préfixe des binaires            (défaut /usr/local)
#   --sysconfdir <dir>  répertoire de configuration     (défaut /etc/wsd)
#   --data-dir <dir>    données wsd                     (défaut /var/lib/wsd)
#   --jailer-dir <dir>  chroots jailer                  (défaut /srv/jailer)
#   --unit-dir <dir>    unités systemd                  (défaut /etc/systemd/system)
#   --doc-dir <dir>     documentation                   (défaut /usr/share/doc/wsd)
#   --with-deps         installe aussi firecracker et jailer (fetch-deps.sh)
#   --skip-host-check   ne pas lancer check-host.sh
#   --dry-run           afficher les actions sans les exécuter
#
# Variable d'environnement DESTDIR : installe sous une racine décalée (staging).
# Dans ce mode, rien n'est touché hors de DESTDIR : ni compte système, ni sysctl,
# ni systemd. Utile pour vérifier le contenu d'une installation sans root.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

PREFIX="/usr/local"
SYSCONFDIR="/etc/wsd"
DATA_DIR="/var/lib/wsd"
JAILER_DIR="/srv/jailer"
UNIT_DIR="/etc/systemd/system"
DOC_DIR="/usr/share/doc/wsd"
WITH_DEPS=0
SKIP_HOST_CHECK=0
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --prefix)          PREFIX="$2"; shift 2 ;;
        --sysconfdir)      SYSCONFDIR="$2"; shift 2 ;;
        --data-dir)        DATA_DIR="$2"; shift 2 ;;
        --jailer-dir)      JAILER_DIR="$2"; shift 2 ;;
        --unit-dir)        UNIT_DIR="$2"; shift 2 ;;
        --doc-dir)         DOC_DIR="$2"; shift 2 ;;
        --with-deps)       WITH_DEPS=1; shift ;;
        --skip-host-check) SKIP_HOST_CHECK=1; shift ;;
        --dry-run)         DRY_RUN=1; shift ;;
        -h|--help)         sed -n '2,24p' "$0"; exit 0 ;;
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
    echo "Installation décalée sous $DESTDIR :"
    echo "  pas de compte système, pas de sysctl, pas de systemd."
else
    SYSCTL_FILE="/etc/sysctl.d/90-wsd.conf"
fi

if [ "$DRY_RUN" != 1 ] && [ "$STAGED" != 1 ] && [ "$(id -u)" != 0 ]; then
    echo "install.sh doit tourner en root (ou avec --dry-run, ou avec DESTDIR=)" >&2
    exit 1
fi

# ── 1. binaires présents ? ────────────────────────────────────────────────────
step "Vérification des binaires"
MISSING=""
for b in wsd wsproxy wsctl; do
    [ -x "$REPO_ROOT/bin/$b" ] || MISSING="$MISSING $b"
done
if [ -n "$MISSING" ]; then
    echo "binaires manquants dans $REPO_ROOT/bin :$MISSING" >&2
    echo "lancer 'make build' d'abord" >&2
    exit 1
fi
info "bin/{wsd,wsproxy,wsctl} présents"
info "wsagent n'est pas installé sur le host : il est intégré au rootfs invité par image/build.sh"

# ── 2. prérequis du host ──────────────────────────────────────────────────────
if [ "$SKIP_HOST_CHECK" = 0 ]; then
    step "Prérequis du host (check-host.sh)"
    if ! bash "$REPO_ROOT/deploy/check-host.sh"; then
        echo
        echo "check-host.sh signale des points KO." >&2
        echo "Corriger, ou relancer avec --skip-host-check pour installer quand même." >&2
        exit 1
    fi
fi

# ── 3. dépendances firecracker / jailer ───────────────────────────────────────
if [ "$WITH_DEPS" = 1 ]; then
    step "firecracker et jailer (versions épinglées)"
    if [ "$DRY_RUN" = 1 ]; then
        info "[dry-run] bash $REPO_ROOT/deploy/fetch-deps.sh --dest $PREFIX/bin --doc-dir $DOC_DIR"
    else
        bash "$REPO_ROOT/deploy/fetch-deps.sh" --dest "$PREFIX/bin" --doc-dir "$DOC_DIR"
    fi
fi

# ── 4. utilisateur et groupe ──────────────────────────────────────────────────
step "Utilisateur et groupe"
if [ "$STAGED" = 1 ]; then
    info "ignoré (installation décalée)"
elif getent group wsd >/dev/null; then
    info "groupe wsd déjà présent"
else
    run groupadd --system wsd
    info "groupe wsd créé"
fi
if [ "$STAGED" = 1 ]; then
    :
elif getent passwd wsproxy >/dev/null; then
    info "utilisateur wsproxy déjà présent"
else
    run useradd --system --gid wsd --no-create-home \
        --home-dir /nonexistent --shell /usr/sbin/nologin wsproxy
    info "utilisateur wsproxy créé (groupe wsd)"
fi

# ── 5. répertoires ────────────────────────────────────────────────────────────
step "Répertoires"
run install -d -m 0755 "$DATA_DIR" "$DATA_DIR/images" "$DATA_DIR/workspaces" "$DATA_DIR/goldens"
run install -d -m 0755 "$JAILER_DIR"
run install -d -m 0750 "$SYSCONFDIR"          # contient les secrets
run install -d -m 0755 "$DOC_DIR"
info "$DATA_DIR/{images,workspaces,goldens}, $JAILER_DIR, $SYSCONFDIR, $DOC_DIR"

# ── 6. binaires ───────────────────────────────────────────────────────────────
step "Binaires"
run install -d -m 0755 "$PREFIX/bin"
for b in wsd wsproxy wsctl; do
    run install -m 0755 "$REPO_ROOT/bin/$b" "$PREFIX/bin/$b"
    info "$PREFIX/bin/$b"
done

# ── 7. configuration ──────────────────────────────────────────────────────────
# Une configuration déjà en place n'est jamais écrasée : la nouvelle version de
# l'exemple est déposée en .new, à comparer à la main.
step "Configuration"
install_config() {
    local src="$1" dst="$2" mode="$3"
    if [ -e "$dst" ]; then
        run install -m "$mode" "$src" "$dst.new"
        info "$dst existe — exemple déposé dans $dst.new (diff à faire)"
        return 1
    fi
    run install -m "$mode" "$src" "$dst"
    info "$dst installé depuis l'exemple"
    return 0
}

FRESH_WSPROXY_CONF=0
install_config "$REPO_ROOT/deploy/wsd.json" "$SYSCONFDIR/wsd.json" 0640 || true
if install_config "$REPO_ROOT/deploy/wsproxy.json" "$SYSCONFDIR/wsproxy.json" 0640; then
    FRESH_WSPROXY_CONF=1
fi

# Secret HMAC des cookies : généré à la première installation seulement.
if [ "$FRESH_WSPROXY_CONF" = 1 ]; then
    if [ "$DRY_RUN" = 1 ]; then
        info "[dry-run] génération du hmac_secret dans $SYSCONFDIR/wsproxy.json"
    else
        secret=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
        sed -i "s/REMPLACER_PAR_SECRET_ALEATOIRE_32_OCTETS/$secret/" \
            "$SYSCONFDIR/wsproxy.json"
        info "hmac_secret généré (32 octets aléatoires)"
    fi
fi

run install -m 0644 "$REPO_ROOT/deploy/versions.env" "$SYSCONFDIR/versions.env"
info "$SYSCONFDIR/versions.env (versions épinglées de référence)"

# Exemples opencode : déposés tels quels, jamais actifs par défaut. opencode
# n'est poussé dans les VM que si wsd.json renseigne opencode.config_template.
run install -m 0644 "$REPO_ROOT/deploy/opencode.json.tmpl.example" \
    "$SYSCONFDIR/opencode.json.tmpl.example"
run install -m 0600 "$REPO_ROOT/deploy/secrets.json.example" \
    "$SYSCONFDIR/secrets.json.example"
info "exemples opencode : $SYSCONFDIR/{opencode.json.tmpl,secrets.json}.example"

# ── 8. sysctl ─────────────────────────────────────────────────────────────────
step "sysctl"
run install -D -m 0644 "$REPO_ROOT/deploy/sysctl-wsd.conf" "$SYSCTL_FILE"
if [ "$STAGED" = 1 ]; then
    info "$SYSCTL_FILE déposé, non appliqué (installation décalée)"
else
    run sysctl --quiet --system
    info "$SYSCTL_FILE appliqué (net.ipv4.ip_forward=1)"
fi

# ── 9. documentation ──────────────────────────────────────────────────────────
step "Documentation"
for d in README.md docs/SPEC.md docs/OPS.md docs/CONFIG.md; do
    [ -f "$REPO_ROOT/$d" ] || continue
    run install -m 0644 "$REPO_ROOT/$d" "$DOC_DIR/$(basename "$d")"
done
info "$DOC_DIR"

# ── 10. unités systemd ────────────────────────────────────────────────────────
step "Unités systemd"
run install -d -m 0755 "$UNIT_DIR"
run install -m 0644 "$REPO_ROOT/deploy/wsd.service"     "$UNIT_DIR/wsd.service"
run install -m 0644 "$REPO_ROOT/deploy/wsproxy.service" "$UNIT_DIR/wsproxy.service"
if [ "$STAGED" != 1 ] && command -v systemctl >/dev/null 2>&1; then
    run systemctl daemon-reload
fi
info "$UNIT_DIR/{wsd,wsproxy}.service"

# ── suite ─────────────────────────────────────────────────────────────────────
cat <<EOF

──────────────────────────────────────────────────────────────────────
Installation terminée. Les services ne sont pas démarrés : la
configuration doit d'abord être adaptée.

  1. Éditer $SYSCONFDIR/wsd.json
       domain, net.egress_iface, egress.allow, vm.*, image_id
  2. Éditer $SYSCONFDIR/wsproxy.json
       domain, oidc (ou laisser issuer vide pour l'auth par token)
  3. Construire l'image invitée et le kernel, puis les déposer :
       make image kernel
       install -d $DATA_DIR/images/<image_id>
       cp build/rootfs.ext4 build/vmlinux $DATA_DIR/images/<image_id>/
  4. Démarrer :
       systemctl enable --now wsd wsproxy
  5. Golden initial, puis test :
       wsctl golden build
       wsctl create test-ws --owner vous@example.com
       wsctl token <id>

Détails dans $DOC_DIR/OPS.md.
──────────────────────────────────────────────────────────────────────
EOF
