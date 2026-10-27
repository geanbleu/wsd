# Administration courante — wsd

Guide d'exploitation d'un déploiement en place. Pour l'installation initiale,
voir [`INSTALL.md`](INSTALL.md) ; pour la référence des champs de configuration,
[`CONFIG.md`](CONFIG.md) ; pour l'architecture, [`SPEC.md`](SPEC.md).

Toutes les commandes `wsctl` passent par le socket unix `/run/wsd/wsd.sock`, en
0660 groupe `wsd` : il faut être root ou membre de ce groupe.

---

## Les commandes du quotidien

```bash
wsctl ls                              # inventaire
wsctl top                             # état et mémoire en direct
wsctl create <nom> --owner <email>    # créer
wsctl token <id>                      # lien d'accès à usage unique
wsctl rm <id> [--keep-data]           # supprimer
wsctl suspend <id> / resume <id>      # forcer un cycle
wsctl golden build / golden get       # golden snapshot
wsctl get <id>                        # détail JSON d'un workspace
```

`wsctl -version` et `wsctl -h` donnent la version installée et la liste des
commandes.

---

## Cycle de vie d'un workspace

```
creating ──→ running ⇄ suspending ──→ suspended ──→ resuming ──→ running
   │            │           │             │             │
   └────────────┴───────────┴─────────────┴─────────────┴──→ error
                                                              │
                              n'importe quel état ──→ deleting ──→ deleted
```

Les transitions autorisées sont codées dans `internal/store` et chaque
changement est sérialisé par un verrou propre au workspace. Une transition
invalide est refusée, elle n'est jamais forcée.

| État | Signification | Qui l'a déclenché |
|---|---|---|
| `creating` | restore depuis le golden en cours | `wsctl create`, API `POST /v1/workspaces` |
| `running` | VM vivante, code-server joignable | fin de création ou de resume |
| `suspending` | snapshot en cours d'écriture | reaper d'inactivité, ou `wsctl suspend` |
| `suspended` | VM arrêtée, état sur disque, netns conservé | fin du suspend |
| `resuming` | restore depuis le snapshot du workspace | première requête HTTP, ou `wsctl resume` |
| `error` | incohérence détectée | échec d'une étape, ou réconciliation au démarrage |
| `deleted` | ressources libérées | `wsctl rm` |

**Le reaper d'inactivité** est dans wsd : un ticker parcourt chaque minute les
workspaces `running` et suspend ceux dont `last_activity` dépasse
`idle_timeout` (30 min par défaut).

**L'activité** est remontée par wsproxy, au plus une fois toutes les 30 s par
workspace, à chaque requête HTTP et tant qu'une WebSocket transfère des octets.
Un onglet VS Code ouvert suffit donc à garder un workspace éveillé.

**Le réveil à la requête** : wsproxy appelle `ensure`, qui déclenche un resume
si nécessaire et attend que le workspace soit `running`, jusqu'à 30 s, en
sondant toutes les 200 ms. Les appels simultanés sont coalescés : dix requêtes
sur un workspace suspendu ne déclenchent qu'un seul resume. Au-delà du délai, le
navigateur reçoit une page « réveil en cours » qui se recharge.

**La réconciliation au démarrage** : wsd passe en `error` les workspaces
`running`, `creating`, `resuming` ou `suspending` dont le process firecracker
n'existe plus. Les netns et chroots orphelins sont signalés mais **jamais
supprimés automatiquement** en v1.

---

## Opérations sur les workspaces

### Créer

```bash
wsctl create mon-projet --owner alice@example.com
```

La création se fait par restore du golden actif. S'il n'y en a pas, wsd retombe
sur un boot à froid : cela fonctionne mais prend des secondes au lieu d'une
fraction de seconde. `wsctl golden get` dit s'il y a un golden actif.

L'`owner` sert au filtrage (`wsctl ls --owner`) et, quand OIDC est actif, au
contrôle d'accès : il est comparé à l'identité du fournisseur.

### Donner l'accès

