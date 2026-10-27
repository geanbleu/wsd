# Spécification technique — wsd

## 1. Objectif

- Créer un workspace (VS Code web + opencode) en moins d'1 s, accessible sur `https://<id>.ws.<domaine>`.
- Nombre de workspaces existants limité par le disque. Nombre de workspaces actifs limité par la RAM réellement modifiée.
- Isolation forte : une microVM par workspace, sous jailer, dans son propre netns, avec un egress filtré.
- Aucun secret dans les images ni dans les snapshots.

Hors périmètre v1 : multi-host, live migration, GPU.

## 2. Vue d'ensemble

```
navigateur ──TLS──> wsproxy ──(unix sock)──> wsd ──> firecracker (jailer) ×N
                       │                              chacun dans netns ws-<id>
                       └── HTTP/WS ──> veth host ──> netns ──DNAT──> invité :8080
```

- **wsd** : source de vérité (SQLite), cycle de vie des VM, réseau, disques.
- **wsproxy** : TLS (ou derrière un terminateur TLS), auth, routage par sous-domaine, wake-on-request, mesure d'activité.
- **wsagent** : dans l'invité, en vsock. Il fait la configuration post-restore et le health check.

## 3. Prérequis host

- Linux ≥ 5.10, x86_64, `/dev/kvm`, cgroup v2.
- `/var/lib/wsd` sur XFS (`reflink=1`) ou btrfs. Le filesystem doit être le même que `/srv/jailer`, pour que les hardlinks et les reflinks fonctionnent.
- Binaires `firecracker` et `jailer` de la dernière release stable, versions épinglées dans `deploy/versions.env`.
- `iproute2`, `nftables`.
- `net.ipv4.ip_forward=1`.

## 4. Image invitée

### 4.1 Rootfs (`image/`)

Le Dockerfile part de `debian:stable-slim` et contient :

- `systemd`, `systemd-sysv`, `udev`, `iproute2`, `ca-certificates`, `git`, `curl`, `openssh-client`, `sudo`, `e2fsprogs` ;
- code-server, version épinglée et installée depuis la release GitHub (.deb) ;
- opencode, version épinglée, via sa méthode d'installation officielle (vérifier la doc) ;
- les toolchains utiles (Go au minimum), versions épinglées ;
- un utilisateur `coder` (uid 1000) avec sudo sans mot de passe dans la VM ;
- `wsagent` (binaire statique) dans `/usr/local/bin`.

Unités systemd invitées :

- `wsagent.service` : `After=local-fs.target`.
- `code-server.service` : `User=coder`, avec `code-server --bind-addr 0.0.0.0:8080 --auth none --disable-telemetry --disable-update-check /workspace` (vérifier les flags pour la version épinglée). L'auth est faite par wsproxy, jamais par code-server.
- `opencode.service` : `User=coder`, `opencode serve` sur `127.0.0.1:4096`, avec `WorkingDirectory=/workspace` (vérifier la commande et les flags).
- `/workspace` n'est **pas** dans fstab : c'est wsagent qui le monte après le restore. code-server et opencode ont `ConditionPathIsMountPoint=/workspace` ou sont (re)démarrés par l'agent après le montage. Choisir une approche et la documenter.

Réseau invité statique : `eth0` = `172.16.0.2/30`, passerelle `172.16.0.1`, DNS = `172.16.0.1` (résolveur relayé par le netns, cf. §6) ou un résolveur configuré.

`image/build.sh` enchaîne `docker build`, `docker create`, `docker export`, puis `mkfs.ext4 -d <dir> -L rootfs build/rootfs.ext4 <taille>`. Il ne doit pas avoir besoin de monter en loop. Le hash du contenu identifie l'image.

### 4.2 Kernel (`kernel/`)

Kernel LTS compilé avec la config invitée recommandée par Firecracker
(`resources/guest_configs/microvm-kernel-ci-<arch>-<série>.config` du repo
Firecracker — le nom porte la série du kernel, et seules certaines séries sont
fournies pour une version donnée de Firecracker), plus :

- `CONFIG_VIRTIO_VSOCKETS=y`
- `CONFIG_VIRTIO_BALLOON=y`
- `CONFIG_VMGENID=y`
- ext4, cgroups v2 et ce que systemd requiert.

