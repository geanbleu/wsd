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
- Activation de VMGenID si la version le supporte, sinon documenter l'absence et s'appuyer sur le réensemencement par l'agent. **Supporté** : `CONFIG_VMGENID=y` dans la config invitée amont de la série 6.18, vérifié par `kernel/build.sh`. Le réensemencement par l'agent reste en place en complément.

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
- Doc d'exploitation : sauvegarde des `workspace.ext4`, nettoyage des orphelins, rotation des goldens (`docs/ADMIN.md`).

**Acceptation :** déploiement sur un host vierge en suivant uniquement `docs/INSTALL.md`.

## Phase 8 — Packaging et documentation

- `README.md` : vue d'ensemble, prérequis, démarrage rapide, carte de la doc.
- `docs/CONFIG.md` : référence de tous les champs de `wsd.json`, `wsproxy.json`
  et `versions.env`, avec défauts et champs requis.
- `deploy/install.sh` et `deploy/uninstall.sh` : idempotents, `--dry-run`,
  `DESTDIR` pour une installation décalée vérifiable sans root. Comptes système,
  répertoires, binaires, configs sans écrasement, secret HMAC généré, sysctl,
  unités systemd.
- `deploy/fetch-deps.sh` : firecracker et jailer aux versions épinglées, SHA256
  vérifié, swagger de la version déposé dans `/usr/share/doc/wsd`.
- `deploy/setup-storage.sh` : prépare le filesystem XFS `reflink=1` qui porte
  `data_dir` et `jailer_dir`, sur périphérique dédié ou image montée en loop,
  avec vérification `FICLONE` réelle, bind mount des chroots et entrées fstab.
  `check-host.sh` teste désormais `FICLONE` au lieu de chercher `reflink=1`
  dans les options de montage, où XFS ne l'expose jamais.
- `make install`, `make uninstall`, `make dist` (archive host + `.sha256`),
  `make fmt`, `make help` ; version de build injectée par ldflags et flag
  `-version` sur les quatre binaires.
- `docs/INSTALL.md` : premier déploiement pas à pas, d'un host vierge au
  premier workspace, avec une vérification par étape et un dépannage par étape.
- `docs/ADMIN.md` : administration courante — cycle de vie, observabilité,
  capacité, images et goldens, sauvegardes, nettoyage, playbooks d'incident,
  routines. Remplace `docs/OPS.md`, découpé en ces deux guides.

**Acceptation :**
- `make build test lint dist` passe ;
- `DESTDIR=<dir> deploy/install.sh --skip-host-check` produit l'arborescence
  attendue, est rejouable sans écraser les configs, et `uninstall.sh --purge`
  remet le répertoire dans l'état initial sans toucher aux données ;
- `check-host.sh` rend OK sur un XFS `reflink=1` et KO sur un filesystem sans
  `FICLONE` (vérifié sur xfs et tmpfs) ;
- déploiement sur un host vierge en suivant uniquement `docs/INSTALL.md`.

## Anomalies

### Ouvertes

1. **Le masquerade host n'est jamais installé.** `netns.SetupHostMasquerade`
   existe mais n'a aucun appelant : la règle de masquerade de `net.pool` vers
   `net.egress_iface` exigée par SPEC §6 n'est donc pas posée, et les VM ne
   joignent rien au-delà de leur veth. À câbler au démarrage de wsd, ou à
   confier à une table nft gérée hors wsd (cf. docs/INSTALL.md § Réseau du
   host) — c'est un choix d'exploitation, wsd touchant alors au pare-feu du
   host.
2. **`net.egress_iface` n'est pas validé** et n'est utilisé que par
   `Teardown` : son nom n'est vérifié à aucun moment. `net.dns` est refusé
   explicitement s'il est vide, mais au moment de la création, pas au démarrage
   de wsd. Les deux devraient être validés dans `config.validate()`.
3. **`wsctl token` produit un lien en `http://`** sans tenir compte de
   `external_scheme` de wsproxy. Derrière un terminateur TLS, le lien doit être
   corrigé à la main.

### Corrigées

Relevées en mettant un premier déploiement en service, dans cet ordre.

| Symptôme | Cause |
|---|---|
| `allocation IP: FOREIGN KEY constraint failed (787)` sur `golden build` | `ip_allocations.workspace_id` référençait `workspaces(id)`, alors que la construction d'un golden alloue un /30 sans ligne de workspace. Contrainte retirée, migration 1 du schéma. |
| `netns setup: nft apply: exit status 1` | le script nft commençait par `flush table`, qui échoue dans le netns neuf d'un workspace. La table est déclarée vide avant d'être vidée. |
| `hardlink … : invalid cross-device link` | `data_dir` et `jailer_dir` sur deux filesystems. Erreur rendue explicite ; le remède est côté configuration. |
| `Permission denied (os error 13) /rootfs.ext4` | jailer ne chown que les répertoires de sa hiérarchie : les drives en lecture-écriture doivent être donnés à l'uid de la VM, les fichiers partagés rester à root en 0644. |
| `agent non disponible après 60 s (ping recv: EOF)` | `net.FileConn` ne sait pas envelopper un descripteur `AF_VSOCK` : l'agent fermait la connexion aussitôt acceptée. `os.NewFile` à la place. |
| `code-server non disponible après 30 s` | le sondage était lancé dans le netns vers l'IP du netns, que la règle DNAT ne couvre pas. Il part désormais du host, par le chemin de wsproxy. |
| workspace annoncé `running` puis mort dans la seconde | `fc.Launch` recevait le contexte de la requête HTTP et le passait à `exec.CommandContext` : Go tuait la VM dès que le handler répondait. La durée de vie d'une VM est gérée par son PID, le contexte ne borne plus que l'attente du socket API. |
| `--owner` et `--keep-data` ignorés | `flag.Parse` s'arrête au premier argument non-flag ; `wsctl rm <id> --keep-data` supprimait donc le disque. |
| console invitée perdue | la sortie du process firecracker n'était pas redirigée, alors que la cmdline contient `console=ttyS0` : un invité qui ne démarrait pas ne laissait aucune trace. Capturée dans `<chroot>/run/console.log`. |

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