```bash
wsctl token <id>
# http://<id>.ws.example.internal/?tkn=…
```

Le token est **à usage unique** — il est supprimé de la base dès qu'il est
échangé — et **sans date d'expiration** : un lien non utilisé reste valable.
L'échange pose un cookie signé en HMAC, valable `cookie_ttl` (24 h par défaut).
Le lien est produit en `http://` sans tenir compte du terminateur TLS : derrière
HTTPS, corriger le schéma à la main.

Révoquer l'accès de tout le monde d'un coup se fait en changeant
`hmac_secret` dans `wsproxy.json` puis en redémarrant wsproxy : tous les cookies
émis deviennent invalides. Il n'y a pas de révocation par workspace en v1.

### Suspendre et réveiller

```bash
wsctl suspend <id>     # snapshot, kill de la VM, RAM host libérée
wsctl resume  <id>     # restore depuis le snapshot du workspace
```

Un workspace repris depuis son propre snapshot **ne partage plus de pages avec
le golden** : son empreinte mémoire devient privée. C'est accepté en v1. Un
cycle suspend/resume est donc moins économe qu'une création fraîche, du point de
vue de la RAM.

### Supprimer

```bash
wsctl rm <id>                # VM, netns, chroot, disques, allocation IP
wsctl rm <id> --keep-data    # garde /var/lib/wsd/workspaces/<id>/workspace.ext4
```

La ligne reste en base avec l'état `deleted` ; elle n'apparaît plus dans
`wsctl ls` ni dans les métriques. Ces lignes s'accumulent sans conséquence
pratique, leur nettoyage est manuel.

Avec `--keep-data`, le disque de l'utilisateur survit : utile avant une
recréation sur un nouveau golden, mais l'association au nouveau workspace n'est
pas automatique en v1 — le fichier doit être remis en place à la main.

---

## Observabilité

### État et mémoire

```bash
wsctl top
```

```
ID                                    NOM        ÉTAT      RSS(MiB)  PSS(MiB)  DERNIÈRE ACTIVITÉ
f3a1…                                 projet-a   running        412       198  il y a 1m12s
```

`RSS` est la résidence totale du process firecracker, `PSS` sa part
proportionnelle — lue dans `/proc/<pid>/smaps_rollup`. **C'est la colonne PSS
qui compte** pour estimer combien de workspaces tiennent sur le host : la
différence entre RSS et PSS mesure ce qui est mutualisé avec les autres clones
du même golden. Un PSS très proche du RSS signale un workspace qui ne partage
plus rien — typiquement repris depuis son propre snapshot.

### Métriques Prometheus

```bash
curl -s --unix-socket /run/wsd/wsd.sock http://wsd/v1/metrics
```

| Métrique | Type | Contenu |
|---|---|---|
| `wsd_workspaces_total{state}` | gauge | nombre de workspaces par état (`creating`, `running`, `suspending`, `suspended`, `resuming`, `error`) |
| `wsd_firecracker_rss_kib` | gauge | somme des RSS des process firecracker, en Kio |
| `wsd_create_duration_seconds` | histogram | latence de création |
| `wsd_resume_duration_seconds` | histogram | latence de resume |
| `wsd_suspend_duration_seconds` | histogram | latence de suspend |

Bornes des histogrammes : 0,1 / 0,5 / 1 / 2 / 5 / 10 / 30 / 60 s. Ils sont
**en mémoire** et repartent de zéro à chaque redémarrage de wsd : les lire comme
des compteurs cumulatifs depuis le dernier démarrage, pas depuis toujours.

L'API n'étant exposée que sur un socket unix, un scrape Prometheus passe par un
exportateur local capable de lire ce socket, ou par un relais que vous placez
devant.

### Journaux

```bash
journalctl -u wsd -f
journalctl -u wsproxy -f
journalctl -u wsd --since "1 hour ago" | grep ws_id=f3a1
```

