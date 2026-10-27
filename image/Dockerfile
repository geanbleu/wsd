FROM debian:stable-slim

# ── versions ──────────────────────────────────────────────────────────────────
ARG CODE_SERVER_VERSION=v4.123.0
ARG OPENCODE_VERSION=v1.19.4
ARG GO_VERSION=1.26.3

# ── paquets système ───────────────────────────────────────────────────────────
RUN apt-get update && apt-get install -y --no-install-recommends \
    systemd systemd-sysv udev \
    iproute2 ca-certificates git curl openssh-client sudo e2fsprogs \
    && rm -rf /var/lib/apt/lists/*

# Activer systemd-networkd pour la config réseau statique
RUN systemctl enable systemd-networkd

# ── réseau statique ───────────────────────────────────────────────────────────
COPY guest/10-eth0.network /etc/systemd/network/10-eth0.network

# ── utilisateur coder ─────────────────────────────────────────────────────────
RUN useradd -m -u 1000 -s /bin/bash coder \
    && echo "coder ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/coder \
    && chmod 440 /etc/sudoers.d/coder \
    && mkdir /workspace

# ── Go toolchain ──────────────────────────────────────────────────────────────
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# ── code-server ───────────────────────────────────────────────────────────────
# Installation depuis le .deb GitHub — vérifier les flags à la mise à jour :
#   code-server --help | grep -E 'disable-telemetry|disable-update-check|auth'
RUN curl -fsSLo /tmp/code-server.deb \
    "https://github.com/coder/code-server/releases/download/${CODE_SERVER_VERSION}/code-server_${CODE_SERVER_VERSION#v}_amd64.deb" \
    && dpkg -i /tmp/code-server.deb \
    && rm /tmp/code-server.deb

# ── opencode ──────────────────────────────────────────────────────────────────
# anomalyco/opencode publie une archive contenant un unique exécutable
# « opencode » (~215 Mio décompressé). Vérifier le nom de l'asset à chaque
# montée de version : utiliser -musl pour une base Alpine, -baseline pour un
# CPU sans AVX2.
RUN curl -fsSL \
    "https://github.com/anomalyco/opencode/releases/download/${OPENCODE_VERSION}/opencode-linux-x64.tar.gz" \
    | tar -xz -C /usr/local/bin opencode \
    && chmod 0755 /usr/local/bin/opencode \
    && /usr/local/bin/opencode --version

# ── wsagent ───────────────────────────────────────────────────────────────────
COPY wsagent /usr/local/bin/wsagent

# ── unités systemd ────────────────────────────────────────────────────────────
COPY guest/wsagent.service     /etc/systemd/system/wsagent.service
COPY guest/code-server.service /etc/systemd/system/code-server.service
COPY guest/opencode.service    /etc/systemd/system/opencode.service

# wsagent démarre automatiquement ; code-server et opencode sont lancés par wsagent
RUN systemctl enable wsagent

# Désactiver les services inutiles dans une microVM
RUN systemctl disable \
    apt-daily.timer apt-daily-upgrade.timer \
    motd-news.timer \
    2>/dev/null || true

# ── machine-id et hostname ─────────────────────────────────────────────────────
# Vidés ici : wsagent les réécrit au restore avec des valeurs uniques par VM.
RUN truncate -s 0 /etc/machine-id && truncate -s 0 /etc/hostname