Ces options sont déjà toutes à `=y` dans la config amont pour la série épinglée,
VMGenID compris : `kernel/build.sh` le vérifie plutôt que de le supposer, et
s'arrête si l'une d'elles n'est pas compilée en dur. Rien ne doit être en
module : la cmdline invitée contient `nomodule` et il n'y a pas d'initramfs.

La sortie est `build/vmlinux`, non compressé.

## 5. Disques et chemins

Arborescence host :

```
/var/lib/wsd/
  images/<image-id>/rootfs.ext4, vmlinux
  golden/<golden-id>/vmstate, mem, rootfs.ext4, workspace-template.ext4
  workspaces/<ws-id>/workspace.ext4      # persistant, à sauvegarder
  workspaces/<ws-id>/suspend/vmstate, mem  # si suspendu
  wsd.db
/srv/jailer/firecracker/<ws-id>/root/    # chroot jailer
```

**Identité des chemins.** Un snapshot enregistre les chemins des drives et du socket vsock tels que Firecracker les voit. Le chroot jailer est identique pour toutes les VM :

- `/vmlinux` (hardlink)
- `/rootfs.ext4` (copie reflink depuis le golden, privée, jetable)
- `/workspace.ext4` (hardlink vers `workspaces/<id>/workspace.ext4`)
- `/mem`, `/vmstate` : hardlink vers le golden, ou vers `suspend/` lors d'un resume
- `/v.sock` : socket vsock

Cette identité rend tous les snapshots restaurables dans n'importe quel chroot sans réécriture de chemin. Si la version de Firecracker permet de surcharger les chemins au load, on garde quand même ce schéma par simplicité.

**Partage mémoire.** Au restore, le fichier `mem` du golden est chargé avec le backend `File`, donc mappé en privé. Le hardlink garantit le même inode : les pages non modifiées sont partagées en page cache entre toutes les VM clonées du même golden. Le fichier golden ne doit **jamais** être modifié en place.

**Disque workspace.** Il a une taille fixe définie par la config (ex. 20 GiB, fichier sparse). Le golden est pris avec un `workspace-template.ext4` déjà formaté à cette même taille. À la création d'un workspace, `wsd` copie le template en reflink. La taille du bloc device est la même partout, donc aucun rescan n'est nécessaire. Le redimensionnement est hors v1.

## 6. Réseau

Pour chaque workspace :