Les logs sont structurés (`log/slog`, format texte). Le champ `ws_id` est présent
sur toutes les lignes liées à un workspace, ce qui permet de suivre un cycle de
vie complet. Le contenu des fichiers poussés dans les VM n'est jamais journalisé.

Chaque VM laisse deux journaux dans son chroot :

```bash
# journal de Firecracker lui-même (API, devices, snapshots)
cat /srv/jailer/firecracker/<id>/root/run/firecracker.log

# console de l'invité : noyau, systemd, unités qui démarrent ou échouent
cat /srv/jailer/firecracker/<id>/root/run/console.log
```

`console.log` est la seule trace d'un invité qui ne démarre pas, ou qui démarre
sans que wsagent réponde. wsd en joint les dernières lignes aux erreurs de
lancement, mais le fichier complet est toujours là — tant que le chroot
existe. Un chroot est supprimé au suspend et à la suppression du workspace,
jamais après un échec : c'est précisément le cas où on en a besoin.

### Santé

```bash
curl -s --unix-socket /run/wsd/wsd.sock http://wsd/v1/health   # {"status":"ok"}
systemctl is-active wsd wsproxy
```

---

## Capacité

Quatre ressources peuvent saturer. Aucune n'a d'alerte intégrée en v1 : c'est à
surveiller depuis l'extérieur.

| Ressource | Comment la lire | Quand s'inquiéter |
|---|---|---|
| **Disque** | `df -h /var/lib/wsd` | les disques workspace sont creux et croissent avec l'usage ; chaque workspace suspendu ajoute un `mem` de la taille de la RAM de la VM |
| **RAM** | `wsctl top`, colonne PSS ; `free -h` | la somme des PSS approche la RAM disponible. Le page cache partagé du golden ne doit pas être compté deux fois |
| **Pool IP** | `sqlite3 /var/lib/wsd/wsd.db "SELECT count(*) FROM ip_allocations"` | un /16 donne 16 384 blocs /30 : la limite réelle sera atteinte ailleurs avant |
| **uid jailer** | `uid_range` dans `wsd.json` | un uid par bloc IP : la plage doit être au moins aussi large que le nombre de workspaces visés |

Un pool épuisé se manifeste par `pool d'IP épuisé` à la création. Des
allocations peuvent subsister si des workspaces ont été supprimés hors de
`wsctl rm` :

```bash
# allocations sans workspace actif correspondant
sqlite3 /var/lib/wsd/wsd.db "
  SELECT a.workspace_id, a.block_idx FROM ip_allocations a
  LEFT JOIN workspaces w ON w.id = a.workspace_id
  WHERE w.id IS NULL OR w.state = 'deleted';"
```

---

## Images et goldens

### Le cycle

`image` (rootfs.ext4 + vmlinux) → `golden` (vmstate + mem + rootfs + template de
disque workspace) → `workspace` (clone reflink du golden). Un workspace déjà créé
ne suit pas les mises à jour : il reste sur le golden qui l'a produit jusqu'à sa
recréation.

### Mettre à jour l'image

```bash
# [machine de build]
make image kernel

# [host] déposer sous un NOUVEL image_id
sudo install -d /var/lib/wsd/images/debian-bookworm-v2
sudo cp build/rootfs.ext4 build/vmlinux /var/lib/wsd/images/debian-bookworm-v2/

# pointer wsd dessus
sudo vi /etc/wsd/wsd.json          # "image_id": "debian-bookworm-v2"
sudo systemctl restart wsd
journalctl -u wsd -n 5 | grep "image invitée"

# nouveau golden, construit sur la nouvelle image
wsctl golden build
wsctl golden get
```

Garder l'ancien `image_id` sur disque jusqu'à ce que le nouveau golden soit
validé : revenir en arrière n'est alors qu'un changement de champ et un
redémarrage.

### Migrer les workspaces existants

Il n'y a pas de migration en place en v1. Pour chaque workspace :

```bash
wsctl ls
wsctl rm <id> --keep-data
wsctl create <même-nom> --owner <même-owner>
# puis remettre workspace.ext4 en place à la main si les données doivent suivre
```

