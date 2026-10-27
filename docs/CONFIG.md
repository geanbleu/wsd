# Référence de configuration — wsd

Trois fichiers, tous en JSON strict (pas de commentaires) :

| Fichier | Lu par | Mode |
|---|---|---|
| `/etc/wsd/wsd.json` | `wsd` au démarrage | 0640 |
| `/etc/wsd/wsproxy.json` | `wsproxy` au démarrage | 0640, contient le secret HMAC |
| `/etc/wsd/secrets.json` | `wsd` à chaque création/resume | 0600, contient les clés API |

Un changement de configuration demande un `systemctl restart wsd` ou
`systemctl restart wsproxy` : rien n'est rechargé à chaud. `secrets.json` et le
template opencode sont l'exception — ils sont relus à chaque création ou resume
de workspace.

Les exemples de référence sont dans `deploy/` et installés par
`deploy/install.sh`.

---

## Nom de domaine

wsd et wsproxy construisent tous les deux l'hôte d'un workspace comme
`<id>.ws.<domain>` : le label `ws` est **ajouté par le code**. Le champ `domain`
doit donc porter le domaine de base, sans `ws.` :

```json
"domain": "example.internal"
```

donne `https://<id>.ws.example.internal`. La même valeur doit figurer dans
`wsd.json` (pour les liens produits par `wsctl token`) et dans `wsproxy.json`
(pour extraire l'`<id>` du `Host`). Un certificat wildcard
`*.ws.example.internal` couvre tous les workspaces.

---

## wsd.json

### Chemins et binaires

| Champ | Défaut | Description |
|---|---|---|
| `data_dir` | **requis** | Racine des données : `images/`, `workspaces/`, `goldens/`, `wsd.db`. |
| `jailer_dir` | **requis** | Base des chroots jailer. Doit être sur le **même filesystem** que `data_dir`, qui doit supporter `FICLONE` : XFS `reflink=1` ou btrfs. Préparation du stockage : [OPS.md § Stockage](OPS.md#stockage) et `deploy/setup-storage.sh`. |
| `firecracker_bin` | **requis** | Chemin du binaire firecracker. |
| `jailer_bin` | **requis** | Chemin du binaire jailer. |
| `socket_path` | `/run/wsd/wsd.sock` | Socket unix de l'API. Le répertoire est créé en 0750. |
| `socket_group` | `wsd` | Groupe propriétaire du socket, qui est en 0660. C'est ce qui permet à wsproxy, membre du groupe, de joindre wsd alors que wsd tourne en root. Si le groupe n'existe pas, wsd le signale et continue : wsproxy ne pourra pas se connecter. Mettre `""` pour ne rien changer au groupe. |
| `image_id` | — | Nom du répertoire sous `data_dir/images/` contenant `rootfs.ext4` et `vmlinux`. Utilisé pour les boots à froid et la construction du golden. |

### Isolation

| Champ | Défaut | Description |
|---|---|---|
| `uid_range` | **requis** | `[min, max]`. jailer reçoit un uid/gid dédié par workspace, calculé comme `min + index du bloc IP`. La plage doit être libre et assez large pour le pool d'IP. |

### VM (`vm`)

| Champ | Défaut | Description |
|---|---|---|
| `vcpus` | `2` | vCPU par VM. |
| `mem_mib` | `2048` | Mémoire invitée. Le cgroup `memory.max` est réglé à cette valeur + 256 MiB de marge. |
| `workspace_disk_gib` | `20` | Taille du disque `/workspace`, fichier sparse. Fixée à la construction du golden : la changer ensuite impose de reconstruire le golden. Le redimensionnement est hors v1. |
| `cpu_max` | `"<vcpus × 100000> 100000"` | Valeur brute du `cpu.max` cgroup v2 : `<quota> <période>` en µs. `"200000 100000"` = 2 CPU pleins. `""` désactive la limite. |

### Réseau (`net`)

| Champ | Défaut | Description |
|---|---|---|
| `pool` | **requis** | Pool d'où sont découpés les /30 veth, ex. `10.200.0.0/16`. L'IPAM est en SQLite ; la taille du pool plafonne le nombre de workspaces. |
| `egress_iface` | — | Interface de sortie du host, cible du masquerade. À adapter au host (`ip route get 1.1.1.1`). |
| `dns` | — | Résolveur autorisé depuis les VM, et annoncé à l'invité. |

`egress_iface` et `dns` ne sont pas validés au démarrage, alors qu'ils sont tous
les deux indispensables : laissés vides, ils produisent des règles `nft`
invalides et la première création de workspace échoue, pas le démarrage de wsd.

L'intérieur de la VM est fixe et identique partout, parce que le snapshot le
contient : `eth0` en `172.16.0.2/30`, passerelle `172.16.0.1` (le tap `tap0` du
netns), code-server sur le port 8080.

### Egress (`egress.allow`)

Liste de règles `{"cidr": ..., "ports": [...]}`. La politique de la chaîne
forward du netns est `drop`. Sont autorisés : l'ESTABLISHED/RELATED, le DNS vers
`net.dns`, puis ces règles. Sont interdits explicitement et quoi qu'il arrive :
`169.254.0.0/16` (métadonnées cloud), le pool des autres workspaces, et les IP
du host autres que son bout de veth.

```json
"egress": {
  "allow": [
    {"cidr": "0.0.0.0/0", "ports": [443]},
    {"cidr": "192.0.2.10/32", "ports": [8080]}
  ]
}
```

### Divers

| Champ | Défaut | Description |
|---|---|---|
| `idle_timeout` | `30m` | Durée d'inactivité au bout de laquelle wsd suspend un workspace `running`. Format `time.ParseDuration` (`45m`, `2h`). Le reaper tourne chaque minute. Une valeur invalide retombe silencieusement sur 30 min. |
| `domain` | — | Domaine de base, cf. plus haut. Sert aux liens de `wsctl token`. |

### opencode (`opencode`)

| Champ | Défaut | Description |
|---|---|---|
| `config_template` | `""` | Template Go `text/template` rendu puis poussé dans la VM en `/home/coder/.config/opencode/opencode.json` (0600, propriétaire `coder`). Vide = aucune config opencode n'est poussée. |
| `secrets_file` | `""` | Fichier JSON plat, dont le contenu sert de données au template. |

Les deux fichiers sont relus à **chaque** création et resume : un changement de
clé API est pris en compte sans redémarrer wsd, et aucun secret ne se retrouve
dans le golden. Si `config_template` désigne un fichier absent, toute création de
workspace échoue — laisser `""` tant qu'opencode n'est pas configuré.

Exemples installés : `/etc/wsd/opencode.json.tmpl.example` et
`/etc/wsd/secrets.json.example`. Pour activer :

```bash
cp /etc/wsd/opencode.json.tmpl.example /etc/wsd/opencode.json.tmpl
cp /etc/wsd/secrets.json.example       /etc/wsd/secrets.json
chmod 0600 /etc/wsd/secrets.json
# renseigner endpoint, api_key, model dans secrets.json
# puis dans wsd.json :
#   "opencode": {"config_template": "/etc/wsd/opencode.json.tmpl",
#                "secrets_file":    "/etc/wsd/secrets.json"}
systemctl restart wsd
```

#### Schéma : attention à la majeure d'opencode

`https://opencode.ai/config.json` décrit le schéma **V2**, alors que la série
1.x épinglée dans `versions.env` est un opencode **V1**. Les deux formes ne sont
pas interchangeables, et l'écart est piégeux :

| Champ | V1 (série 1.x, épinglée) | V2 |
|---|---|---|
| provider | `provider` (singulier) | `providers` |
| permissions | `permission` (singulier) | `permissions` |

Donné à opencode 1.19.4, un `permissions` est **rejeté** (« V2 permissions are
not supported by OpenCode V1 ») et un `providers` est accepté puis
**silencieusement ignoré** — l'endpoint LLM n'est alors jamais configuré, sans
le moindre message. Le template fourni utilise la forme V1.

Forme V1 des permissions : un objet par outil (`read`, `edit`, `glob`, `grep`,
`list`, `bash`, `task`, `webfetch`…) dont la valeur est `"allow"`, `"ask"`,
`"deny"`, ou un objet motif → action, par exemple
`"bash": {"git status": "allow", "*": "ask"}`.

Après toute modification du template, le vérifier avec le binaire de la version
épinglée, qui valide la config et affiche ce qu'il en a réellement retenu :

```bash
opencode debug config --pure
```

Un `provider` absent de la sortie, ou un `apiKey` qui n'apparaît pas masqué en
`***`, signale que le bloc n'a pas été pris en compte.

#### Le provider est téléchargé à l'exécution

`npm: "@ai-sdk/openai-compatible"` est récupéré par opencode **dans la VM**, au
premier appel au modèle, pas à la construction de l'image. L'egress du workspace
doit donc laisser passer le registre npm (`registry.npmjs.org`, port 443) en
plus de l'endpoint LLM. Avec l'`egress.allow` permissif de l'exemple
(`0.0.0.0/0:443`) c'est transparent ; avec une liste blanche serrée, opencode
reste muet et l'appel au modèle échoue. opencode télécharge de la même façon ses
outils annexes dans `~/.cache/opencode/bin`.

Le serveur opencode écoute dans l'invité sur `127.0.0.1:4096`
(`image/guest/opencode.service`). Le port **doit** être passé explicitement :
`opencode serve` sans `--port` écoute sur un port aléatoire.

---

## wsproxy.json

| Champ | Défaut | Description |
|---|---|---|
| `domain` | **requis** | Domaine de base. Un `Host` qui ne finit pas par `.ws.<domain>` reçoit un 404. |
| `listen` | `:8080` | Adresse d'écoute HTTP. wsproxy ne termine pas TLS : le mettre derrière HAProxy, nginx ou un équivalent. |
| `wsd_socket` | `/run/wsd/wsd.sock` | Socket de l'API wsd. Doit correspondre à `socket_path` de `wsd.json`. |
| `hmac_secret` | **requis** | Clé de signature des cookies d'auth. `install.sh` en génère une de 32 octets à la première installation. La changer invalide toutes les sessions. |
| `cookie_name` | `wsd_auth` | Nom du cookie d'auth. |
| `cookie_ttl` | `24h` | Durée de validité du cookie. |

### OIDC (`oidc`)

Laisser `issuer` vide pour rester en auth par token (`wsctl token <id>`).
Renseigné, OIDC devient le mode d'authentification des navigateurs.

| Champ | Description |
|---|---|
| `issuer` | URL de l'émetteur, ex. `https://accounts.google.com`. La découverte se fait au démarrage : si elle échoue, wsproxy ne démarre pas. |
| `client_id`, `client_secret` | Identifiants de l'application. |
| `external_scheme` | `https` par défaut. Schéma utilisé pour construire le `redirect_uri`, qui est `<scheme>://<id>.ws.<domain>/_oidc/callback`. À déclarer chez le fournisseur — un fournisseur qui n'accepte pas les wildcards demande une entrée par workspace, ou un domaine de callback unique en reverse proxy. |

---

## versions.env

Versions épinglées, lues par `image/build.sh`, `kernel/build.sh` et
`deploy/fetch-deps.sh`. Une copie de référence est installée en
`/etc/wsd/versions.env`, mais ce sont les builds qui lisent le fichier du dépôt.

| Variable | Usage |
|---|---|
| `FIRECRACKER_VERSION`, `JAILER_VERSION` | Doivent être identiques : les deux binaires viennent de la même release GitHub. `fetch-deps.sh` refuse de continuer sinon. Sert aussi à choisir la config kernel de référence. |
| `CODE_SERVER_VERSION` | `.deb` installé dans le rootfs invité. Vérifier les flags de code-server à chaque montée de version. |
| `OPENCODE_VERSION` | Binaire installé dans le rootfs invité. Vérifier le nom de l'asset de release et le schéma de config. |
| `GO_VERSION` | Toolchain Go installée dans l'invité. |
| `KERNEL_VERSION` | Kernel LTS invité compilé par `kernel/build.sh`. |

Le swagger de la version exacte de Firecracker est déposé par `fetch-deps.sh`
dans `/usr/share/doc/wsd/firecracker_spec-<version>.yaml` : c'est la référence à
consulter avant de toucher aux appels de l'API Firecracker.

---

## Limites connues

- `wsctl token` produit un lien en `http://`, sans tenir compte de
  `external_scheme` de wsproxy. Derrière un terminateur TLS, remplacer le
  schéma à la main dans le lien.
- `idle_timeout` est global : pas de valeur par workspace.
- `workspace_disk_gib` est figé par le golden ; agrandir un disque workspace
  existant est hors v1.
