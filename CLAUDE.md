# wsd — workspaces VS Code éphémères sur Firecracker

Plateforme qui lance à la demande des microVMs Firecracker. Chacune contient code-server (VS Code web) et opencode comme harness d'agent. Un workspace inactif est suspendu sur disque, puis réveillé à la première requête. L'objectif est un nombre de workspaces limité par le disque, pas par la RAM.

La spec complète est dans `docs/SPEC.md` et le plan par phases dans `docs/PLAN.md`. **Lis les deux avant toute tâche.** Travaille toujours dans le cadre d'une phase du plan. `README.md` donne la vue d'ensemble, `docs/INSTALL.md` le premier déploiement, `docs/ADMIN.md` l'administration courante et `docs/CONFIG.md` la référence de configuration.

## Conventions

- Go, dernière version stable. `CGO_ENABLED=0` partout : tous les binaires sont statiques.
- **Stdlib d'abord.**
  - Routage HTTP : `net/http`, avec les patterns de `ServeMux` de Go ≥ 1.22.
  - Logs : `log/slog`.
  - Proxy : `net/http/httputil`.
  - API Firecracker : un client HTTP maison sur socket unix (`internal/fc`), **pas** firecracker-go-sdk.
- Dépendances autorisées : `golang.org/x/sys` (vsock, syscalls), `modernc.org/sqlite` (SQLite pur Go), `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` (phase auth uniquement). Toute autre dépendance doit être justifiée et me demander validation.
- Opérations réseau host (netns, veth, tap, nftables) : appels à `ip` et `nft` via `os/exec`. Les scripts nft sont générés depuis des `text/template` puis appliqués avec `nft -f`. Pas de lib netlink.
- Erreurs wrappées avec `%w` et contexte. Pas de panic hors `main`.
- Config du daemon en JSON (`/etc/wsd/wsd.json`), décodée dans une struct avec validation explicite.

## Arborescence

```
cmd/wsd/        daemon de contrôle (1 par host, root)
cmd/wsproxy/    reverse proxy HTTP/WebSocket + auth + wake-on-request
cmd/wsagent/    agent invité (dans la VM), écoute en vsock
cmd/wsctl/      CLI d'admin (parle à wsd via socket unix)
internal/fc/        client API Firecracker + lancement via jailer
internal/netns/     création/suppression netns, veth, tap, règles nft
internal/store/     état SQLite, machine à états des workspaces
internal/disk/      copies reflink, création des disques workspace
internal/agentproto/ protocole host<->agent (JSON délimité par lignes)
internal/api/       handlers HTTP de wsd
internal/config/    config du daemon + validation explicite
internal/version/   version de build, injectée par le Makefile
image/          Dockerfile du rootfs invité + build.sh (→ ext4)
kernel/         build.sh + config du kernel invité
deploy/         install/uninstall, fetch-deps, unités systemd, exemples de config
test/integration/   tests nécessitant root + KVM (build tag `integration`)
```

## Commandes

- `make build` : tous les binaires dans `bin/`.
- `make test` : tests unitaires, sans root.
- `sudo make test-integration` : tests d'intégration, avec root et `/dev/kvm`.
- `make image` : rootfs ext4 dans `build/rootfs.ext4`.
- `make kernel` : `build/vmlinux`.
- `make lint` : `go vet`, `gofmt -l` + `staticcheck` s'il est installé.
- `make install` / `make uninstall` : `deploy/install.sh` / `deploy/uninstall.sh` sur le host courant (root).
- `make dist` : archive de déploiement host dans `dist/`.
- `make help` : liste les cibles.

## Règles de travail

- **Vérifie la doc amont avant d'implémenter** tout ce qui touche :
  - l'API Firecracker (snapshot create/load, vsock, balloon, VMGenID) ;
  - les flags de jailer ;
  - les flags de code-server ;
  - le schéma de config d'opencode.

  Ces points évoluent. N'invente pas de champs. En cas de doute, ouvre le swagger Firecracker de la version utilisée (`src/firecracker/swagger/firecracker.yaml`).
- Toute commande qui modifie le host (netns, nft, montage, suppression dans `/var/lib/wsd`, `/srv/jailer`) : demande-moi avant de l'exécuter en dehors des tests d'intégration.
- Chaque phase se termine quand ses critères d'acceptation (`docs/PLAN.md`) passent. Montre la sortie qui le prouve.
- Pas de code mort, pas de TODO laissés sans issue correspondante dans `docs/PLAN.md`.
- Commits petits, un par étape logique, messages en français à l'impératif.