À planifier avec les utilisateurs : c'est une interruption, et le disque de
travail ne suit pas automatiquement.

### Rotation des goldens

Les anciens goldens ne sont pas supprimés automatiquement. Une fois tous les
workspaces migrés :

```bash
wsctl golden get                       # identifier le golden actif
ls -lh /var/lib/wsd/goldens/

# vérifier qu'aucun workspace ne référence celui qu'on veut retirer
sqlite3 /var/lib/wsd/wsd.db \
  "SELECT id,name,state,golden_id FROM workspaces WHERE state != 'deleted';"

sudo rm -rf /var/lib/wsd/goldens/<ancien-id>
sqlite3 /var/lib/wsd/wsd.db "DELETE FROM goldens WHERE id='<ancien-id>'"
```

Ne jamais supprimer le répertoire d'un golden encore référencé par un workspace
suspendu : son resume échouerait. Et ne **jamais** modifier un fichier `mem` de
golden en place — c'est l'inode partagé par tous les clones.

Les fichiers d'un golden doivent rester `root:root` en 0644 : ils sont
hardlinkés dans le chroot de chaque VM clonée, qui tourne sous un uid dédié, et
un `mem` qui ne serait plus lisible par tous ferait échouer les restores avec
`Permission denied`. wsd les normalise à la construction ; ne pas les chown
après coup.

---

## Sauvegarde et restauration

### Ce qu'il faut sauvegarder

| Chemin | Contenu | Criticité |
|---|---|---|
| `/var/lib/wsd/workspaces/<id>/workspace.ext4` | données utilisateur | **irremplaçable** |
| `/var/lib/wsd/wsd.db` | état, IPAM, goldens, tokens | reconstruisible à grand-peine |
| `/etc/wsd/` | configuration, `hmac_secret`, secrets opencode | irremplaçable (secrets) |
| `/var/lib/wsd/goldens/` | snapshots | reconstruisible avec `golden build` |
| `/var/lib/wsd/images/` | rootfs et kernel | reconstruisible avec `make image kernel` |

### Sauvegarder

Le plus propre est un snapshot au niveau du filesystem (LVM, ou `btrfs send` si
le stockage est en btrfs) : il est cohérent et n'interrompt rien.

À défaut, par workspace, **sur un workspace suspendu** — copier un
`workspace.ext4` monté dans une VM vivante donne un filesystem incohérent.
Noter que ce fichier appartient à l'uid jailé de sa VM, pas à root : la
sauvegarde et la restauration se font donc en root, et une restauration doit
préserver ou rétablir ce propriétaire (wsd le repose de toute façon au
démarrage suivant de la VM).

```bash
wsctl suspend <id>
sudo cp --reflink=auto /var/lib/wsd/workspaces/<id>/workspace.ext4 \
     /backup/<id>-$(date +%Y%m%d).ext4
```

La base, à l'arrêt ou avec l'API SQLite de sauvegarde :

```bash
sqlite3 /var/lib/wsd/wsd.db ".backup /backup/wsd-$(date +%Y%m%d).db"
sudo tar czf /backup/etc-wsd-$(date +%Y%m%d).tar.gz -C /etc wsd
```

### Restaurer

```bash
sudo systemctl stop wsproxy wsd
sudo cp /backup/wsd-20261027.db /var/lib/wsd/wsd.db
sudo tar xzf /backup/etc-wsd-20261027.tar.gz -C /etc
sudo cp /backup/<id>-20261027.ext4 /var/lib/wsd/workspaces/<id>/workspace.ext4
sudo systemctl start wsd wsproxy
journalctl -u wsd -n 30
```

Après restauration d'une base plus ancienne que l'état réel du host, la
réconciliation signalera des incohérences : des workspaces passeront en `error`,
et des netns ou chroots deviendront orphelins. Les traiter avec la section
Nettoyage.

---

## Mettre à jour les binaires

