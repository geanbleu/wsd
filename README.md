# wsd — workspaces VS Code éphémères sur Firecracker

wsd lance à la demande des microVMs Firecracker. Chacune contient code-server
(VS Code dans le navigateur) et opencode comme harness d'agent. Un workspace
inactif est suspendu sur disque, puis réveillé à la première requête HTTP :
le nombre de workspaces existants est donc limité par le disque, pas par la RAM.

Une VM par workspace, sous jailer, dans son propre netns, avec un egress filtré.
Aucun secret n'est présent dans les images ni dans les snapshots : ils ne
transitent que par vsock, au moment du restore.

```
navigateur ──TLS──> wsproxy ──(unix sock)──> wsd ──> firecracker (jailer) ×N
                       │                              chacun dans netns ws-<id>
                       └── HTTP/WS ──> veth host ──> netns ──DNAT──> invité :8080
```

| Composant | Rôle |
|---|---|
| `wsd` | daemon de contrôle, root, un par host. Source de vérité (SQLite), cycle de vie des VM, réseau, disques. API sur `/run/wsd/wsd.sock`. |
| `wsproxy` | reverse proxy HTTP/WebSocket. Routage par sous-domaine `<id>.ws.<domaine>`, auth (token HMAC ou OIDC), wake-on-request, mesure d'activité. |
| `wsagent` | agent invité, dans la VM, écoute en vsock. Configuration post-restore (horloge, RNG, machine-id, montage de `/workspace`, secrets) et health check. |
| `wsctl` | CLI d'admin, parle à wsd par le socket unix. |

Un workspace est créé par *restore* d'un **golden snapshot** : la VM n'est pas
bootée, elle est restaurée depuis un état mémoire déjà chaud. Le fichier `mem`
du golden est partagé en page cache entre tous les clones grâce à des hardlinks
et au backend mémoire `File` de Firecracker.

## Prérequis

- Linux ≥ 5.10 x86_64, `/dev/kvm`, cgroup v2.
- `/var/lib/wsd` et `/srv/jailer` **sur le même filesystem**, en XFS
  (`reflink=1`) ou btrfs — les hardlinks et les copies reflink en dépendent.
- `iproute2`, `nftables`, `net.ipv4.ip_forward=1`.
- Pour construire l'image invitée : `docker`, `e2fsprogs` ≥ 1.45 (`mkfs.ext4 -d`).
- Pour construire le kernel invité : `make`, `gcc`, `bison`, `flex`, `bc`,
  `libelf-dev`, `libssl-dev`, `pahole`.

Tout vérifier d'un coup :

```bash
bash deploy/check-host.sh
```

## Démarrage rapide

```bash
make build                       # binaires statiques dans bin/
sudo make install INSTALL_FLAGS=--with-deps
```

`install.sh` est idempotent : il crée le groupe `wsd` et l'utilisateur
`wsproxy`, les répertoires, installe les binaires, les unités systemd, le
fichier sysctl, génère le secret HMAC de wsproxy et ne remplace jamais une
configuration déjà en place (il dépose l'exemple en `.new`). `--with-deps`
télécharge en plus `firecracker` et `jailer` aux versions épinglées dans
`deploy/versions.env`, avec vérification SHA256.

Puis, après avoir adapté `/etc/wsd/wsd.json` et `/etc/wsd/wsproxy.json` :

```bash
make image kernel                                   # rootfs.ext4 + vmlinux
install -d /var/lib/wsd/images/debian-bookworm-v1   # = image_id de wsd.json
cp build/rootfs.ext4 build/vmlinux /var/lib/wsd/images/debian-bookworm-v1/

systemctl enable --now wsd wsproxy
wsctl golden build                                  # golden snapshot initial
wsctl create mon-projet --owner moi@example.com
wsctl token <id>                                    # lien d'accès à ouvrir
```

La procédure complète, le dépannage et les opérations courantes sont dans
[`docs/OPS.md`](docs/OPS.md).

## Documentation

| Fichier | Contenu |
|---|---|
| [`docs/SPEC.md`](docs/SPEC.md) | spécification technique : réseau, disques, snapshots, protocole agent, sécurité |
| [`docs/PLAN.md`](docs/PLAN.md) | plan par phases, critères d'acceptation, mesures relevées, limites connues |
| [`docs/CONFIG.md`](docs/CONFIG.md) | référence de tous les champs de `wsd.json`, `wsproxy.json` et `versions.env` |
| [`docs/OPS.md`](docs/OPS.md) | exploitation : installation, mise à jour d'image, sauvegarde, orphelins, dépannage |
| [`CLAUDE.md`](CLAUDE.md) | conventions de code et règles de travail du dépôt |

## Arborescence

```
cmd/wsd/        daemon de contrôle (1 par host, root)
cmd/wsproxy/    reverse proxy HTTP/WebSocket + auth + wake-on-request
cmd/wsagent/    agent invité (dans la VM), écoute en vsock
cmd/wsctl/      CLI d'admin
internal/fc/        client API Firecracker + lancement via jailer
internal/netns/     netns, veth, tap, règles nft
internal/store/     état SQLite, machine à états des workspaces
internal/disk/      copies reflink, création des disques workspace
internal/agentproto/ protocole host↔agent (JSON délimité par lignes)
internal/api/       handlers HTTP de wsd
internal/config/    config du daemon + validation
internal/version/   version de build, injectée par le Makefile
image/          Dockerfile du rootfs invité + build.sh (→ ext4)
kernel/         build.sh + config du kernel invité
deploy/         install/uninstall, unités systemd, exemples de config
test/integration/   tests nécessitant root + KVM (build tag `integration`)
```

## Commandes

```
make build             binaires statiques dans bin/
make test              tests unitaires, sans root
sudo make test-integration   tests d'intégration (root + /dev/kvm)
make lint              go vet, gofmt, staticcheck s'il est installé
make image             rootfs invité dans build/rootfs.ext4
make kernel            kernel invité dans build/vmlinux
sudo make install      installe sur ce host (INSTALL_FLAGS=--with-deps)
sudo make uninstall    désinstalle (UNINSTALL_FLAGS=--purge)
make dist              archive dist/wsd-<version>-linux-amd64.tar.gz
make help              liste les cibles
```

`make dist` produit une archive de déploiement côté host : binaires, `deploy/`
et documentation, avec son `.sha256`. L'image invitée (`rootfs.ext4`,
`vmlinux`), bien plus volumineuse, se construit depuis le dépôt avec
`make image kernel` et se distribue séparément. L'archive contient le même
`deploy/install.sh` que le dépôt :

```bash
tar xzf wsd-<version>-linux-amd64.tar.gz
cd wsd-<version>-linux-amd64
sudo bash deploy/install.sh --with-deps
```

Les binaires affichent leur version :

```bash
wsd -version        # wsd v0.1.0 (go1.26.4 linux/amd64)
```

## État

Les phases 0 à 7 de [`docs/PLAN.md`](docs/PLAN.md) sont implémentées : image et
boot à froid, création et suppression, golden snapshot et restore, proxy et
tokens, suspend/resume avec reaper d'inactivité, opencode et limites de
ressources, OIDC et métriques. Les critères d'acceptation chiffrés (latences,
partage mémoire) restent à mesurer sur un host disposant de KVM — le tableau
« Mesures relevées » de `docs/PLAN.md` est encore vide.

Hors périmètre v1 : multi-host, live migration, GPU, redimensionnement des
disques workspace.
