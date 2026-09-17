# Installation et déploiement de Server-com

> Ce document explique uniquement **comment passer de zéro à une mise en production** ; pour les inspections de routine / le dépannage, voir 《日常运维》(06-OPS.md), et pour la protection des données, voir 《备份与恢复》(07-BACKUP.md).
> Les scripts fournis avec ce dépôt se trouvent dans `deploy/`, et la grande majorité des étapes disposent de scripts tout faits (idempotents, réexécutables).
>
> **Chaque étape propose deux méthodes, au choix** : **① utiliser les scripts** (scripts tout faits dans `deploy/`, idempotents, recommandés) —
> **② utiliser les commandes manuelles** (sans aucun script, en tapant les commandes brutes une par une, pour faciliter l'audit et la vérification de chaque étape).
> Les deux méthodes produisent exactement le même résultat ; choisissez-en une selon vos habitudes, il n'est pas nécessaire de faire les deux.

---

## 1. Forme de déploiement et prérequis

L'ensemble du netdisk est un **binaire unique Go** (`netdisk`), avec seulement quatre composants externes qui coopèrent avec lui :

```
Nginx(80/443) ─proxy inverse─▶ netdisk(:8080) ─▶ PostgreSQL 17 (:5432)
                                              ─▶ Redis (:6379)
```

| Composant | Exigence de version | Où l'installer |
|---|---|---|
| netdisk | Produit de la construction du dépôt | Machine locale `/opt/netdisk/` |
| PostgreSQL | **Minimum 17** (voir ci-dessous) | Machine locale |
| Redis | 7+ | Machine locale |
| Nginx | Recommandé 1.26+ | Machine locale (production) |

> **Pourquoi un support minimal de PostgreSQL 17 ?**
> La valeur par défaut de la clé primaire utilise `gen_random_uuid()` (voir le script de migration `00001_init.sql`) — c'est une fonction de génération d'UUID **intégrée depuis PG 13**, donc la limite de version ne se situe pas au niveau de l'UUID. Ce projet fixe le support minimal à **PG 17** (aligné sur la version fournie par les distributions courantes), et le document d'architecture ADR-1 s'y conforme. Sur le terrain, une version 15/16 fonctionnera aussi (gen_random_uuid est compatible), mais la stratégie d'archivage / de support à long terme est maintenue sur la base de 17. Le système ne prend actuellement en charge qu'une seule base de données (pas de connexion à MySQL, etc.).

---

## 2. Installation des logiciels dépendants

> Cette étape installe : l'outil de sauvegarde (borg/rsync), PostgreSQL 17, Redis, Nginx.
> Point clé : **le dépôt système de Debian 13 fournit nativement PostgreSQL 17**, ce qui correspond exactement à la version minimale prise en charge par ce projet ; il suffit de l'installer directement, **aucune source PGDG supplémentaire n'est nécessaire** (si le dépôt système sur le terrain est d'une version inférieure, par ex. Debian 12 fournit la 15, ajoutez alors la source PGDG pour installer la 17 selon les commandes manuelles ci-dessous).

```sh
# Confier au script : installe postgresql-17/redis/nginx/borg → arrête Apache pour libérer 80/443
sudo bash deploy/provision/01-install-packages.sh
```

Le script affiche le numéro de version de chaque composant pour confirmation, et imprime enfin `INSTALL_DONE`.

> **Sans dépendre du script, procéder pas à pas avec les commandes manuelles** (équivalent à `01-install-packages.sh`) :

```sh
# ① Installer PostgreSQL 17 / Redis / Nginx / l'outil de sauvegarde borg
#    (Debian 13 fournit nativement PG17, installez directement ; n'ajoutez la source PGDG que si
#     le dépôt système est trop ancien, voir la note ci-dessous)
sudo apt-get update
sudo apt-get install -y postgresql-17 redis-server nginx borgbackup

# ② Arrêter Apache pour libérer 80/443 (uniquement si Apache est installé sur la machine)
sudo systemctl disable --now apache2 2>/dev/null || echo 'Pas d'Apache, ignoré'
```

> La version script regroupe ces 3 étapes en une seule commande `sudo bash deploy/provision/01-install-packages.sh`, en faisant en plus « l'affichage de confirmation des numéros de version de chaque composant + l'impression de `INSTALL_DONE` ».

> Astuce : la source officielle PGDG est lente dans certaines régions ; vous pouvez utiliser un miroir national
> (par ex. `https://mirror.nju.edu.cn/postgresql/repos/apt`).

---

## 3. Préparation du système (ce que fait cette étape)

> Objectif : créer un compte de service **non connectable**, créer les répertoires du programme et régler leur « propriétaire », générer des mots de passe aléatoires.

```sh
sudo bash deploy/provision/02-provision-base.sh
```

Concrètement, il effectue trois opérations (toutes vérifiables via `ls`) :

1. **Créer le compte de service `netdisk`** : un compte utilisé uniquement pour exécuter le service, non connectable
   (avec les privilèges minimaux ; le programme lit seul le binaire, écrit seul dans son répertoire de données).
2. **Créer les répertoires et régler leur propriétaire** (le plus critique : un mauvais propriétaire fera refuser l'auto-vérification au démarrage du service) :

   | Répertoire | Propriétaire | Contenu |
   |---|---|---|
   | `/opt/netdisk/data` | netdisk | Racine des objets et du temporaire (objets adressés par contenu, temporaire TUS) |
   | `/etc/netdisk` | root:netdisk | Fichier de configuration + secrets |
   | `/var/log/netdisk` | netdisk | Journaux applicatifs |
   | `/var/lib/netdisk` | netdisk | JSON d'état de sauvegarde / d'entraînement de restauration |

3. **Générer le fichier de mots de passe aléatoires `/etc/netdisk/secrets.env`** : génère en une fois le mot de passe de la base, la clé JWT,
   le mot de passe Redis, etc. (**jamais dans le dépôt de versions, jamais dans le yaml**), pour être lu par les scripts et le programme ultérieurs.

> **Sans dépendre du script, procéder pas à pas avec les commandes manuelles** (correspondant une à une aux 3 opérations de `02-provision-base.sh`) :

```sh
# ① Créer le compte de service netdisk non connectable (compte système, sans répertoire personnel, shell verrouillé)
sudo useradd --system --no-create-home --shell /usr/sbin/nologin netdisk

# ② Créer les répertoires et régler leur propriétaire (mauvais propriétaire = refus à l'auto-vérification du démarrage)
sudo install -d -o netdisk -g netdisk -m 0750 /opt/netdisk/data    # racine objets et temporaire
sudo install -d -o root   -g netdisk -m 0750 /etc/netdisk          # configuration + secrets
sudo install -d -o netdisk -g netdisk -m 0750 /var/log/netdisk     # journaux applicatifs
sudo install -d -o netdisk -g netdisk -m 0750 /var/lib/netdisk     # état sauvegarde / entraînement

# ③ Générer le fichier de mots de passe aléatoires secrets.env (d'abord générer la chaîne aléatoire,
#    l'écrire, enfin resserrer les permissions)
DB_PASS=$(openssl rand -base64 24)
REDIS_PASS=$(openssl rand -base64 24)
JWT_SECRET=$(openssl rand -base64 48)
sudo tee /etc/netdisk/secrets.env >/dev/null <<EOF
NETDISK_DB_PASSWORD=$DB_PASS
NETDISK_REDIS_PASSWORD=$REDIS_PASS
NETDISK_JWT_SECRET=$JWT_SECRET
EOF
sudo chown root:netdisk /etc/netdisk/secrets.env
sudo chmod 0640 /etc/netdisk/secrets.env
```

> Les noms de champs doivent suivre les variables d'environnement réellement lues par le programme (référez-vous à `deploy/systemd/secrets.env.example`).
> La version script regroupe ces 3 étapes en une seule commande `sudo bash deploy/provision/02-provision-base.sh`.
> Les mots de passe ne sont générés qu'une fois ; pour les faire tourner, modifiez ce fichier puis redémarrez le service.

---

## 4. Configuration de PostgreSQL

> Cette étape configure PG dans un état « adapté à cette machine + sécurisé » : n'écouter que la machine locale, s'adapter à une petite mémoire,
> activer l'archivage WAL (préalable à la récupération à un point dans le temps), créer le rôle et la base dédiés au programme.

```sh
sudo bash deploy/provision/03-provision-postgresql.sh
```

L'objectif de chaque configuration (tout est écrit dans `/etc/postgresql/17/main/conf.d/`) :

| Configuration | Valeur | Pourquoi |
|---|---|---|
| `listen_addresses` | `localhost` | N'écouter que la machine locale ; le port PG **n'apparaît pas sur la carte réseau** |
| `max_connections` | 100 | Côté application max 32 connexions, laissant de la marge pour l'exploitation/psql |
| `shared_buffers` | 128MB | Adaptation à petite mémoire (sinon les valeurs par défaut pour grande mémoire saturent la RAM) |
| `archive_mode=on` + `archive_command` | — | **Archivage WAL** : préalable à la récupération à un point dans le temps (PITR) ; `pg_wal` ne sature pas le disque |
| pg_hba | Boucle locale uniquement | Refuse la connexion directe du LAN, seul l'accès local est possible |

Enfin, il effectue aussi :
- Créer le rôle `netdisk` et la base `netdisk` (owner=netdisk) ;
- Créer l'extension `pg_trgm` (nécessaire au script de création de tables ; créée à l'avance par le superutilisateur pour éviter les écarts de droits lors de la migration).

> **Sans dépendre du script, procéder pas à pas avec les commandes manuelles** (équivalent à `03-provision-postgresql.sh`) :

```sh
# ① Réglages + activation de l'archivage WAL (écrit dans conf.d, pour éviter d'être écrasé par une
#    future mise à niveau de la grande version)
sudo tee -a /etc/postgresql/17/main/conf.d/netdisk.conf >/dev/null <<'EOF'
listen_addresses = 'localhost'
max_connections = 100
shared_buffers = 128MB
archive_mode = on
archive_command = 'test ! -f /var/lib/postgresql/wal_archive/%f && cp %p /var/lib/postgresql/wal_archive/%f'
EOF
sudo install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/wal_archive

# ② Créer le rôle et la base dédiés au programme (mot de passe pris dans secrets.env du §3)
sudo -u postgres psql <<'EOF'
CREATE ROLE netdisk LOGIN PASSWORD '<mot de passe de la base>';
CREATE DATABASE netdisk OWNER netdisk ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
EOF

# ③ Créer l'extension pg_trgm (créée dans la base par le superutilisateur, pour la migration)
sudo -u postgres psql -d netdisk -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'

# ④ Redémarrer pour prendre effet (le pg_hba par défaut de Debian n'autorise déjà que la machine locale,
#    aucune modification supplémentaire nécessaire)
sudo systemctl restart postgresql
```

> Si le paramétrage régional (locale) de la base sur le terrain n'est pas UTF-8, utilisez impérativement la méthode `TEMPLATE template0` + `LC_COLLATE 'C'` de l'étape ② pour créer la base, afin de garantir un ordre d'octets déterminé, proche de la production Linux.
> La version script regroupe les 4 étapes ci-dessus en une seule commande `sudo bash deploy/provision/03-provision-postgresql.sh` (avec auto-vérification version/connectivité).

---

## 5. Configuration de Redis

> Cette étape configure Redis dans un état « la session ne se perd pas au redémarrage, personne n'est évincé par éviction ».

```sh
sudo bash deploy/provision/04-provision-redis.sh
```

Ce qui est stocké dans Redis n'est pas un cache ordinaire, mais des **tokens / fenêtres de limitation / curseurs de synchronisation** — s'ils sont perdus, c'est comme si « tous les utilisateurs étaient déconnectés ».
Il y a donc **trois exigences strictes**, dont l'absence de l'une causera un problème :

| Exigence | Valeur | Conséquence en cas d'absence |
|---|---|---|
| Définir un mot de passe d'accès | `requirepass <mot de passe>` | Connexion / limitation de débit entièrement hors service |
| Activer la persistance AOF | `appendonly yes` + `appendfsync everysec` | Un redémarrage = tous les utilisateurs déconnectés |
| Interdire l'éviction de clés | `maxmemory-policy noeviction` | Les clés token/curseur évincées = déconnexion aléatoire des utilisateurs |

Le script fait aussi : lier l'écoute à la machine locale (`bind 127.0.0.1`), et après redémarrage tester avec le mot de passe (non authentifié doit être refusé, authentifié peut PING).

> La limite de mémoire à 96MB est une adaptation à petite mémoire ; au plafond, comme c'est `noeviction`, cela **renvoie une erreur** plutôt que de supprimer silencieusement les clés —
> l'erreur expose le problème, l'éviction ne ferait que déconnecter des utilisateurs en silence.

> **Sans dépendre du script, procéder pas à pas avec les commandes manuelles** (équivalent à `04-provision-redis.sh`) :

```sh
# Trois exigences strictes + liaison locale + limite petite mémoire, écrites en bloc ajouté
# à la configuration Redis de Debian
sudo tee -a /etc/redis/redis.conf >/dev/null <<EOF
bind 127.0.0.1
requirepass <mot de passe Redis>
appendonly yes
appendfsync everysec
maxmemory-policy noeviction
maxmemory 96mb
EOF
sudo systemctl restart redis-server

# Test réel : non authentifié doit être refusé, authentifié peut PING
redis-cli -a <mot de passe Redis> --no-auth-warning PING   # doit renvoyer PONG
```

> Les trois exigences strictes sont indispensables (voir le tableau ci-dessus). La version script `sudo bash deploy/provision/04-provision-redis.sh`
> fait en plus un test avec le mot de passe après redémarrage (non authentifié doit être refusé).

---

## 6. Construction et déploiement de netdisk

### 6.1 Construction (l'ordre est important)

```sh
# Environnement (accélérateur national + désactivation de la vérification)
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off
cd Server-com
# ⚠ Le front-end du panneau d'administration est compilé (embed) dans le binaire à la compilation :
#   si le front a été modifié, il faut d'abord pnpm build puis go build ;
#   l'ordre inversé intégrerait d'anciennes pages (compile, mais ne signale pas d'erreur)
deploy/build-release.sh 1.0.0     # → deploy/dist/netdisk-1.0.0-linux-amd64
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<hôte>:/tmp/
```

> **Sans dépendre du script, procéder pas à pas avec les commandes manuelles** (équivalent à `build-release.sh`) :
> L'ordre central ne doit pas être inversé : **d'abord `pnpm build` du front, puis `go build`** (l'ordre inversé intégrerait d'anciennes pages).

```sh
# ① Environnement (accélérateur national + désactivation de la vérification)
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off

# ② D'abord construire le front-end du panneau d'administration (dépôt web), et copier le produit
#    dans Server-com/internal/webui/dist (pour l'embed)
cd Server-com/web && pnpm install && pnpm build
# Le produit en place est ensuite empaqueté dans le binaire par la directive embed
# (web/apps/*/dist → internal/webui/dist)

# ③ Compiler le binaire unique côté serveur
cd ../ && go build -o deploy/dist/netdisk-1.0.0-linux-amd64 ./cmd/netdisk

# ④ Copier vers la machine cible
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<hôte>:/tmp/
```

### 6.2 Déploiement sur la machine cible

```sh
sudo bash deploy/install.sh /tmp/netdisk-1.0.0-linux-amd64 http://<URL-publique>
```

`install.sh` est idempotent, et effectue : création utilisateur/répertoires → génération/réutilisation des secrets → installation du binaire et de la configuration →
installation du service systemd → auto-vérification → démarrage → **auto-preuve de version** (en cas de succès, la dernière ligne imprime `INSTALL_DONE`).

> **Sans dépendre du script, procéder pas à pas avec les commandes manuelles** (équivalent à `install.sh`, compte de service/répertoires/mots de passe déjà prêts au §3) :

```sh
# ① Placer le binaire et la configuration (le propriétaire doit être correct, sinon l'auto-vérification échoue)
sudo install -o root -g netdisk -m 0755 /tmp/netdisk-1.0.0-linux-amd64 /opt/netdisk/netdisk
sudo install -o root -g netdisk -m 0644 deploy/config/config.example.yaml /etc/netdisk/config.yaml
# Si les secrets n'ont pas été générés au §3, il faut en ajouter un (voir §3 ③) ; sinon réutilisez-les, ne pas écraser

# ② Installer l'unité systemd et démarrer (l'unité contient déjà « auto-vérification ExecStartPre → migration+démarrage ExecStart »)
sudo install -o root -g root -m 0644 deploy/systemd/netdisk.service /etc/systemd/system/netdisk.service
sudo systemctl daemon-reload
sudo systemctl enable --now netdisk

# ③ Confirmer qu'il tourne
sudo systemctl is-active netdisk          # → active
sudo systemctl status netdisk -l --no-pager
```

> Point clé : dans `netdisk.service`, `ExecStart=/opt/netdisk/netdisk -migrate ...` signifie « migrer d'abord, puis continuer le démarrage »,
> **ne quitte pas** — en mode manuel, ne le traitez pas non plus comme une commande ponctuelle à attendre au premier plan ; laissez systemd le gérer.

### 6.3 Migration de la base de données

La migration est intégrée dans l'unité systemd, **exécutée automatiquement au démarrage du service**, sans besoin de la lancer manuellement :

```ini
ExecStartPre=/opt/netdisk/netdisk -check -config /etc/netdisk/config.yaml   # auto-vérification au démarrage
ExecStart=/opt/netdisk/netdisk -migrate -config /etc/netdisk/config.yaml    # migrer d'abord, puis continuer le démarrage
```

> Note : la sémantique de `-migrate` est « exécuter la migration puis continuer le démarrage du service », **ne quitte pas** —
> ne le traitez pas comme une commande au premier plan à attendre dans un script de déploiement, sinon cela restera bloqué indéfiniment.

> Si vous **ne voulez pas dépendre de la migration automatique systemd** et souhaitez exécuter la migration séparément avant le démarrage,
> vous pouvez utiliser goose pour lancer un `up` sur la base cible
> (le répertoire de migration se trouve dans `internal/migrate/sql` ; généralement inutile, le démarrage la fait automatiquement) :

```sh
export NETDISK_DB_DSN='postgres://netdisk:<mot de passe de la base>@127.0.0.1:5432/netdisk?sslmode=disable'
goose -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" status   # voir la version d'abord
goose -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up        # exécuter la migration
```

> Les deux méthodes sont équivalentes ; tant que `-migrate` est présent dans l'unité systemd, même sans lancer goose manuellement, la migration se fera au démarrage.

---

## 7. Configuration du proxy inverse Nginx

> Nginx est le seul point d'entrée exposé ; netdisk lui-même n'écoute que sur 127.0.0.1:8080.

```sh
# apply : installe le site / les en-têtes proxy / les réglages ; verify : vérifie ligne par ligne
# avec nginx -T (configuration active)
sudo bash deploy/nginx/apply.sh
sudo bash deploy/nginx/verify.sh
```

**Quelques pièges fréquents** (gérés par le script, expliqués ici) :
- Les interfaces de téléversement/TUS doivent désactiver `proxy_request_buffering`, sinon le corps de la requête atterrit d'abord sur le disque temporaire de Nginx,
  et la reprise de téléversement ainsi que la progression ne seraient plus réels ;
- Les interfaces de téléchargement désactivent `proxy_buffering`, les interfaces SSE désactivent `proxy_buffering` et augmentent `proxy_read_timeout`.

> **Sans dépendre du script, procéder pas à pas avec les commandes manuelles** (équivalent à `apply.sh` + `verify.sh`) :

```sh
# ① Écrire la configuration du site (minimale et fonctionnelle, lignes clés annotées)
sudo tee /etc/nginx/sites-available/netdisk >/dev/null <<'EOF'
server {
    listen 80;
    server_name <domaine public>;

    # Proxy inverse général : transmet l'IP réelle
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }

    # Téléversement TUS : désactiver request_buffering (reprise/progression réels)
    location /uploads/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering off;
    }

    # WebDAV : désactiver aussi request_buffering
    location /dav/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering off;
        proxy_buffering off;
    }

    # SSE : désactiver buffering + augmenter le délai de lecture
    location /sync/events {
        proxy_pass http://127.0.0.1:8080;
        proxy_buffering off;
        proxy_read_timeout 86400s;
    }
}
EOF

# ② Activer le site, vérifier la syntaxe, recharger
sudo ln -sf /etc/nginx/sites-available/netdisk /etc/nginx/sites-enabled/
sudo nginx -t                                  # continuer seulement si la syntaxe est correcte
sudo systemctl reload nginx

# ③ Alternative à verify.sh : auto-vérifier les points clés avec la « configuration active »
#    (et non le fichier sur disque)
sudo nginx -T | grep -E 'proxy_(request_)?buffering|proxy_read_timeout'
```

> En écriture manuelle, l'oubli le plus fréquent concerne les trois réglages `proxy_*` ci-dessus ; leur absence rend le téléversement/reprise/SSE anormaux.
> La version script utilise `deploy/nginx/verify.sh` basé sur `nginx -T` pour affirmer ces points ligne par ligne, vous épargnant la vérification.

---

## 8. Réception après déploiement

```sh
# 1) Chaque point d'entrée est accessible
curl -s -o /dev/null -w '%{http_code}\n' http://<hôte>/admin/    # 200 « Panneau d'administration du netdisk »
curl -s http://127.0.0.1:8080/healthz                            # 200

# 2) L'administrateur initial a été créé automatiquement lors de « l'initialisation de la base »
#    Nom d'utilisateur admin, mot de passe initial admin123 (peut être remplacé avant le premier
#    démarrage via NETDISK_BOOTSTRAP_ADMIN_PASSWORD)
#    Après connexion, modifiez immédiatement le mot de passe (recommandé) :
/opt/netdisk/bin/passwd -config /etc/netdisk/config.yaml \
  -username admin -role super_admin -prompt

# 3) Sonde de bout en bout : connexion directe à l'application (ne pas passer par le proxy inverse
#    — les en-têtes SSE seraient consommés par Nginx)
/opt/netdisk/bin/probesmoke -base http://127.0.0.1:8080 -user admin -pass admin123
```

---

## 9. Prochaines étapes après installation terminée

- Faire une **sauvegarde initiale** et exécuter un entraînement de restauration (voir 《备份与恢复》07-BACKUP.md), confirmer « restaurable » avant la mise en service officielle ;
- Configurer la surveillance pour récupérer `/metrics` et les règles d'alerte (voir 《日常运维》06-OPS.md).