```bash
make build                                      # ou extraire la nouvelle archive
sudo bash deploy/install.sh --skip-host-check
sudo systemctl restart wsd wsproxy
wsd -version && wsproxy -version && wsctl -version
```

`install.sh` ne remplace jamais une configuration en place : il dépose la
nouvelle version de l'exemple en `.new`. Après une mise à jour, comparer :

```bash
diff /etc/wsd/wsd.json /etc/wsd/wsd.json.new
```

Les workspaces en cours ne sont pas interrompus : `wsd.service` est en
`KillMode=process`, les firecracker tournent dans leurs propres cgroups, et wsd
réconcilie son état au redémarrage.

### Migrations de schéma

Le schéma de `wsd.db` est migré automatiquement au démarrage de wsd, dans une
transaction par migration, avec un `foreign_key_check` ensuite. La version
appliquée est suivie dans `PRAGMA user_version`.

```bash
sudo systemctl stop wsd
sqlite3 /var/lib/wsd/wsd.db ".backup /backup/wsd-avant-maj.db"
# installer, puis
sudo systemctl start wsd
sqlite3 /var/lib/wsd/wsd.db "PRAGMA user_version"
```

### Mettre à jour firecracker

Changer `FIRECRACKER_VERSION` et `JAILER_VERSION` dans `deploy/versions.env` —
les deux doivent rester identiques — puis :

```bash
sudo bash deploy/fetch-deps.sh
```

Un changement de version de Firecracker invalide potentiellement les snapshots
existants : **reconstruire le golden** et considérer les workspaces suspendus
comme non reprenables. Les suspendre, les réveiller et les laisser en `running`
avant la mise à jour, ou accepter de les recréer.

---

## Désinstaller

```bash
sudo bash deploy/uninstall.sh              # services, unités, binaires, doc
sudo bash deploy/uninstall.sh --purge      # + /etc/wsd, compte wsproxy, groupe wsd
sudo bash deploy/uninstall.sh --purge-data # + disques workspace, goldens, chroots
```

Par défaut, la configuration, les données et le compte système sont conservés.
`--purge-data` détruit les données utilisateur et demande une confirmation
explicite.

Supprimer les workspaces avec `wsctl rm` **avant** de désinstaller : sinon
netns, veth, règles nftables et chroots restent sur le host. Le script le
signale et demande confirmation. `firecracker` et `jailer` ne sont pas touchés,
ni le montage préparé par `setup-storage.sh`, ni la règle `wsd-host` posée à la
main.

---

## Nettoyage

### Workspaces en erreur

```bash
wsctl ls | grep error
wsctl get <id>                  # comprendre avant de supprimer
wsctl rm <id> --keep-data       # garde le disque de l'utilisateur
```

### Ressources orphelines

wsd ne supprime jamais automatiquement ce qu'il ne reconnaît plus, et **une
création qui échoue ne libère rien** : le netns, le chroot jailer et l'allocation
IP restent en place, le workspace passant simplement en `error`. Un `wsctl rm
<id>` sur ces workspaces libère tout — c'est le premier geste après une série de
créations en échec, typiquement pendant la mise au point d'un déploiement.

L'inventaire croisé se fait à la main :

```bash
# ce que wsd connaît
wsctl ls

# ce qui existe sur le host
ip netns list | grep '^ws-'
ls /srv/jailer/firecracker/
ls /var/lib/wsd/workspaces/
sudo nft list tables | grep 'wsd-'
pgrep -a -x firecracker
```

Tout ce qui apparaît côté host sans workspace actif correspondant est orphelin.
Suppression, en adaptant l'identifiant court (8 premiers caractères de l'id pour
les netns et les tables nft) :

```bash
sudo ip netns del ws-<id8>           # supprime aussi veth et tap du netns
sudo ip link del vh-<id8>            # le bout host, s'il subsiste
sudo nft delete table ip wsd-<id8>
sudo rm -rf /srv/jailer/firecracker/<id>
# le disque de l'utilisateur, seulement si on est sûr :
sudo rm -rf /var/lib/wsd/workspaces/<id>
```

