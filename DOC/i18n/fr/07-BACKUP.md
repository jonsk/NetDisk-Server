# Sauvegarde et restauration de Server-com

> Objectif central en une phrase : **à tout moment, restaurer « base de données + objets » au même point dans le temps**.
> Les scripts accompagnant se trouvent dans `deploy/backup/`, pilotés automatiquement par le minuteur systemd.

---

## 1. Objectif et stratégie de sauvegarde

Le netdisk a deux types de données, **qui doivent pouvoir être restaurées ensemble au même instant** :

1. **Métadonnées** (utilisateurs/espaces/entrées de répertoire/quotas… dans PostgreSQL) ;
2. **Fichiers d'objets** (le véritable contenu, stocké localement sur disque selon le hash du contenu).

Ne sauvegarder que l'un sans l'autre donne un état corrompu « le fichier est là mais la base pointe vers un autre objet » ou « le fichier est entièrement à 0 octet ».

| Données | Méthode | Fréquence | Rétention |
|---|---|---|---|
| PostgreSQL | `pg_dump -Fc` (sauvegarde intégrale quotidienne de la base) + **archivage WAL** (réalise la récupération à un point dans le temps) | Intégrale quotidienne 02:30 ; WAL en temps réel | dump 14 jours |
| Répertoire d'objets `/opt/netdisk/data` | borg (incrémental dédupliqué) | **toutes les 4 heures** | 7 j / 4 sem / 6 mois |
| Redis (AOF) | Entre dans la sauvegarde avec le répertoire d'objets | Idem ci-dessus | Idem ci-dessus |

Deux minuteurs déclenchent automatiquement (tous deux pilotés par `deploy/backup/netdisk-backup.sh`) :

```sh
netdisk-backup.sh full      # Quotidien : auto-contrôle WAL + incrémental objets + PG intégral + vérification lecture + nettoyage rétention
netdisk-backup.sh objects   # Toutes les 4 heures : uniquement incrémental du répertoire d'objets
```

### Trois conceptions « anti-défaillance » dans la sauvegarde

1. **Vérifier la lecture immédiatement après la sauvegarde** : `pg_restore --list <dump>` qui ne s'ouvre pas = échec — une sauvegarde qui n'écrit que sans vérifier n'est qu'un fichier « que vous croyez exister ».
2. **L'archivage WAL regarde « l'échec d'ajout » plutôt que « si c'est zéro »** : `failed_count` est une valeur cumulée, jamais réinitialisée.
   Juger « si c'est 0 » transformerait un échec historique en « la sauvegarde échoue désormais toujours ». On enregistre désormais une base et on alerte uniquement sur **l'incrément**.
3. **Écriture atomique du JSON d'état** (fichier temporaire + `mv`) : évite que Prometheus ne capture un fichier à moitié écrit.
   « L'état de sauvegarde est inconnu » et « la sauvegarde a échoué » sont deux incidents totalement différents.

---

## 2. La sauvegarde peut-elle vraiment restaurer ? — Exercice de restauration

> Sauvegarde ≠ peut restaurer. La véritable vérification est **l'exercice de restauration** : une fois par trimestre, `deploy/backup/netdisk-restore-drill.sh`.

L'exercice fait quatre choses, **dont l'échec d'une seule compte comme échec de l'exercice** :

1. Restaurer la sauvegarde la plus récente **vers une base temporaire indépendante** (`netdisk_drill_<date>`, **ne touche jamais la base de production**) ;
2. **Comparer le nombre de lignes des tables clés** production vs base restaurée (users/spaces/files/file_objects/space_members/sync_feed/audit_logs) ;
3. **Rejeu du répertoire d'objets** : restaurer depuis la sauvegarde vers un répertoire temporaire, comparer le hash de contenu (sha256) de chaque objet vivant ;
4. Écrire le moment de l'exercice, alerter si **plus de 90 jours sans exercice**.

```sh
sudo /opt/netdisk/bin/netdisk-restore-drill.sh     # Enregistré dans /var/backups/netdisk/logs/drill-<date>.log
```

> Il faut conserver au moins un objet vivant, sinon l'exercice ne peut vérifier que « l'archive peut être décompressée », pas que « le contenu peut être rejoué »
> (un fichier restauré à 0 octet « existe » tout autant). Vous pouvez en créer un par téléversement direct WebDAV :
> `curl -u admin:<mot de passe> -T <fichier local> http://127.0.0.1:8080/webdav/<space_id>/<nom de fichier>`
> (le chemin **doit comporter space_id**).

---

## 3. Récupération à un point dans le temps (PITR) : restaurer vers un moment donné

