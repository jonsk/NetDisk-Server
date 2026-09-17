# Exploitation courante de Server-com

> Ce document explique **comment gérer après la mise en ligne** : inspection de routine, démarrage/arrêt, mise à niveau, surveillance et alertes, dépannage des incidents courants.
> Pour l'installation initiale, voir 《安装与部署》(05-INSTALL.md) ; pour la protection des données, voir 《备份与恢复》(07-BACKUP.md).

---

## 1. Inspection de routine

Voici une liste d'inspection que l'on peut suivre directement, à passer en revue chaque jour/semaine.

```sh
# ① Les services et dépendances tournent tous
systemctl is-active postgresql redis-server netdisk nginx

# ② Voir si des anomalies apparaissent dans les journaux
journalctl -u netdisk -n 50 --no-pager

# ③ La sauvegarde a-t-elle réussi récemment (critique ! voir 07-BACKUP.md)
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_backup_last_success_timestamp_seconds'

# ④ Le disque est-il suffisant (refus du téléversement au-delà de 90 %)
df -h /opt/netdisk/data /var/lib/postgresql /var/backups/netdisk
```

| Point de contrôle | Quand c'est normal | En cas d'anomalie |
|---|---|---|
| Tous les services actifs | Les 4 sont actifs | Voir §6 Dépannage |
| Les données PG augmentent et le répertoire d'archivage WAL aussi | L'archivage avance normalement | Archivage bloqué = `pg_wal` sature le disque |
| Dernière sauvegarde < 26h | L'horodatage est récent | Consulter immédiatement le journal de sauvegarde |
| Patrouille d'objets en cours | `netdisk_object_patrol_last_timestamp_seconds` < 48h par rapport à maintenant | Patrouille à l'arrêt = fuite/perte sans surveillance |
| Niveau d'eau disque < 90 % | — | ≥90 % refus des nouveaux téléversements (retour 507) |

---

## 2. Ordre de démarrage / arrêt

> L'ordre a son importance : **base de données d'abord, application ensuite, proxy inverse en dernier** ; à l'arrêt, l'inverse.

```sh
# Démarrage
systemctl start postgresql
systemctl start redis-server
systemctl start netdisk      # complète automatiquement la migration de la base au démarrage
systemctl start nginx
systemctl is-active postgresql redis-server netdisk nginx

# Arrêt (d'abord Nginx puis l'application, pour vider proprement les téléversements/téléchargements en cours)
systemctl stop nginx
systemctl stop netdisk
systemctl stop redis-server
systemctl stop postgresql
```

---

## 3. Mise à niveau et retour arrière

### 3.1 Mise à niveau du binaire netdisk (courante)

```sh
deploy/build-release.sh <nouvelle version>
scp deploy/dist/netdisk-<nouvelle version>-linux-amd64 root@<hôte>:/tmp/
# Recommandé : passer par install.sh (idempotent, arrête automatiquement l'ancien processus → auto-vérification → démarrage → auto-preuve de version)
sudo bash deploy/install.sh /tmp/netdisk-<nouvelle version>-linux-amd64 http://<URL publique>
```

> Retour arrière = réinstaller l'**ancien** binaire puis redémarrer (les migrations sont ajoutées de façon rétrocompatible, généralement sans besoin de rétrograder la base).
> Si le service a été arrêté par limitation suite à des crashes répétés (`systemctl start` renvoie « Start request repeated too quickly »),
> après avoir corrigé la cause racine, faites d'abord `systemctl reset-failed netdisk` puis démarrez.

### 3.2 Mise à niveau de la grande version PostgreSQL (ex. 17→18)

> La base de référence locale est 17 ; si le terrain passe à une version majeure supérieure, utilisez `pg_upgrade`. **Une sauvegarde vérifiée restaurable est obligatoire avant la mise à niveau**,
> car en cas d'échec de `pg_upgrade`, les deux instances (ancienne et nouvelle) risquent de ne plus démarrer.

```sh
systemctl stop netdisk                       # d'abord arrêter l'application, pour éviter les écritures pendant la mise à niveau
su - postgres -c "/usr/lib/postgresql/17/bin/pg_dumpall > /var/backups/netdisk/pre-upgrade.sql"
apt-get install -y postgresql-18             # installer la nouvelle version (source PGDG)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_upgrade \
  --old-datadir=/var/lib/postgresql/17/main --new-datadir=/var/lib/postgresql/18/main \
  --old-bindir=/usr/lib/postgresql/17/bin --new-bindir=/usr/lib/postgresql/17/bin --check"
# Après validation du --check, retirez --check et relancez, puis re-collectez les statistiques
su - postgres -c "/usr/lib/postgresql/17/bin/vacuumdb --all --analyze-in-stages"
```