Puis libérer l'allocation IP restée en base :

```bash
sqlite3 /var/lib/wsd/wsd.db "DELETE FROM ip_allocations WHERE workspace_id='<id>'"
```

### Tokens et lignes supprimées

```bash
sqlite3 /var/lib/wsd/wsd.db "SELECT count(*) FROM tokens"      # non utilisés
sqlite3 /var/lib/wsd/wsd.db "DELETE FROM tokens WHERE created_at < strftime('%s','now','-7 days')"
sqlite3 /var/lib/wsd/wsd.db "SELECT count(*) FROM workspaces WHERE state='deleted'"
```

Les tokens n'expirant pas, les purger périodiquement est une bonne hygiène.

---

## La base de données

```bash
sqlite3 /var/lib/wsd/wsd.db ".schema"
sqlite3 /var/lib/wsd/wsd.db ".tables"      # workspaces ip_allocations goldens tokens
```

Requêtes de lecture utiles :

```bash
# inventaire complet avec dates lisibles
sqlite3 -header -column /var/lib/wsd/wsd.db "
  SELECT substr(id,1,8) AS id, name, owner, state,
         datetime(created_at,'unixepoch','localtime')    AS cree,
         datetime(last_activity,'unixepoch','localtime') AS activite
  FROM workspaces WHERE state != 'deleted' ORDER BY last_activity DESC;"

# workspaces inactifs depuis plus de 24 h
sqlite3 /var/lib/wsd/wsd.db "
  SELECT substr(id,1,8), name, state FROM workspaces
  WHERE state != 'deleted' AND last_activity < strftime('%s','now','-1 day');"

# correspondance workspace → IP
sqlite3 -header -column /var/lib/wsd/wsd.db "
  SELECT substr(w.id,1,8) AS id, w.name, a.block_idx, a.host_ip, a.ns_ip
  FROM workspaces w JOIN ip_allocations a ON a.workspace_id = w.id
  WHERE w.state != 'deleted';"
```

**Règle** : lire librement, écrire seulement wsd arrêté. wsd est la source de
vérité et sérialise ses transitions ; une écriture concurrente depuis `sqlite3`
peut contredire l'état réel du host. La base est en WAL, donc la lecture ne
bloque jamais wsd.

---

## Playbooks d'incident

### Un workspace est en `error`

1. `wsctl get <id>` pour l'état et le PID enregistré.
2. `journalctl -u wsd | grep ws_id=<id>` pour la cause d'origine.
3. `cat /srv/jailer/firecracker/<id>/root/run/firecracker.log` si la VM avait
   démarré.
4. Un workspace `error` ne peut aller que vers `deleting` : il n'est pas
   réparable en place. `wsctl rm <id> --keep-data`, puis recréer.

### Le navigateur renvoie 404 « workspace introuvable »

Le `Host` reçu par wsproxy ne finit pas par `.ws.<domain>`. Vérifier que
`domain` est identique dans `wsd.json` et `wsproxy.json`, **sans** préfixe
`ws.`, et que le terminateur TLS transmet l'en-tête `Host` intact.

### 403 « Non autorisé »

Pas de cookie valide. Émettre un lien avec `wsctl token <id>`. Si OIDC est
actif, vérifier dans `journalctl -u wsproxy` que le `redirect_uri` attendu est
bien déclaré chez le fournisseur, et que l'`owner` du workspace correspond à
l'identité renvoyée.

### 503, ou la page « réveil en cours » qui ne passe pas

Le resume n'aboutit pas en 30 s.

```bash
wsctl get <id>          # bloqué en resuming ?
wsctl top               # RSS qui monte ?
journalctl -u wsd -n 50 | grep ws_id=<id>
ls -lh /var/lib/wsd/workspaces/<id>/suspend/     # mem et vmstate présents ?
```

Si `suspend/` est incomplet, le snapshot est inutilisable : `wsctl rm --keep-data`
puis recréer.

