# Guide d'exploitation — wsd

## Prérequis du host

Vérifier la compatibilité avec `deploy/check-host.sh` :

```
bash deploy/check-host.sh
```

Points vérifiés : `/dev/kvm`, cgroup v2, filesystem reflink sur `/var/lib/wsd` et
`/srv/jailer` (même FS), présence de `ip`, `nft`, `firecracker`, `jailer`,
`ip_forward`.

`firecracker` et `jailer` sont signalés KO tant qu'ils ne sont pas installés :
`deploy/install.sh --with-deps` (ou `deploy/fetch-deps.sh`) s'en charge. Le point
« filesystem reflink » est le plus contraignant : voir [Stockage](#stockage) et
`deploy/setup-storage.sh`. La liste des paquets nécessaires aux builds est dans
le [README](../README.md).

## Stockage

### Ce qui est exigé, et pourquoi

wsd ne peut pas tourner sur n'importe quel filesystem. Deux mécanismes du noyau
sont utilisés sur le chemin critique :

- **`FICLONE` (copies reflink)** — à chaque création de workspace, le disque
  `workspace.ext4` (20 GiB par défaut) est cloné depuis le template du golden ;
  à chaque démarrage de VM, le `rootfs.ext4` du golden est cloné dans le chroot
  pour que la VM puisse écrire dessus sans jamais toucher le fichier golden. Un
  `cp` classique écrirait des gigaoctets à chaque fois : la cible de création en
  moins d'une seconde en dépend directement. Seuls **XFS avec `reflink=1`** et
  **btrfs** implémentent `FICLONE` ; ext4 renvoie `EOPNOTSUPP` et wsd échoue
  avec une erreur explicite.
- **Les hardlinks** — `vmlinux`, `workspace.ext4`, `mem` et `vmstate` sont
  hardlinkés dans le chroot jailer. Un hardlink ne traverse pas les
  filesystems : `data_dir` et `jailer_dir` doivent donc être **sur le même
  filesystem**. C'est aussi ce qui fait que les pages du `mem` du golden sont
  mutualisées en page cache entre tous les clones — l'économie de RAM vient de
  là, pas du reflink.

### Le préparer

`deploy/setup-storage.sh` formate, monte, vérifie et prépare l'arborescence.
Trois situations.

**1. Un disque ou une partition dédiés** — le cas normal en production :

```bash
sudo bash deploy/setup-storage.sh --device /dev/nvme1n1 \
     --mount /var/lib/wsd --jailer /srv/jailer --fstab
```

Le script formate en `mkfs.xfs -m reflink=1`, monte sur `/var/lib/wsd`, crée
`images/`, `workspaces/`, `goldens/`, `jailer/`, monte en bind
`/var/lib/wsd/jailer` sur `/srv/jailer` (même superbloc, donc les hardlinks
fonctionnent entre les deux chemins), et écrit les entrées `fstab` par UUID. Le
formatage demande une confirmation explicite : il détruit le contenu du
périphérique.

Sans `--jailer`, aucun bind mount n'est créé et il suffit de mettre
`"jailer_dir": "/var/lib/wsd/jailer"` dans `wsd.json` — c'est plus simple, mais
les chroots se retrouvent sous `data_dir`.

**2. Le filesystem racine est déjà XFS `reflink=1` ou btrfs** — rien à
formater. Il reste à s'assurer que `data_dir` et `jailer_dir` sont sur ce
filesystem (donc qu'aucun des deux n'est un point de montage séparé), puis à
créer l'arborescence, ce que fait `install.sh`. Vérifier le flag :

```bash
xfs_info "$(findmnt -n -o TARGET --target /var/lib/wsd)" | grep reflink
```

Depuis xfsprogs 5.1, `reflink=1` est le défaut de `mkfs.xfs` : un XFS récent
l'a déjà.

**3. Le filesystem racine n'a pas reflink (ext4 typiquement)** — une image XFS
montée en loop permet de démarrer sans repartitionner :

```bash
sudo bash deploy/setup-storage.sh --image /var/lib/wsd.img --size 500G \
     --mount /var/lib/wsd --jailer /srv/jailer --fstab
```

L'image est un fichier sparse : elle ne consomme que ce qui est écrit. En
contrepartie, les performances sont celles du filesystem sous-jacent plus la
couche loop, et la sauvegarde se fait au niveau de l'image entière, puisque le
FS hôte ne voit qu'un seul gros fichier. À réserver aux maquettes et aux hosts
qu'on ne peut pas repartitionner.

### Vérifier

Dans les trois cas, le test qui compte est un `FICLONE` réel, c'est ce que fait
`check-host.sh` quand le répertoire est inscriptible :

```bash
bash deploy/check-host.sh | grep reflink
#  [OK] /var/lib/wsd filesystem reflink : ok (FICLONE testé sur xfs, /var/lib/wsd)
```

À la main :

```bash
head -c 4096 /dev/urandom > /var/lib/wsd/.probe
cp --reflink=always /var/lib/wsd/.probe /var/lib/wsd/.probe.clone && echo OK
rm -f /var/lib/wsd/.probe /var/lib/wsd/.probe.clone
```

XFS n'expose **pas** `reflink` dans les options de montage — c'est un flag de
`mkfs`, invisible dans `/proc/mounts`. Chercher `reflink=1` dans la sortie de
`mount` ou de `findmnt` ne prouve donc rien, ni dans un sens ni dans l'autre.

### Dimensionner

| Poste | Taille | Remarque |
|---|---|---|
| `workspaces/<id>/workspace.ext4` | `workspace_disk_gib` nominal (20 GiB) | fichier sparse cloné en reflink : ne consomme au départ que les blocs du template, puis les blocs réellement écrits par l'utilisateur |
| `goldens/<id>/mem` | ≈ `vm.mem_mib` | un par golden, partagé en page cache par tous les clones |
| `goldens/<id>/rootfs.ext4` | taille du rootfs (≈ 2 GiB) | les clones partagent les blocs non modifiés |
| `workspaces/<id>/suspend/{mem,vmstate}` | ≈ `vm.mem_mib` par workspace suspendu | un workspace repris depuis son propre snapshot ne partage plus de pages avec le golden |
| chroots jailer | négligeable | hardlinks et clones reflink |

Le volume réel est donc très inférieur à la somme des tailles nominales, mais il
croît avec l'usage : prévoir de la marge et surveiller `df`, pas la somme des
`workspace_disk_gib`. Un workspace suspendu coûte en plus un `mem` de la taille
de la RAM de la VM.

## Installation initiale

Tout se fait avec `deploy/install.sh`, qui est idempotent : il peut être relancé
sans risque, et ne remplace jamais une configuration déjà en place (il dépose la
nouvelle version de l'exemple à côté, en `.new`).

### 1. Récupérer les fichiers

Depuis le dépôt :

```bash
make build
```

Ou depuis une archive produite par `make dist` sur une machine de build :

```bash
tar xzf wsd-<version>-linux-amd64.tar.gz
cd wsd-<version>-linux-amd64
```

### 2. Installer

```bash
sudo bash deploy/install.sh --with-deps
```

Ce que fait le script :

| Étape | Détail |
|---|---|
| Vérification | `bin/{wsd,wsproxy,wsctl}` présents, puis `check-host.sh`. Il s'arrête si un prérequis est KO (`--skip-host-check` pour forcer). |
| Dépendances | `--with-deps` télécharge `firecracker` et `jailer` aux versions de `versions.env` depuis les releases GitHub, vérifie le SHA256, et dépose le swagger de la version dans `/usr/share/doc/wsd/`. |
| Comptes | groupe système `wsd`, utilisateur système `wsproxy` (groupe `wsd`, sans shell ni home). |
| Répertoires | `/var/lib/wsd/{images,workspaces,goldens}`, `/srv/jailer`, `/etc/wsd` (0750), `/usr/share/doc/wsd`. |
| Binaires | `wsd`, `wsproxy`, `wsctl` dans `/usr/local/bin`. `wsagent` n'est **pas** installé sur le host : il est intégré au rootfs invité par `image/build.sh`. |
| Configuration | `/etc/wsd/{wsd,wsproxy}.json` depuis les exemples, `versions.env` de référence, exemples opencode en `.example`. Un `hmac_secret` de 32 octets aléatoires est généré à la première installation. |
| sysctl | `/etc/sysctl.d/90-wsd.conf` (`net.ipv4.ip_forward=1`), appliqué immédiatement. |
| systemd | `wsd.service` et `wsproxy.service`, puis `daemon-reload`. Les services ne sont **pas** démarrés. |

Options utiles : `--dry-run` (affiche sans exécuter), `--prefix`, `--sysconfdir`,
`--data-dir`, `--jailer-dir`, `--unit-dir`. `DESTDIR=<dir>` installe sous une
racine décalée, sans toucher aux comptes, à sysctl ni à systemd — pratique pour
inspecter le contenu d'une installation. `deploy/install.sh --help` liste tout.

### 3. Configurer

Référence complète de tous les champs : [`CONFIG.md`](CONFIG.md).

Au minimum, dans `/etc/wsd/wsd.json` :

- `domain` : le domaine **de base**, sans `ws.` — les hôtes des workspaces sont
  `<id>.ws.<domain>` ;
- `net.egress_iface` : l'interface de sortie du host (`ip route get 1.1.1.1`) ;
- `net.dns` : le résolveur autorisé depuis les VM ;
- `egress.allow` : ce que les workspaces ont le droit de joindre ;
- `image_id` : le nom du répertoire d'image à créer à l'étape suivante ;
- `vm.*` : vCPU, mémoire, taille du disque workspace.

Et dans `/etc/wsd/wsproxy.json` : le même `domain`, puis soit rien de plus
(auth par token), soit la section `oidc`.

`socket_group` (défaut `wsd`) est ce qui permet à wsproxy de joindre wsd : wsd
tourne en root et donne son socket `/run/wsd/wsd.sock` à ce groupe en 0660. Si
le groupe n'existe pas, wsd le signale dans son journal et wsproxy ne pourra
pas se connecter.

### 4. Image invitée et kernel

Ces deux builds ont besoin de `docker` et d'une toolchain de compilation
kernel ; ils se font depuis le dépôt, pas depuis l'archive de déploiement.

```bash
make image           # → build/rootfs.ext4
make kernel          # → build/vmlinux  (long : compilation complète)

install -d /var/lib/wsd/images/debian-bookworm-v1    # = image_id de wsd.json
cp build/rootfs.ext4 build/vmlinux /var/lib/wsd/images/debian-bookworm-v1/
```

Le nom du répertoire doit être exactement l'`image_id` de `wsd.json`.

### 5. Démarrer

```bash
systemctl enable --now wsd wsproxy
systemctl status wsd wsproxy
journalctl -u wsd -f
```

### 6. Golden snapshot initial

```bash
wsctl golden build     # boot à froid, attente de code-server, snapshot Full
wsctl golden get       # vérifier le golden actif
```

Sans golden actif, les créations de workspace retombent sur un boot à froid :
elles fonctionnent, mais sont lentes.

### 7. Test de bout en bout

```bash
wsctl create test-ws --owner admin@example.com
wsctl ls
wsctl token <id>       # ouvrir le lien dans un navigateur
```

### 8. Mise à jour des binaires

```bash
make build                                   # ou décompresser la nouvelle archive
sudo bash deploy/install.sh --skip-host-check
systemctl restart wsd wsproxy
```

Les workspaces en cours ne sont pas interrompus : `wsd.service` est en
`KillMode=process`, les firecracker tournent dans leurs propres cgroups, et wsd
réconcilie son état au redémarrage.

Le schéma de `wsd.db` est migré automatiquement au démarrage de wsd, dans une
transaction par migration ; la version appliquée est suivie dans
`PRAGMA user_version`. Sauvegarder la base avant une montée de version reste la
précaution d'usage :

```bash
systemctl stop wsd
cp /var/lib/wsd/wsd.db /var/lib/wsd/wsd.db.bak
# puis installer et redémarrer
sqlite3 /var/lib/wsd/wsd.db "PRAGMA user_version"   # version après migration
```

Vérifier ce qui tourne :

```bash
wsd -version && wsproxy -version && wsctl -version
```

### 9. Désinstallation

```bash
sudo bash deploy/uninstall.sh              # services, unités, binaires, doc
sudo bash deploy/uninstall.sh --purge      # + /etc/wsd, compte wsproxy, groupe wsd
sudo bash deploy/uninstall.sh --purge-data # + disques workspace, goldens, chroots
```

`--purge-data` demande une confirmation explicite : il détruit les données
utilisateur. Supprimer les workspaces avec `wsctl rm <id>` **avant** de
désinstaller, sinon les netns, veth, règles nftables et chroots restent sur le
host — le script le signale et demande confirmation.

---

## Opérations courantes

### Créer / supprimer un workspace

```bash
wsctl create mon-projet --owner alice@example.com
wsctl rm <id>
```

### Suspendre / réveiller manuellement

```bash
wsctl suspend <id>
wsctl resume  <id>
```

### État des VMs en temps réel

```bash
wsctl top
```

Colonnes : ID, NOM, ÉTAT, RSS (MiB privé), PSS (MiB partagé), DERNIÈRE ACTIVITÉ.

### Métriques Prometheus

```bash
curl --unix-socket /run/wsd/wsd.sock http://wsd/v1/metrics
```

Le socket est en 0660, groupe `wsd` : lire les métriques demande d'être root ou
membre du groupe. Pour un scrape Prometheus, passer par un exportateur qui lit
ce socket.

---

## Mise à jour d'image

1. Construire la nouvelle image et le nouveau kernel :
   ```bash
   make image kernel
   ```
2. Copier dans un nouveau répertoire image :
   ```bash
   mkdir -p /var/lib/wsd/images/debian-bookworm-v2
   cp build/rootfs.ext4 build/vmlinux /var/lib/wsd/images/debian-bookworm-v2/
   ```
3. Mettre à jour `image_id` dans `/etc/wsd/wsd.json` :
   ```json
   "image_id": "debian-bookworm-v2"
   ```
4. Recharger wsd :
   ```bash
   systemctl restart wsd
   ```
5. Construire un nouveau golden (il utilisera la nouvelle image) :
   ```bash
   wsctl golden build
   ```
6. Les workspaces existants **continuent de tourner** sur l'ancien golden et
   l'ancienne image jusqu'à leur prochaine recréation.  Pour les migrer :
   ```bash
   wsctl ls
   # pour chaque workspace à migrer :
   wsctl rm <id> --keep-data   # garde /var/lib/wsd/workspaces/<id>/workspace.ext4
   wsctl create <nom> --owner <owner>
   ```

---

## Sauvegarde des données workspace

Les données utilisateur se trouvent dans `/var/lib/wsd/workspaces/<id>/workspace.ext4`.

```bash
# Snapshot LVM ou btrfs send/receive du FS hôte (recommandé)
# OU copie directe (le workspace doit être suspendu ou supprimé) :
wsctl suspend <id>
cp /var/lib/wsd/workspaces/<id>/workspace.ext4 /backup/<id>-$(date +%Y%m%d).ext4
```

---

## Nettoyage des orphelins

Au démarrage, wsd réconcilie l'état et marque en `error` les workspaces dont le
process firecracker n'existe plus. Ces workspaces ne sont **jamais supprimés
automatiquement** en v1.

```bash
# Lister les workspaces en erreur
wsctl ls | grep error

# Supprimer les orphelins manuellement
wsctl rm <id>

# Vérifier l'absence de chroots résiduels
ls /srv/jailer/firecracker/

# Vérifier l'absence de netns résiduels
ip netns list | grep ws-
```

---

## Rotation des goldens

Les anciens goldens ne sont pas supprimés automatiquement. Une fois tous les
workspaces migrés vers le nouveau golden :

```bash
# Identifier l'ancien golden
wsctl golden get   # affiche le golden actif (nouveau)
ls /var/lib/wsd/goldens/

# Supprimer les anciens (adapter l'ID)
rm -rf /var/lib/wsd/goldens/<ancien-id>
# Puis supprimer l'entrée en base si nécessaire (SQLite direct) :
sqlite3 /var/lib/wsd/wsd.db "DELETE FROM goldens WHERE id='<ancien-id>'"
```

---

## Dépannage

| Symptôme | Commande de diagnostic |
|---|---|
| `jailer launch: kernel invité (vmlinux) introuvable` (ou `rootfs invité`) | l'image invitée n'est pas en place : `ls /var/lib/wsd/images/<image_id>/`. Elle doit contenir `vmlinux` et `rootfs.ext4` (§ Installation initiale, étape 4). wsd le signale aussi au démarrage : `journalctl -u wsd \| grep "image invitée"` |
| VM ne démarre pas | `journalctl -u wsd -n 100` ; log firecracker dans `/srv/jailer/firecracker/<id>/root/run/firecracker.log` |
| code-server inaccessible | `wsctl get <id>` ; vérifier l'état `running` |
| Page « réveil en cours » ne passe pas | `wsctl top` pour voir RSS et état |
| OIDC auth échoue | `journalctl -u wsproxy -n 50` ; vérifier `redirect_uri` enregistrée côté provider |
| wsproxy ne joint pas wsd | `ls -l /run/wsd/wsd.sock` (attendu `srw-rw---- root wsd`), `id wsproxy` (doit être dans `wsd`), `socket_group` dans `wsd.json` |
| 404 « workspace introuvable » | le `Host` doit être `<id>.ws.<domain>` et `domain` identique dans les deux configs, sans préfixe `ws.` |
| La VM ne joint rien (ni DNS, ni registre) | le masquerade host n'est pas posé par wsd : `nft list table ip wsd-host`. Le poser à la main (en adaptant `net.pool` et `net.egress_iface`) :<br>`nft -f - <<<'table ip wsd-host { chain postrouting { type nat hook postrouting priority srcnat; ip saddr 10.200.0.0/16 oif eth0 masquerade } }'`<br>Vérifier aussi l'`egress.allow` de `wsd.json`, et que le registre npm y est si opencode est activé. |
| Pool IP épuisé | `sqlite3 /var/lib/wsd/wsd.db "SELECT count(*) FROM ip_allocations"` ; supprimer des workspaces |