> Prérequis : `archive_mode=on` + `archive_command` normaux (configurés lors de l'installation), et **une sauvegarde physique de base** comme point de départ.

```sh
# ① Prendre une sauvegarde de base (sauvegarde physique, combinée au WAL pour « revenir dans le passé »)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_basebackup -D /var/backups/netdisk/base -Ft -z -X fetch"

# ② Restaurer vers un « répertoire d'instance indépendant » (ne pas écraser les données de production)
install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/17/restore
tar -xzf /var/backups/netdisk/base/base.tar.gz -C /var/lib/postgresql/17/restore

# ③ Écrire l'objectif de restauration (restaurer vers le moment 2026-09-12 19:53)
cat >> /var/lib/postgresql/17/restore/postgresql.auto.conf <<'EOF'
restore_command = 'cp /var/lib/postgresql/wal_archive/%f %p'
recovery_target_time = '2026-09-12 19:53:00+08'
recovery_target_action = 'promote'
EOF
touch /var/lib/postgresql/17/restore/recovery.signal
chown -R postgres:postgres /var/lib/postgresql/17/restore

# ④ Démarrer l'instance de restauration sur un autre port (coexiste avec la production, ne touche pas la production)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_ctl -D /var/lib/postgresql/17/restore \
  -o '-p 5433' -l /tmp/pitr.log start"
su - postgres -c "psql -p 5433 -Atc 'SELECT count(*) FROM files' netdisk"
```

**Le script d'exercice PITR** (`netdisk-pitr-drill.sh`) automatise cela et fait réellement « un voyage dans le temps »,
le critère n'est pas « la base peut démarrer », mais **les données postérieures au moment cible doivent être absentes** :
insérer `before`(T0) → prendre la sauvegarde physique → enregistrer le moment cible T → insérer `after`(T1>T) →
restaurer depuis la sauvegarde vers T → assertion : `before` existe en base, `after` **n'existe pas**.

Points pratiques :
- La sauvegarde de base au format **plain** (`-Fp`, directement un répertoire de données utilisable) est plus simple que `-Ft` (compressé et empaqueté) ;
- Les paramètres de l'instance de restauration tels que `max_connections` doivent être **≥ à ceux du maître**, sinon PG refuse directement la restauration ;
- L'exercice doit se faire sur un **réplica** : après promote, ce répertoire a été écrit, ce n'est plus « la sauvegarde de ce moment »,
  la sauvegarde physique est un artefact, l'exercice ne peut toucher qu'à sa copie.

---

## 4. Rejeu des fichiers d'objets (séparément)

```sh
export BORG_REPO=/var/backups/netdisk/borg BORG_PASSPHRASE=$(cat /etc/netdisk/borg.passphrase)
borg list --last 3 "$BORG_REPO"                  # Voir les derniers
cd /tmp/restore && borg extract "$BORG_REPO::full-2026-09-12T19:53:38"
# Restauré sous la forme /tmp/restore/opt/netdisk/data/objects/xx/yy/<sha256>
```

> **Les objets et la base doivent provenir du même moment** : rejouer seulement les objets sans rejouer la base = « le fichier est là mais la base pointe vers un autre objet » ;
> rejouer seulement la base sans rejouer les objets = « le fichier est entièrement à 0 octet ». Le script d'exercice lie les deux ensemble précisément pour éviter cela.

---

## 5. AOF de Redis

Ce qui est stocké dans Redis sont l'état de connexion et les curseurs (**ce n'est pas un cache** ; les « trois exigences strictes » de la persistance à activer sont dans 05-INSTALL.md §5). Le point de cette section est de sauvegarder également l'AOF :

```sh
redis-cli -a <mot de passe> --no-auth-warning BGREWRITEAOF      # Déclencher manuellement une réécriture AOF
ls -l /var/lib/redis/appendonlydir/                     # AOF et manifest y sont écrits
```

Méthode de sauvegarde : entrer dans borg avec le répertoire d'objets, ou faire un `cp` séparé après `BGREWRITEAOF` quotidien. **Si l'AOF est perdu → tous les utilisateurs se reconnectent** (les données ne sont pas perdues).

---

## 6. Alertes correspondantes

Alertes fortement liées à la sauvegarde/restauration (règles dans `deploy/prometheus/netdisk-alerts.yml`, critères d'indicateurs détaillés dans 06-OPS.md §4) :

- `NetdiskBackupStale` / `NetdiskObjectsBackupStale` : la sauvegarde intégrale/des objets n'a pas réussi au-delà de la fréquence attendue ;
- `NetdiskWalArchiveFailing` : l'archivage WAL échoue continuellement, `pg_wal` sature le disque ;
- `NetdiskRestoreDrillOverdue` : plus de 90 jours depuis le dernier exercice, il est temps de faire un exercice de restauration.

---

## 7. Pas encore fait actuellement (indiqué honnêtement)

- **Sauvegarde physique de base hebdomadaire** (le « point de départ » du PITR). Actuellement `netdisk-backup.sh` ne fait que `pg_dump` (sauvegarde logique),
  et ne produit pas de sauvegarde physique de base — donc la portée du « retour dans le passé » est limitée à la sauvegarde de base laissée par l'exercice manuel.
  Il est recommandé d'ajouter un **minuteur `pg_basebackup` hebdomadaire** avant la mise en service (conserver 4 exemplaires).
- **Réplica hors site**. Actuellement toutes les copies de sauvegarde sont sur le même disque, **si le disque casse = données et sauvegarde perdues ensemble**,
  c'est le plus grand point de défaillance unique de la solution actuelle. Il est recommandé d'ajouter un dépôt borg distant ou une copie de stockage objet, pour synchroniser les sauvegardes hors de la machine.
