# Plan de réalisation — wsd

Chaque phase est livrable seule. On ne passe à la suivante que lorsque les critères d'acceptation passent, preuves à l'appui (sortie de commande, logs, mesures).

## Phase 0 — Squelette

- `go mod init`, arborescence de `CLAUDE.md`, `Makefile` (build, test, lint, image, kernel, test-integration).
- `deploy/versions.env` : versions épinglées de Firecracker, jailer, code-server, opencode, Go et du kernel.
- Script `deploy/check-host.sh` : vérifie `/dev/kvm`, cgroup v2, filesystem reflink sur `/var/lib/wsd` et `/srv/jailer` (même filesystem), présence de `ip`, `nft`, `firecracker`, `jailer`, `ip_forward`.

**Acceptation :** `make build test lint` passe. `check-host.sh` affiche un rapport clair OK/KO par point.

## Phase 1 — Image et boot à froid

- `kernel/build.sh` : compile `build/vmlinux` avec la config invitée Firecracker et les options de SPEC §4.2.
- `image/Dockerfile`, `image/build.sh` : produit `build/rootfs.ext4` (SPEC §4.1), sans montage loop.
- `cmd/wsagent` minimal : écoute en vsock, répond à `ping`.
- `internal/fc` : client HTTP sur socket unix (machine-config, boot-source, drives, network-interfaces, vsock, actions, état VM).
- Script `test/manual/boot.sh` : un netns + tap à la main, boot à froid d'une VM, sans jailer pour ce premier test.

**Acceptation :** depuis le host, `curl` vers code-server dans la VM répond. `ping` vsock vers l'agent répond. Le temps de boot à froid jusqu'à code-server prêt est mesuré et noté ici.

## Phase 2 — wsd : création et suppression (boot à froid)

- `internal/store` : schéma SQLite (workspaces, allocations IP, goldens), machine à états (SPEC §7.5), migrations embarquées (`embed`).
- `internal/netns` : création et suppression netns, veth, tap, routes, NAT, règles nft d'egress depuis `text/template` (SPEC §6).
- `internal/disk` : reflink (`FICLONE` via `x/sys/unix`), erreur explicite si non supporté, création du disque workspace sparse.
- `internal/fc` : lancement sous jailer (uid par workspace, netns, cgroup).
- `internal/api` + `cmd/wsd` : endpoints POST/GET/DELETE `/v1/workspaces`.
- `cmd/wsctl` : `create`, `ls`, `rm`.
- Réconciliation au démarrage de wsd.

**Acceptation (test d'intégration) :**
- créer 3 workspaces, joindre leurs code-server via leurs IP veth, les supprimer ;
- après suppression : plus aucun netns, veth, chroot, process ni règle nft résiduels (le test le vérifie) ;
- depuis l'invité, impossible de joindre un autre workspace, `169.254.169.254` ou le host ; joindre un CIDR autorisé fonctionne.

## Phase 3 — Golden snapshot et restore

- `wsctl golden build` (SPEC §7.2), avec `prepare_snapshot` côté agent.
- Création par restore (SPEC §7.3) avec chemins identiques (SPEC §5).
- Agent : `post_restore` complet (SPEC §8), relance du listener vsock après restore.
- Activation de VMGenID si la version le supporte, sinon documenter l'absence et s'appuyer sur le réensemencement par l'agent.

**Acceptation :**
- création d'un workspace jusqu'à code-server prêt en moins de 1 s (p50 sur 20 créations), mesuré et noté ici ;
- 20 workspaces clonés : `machine-id`, `/proc/sys/kernel/random/boot_id` après réensemencement, et 32 octets lus dans `/dev/urandom` tous différents d'une VM à l'autre ; l'horloge invitée à moins de 1 s du host ;
- mesure via `smaps_rollup` : la part partagée (`Pss` vs `Rss`) montre que les pages du golden sont mutualisées. Chiffres notés ici.

## Phase 4 — wsproxy

- Routage par sous-domaine, appel à `ensure`, `ReverseProxy` avec WebSocket.
- Auth v1 : `wsctl token <id>` génère un lien à usage unique qui pose un cookie HMAC.
- Remontée d'activité (SPEC §11).

**Acceptation :** VS Code s'ouvre dans un navigateur via `https://<id>.ws.<domaine>`, le terminal intégré fonctionne, et il n'est pas accessible sans cookie valide.

## Phase 5 — Suspend / resume

- Suspend et resume (SPEC §7.4), reaper d'inactivité dans wsd.
- `ensure` coalescé, page « réveil en cours » côté proxy.

**Acceptation :**
- un fichier créé dans `/workspace` et un processus lancé dans le terminal survivent à un cycle suspend/resume ;
- après `idle_timeout`, le workspace est suspendu automatiquement et la RAM host libérée ;
- une requête navigateur réveille le workspace ; latence de resume mesurée et notée ici ;
- 10 requêtes simultanées sur un workspace suspendu ne déclenchent qu'un seul resume.

## Phase 6 — opencode et ressources

- Génération de la config opencode depuis le template et les secrets (SPEC §9), poussée au restore.
- Extension VS Code opencode préinstallée si retenue.
- Balloon : configuration et stats. Documenter le gain mesuré.
- cgroup `memory.max` et `cpu.max` par VM.

**Acceptation :**
- dans un workspace neuf, opencode répond en utilisant l'endpoint LLM configuré, sans aucune clé dans l'image ni dans le golden (vérifié par `grep` sur les fichiers golden) ;
- test de charge : N workspaces actifs simultanément (N à déterminer selon la RAM du host). RSS total, page cache partagé et comportement en cas de pression mémoire notés ici.

## Phase 7 — Exploitation

- Auth OIDC dans wsproxy, contrôle `owner`.
- `/v1/metrics`, `wsctl top`.
- Unités systemd (`deploy/`), exemple `wsd.json`, procédure de mise à jour d'image : nouveau golden, les workspaces existants restent sur l'ancien jusqu'à leur prochaine recréation.
- Doc d'exploitation `docs/OPS.md` : sauvegarde des `workspace.ext4`, nettoyage des orphelins, rotation des goldens.

**Acceptation :** déploiement sur un host vierge en suivant uniquement `docs/OPS.md`.

## Pistes hors v1

- Snapshots Diff avec dirty page tracking pour accélérer le suspend.
- Restore via UFFD pour un chargement mémoire paresseux.
- Multi-host : scheduler sur la RAM privée, réplication des goldens.
- Redimensionnement des disques workspace.

## Mesures relevées

| Mesure | Valeur | Date |
|---|---|---|
| Boot à froid jusqu'à code-server prêt | | |
| Création par restore (p50 / p95) | | |
| Resume depuis suspend (p50 / p95) | | |
| RSS privé moyen par workspace actif | | |
| Taille fichier mem golden | | |
