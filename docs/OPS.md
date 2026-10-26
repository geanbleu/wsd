# Guide d'exploitation — wsd

## Prérequis du host

Vérifier la compatibilité avec `deploy/check-host.sh` :

```
bash deploy/check-host.sh
```

Points vérifiés : `/dev/kvm`, cgroup v2, filesystem reflink sur `/var/lib/wsd` et
`/srv/jailer` (même FS), présence de `ip`, `nft`, `firecracker`, `jailer`,
`ip_forward`.

## Installation initiale

### 1. Binaires

```bash
make build
install -m 0755 bin/wsd bin/wsproxy bin/wsctl /usr/local/bin/
install -m 0755 bin/wsagent /usr/local/bin/   # intégré à l'image invitée
```

### 2. Utilisateur et groupes

```bash
groupadd -r wsd
useradd -r -g wsd -s /usr/sbin/nologin wsproxy
# wsd tourne en root ; wsproxy dans le groupe wsd pour accéder au socket
```

### 3. Répertoires

```bash
mkdir -p /var/lib/wsd/{images,workspaces,goldens}
mkdir -p /srv/jailer
mkdir -p /run/wsd
mkdir -p /etc/wsd
chmod 0750 /etc/wsd   # contient les secrets
```

### 4. Firecracker et jailer

Versions épinglées dans `deploy/versions.env`. Télécharger depuis les releases
GitHub de firecracker-microvm et les installer dans `/usr/local/bin/`.

### 5. Image invitée

```bash
make image           # produit build/rootfs.ext4
make kernel          # produit build/vmlinux
mkdir -p /var/lib/wsd/images/debian-bookworm-v1
cp build/rootfs.ext4 build/vmlinux /var/lib/wsd/images/debian-bookworm-v1/
```

Le `image_id` dans `wsd.json` doit correspondre au nom du répertoire (ici
`debian-bookworm-v1`).

### 6. Configuration

Copier et adapter les exemples :

```bash
cp deploy/wsd.json     /etc/wsd/wsd.json
cp deploy/wsproxy.json /etc/wsd/wsproxy.json
```

Générer un secret HMAC aléatoire pour wsproxy :

```bash
openssl rand -hex 32
# coller la valeur dans wsproxy.json → hmac_secret
```

Si opencode est utilisé, créer le template et le fichier secrets :

```bash
# /etc/wsd/opencode.json.tmpl  (Go text/template, données = contenu de secrets.json)
# /etc/wsd/secrets.json        (JSON plat, ex. {"api_key":"sk-...","endpoint":"..."})
chmod 0600 /etc/wsd/secrets.json
```

### 7. Systemd

```bash
cp deploy/wsd.service     /etc/systemd/system/
cp deploy/wsproxy.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now wsd wsproxy
```

### 8. Golden snapshot initial

```bash
wsctl golden build
wsctl golden get   # vérifier le golden actif
```

### 9. Test de bout en bout

```bash
wsctl create test-ws --owner admin@example.com
wsctl ls
wsctl token <id>   # ouvrir le lien dans un navigateur
```

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
wsctl -socket /run/wsd/wsd.sock get-metrics   # ou directement :
curl --unix-socket /run/wsd/wsd.sock http://wsd/v1/metrics
```

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
   wsctl rm <id>        # supprime (garde workspace.ext4 si --keep_data=1)
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
| VM ne démarre pas | `journalctl -u wsd -n 100` ; log firecracker dans `/srv/jailer/firecracker/<id>/root/run/firecracker.log` |
| code-server inaccessible | `wsctl get <id>` ; vérifier l'état `running` |
| Page « réveil en cours » ne passe pas | `wsctl top` pour voir RSS et état |
| OIDC auth échoue | `journalctl -u wsproxy -n 50` ; vérifier `redirect_uri` enregistrée côté provider |
| Pool IP épuisé | `sqlite3 /var/lib/wsd/wsd.db "SELECT count(*) FROM ip_allocations"` ; supprimer des workspaces |