---

## 4. Surveillance et alertes

- **Point d'exposition** : `GET /metrics` (format texte Prometheus, collecteur **maison**).
  Par défaut, seule la **boucle locale / un proxy de confiance** est autorisée à récupérer ; pour une récupération inter-machine, configurez `NETDISK_METRICS_TOKEN` et transmettez `Authorization: Bearer <token>`.
- **Récupération** : `deploy/prometheus/prometheus.yml` (intervalle 30s par défaut).
- **Règles d'alerte** : `deploy/prometheus/netdisk-alerts.yml` (une dizaine de règles), vérifiées avec `promtool check rules`.

**Les indicateurs les plus à surveiller** (signification + critère) :

| Indicateur | Signification | Quand intervenir |
|---|---|---|
| `netdisk_process_resident_memory_bytes` | Mémoire résidente du processus | Proche de la limite (320M) = tué et redémarré par systemd |
| `netdisk_disk_used_percent` | Niveau d'eau disque de la zone d'objets | ≥90 % refus des téléversements ; `>85` alerter d'abord |
| `netdisk_backup_last_success_timestamp_seconds` | Moment du dernier succès de sauvegarde | `now()-lui > 26h` alerter (critical) |
| `netdisk_object_missing_total` | Nombre d'objets présents en base mais absents sur disque | **≥1 = donnée illisible** (critical) |
| `netdisk_object_leak_bytes` | Présents sur disque mais absents en base (occupent de l'espace) | >64MiB = disque qui ne fait que grossir |
| `netdisk_quota_drift_bytes` | Écart entre le quota comptable et l'usage réel | >1MiB = bug suspecté dans le calcul du quota |
| `netdisk_restore_drill_last_timestamp_seconds` | Moment du dernier entraînement de restauration | Alerter si >90 jours sans entraînement |

> **Principe important** : un indicateur illisible **n'émet pas d'échantillon** plutôt que de renvoyer 0 (renvoyer 0 ferait mentir l'alerte).
> Par exemple, en cas d'échec de détection de la capacité disque, les trois courbes de capacité disparaîtront entièrement — c'est une « illisibilité » normale, non un disque plein.

---

## 5. Sécurité réseau et points clés des permissions

- `/metrics` renvoyant **401** est un **comportement voulu** : il n'autorise que la boucle locale / un proxy de confiance. La récupération inter-machine doit impérativement transporter un Bearer token.
- Les clés ne vont jamais dans le yaml / le dépôt de versions, ne résident que dans `/etc/netdisk/secrets.env` (rotation = modifier ce fichier + redémarrer le service).
- PostgreSQL n'écoute que la boucle locale, Redis n'écoute que la boucle locale : les ports de ces deux services **ne doivent pas apparaître sur la carte réseau**.

---

## 6. Dépannage des incidents courants (runbook)

Chaque point est rédigé sous la forme **symptôme → que regarder d'abord → causes courantes et traitement**, les commandes peuvent être collées directement.

### 6.1 Le service ne démarre pas

```sh
systemctl status netdisk -l
journalctl -u netdisk -n 60 --no-pager
```

| Symptôme | Signification | Traitement |
|---|---|---|
| `ExecStartPre ... status=1/FAILURE` | **Bloqué par l'auto-vérification au démarrage** (config/répertoire/port/dépendance) | Consulter la liste complète des problèmes qu'il affiche d'un coup, et les corriger un par un |
| `active (running)` mais requête 502 | Le service tourne, mais l'adresse d'écoute ne correspond pas à celle pointée par Nginx | Confronter `http_addr` de la configuration et l'upstream Nginx |
| `Start request repeated too quickly` | Crashes répétés ayant déclenché la limitation | Après correction de la cause racine, `systemctl reset-failed netdisk` |

Deux types courants d'erreur d'auto-vérification :
- **Répertoire non inscriptible** : `chown -R netdisk:netdisk /opt/netdisk/data` (si `objects/` ou `tus-tmp/` ont déjà été créés manuellement en root, leur propriétaire est root).
- **Système de fichiers en lecture seule** : le service est verrouillé par `ProtectSystem=strict` — si vous avez modifié un chemin dans les secrets, vous devez **modifier de façon synchrone `ReadWritePaths` de l'unité systemd** pour y ajouter le nouveau chemin.

### 6.2 Téléversement/téléchargement bloqué à 0 %

```sh
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_(http|tus|sse)'
tail -f /var/log/nginx/netdisk.access.log | grep -E 'rt=|urt='
```

- `urt=` (temps en amont) bien inférieur à `rt=` (temps total) → le temps passe dans Nginx → vérifier si TUS/WebDAV a désactivé `proxy_request_buffering` (sinon le corps de la requête atterrit d'abord sur le disque temporaire de Nginx, et progression/reprise ne sont plus réels).
- Téléchargement lent → vérifier `proxy_buffering off` sur l'interface de téléchargement.
- Événements SSE avec un délai de dizaines de secondes → Nginx a mis en tampon le flux d'événements → confirmer `proxy_buffering off` + `proxy_read_timeout 86400s` sur l'interface SSE.
  (Ces trois points font l'objet d'assertions dans `deploy/nginx/verify.sh`.)

### 6.3 Tous les utilisateurs déconnectés / déconnexion immédiate après connexion

```sh
redis-cli -a <mot de passe> --no-auth-warning CONFIG GET appendonly appendfsync maxmemory-policy
```

- Tout élément non satisfait est anormal — les **trois exigences strictes** (`requirepass` / `appendonly yes`+`everysec` / `maxmemory-policy noeviction`)
  et la conséquence de chaque omission sont détaillés dans 05-INSTALL.md §5 ; corrigez point par point selon le tableau, puis redémarrez Redis.
- À retenir : ce qui est dans Redis **n'est pas un cache** ; ce qui est perdu, ce sont l'état de connexion et les curseurs, non les données de fichiers, et cela n'affecte pas les fichiers déjà sur disque.

### 6.4 La sauvegarde n'a pas réussi

```sh
systemctl status netdisk-backup.service
journalctl -u netdisk-backup -n 40 --no-pager
cat /var/lib/netdisk/backup-status.json
su - postgres -c "psql -Atc 'SELECT archived_count, failed_count FROM pg_stat_archiver' netdisk"
ls -l /var/lib/postgresql/wal_archive | tail -3
```

- **Échec d'archivage WAL** → dans neuf cas sur dix, le répertoire d'archivage est injoignable : le répertoire d'archivage **ne doit pas** se trouver sous `/var/lib/netdisk`
  (c'est `netdisk:netdisk 0750`, l'utilisateur postgres n'a pas le droit de le traverser), il doit être placé dans `/var/lib/postgresql/wal_archive`
  (l'arborescence propre à postgres).
- **`pg_dump` Permission denied** → le répertoire de dump doit appartenir à **postgres** (il s'exécute en tant que postgres).

### 6.5 Alerte de patrouille d'objets

L'audit est **en lecture seule**, ne répare jamais automatiquement — **ne supprimez pas manuellement d'objets, n'exécutez pas de script de nettoyage avant d'avoir compris la cause**.
En cas d'alerte `object_missing`, procédez au rejeu d'objets selon 07-BACKUP.md pour localiser.

### 6.6 Disque bientôt plein

```sh
du -sh /opt/netdisk/data /var/backups/netdisk /var/lib/postgresql/wal_archive /var/log
```

Trois endroits augmentent : répertoire d'objets, sauvegardes, archivage WAL. **N'effacez pas** l'archivage WAL pour libérer de l'espace
(c'est toute la capacité de récupération à un point dans le temps). Ordre : d'abord synchroniser la sauvegarde hors de la machine, puis supprimer les anciens dumps locaux (le script conserve 14 jours),
puis envisager de laisser la fenêtre de recyclage de 24h de l'application nettoyer les tombes des objets supprimés (**ne supprimez pas manuellement**).

### 6.7 Dépannage le plus rapide pour « rien n'a été fait mais ça ne passe pas »

```sh
ss -tlnp | grep -E '80|8080|5432|6379'      # 1. qui écoute, où écoute-t-on
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz   # 2. contourner le proxy, connexion directe
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1/admin/         # 3. puis via le proxy inverse
journalctl -u netdisk -n 20 --no-pager      # 4. voir le journal de l'application (client_ip transmis ?)
nginx -T | grep -A5 'location /tus'         # 5. voir la configuration « active » du proxy inverse (pas le fichier sur disque)
```

La différence entre les étapes 2 et 3 permet de situer immédiatement le problème du côté « application » ou « proxy inverse » ;
à l'étape 4, si `client_ip` affiche `127.0.0.1` alors que le client réel est sur une autre machine, cela signifie que `X-Real-IP` n'est pas transmis ou que le proxy de confiance est mal configuré.