### `wsagent: agent non disponible après 60 s`

La VM a été lancée mais l'agent invité ne répond pas en vsock. L'erreur porte
la dernière erreur de connexion et la fin de la console, ce qui sépare les trois
causes :

| Dernière erreur | Cause | Piste |
|---|---|---|
| `no such file or directory` sur `v.sock` | Firecracker n'a pas créé le socket vsock | `firecracker.log` ; vérifier que `PUT /vsock` a bien été accepté |
| `connection refused` au handshake | l'invité tourne, mais rien n'écoute sur le port 1024 | `console.log` : wsagent a-t-il démarré ? |
| `ping recv: EOF` | le handshake a réussi, donc l'agent écoute, mais il ferme la connexion sans répondre | l'agent de l'image est trop ancien ou défaillant : journal de `wsagent.service` dans l'invité, et reconstruire l'image (`make image`) puis le golden |
| rien dans la console | l'invité ne démarre pas du tout | noyau sans virtio en dur, rootfs illisible, mauvais `image_id` |

```bash
chroot=/srv/jailer/firecracker/<id>/root
sudo cat $chroot/run/console.log
sudo cat $chroot/run/firecracker.log
sudo ls -l $chroot/v.sock
```

Ce que la console doit montrer dans un démarrage sain : le montage du rootfs
(`EXT4-fs (vda): mounted filesystem`), `systemd[1]: Detected virtualization
kvm`, puis le démarrage de `wsagent.service`. Si elle s'arrête avant le
montage, le problème est dans le noyau ou le rootfs — un noyau dont les pilotes
virtio ne sont pas compilés en dur ne trouve pas `/dev/vda`, la cmdline
contenant `nomodule` et l'invité n'ayant pas d'initramfs. `make kernel`
vérifie ce point depuis la correction du script, mais un `vmlinux` construit
avant ne l'a pas été : le reconstruire lève le doute.

### `code-server non disponible après 30 s`

L'agent a répondu, donc l'invité tourne : il reste à savoir si code-server ne
sert pas, ou si le chemin réseau du host vers la VM est cassé. L'erreur nomme
l'adresse sondée et la cause réseau. Deux commandes les séparent :

```bash
# le chemin complet, celui que wsproxy utilisera : host → veth → DNAT → invité
curl -v http://<ip-veth-netns>:8080/healthz

# en court-circuitant le DNAT, directement sur l'invité depuis le netns
sudo ip netns exec ws-<id8> curl -v http://172.16.0.2:8080/healthz
```

| Première | Seconde | Conclusion |
|---|---|---|
| échoue | répond | code-server va bien, le DNAT ou le forwarding du netns est en cause : `ip netns exec ws-<id8> nft list ruleset` |
| échoue | échoue | code-server ne sert pas : `console.log`, et l'état de `code-server.service` dans l'invité |
| répond | — | c'était transitoire : code-server a mis plus de 30 s à démarrer |

`wsctl get <id>` donne l'IP à utiliser dans la première commande, et les huit
premiers caractères de l'id donnent le nom du netns.

### La VM tourne mais ne joint rien

```bash
sudo nft list table ip wsd-host                  # masquerade host posé ?
sudo ip netns exec ws-<id8> nft list ruleset     # règles du workspace
sudo ip netns exec ws-<id8> sysctl net.ipv4.ip_forward   # doit valoir 1
sudo ip netns exec ws-<id8> ip route
```

Puis le contenu de `egress.allow` : la politique par défaut est `drop`, et seuls
le DNS déclaré et les règles listées passent.

### Le host est sous pression mémoire

```bash
wsctl top                       # trier mentalement par PSS
free -h
```

Suspendre les workspaces les moins actifs libère leur RAM immédiatement :

```bash
wsctl suspend <id>
```

Baisser `idle_timeout` dans `wsd.json` puis redémarrer wsd rend le reaper plus
agressif pour la suite.

### Le disque est plein