- un netns `ws-<id>` ;
- dans le netns : un tap `tap0` en `172.16.0.1/30`, identique partout et attendu par le snapshot ;
- une paire veth `vh-<id8>` (host) / `vn-<id8>` (netns), sur un /30 alloué dans `10.200.0.0/16` (IPAM dans SQLite) ;
- dans le netns :
  - route par défaut via l'IP veth côté host ;
  - SNAT de `172.16.0.2` vers l'IP veth côté netns ;
  - DNAT de `<ip veth netns>:8080` vers `172.16.0.2:8080` (c'est par là que wsproxy joint code-server) ;
- côté host : masquerade de `10.200.0.0/16` vers l'interface de sortie.

Egress (nft dans le netns, chaîne forward, politique drop) :

- autoriser ESTABLISHED/RELATED ;
- autoriser DNS vers le résolveur configuré ;
- autoriser la liste `egress.allow` de la config (CIDR + ports) : registres, git, endpoint LLM ;
- interdire explicitement `169.254.0.0/16`, `10.200.0.0/16` (les autres workspaces), et les IP du host hors veth.

Firecracker est lancé dans le netns via l'option netns de jailer.

## 7. Firecracker

### 7.1 Configuration VM (golden)

- vCPU : 2 par défaut, mémoire : 2048 MiB par défaut (configurable).
- Drives : `rootfs` (`/rootfs.ext4`, root, rw), `workspace` (`/workspace.ext4`, rw).
- Net : `eth0` sur `tap0`, MAC fixe (identique pour tous les clones, c'est voulu).
- Vsock : `guest_cid` 3, `uds_path` `/v.sock`.
- Balloon : `deflate_on_oom: true`, stats activées (phase 6).
- VMGenID : activé si la version le supporte, pour que le kernel réensemence le RNG au restore.
- Kernel cmdline : `console=ttyS0 reboot=k panic=1 pci=off` + ce que la config requiert.

### 7.2 Golden snapshot (`wsctl golden build <image-id>`)

1. Préparer un chroot et un netns temporaires.
2. Boot à froid.
3. Attendre que l'agent réponde `ready` en vsock, puis que `GET http://<veth>:8080/healthz` de code-server réponde (vérifier l'endpoint de santé de la version).
4. `PATCH /vm {"state":"Paused"}`.
5. `PUT /snapshot/create` (Full) vers `vmstate` et `mem`.
6. Kill, puis déplacer dans `golden/<golden-id>/`. Le golden actif est marqué dans SQLite.

Avant le snapshot, l'agent fait `sync` et vide le page cache invité (`echo 3 > /proc/sys/vm/drop_caches`) pour réduire la taille du fichier mem.

### 7.3 Création d'un workspace

1. Insérer en base (`creating`), allouer le /30 et créer le netns.
2. Créer le chroot : hardlinks et reflinks (§5).
3. Lancer jailer + firecracker (uid/gid dédié par workspace dans une plage configurée, cgroup avec `memory.max`).
4. `PUT /snapshot/load` avec le backend mémoire `File` sur `/mem`, `/vmstate`, `resume_vm: true` (vérifier les noms de champs exacts dans le swagger).
5. Séquence post-restore de l'agent (§8).
6. Passer en `running`.

### 7.4 Suspend / resume

- **Suspend** : `Paused`, snapshot Full vers `workspaces/<id>/suspend/`, kill de firecracker, suppression du chroot (le rootfs privé est conservé en le déplaçant dans `workspaces/<id>/rootfs.ext4`). Le netns est conservé, le state passe à `suspended`.
- **Resume** : chroot recréé avec le rootfs du workspace et `mem`/`vmstate` de `suspend/`, load, séquence post-restore, state `running`.
- Un workspace repris depuis son propre snapshot ne partage plus de pages avec le golden. C'est accepté en v1. Les snapshots Diff et le dirty tracking sont une piste pour plus tard.

### 7.5 Machine à états

`creating → running ⇄ suspending → suspended → resuming → running`. Tout état peut passer à `error` ou `deleting → deleted`. Les transitions passent par une seule fonction qui verrouille par workspace (mutex par id) et persiste avant et après chaque étape. Au démarrage, wsd réconcilie :

- les VM `running` sans process vivant passent en `error` ;
- les netns ou chroots orphelins sont signalés, jamais supprimés automatiquement en v1.

## 8. Protocole host ↔ agent

Transport vsock, port 1024. Côté host, on se connecte au socket unix `/v.sock` du chroot avec le handshake Firecracker (`CONNECT 1024\n` → `OK <port>\n`).

Messages JSON délimités par ligne, requête/réponse avec un `id` :

- `{"op":"ping"}` → `{"ok":true,"ready":bool}`
- `{"op":"post_restore","time_unix_nano":..., "machine_id":"...", "hostname":"...", "files":[{"path":"...","mode":384,"owner":"coder","content_b64":"..."}], "mount_workspace":true, "restart_units":["code-server","opencode"]}`
  - règle l'horloge (`clock_settime`) ;
  - réensemence le RNG en écrivant des octets aléatoires fournis par le host dans `/dev/urandom` (en complément de VMGenID) ;
  - écrit le machine-id et le hostname ;
  - écrit les fichiers (dont la config opencode, cf. §9) ;
  - monte `/dev/vdb` sur `/workspace` (le formate s'il est vierge, chown coder) ;
  - redémarre les unités ;
  - attend qu'elles soient actives.
- `{"op":"prepare_snapshot"}` → sync + drop_caches.
- `{"op":"stats"}` → mémoire et charge, pour le debug.

Après un restore, les connexions vsock existantes sont réinitialisées. L'agent doit relancer son listener si `Accept` échoue. Le host réessaie la connexion avec un backoff court, sur 5 s maximum.

## 9. Configuration opencode

Elle est générée par wsd depuis `wsd.json` (section `opencode`) et poussée par `post_restore` dans `/home/coder/.config/opencode/opencode.json`. Contenu typique : un provider compatible OpenAI pointant vers l'endpoint LLM interne, le modèle par défaut et les permissions. **Le schéma exact doit être vérifié dans la doc opencode de la version épinglée.** Les clés API ne viennent que de la config wsd (ou d'un fichier référencé), jamais de l'image.

L'extension VS Code d'opencode, si elle est utilisée, est préinstallée dans l'image (`code-server --install-extension`).

## 10. API wsd

Elle est exposée sur le socket unix `/run/wsd/wsd.sock`, en mode 0660, groupe `wsd`. Le JSON est utilisé partout.

| Méthode | Chemin | Effet |
|---|---|---|
| POST | `/v1/workspaces` | `{"name","owner"}` → crée depuis le golden actif, renvoie l'objet |
| GET | `/v1/workspaces` | liste (filtre `?owner=`) |
| GET | `/v1/workspaces/{id}` | détail |
| POST | `/v1/workspaces/{id}/suspend` | suspend |
| POST | `/v1/workspaces/{id}/resume` | resume |
| DELETE | `/v1/workspaces/{id}` | supprime (VM, netns, disques ; `?keep_data=1` garde workspace.ext4) |
| POST | `/v1/workspaces/{id}/ensure` | resume si nécessaire, bloque jusqu'à `running`, renvoie `{"upstream":"10.200.x.y:8080"}` |
| POST | `/v1/workspaces/{id}/activity` | met à jour `last_activity` |
| GET | `/v1/golden` · POST `/v1/golden` | liste / construit un golden |
| GET | `/v1/health` | santé du daemon |

Les opérations concurrentes sur un même id sont sérialisées. `ensure` est idempotent et coalescé : plusieurs appels simultanés ne déclenchent qu'un seul resume.

## 11. wsproxy

- Écoute en HTTP (TLS en option avec certificat wildcard, ou derrière HAProxy).
- Extrait `<id>` du sous-domaine `<id>.ws.<domaine>`. Le champ `domain` de la
  config porte le domaine de base : le label `ws` est ajouté par le code, des
  deux côtés (wsproxy pour le routage, wsd pour les liens de `wsctl token`).
- Auth :
  - phase 4 : cookie signé (HMAC) délivré par un token d'accès ;
  - phase 7 : OIDC, avec vérification que `owner` du workspace correspond au `sub`/email.
- Appelle `ensure`, puis `httputil.ReverseProxy` vers l'upstream. Le WebSocket est géré nativement.
- Pendant un resume, la requête attend avec un timeout de 30 s. Si elle vient d'un navigateur, afficher une page « réveil en cours » qui se recharge.
- Activité : à chaque requête et tant qu'une connexion WebSocket a transféré des octets récemment, wsproxy envoie au plus un `activity` toutes les 30 s par workspace.
- Le reaper d'inactivité est dans **wsd** : un ticker suspend les workspaces `running` dont `last_activity` dépasse `idle_timeout` (défaut 30 min).

## 12. Sécurité

- Firecracker toujours sous jailer, avec uid/gid unique par workspace et le seccomp par défaut de Firecracker.
- cgroup v2 par VM : `memory.max` = mémoire VM + marge, `cpu.max` configurable.
- wsd tourne en root (netns, nft) et wsproxy en utilisateur dédié du groupe `wsd`.
- Rien de sensible dans les goldens. Les secrets transitent uniquement par vsock au restore.
- Un workspace ne voit ni les autres, ni le host, ni les IP link-local (§6).
- Logs structurés (`slog`) avec `ws_id` sur toutes les lignes, sans contenu de fichiers poussés.

## 13. Configuration `wsd.json` (exemple)

```json
{
  "data_dir": "/var/lib/wsd",
  "jailer_dir": "/srv/jailer",
  "firecracker_bin": "/usr/local/bin/firecracker",
  "jailer_bin": "/usr/local/bin/jailer",
  "uid_range": [200000, 299999],
  "vm": {"vcpus": 2, "mem_mib": 2048, "workspace_disk_gib": 20},
  "net": {"pool": "10.200.0.0/16", "egress_iface": "eth0", "dns": "1.1.1.1"},
  "egress": {"allow": [{"cidr": "0.0.0.0/0", "ports": [443]}, {"cidr": "192.0.2.10/32", "ports": [8080]}]},
  "idle_timeout": "30m",
  "opencode": {"config_template": "/etc/wsd/opencode.json.tmpl", "secrets_file": "/etc/wsd/secrets.json"},
  "domain": "example.internal"
}
```

## 14. Observabilité

- `GET /v1/metrics` au format texte Prometheus, écrit à la main sans dépendance. Métriques : nombre de workspaces par état, latences create/resume/suspend (histogramme simple), RSS total des process firecracker.
- `wsctl top` : liste des VM avec RSS privé et partagé (lu dans `/proc/<pid>/smaps_rollup`), dernière activité et état.
