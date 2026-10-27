# Premier déploiement — wsd

Ce guide part d'un host vierge et s'arrête quand un workspace s'ouvre dans un
navigateur. Il est linéaire : chaque étape se termine par une vérification, et
il ne faut pas passer à la suivante si elle échoue.

La référence exhaustive des champs de configuration est dans
[`CONFIG.md`](CONFIG.md). L'exploitation au quotidien est dans
[`ADMIN.md`](ADMIN.md). L'architecture et les choix techniques sont dans
[`SPEC.md`](SPEC.md).

**Compter environ deux heures**, dont l'essentiel en compilation du kernel
invité (30 à 60 min selon la machine) et en construction du rootfs (10 à
20 min, dépendant du débit réseau). L'installation elle-même prend quelques
minutes.

---

## 0. À lire avant de commencer

### Ce qui fonctionne et ce qui ne fonctionne pas encore

Trois anomalies connues sont ouvertes, détaillées dans
[`PLAN.md` § Anomalies ouvertes](PLAN.md#anomalies-ouvertes). Deux ont un effet
direct sur ce guide :

| Anomalie | Effet sur un premier déploiement |
|---|---|
| **Les VM sont tuées à la fin de la requête HTTP** (`fc.Launch` reçoit le contexte de la requête) | `wsctl create` répond `201` puis la VM meurt. `wsctl golden build` n'est **pas** concerné : il prend son snapshot avant que le handler ne retourne. En clair : la construction du golden aboutit, la création d'un workspace utilisable non. |
| **Le masquerade host n'est jamais posé** par wsd (`netns.SetupHostMasquerade` n'a pas d'appelant) | Sans règle nft posée à la main (étape 6), les VM ne joignent rien au-delà de leur veth : ni DNS, ni registre, ni endpoint LLM. |
| `net.egress_iface` n'est pas validé | Une interface inexistante ne se voit qu'à la première création de workspace. |

Ce guide indique le contournement là où il faut. Déployer aujourd'hui a du sens
pour valider le host, le stockage, le réseau et le golden ; pas pour ouvrir le
service à des utilisateurs.

### Les deux machines

| Rôle | Ce qu'elle fait | Ce qu'il lui faut |
|---|---|---|
| **Machine de build** | `make build`, `make image`, `make kernel`, `make dist` | Go, docker, toolchain de compilation kernel, ~20 Gio libres |
| **Host de production** | fait tourner `wsd`, `wsproxy` et les microVMs | KVM, cgroup v2, XFS reflink, `ip`, `nft`, firecracker, jailer |

Les deux peuvent être la même machine, et c'est le plus simple pour un premier
déploiement. Le reste du guide suppose que oui ; les étapes qui n'ont de sens
que sur la machine de build sont signalées **[build]**.

### Ce que le déploiement installe

```
/usr/local/bin/{wsd,wsproxy,wsctl}      binaires statiques
/usr/local/bin/{firecracker,jailer}     versions épinglées
/etc/wsd/wsd.json                       configuration du daemon
/etc/wsd/wsproxy.json                   configuration du proxy
/etc/systemd/system/{wsd,wsproxy}.service
/etc/sysctl.d/90-wsd.conf               net.ipv4.ip_forward=1
/var/lib/wsd/                           images, workspaces, goldens, wsd.db
/srv/jailer/                            chroots jailer
/usr/share/doc/wsd/                     cette documentation + swagger Firecracker
```

`wsagent` n'est pas installé sur le host : il est intégré au rootfs invité par
`image/build.sh`.

---

## 1. Prérequis du host

### Matériel et noyau

- x86_64 avec virtualisation matérielle active dans le BIOS/UEFI (VT-x ou AMD-V).
- Linux ≥ 5.10 côté host, `/dev/kvm` présent, cgroup v2 monté en unified.
- Une microVM consomme par défaut 2 vCPU et 2048 MiB déclarés. Le dimensionnement
  réel dépend de la mémoire *modifiée* par VM, pas de la mémoire déclarée :
  c'est tout l'intérêt du partage de pages du golden.

Vérifier KVM :

```bash
ls -l /dev/kvm
grep -cE '^flags.*(vmx|svm)' /proc/cpuinfo    # > 0 attendu
```

Dans une VM, la virtualisation imbriquée doit être activée par l'hyperviseur
hôte, sinon `/dev/kvm` est absent et rien ne démarrera.

### Paquets

Sur le **host** :

```bash
# RHEL / Rocky / Alma 9
dnf install -y iproute nftables xfsprogs procps-ng curl sqlite

# Debian / Ubuntu
apt install -y iproute2 nftables xfsprogs procps curl sqlite3
```

Sur la **machine de build**, en plus :

```bash
# RHEL / Rocky / Alma 9
dnf install -y git make gcc flex bison bc elfutils-libelf-devel openssl-devel \
               perl dwarves e2fsprogs tar xz
dnf install -y docker-ce      # ou podman avec un alias docker

# Debian / Ubuntu
apt install -y git make gcc flex bison bc libelf-dev libssl-dev \
               perl pahole e2fsprogs tar xz-utils docker.io
```

Go dans sa dernière version stable, selon la méthode de la distribution ou
depuis go.dev. La version utilisée pour l'invité est épinglée séparément dans
`deploy/versions.env` (`GO_VERSION`) et n'a pas à être celle du host.

### Diagnostic d'ensemble

```bash
git clone <votre-remote>/wsd && cd wsd
bash deploy/check-host.sh
```

Le script rend un rapport OK/KO point par point. À ce stade, il est normal que
`firecracker`, `jailer` et le filesystem reflink soient KO : les étapes 2 et 3
s'en occupent. Tout le reste doit être OK avant de continuer.

Le test du filesystem est un `FICLONE` réel quand le répertoire est
inscriptible. XFS n'expose pas `reflink` dans les options de montage — c'est un
flag de `mkfs`, invisible dans `/proc/mounts` — donc ne pas se fier à `mount`
ou `findmnt` pour ce point.

---

## 2. Stockage

C'est le prérequis le plus contraignant, et celui qui demande une décision
irréversible (le formatage). La justification technique complète est dans
[`ADMIN.md` § Pourquoi reflink et hardlink](ADMIN.md#pourquoi-reflink-et-hardlink) ;
en résumé, wsd a besoin de :

- **`FICLONE`** pour cloner le disque workspace (20 GiB par défaut) et le rootfs
  du golden sans recopier les données — seuls XFS `reflink=1` et btrfs savent le
  faire, ext4 non ;
- **des hardlinks** entre `data_dir` et `jailer_dir`, qui doivent donc être sur
  **le même filesystem**.

### Cas 1 : un disque ou une partition dédiés (production)

```bash
lsblk                      # repérer le périphérique, vérifier qu'il est libre
sudo bash deploy/setup-storage.sh --device /dev/nvme1n1 \
     --mount /var/lib/wsd --jailer /srv/jailer --fstab
```

Le script formate en `mkfs.xfs -m reflink=1` après confirmation explicite,
monte, **vérifie `FICLONE` pour de bon**, crée l'arborescence, monte en bind
`/var/lib/wsd/jailer` sur `/srv/jailer` — même superbloc, donc les hardlinks
fonctionnent entre les deux chemins — et écrit les entrées `fstab` par UUID.

### Cas 2 : le filesystem racine est déjà XFS `reflink=1` ou btrfs

Rien à formater. Vérifier, puis passer à l'étape 3 — `install.sh` créera
l'arborescence :

```bash
xfs_info "$(findmnt -n -o TARGET --target /var/lib)" | grep reflink
```

Depuis xfsprogs 5.1, `reflink=1` est le défaut de `mkfs.xfs` : un XFS récent
l'a déjà. Il faut en revanche que ni `/var/lib/wsd` ni `/srv/jailer` ne soient
des points de montage séparés.

### Cas 3 : filesystem racine sans reflink (ext4)

Une image XFS montée en loop permet de démarrer sans repartitionner :

```bash
sudo bash deploy/setup-storage.sh --image /var/lib/wsd.img --size 500G \
     --mount /var/lib/wsd --jailer /srv/jailer --fstab
```

L'image est creuse et ne consomme que ce qui est écrit. En contrepartie, les
performances sont celles du filesystem sous-jacent plus la couche loop, et la
sauvegarde se fait au niveau de l'image entière. À réserver aux maquettes.

### Dimensionner

| Poste | Taille | Remarque |
|---|---|---|
| `workspaces/<id>/workspace.ext4` | 20 GiB nominal | creux et cloné en reflink : ne consomme au départ que les blocs du template, puis ce que l'utilisateur écrit |
| `goldens/<id>/mem` | ≈ `vm.mem_mib` | un par golden, partagé en page cache par tous les clones |
| `goldens/<id>/rootfs.ext4` | ≈ taille du rootfs | les clones partagent les blocs non modifiés |
| `workspaces/<id>/suspend/{mem,vmstate}` | ≈ `vm.mem_mib` **par workspace suspendu** | un workspace repris de son propre snapshot ne partage plus de pages avec le golden |
| `images/<image-id>/` | rootfs + vmlinux | un par version d'image |

Le volume réel est très inférieur à la somme des tailles nominales, mais il
croît avec l'usage. Surveiller `df`, jamais la somme des `workspace_disk_gib`.

### ✅ Vérification

```bash
bash deploy/check-host.sh | grep -E "reflink|même device"
# [OK] /var/lib/wsd filesystem reflink : ok (FICLONE testé sur xfs, /var/lib/wsd)
# [OK] /srv/jailer filesystem reflink : ok (FICLONE testé sur xfs, /srv/jailer)
# [OK] /var/lib/wsd et /srv/jailer même device : ok (device …)
```

---

## 3. firecracker et jailer

Versions épinglées dans `deploy/versions.env`. Elles doivent être identiques
entre elles : les deux binaires viennent de la même release.

```bash
sudo bash deploy/fetch-deps.sh
```

Le script télécharge l'archive de la release GitHub, **vérifie son SHA256**,
installe `firecracker` et `jailer` dans `/usr/local/bin`, et dépose le swagger
de la version exacte dans `/usr/share/doc/wsd/`. C'est ce swagger qui fait
référence pour l'API Firecracker, pas la documentation en ligne.

### ✅ Vérification

```bash
firecracker --version   # Firecracker v1.17.0
jailer --version        # Jailer v1.17.0
```

---

## 4. Installation

```bash
make build
sudo bash deploy/install.sh
```

`install.sh` est **idempotent** : il peut être relancé sans risque et ne
remplace jamais une configuration en place — il dépose la nouvelle version de
l'exemple à côté, en `.new`. Détail de ce qu'il fait dans
[`ADMIN.md` § Mettre à jour les binaires](ADMIN.md#mettre-à-jour-les-binaires).

Options utiles :

| Option | Effet |
|---|---|
| `--with-deps` | fait aussi l'étape 3 |
| `--dry-run` | affiche les actions sans les exécuter |
| `--skip-host-check` | n'arrête pas l'installation sur un point KO de `check-host.sh` |
| `--prefix`, `--sysconfdir`, `--data-dir`, `--jailer-dir`, `--unit-dir` | chemins |
| `DESTDIR=<dir>` (variable) | installe sous une racine décalée, sans toucher aux comptes, à sysctl ni à systemd — pour inspecter ce qu'une installation contient |

Les services **ne sont pas démarrés** : la configuration doit d'abord être
adaptée.

### ✅ Vérification

```bash
wsd -version && wsproxy -version && wsctl -version
getent group wsd && getent passwd wsproxy
ls -l /etc/wsd/
```

---

## 5. Configuration

Cinq décisions à prendre, toutes dans `/etc/wsd/wsd.json`. La référence
complète de chaque champ, avec ses défauts, est dans [`CONFIG.md`](CONFIG.md).

### 5.1 Le domaine

wsd et wsproxy construisent tous les deux l'hôte d'un workspace comme
`<id>.ws.<domain>` : **le label `ws` est ajouté par le code**. Le champ `domain`
porte donc le domaine de base, sans `ws.` :

```json
"domain": "example.internal"
```

donne `https://<id>.ws.example.internal`. La même valeur doit figurer dans
`wsd.json` (liens produits par `wsctl token`) et dans `wsproxy.json` (extraction
de l'`<id>` depuis l'en-tête `Host`).

### 5.2 L'interface de sortie et le résolveur

```bash
ip route get 1.1.1.1    # la ligne « dev <iface> » donne egress_iface
```

```json
"net": {
  "pool": "10.200.0.0/16",
  "egress_iface": "eth0",
  "dns": "1.1.1.1"
}
```

`pool` est découpé en /30 par workspace, un bloc par workspace : un /16 plafonne
à 16 384 workspaces, bien au-delà de ce qu'un host tiendra. Le choisir hors de
tout réseau déjà routé chez vous.

`dns` est le seul résolveur que les VM auront le droit de joindre. Vide, la
création de workspace échoue avec un message explicite.

### 5.3 Ce que les workspaces ont le droit de joindre

```json
"egress": {
  "allow": [
    {"cidr": "0.0.0.0/0", "ports": [443]},
    {"cidr": "0.0.0.0/0", "ports": [80]}
  ]
}
```

La politique de la chaîne `forward` du netns est `drop`. Sont autorisés
l'ESTABLISHED/RELATED, le DNS vers `net.dns`, puis ces règles. Sont interdits
quoi qu'il arrive : `169.254.0.0/16` (métadonnées cloud), le pool des autres
workspaces, et `100.64.0.0/10`.

Si opencode est activé (étape 12), le registre npm doit être joignable : le
paquet du provider est téléchargé dans la VM au premier appel au modèle.

### 5.4 Le dimensionnement des VM

```json
"vm": {
  "vcpus": 2,
  "mem_mib": 2048,
  "workspace_disk_gib": 20,
  "cpu_max": "200000 100000"
}
```

`workspace_disk_gib` est **figé par le golden** : le changer ensuite impose de
reconstruire le golden. Le décider maintenant. Le redimensionnement d'un disque
workspace existant est hors v1.

### 5.5 L'authentification du proxy

Dans `/etc/wsd/wsproxy.json`, le `hmac_secret` a été généré aléatoirement par
`install.sh`. Mettre le même `domain` qu'au 5.1. Laisser `oidc.issuer` vide pour
rester en authentification par token le temps de la recette ; OIDC se branche à
l'étape 11.

```json
{
  "domain": "example.internal",
  "listen": ":8080",
  "wsd_socket": "/run/wsd/wsd.sock",
  "hmac_secret": "…généré…",
  "cookie_name": "wsd_auth",
  "cookie_ttl": "24h",
  "oidc": {"issuer": ""}
}
```

### ✅ Vérification

```bash
python3 -m json.tool /etc/wsd/wsd.json     > /dev/null && echo "wsd.json : JSON valide"
python3 -m json.tool /etc/wsd/wsproxy.json > /dev/null && echo "wsproxy.json : JSON valide"
grep -o '"domain": "[^"]*"' /etc/wsd/wsd.json /etc/wsd/wsproxy.json   # doivent correspondre
```

---

## 6. Réseau du host

### 6.1 Forwarding

Posé par `install.sh` dans `/etc/sysctl.d/90-wsd.conf` :

```bash
sysctl net.ipv4.ip_forward     # = 1
```

Le forwarding **à l'intérieur** de chaque netns est réglé par wsd lui-même à la
création du workspace.

### 6.2 Masquerade — à poser à la main

wsd ne pose pas cette règle (anomalie 1b). Sans elle, les VM ne sortent pas du
host. En adaptant `net.pool` et `net.egress_iface` :

```bash
sudo nft -f - <<'EOF'
table ip wsd-host {
  chain postrouting {
    type nat hook postrouting priority srcnat;
    ip saddr 10.200.0.0/16 oif eth0 masquerade
  }
}
EOF
sudo nft list table ip wsd-host
```

Pour la rendre persistante, l'ajouter à la configuration nftables de la
distribution (`/etc/nftables.conf` ou `/etc/sysconfig/nftables.conf` selon le
système), et **pas** dans un fichier que wsd gère.

### 6.3 DNS et TLS

wsproxy écoute en HTTP clair et ne termine pas TLS. Deux choses à mettre en
place côté infrastructure :

- un enregistrement DNS **wildcard** `*.ws.example.internal` pointant vers le
  host ;
- un terminateur TLS devant wsproxy avec un certificat wildcard
  `*.ws.example.internal` — HAProxy, nginx, Traefik, au choix. Il doit
  transmettre l'en-tête `Host` intact, puisque c'est lui qui porte l'`<id>` du
  workspace, et laisser passer les WebSockets (c'est par là que passe le
  terminal de VS Code).

Exemple minimal nginx :

```nginx
server {
    listen 443 ssl;
    server_name *.ws.example.internal;
    ssl_certificate     /etc/ssl/certs/ws-wildcard.pem;
    ssl_certificate_key /etc/ssl/private/ws-wildcard.key;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 3600s;   # sessions VS Code longues
    }
}
```

`proxy_read_timeout` doit être généreux : une session VS Code garde des
WebSockets ouvertes longtemps.

---

## 7. Image invitée et kernel **[build]**

Les deux artefacts sont volumineux et longs à produire. Ils se construisent
depuis le dépôt, pas depuis l'archive `make dist`.

### 7.1 Le kernel

```bash
make kernel        # → build/vmlinux
```

Le script télécharge le kernel de la version de `versions.env`, récupère la
config invitée de référence du dépôt Firecracker
(`microvm-kernel-ci-<arch>-<série>.config`), **vérifie** que toutes les options
requises par SPEC §4.2 sont compilées en dur, puis compile `vmlinux`.

Deux points de vigilance :

- la série du kernel doit être l'une de celles pour lesquelles la version de
  Firecracker épinglée fournit une config (pour v1.17.0 et x86_64 : 5.10, 6.1,
  6.18). Le script s'arrête en listant les séries disponibles si ce n'est pas le
  cas ;
- rien ne doit être en module : la cmdline invitée contient `nomodule` et il n'y
  a pas d'initramfs. Le script le vérifie après `olddefconfig` et refuse de
  compiler sinon.

Compter 30 à 60 minutes. Le résultat est un `vmlinux` non compressé.

### 7.2 Le rootfs

```bash
make image         # → build/rootfs.ext4
```

Le script compile `wsagent`, construit l'image Docker (Debian stable + systemd,
code-server, opencode, toolchain Go, utilisateur `coder`), l'exporte et en fait
un ext4 avec `mkfs.ext4 -d`, sans montage loop. Taille par défaut 4 GiB,
ajustable par `ROOTFS_SIZE`.

### 7.3 Déposer les artefacts

Le nom du répertoire doit être **exactement** l'`image_id` de `wsd.json` :

```bash
sudo install -d /var/lib/wsd/images/debian-bookworm-v1
sudo cp build/rootfs.ext4 build/vmlinux /var/lib/wsd/images/debian-bookworm-v1/
```

### ✅ Vérification

```bash
ls -l /var/lib/wsd/images/debian-bookworm-v1/
# rootfs.ext4 et vmlinux doivent être présents
file /var/lib/wsd/images/debian-bookworm-v1/vmlinux | grep -q ELF && echo "vmlinux : OK"
```

---

## 8. Premier démarrage

```bash
sudo systemctl enable --now wsd
sudo systemctl status wsd
sudo journalctl -u wsd -n 30
```

wsd signale au démarrage si l'image invitée est absente ou incomplète, et quoi
faire. Ce que le journal doit contenir :

```
level=INFO msg="image invitée en place" image_id=debian-bookworm-v1 dir=/var/lib/wsd/images/…
level=INFO msg="wsd démarré" socket=/run/wsd/wsd.sock
```

Et ce qui signale un problème :

```
level=WARN msg="image invitée incomplète : la création de workspace échouera" …
level=WARN msg="groupe du socket introuvable, …"   → wsproxy ne pourra pas joindre wsd
```

Puis le proxy :

```bash
sudo systemctl enable --now wsproxy
sudo journalctl -u wsproxy -n 20
```

### ✅ Vérification

```bash
# le socket doit être en 0660, groupe wsd
ls -l /run/wsd/wsd.sock          # srw-rw---- 1 root wsd
id wsproxy                       # doit contenir le groupe wsd

# l'API répond
sudo curl -s --unix-socket /run/wsd/wsd.sock http://wsd/v1/health
# {"status":"ok"}

wsctl ls                         # « aucun workspace »
```

Si `wsctl` répond `permission denied`, c'est le groupe du socket : voir
`socket_group` dans [`CONFIG.md`](CONFIG.md).

---

## 9. Golden snapshot initial

Le golden est l'état mémoire d'une VM déjà démarrée, avec code-server prêt.
C'est lui qui permet de créer un workspace par *restore* au lieu d'un boot.

```bash
time wsctl golden build
```

Ce que wsd fait : netns et chroot temporaires, boot à froid, attente de l'agent
en vsock puis de code-server, `sync` et vidage du page cache invité, pause de la
VM, snapshot Full, et déplacement dans `goldens/<id>/`. C'est la seule étape qui
démarre une VM de bout en bout : si elle passe, le host, le stockage, le réseau
et l'image sont bons.

En cas d'échec, le log firecracker de la VM temporaire est le premier endroit à
regarder :

```bash
sudo journalctl -u wsd -n 80
sudo ls /srv/jailer/firecracker/            # un chroot résiduel nomme le build
sudo cat /srv/jailer/firecracker/<id>/root/run/firecracker.log
```

### ✅ Vérification

```bash
wsctl golden get
sudo ls -lh /var/lib/wsd/goldens/*/
# vmstate, mem, rootfs.ext4, workspace-template.ext4
```

Le fichier `mem` doit faire environ `vm.mem_mib`. Noter la durée du build et la
taille de `mem` dans le tableau « Mesures relevées » de [`PLAN.md`](PLAN.md).

---

## 10. Premier workspace

```bash
wsctl create recette --owner vous@example.com
wsctl ls
wsctl get <id>
```

Puis l'accès navigateur. `wsctl token` produit un lien à usage unique qui pose
le cookie d'authentification :

```bash
wsctl token <id>
# http://<id>.ws.example.internal/?tkn=…
```

Le lien est émis en `http://` sans tenir compte du terminateur TLS : derrière
HTTPS, remplacer le schéma à la main. Le token est à usage unique et sans date
d'expiration ; le cookie qu'il pose vaut `cookie_ttl` (24 h par défaut).

**C'est ici que l'anomalie des VM tuées se manifeste** : la réponse arrive, puis
le process firecracker est tué avec le contexte de la requête. Symptôme : le
workspace passe `running` puis devient injoignable, et `wsctl top` montre un
RSS nul ou le workspace basculé en `error` au redémarrage de wsd. Tant que
l'anomalie 1 n'est pas corrigée, l'étape 10 ne peut pas aboutir.

### ✅ Vérification (quand l'anomalie 1 sera corrigée)

```bash
wsctl get <id>          # state=running, upstream=10.200.x.y:8080
curl -sI http://10.200.x.y:8080/ | head -1      # 200 ou 302 depuis le host
wsctl top               # RSS et PSS non nuls
```

Puis dans le navigateur : VS Code s'ouvre, le terminal intégré fonctionne (c'est
le test du WebSocket de bout en bout), et un fichier créé dans `/workspace`
survit à `wsctl suspend` puis `wsctl resume`.

---

## 11. OIDC (optionnel)

Tant que `oidc.issuer` est vide, l'accès se fait par token. Pour brancher un
fournisseur d'identité, dans `/etc/wsd/wsproxy.json` :

```json
"oidc": {
  "issuer": "https://accounts.google.com",
  "client_id": "…",
  "client_secret": "…",
  "external_scheme": "https"
}
```

La découverte OIDC est faite au démarrage : si l'issuer est injoignable,
**wsproxy ne démarre pas**. Le `redirect_uri` à déclarer chez le fournisseur est
`<external_scheme>://<id>.ws.<domain>/_oidc/callback`, soit une URI **par
workspace**. Un fournisseur qui n'accepte pas les wildcards impose donc soit de
déclarer chaque workspace, soit de faire converger le callback vers un domaine
unique au niveau du reverse proxy. À vérifier avant de s'engager sur OIDC.

```bash
sudo systemctl restart wsproxy
sudo journalctl -u wsproxy -n 20 | grep OIDC    # « OIDC activé »
```

L'`owner` du workspace est comparé à l'identité OIDC : créer les workspaces avec
l'`--owner` qui correspond à ce que le fournisseur renvoie.

---

## 12. opencode (optionnel)

opencode est désactivé par défaut, parce qu'un template inexistant ferait
échouer toute création de workspace.

```bash
sudo cp /etc/wsd/opencode.json.tmpl.example /etc/wsd/opencode.json.tmpl
sudo cp /etc/wsd/secrets.json.example       /etc/wsd/secrets.json
sudo chmod 0600 /etc/wsd/secrets.json
sudo vi /etc/wsd/secrets.json      # endpoint, api_key, model
```

Puis dans `wsd.json` :

```json
"opencode": {
  "config_template": "/etc/wsd/opencode.json.tmpl",
  "secrets_file": "/etc/wsd/secrets.json"
}
```

```bash
sudo systemctl restart wsd
```

Les deux fichiers sont relus à **chaque** création et resume : une rotation de
clé ne demande pas de redémarrage, et aucun secret ne se retrouve dans le
golden ni dans l'image. Le schéma de `opencode.json` dépend de la majeure
d'opencode — la série 1.x veut `provider` et `permission` au singulier — et le
détail est dans [`CONFIG.md` § opencode](CONFIG.md#opencode-opencode).

### ✅ Vérification

Aucun secret ne doit se trouver dans les artefacts partagés :

```bash
sudo grep -rl "$(python3 -c 'import json;print(json.load(open("/etc/wsd/secrets.json"))["api_key"])')" \
    /var/lib/wsd/goldens /var/lib/wsd/images 2>/dev/null
# aucune sortie attendue
```

---

## 13. Recette finale

| # | Vérification | Commande |
|---|---|---|
| 1 | Prérequis host tous OK | `bash deploy/check-host.sh` |
| 2 | `FICLONE` fonctionne sur le stockage | `check-host.sh \| grep reflink` |
| 3 | Binaires aux versions attendues | `wsd -version; firecracker --version` |
| 4 | Services actifs | `systemctl is-active wsd wsproxy` |
| 5 | Socket accessible à wsproxy | `ls -l /run/wsd/wsd.sock; id wsproxy` |
| 6 | API vivante | `curl --unix-socket /run/wsd/wsd.sock http://wsd/v1/health` |
| 7 | Image en place | `journalctl -u wsd \| grep "image invitée"` |
| 8 | Golden actif | `wsctl golden get` |
| 9 | Masquerade host posé | `nft list table ip wsd-host` |
| 10 | DNS wildcard résout | `getent hosts test.ws.example.internal` |
| 11 | TLS et WebSocket | ouvrir un workspace, utiliser le terminal intégré |
| 12 | Métriques exposées | `curl --unix-socket /run/wsd/wsd.sock http://wsd/v1/metrics` |

---

## 14. Dépannage par étape

| Symptôme | Étape | Piste |
|---|---|---|
| `/dev/kvm` absent | 1 | virtualisation désactivée dans le BIOS, ou imbrication non activée par l'hyperviseur hôte |
| `FICLONE refusé par ext4` | 2 | `deploy/setup-storage.sh`, cas 3 pour une image loop |
| `devices différents` entre `/var/lib/wsd` et `/srv/jailer`, ou `invalid cross-device link` au premier `create` | 2 | les deux chemins doivent être sur un seul filesystem. Soit `jailer_dir` sous `data_dir` (`"jailer_dir": "/var/lib/wsd/jailer"`, aucun montage), soit un bind mount — c'est ce que fait `--jailer` de `setup-storage.sh` |
| Une création échouée laisse des traces | 10 | le workspace passe en `error` sans rien libérer : `wsctl rm <id>` supprime netns, chroot et allocation IP |
| `sha256sum: FAILED` | 3 | téléchargement corrompu ou release modifiée : relancer, et vérifier `versions.env` |
| `wsctl: permission denied` sur le socket | 8 | `socket_group`, appartenance au groupe `wsd`, `ls -l /run/wsd/wsd.sock` |
| `wsd` ne démarre pas, `config:` dans le journal | 5 | JSON invalide ou champ requis manquant ; voir `CONFIG.md` |
| `wsproxy` ne démarre pas | 11 | issuer OIDC injoignable : la découverte est faite au démarrage |
| `netns setup: nft apply` | 9/10 | l'erreur porte désormais la sortie de `nft` ; vérifier `net.dns` et `net.egress_iface` |
| `kernel invité (vmlinux) introuvable` | 7 | artefacts non déposés dans `images/<image_id>/` |
| `golden build` échoue après le boot | 9 | log firecracker dans `/srv/jailer/firecracker/<id>/root/run/firecracker.log` |
| La VM démarre mais ne joint rien | 6 | masquerade host absent, ou `egress.allow` trop restrictif |
| 404 « workspace introuvable » | 6/10 | le `Host` doit être `<id>.ws.<domain>`, et `domain` identique dans les deux configs, sans préfixe `ws.` |

---

## Et ensuite

L'exploitation au quotidien — inventaire, capacité, sauvegardes, rotation des
goldens, playbooks d'incident — est dans [`ADMIN.md`](ADMIN.md).