```bash
df -h /var/lib/wsd
sudo du -sh /var/lib/wsd/* | sort -h
sudo du -sh /var/lib/wsd/workspaces/* | sort -h | tail -20
```

Les gros postes sont en général les `suspend/mem` des workspaces suspendus et
les anciens goldens. Réveiller un workspace supprime son `suspend/`, et la
section Rotation des goldens traite le reste.

---

## Routines

### Chaque jour

```bash
systemctl is-active wsd wsproxy
wsctl ls | grep -c error
df -h /var/lib/wsd | tail -1
```

### Chaque semaine

```bash
wsctl top                                   # dérive du PSS, workspaces oubliés
journalctl -u wsd --since "7 days ago" | grep -c level=ERROR
ip netns list | grep -c '^ws-'              # à comparer à wsctl ls
sqlite3 /var/lib/wsd/wsd.db "SELECT count(*) FROM ip_allocations"
# sauvegarde de la base et de /etc/wsd
```

### Chaque mois

- vérifier les versions amont de `deploy/versions.env` (Firecracker,
  code-server, opencode, kernel) et planifier une mise à jour d'image ;
- purger les tokens non utilisés ;
- retirer les goldens et images qui ne sont plus référencés ;
- tester une restauration de sauvegarde sur un host de recette — une sauvegarde
  jamais restaurée n'est pas une sauvegarde.

---

## Pourquoi reflink et hardlink

Cette contrainte de stockage revient dans toutes les opérations, autant en avoir
la raison en tête.

- **`FICLONE` (copies reflink)** est utilisé à la création d'un workspace pour
  cloner le disque `workspace.ext4` depuis le template du golden, et à chaque
  démarrage de VM pour cloner le `rootfs.ext4` du golden dans le chroot. Un `cp`
  classique écrirait des gigaoctets à chaque fois. Le CoW du reflink donne aussi
  gratuitement l'isolation : la VM écrit sur son rootfs sans jamais modifier le
  fichier golden, dont la modification corromprait tous les clones. Seuls XFS
  `reflink=1` et btrfs savent le faire.
- **Les hardlinks** placent `vmlinux`, `workspace.ext4`, `mem` et `vmstate` dans
  le chroot jailer. Un hardlink ne traverse pas les filesystems, d'où
  l'obligation que `data_dir` et `jailer_dir` partagent le même filesystem. Même
  inode pour `mem` plus le backend mémoire `File` de Firecracker : les pages non
  modifiées du golden sont mutualisées en page cache entre tous les clones.
  **L'économie de RAM vient de là**, pas du reflink.

Le test qui tranche est un `FICLONE` réel, ce que font `check-host.sh` et
`setup-storage.sh`. XFS n'expose pas `reflink` dans les options de montage.

---

## Limites à connaître

Elles changent la façon de travailler au quotidien :

| Limite | Conséquence |
|---|---|
| Un workspace repris de son propre snapshot ne partage plus de pages avec le golden | la densité se dégrade après un cycle suspend/resume |
| `idle_timeout` est global | pas de politique par workspace ni par utilisateur |
| `workspace_disk_gib` est figé par le golden | un changement impose de reconstruire le golden ; agrandir un disque existant est hors v1 |
| `wsctl token` produit un lien `http://` | corriger le schéma à la main derrière un terminateur TLS |
| Les tokens n'expirent pas | un lien non utilisé reste valable : les purger |
| Pas de révocation par workspace | seule la rotation de `hmac_secret` révoque, et elle révoque tout |
| Les orphelins ne sont jamais supprimés automatiquement | inventaire croisé périodique à faire |
| Histogrammes de latence en mémoire | les métriques repartent de zéro à chaque redémarrage de wsd |
| Mono-host | pas de scheduler, pas de réplication des goldens |

Les anomalies encore ouvertes, dont le masquerade host que wsd ne pose pas,
sont suivies dans [`PLAN.md` § Anomalies](PLAN.md#anomalies), avec la liste de
celles déjà corrigées et leur cause.
