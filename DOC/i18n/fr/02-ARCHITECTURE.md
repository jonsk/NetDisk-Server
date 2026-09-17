# Document de conception architecturale de Server-com

> Version : fourni avec l'édition communautaire de Server-com
> Liens : `DOC/05-INSTALL.md` (installation/déploiement), `DOC/06-OPS.md` (exploitation courante), `DOC/07-BACKUP.md` (sauvegarde/restauration), `DOC/08-TEST.md` (document de test), `DOC/04-API_GUIDE.md` (interfaces), `DOC/03-CODE_READING_GUIDE.md` (guide de lecture du code)

---

## 1. Vue d'ensemble et positionnement

`Server-com` est le **serveur d'un netdisk d'entreprise**, écrit en **Go**, compilé à terme en **un seul fichier exécutable** (binaire unique).
Il fournit des espaces de partage d'équipe, un téléversement multi-protocoles (TUS / WebDAV / multipart), la finalisation avec gestion de versions,
la perception de synchronisation en temps réel, des permissions fines et une gouvernance des quotas ; il peut être déployé comme netdisk privé
ou servir de hub de fichiers pour l'entreprise.

L'édition communautaire (Server-com) ne conserve que la **connexion par mot de passe + le panneau d'administration**, et est un sous-ensemble fonctionnel de l'édition commerciale (Server-Ent).

**Idée centrale : l'ensemble du service est une « grande boîte », autour de laquelle quatre composants coopèrent.**

## 2. Architecture globale

### 2.1 Composition du déploiement (le quatuor)

| Composant | Rôle | Remarques |
|---|---|---|
| **netdisk** (produit de ce dépôt) | Reçoit les requêtes HTTP, traite les activités métier, lit et écrit les métadonnées et les objets | Un seul binaire prend en charge REST / WebDAV / TUS / SSE / le panneau d'administration intégré |
| **PostgreSQL 17 (minimum pris en charge)** | Unique moteur de métadonnées (tous les « entrées de répertoire / utilisateurs / espaces / quotas » s'y trouvent) | Inclut l'archivage WAL pour la récupération à un point dans le temps. **Minimum 17** : la valeur par défaut de la clé primaire utilise `gen_random_uuid()` (intégré depuis PG 13), le plancher n'est donc pas fixé par l'UUID ; minimum 17 (aligné sur les versions des distributions). Exigences de version voir 05-INSTALL.md §1 |
| **Redis** | Sessions (token) / fenêtres de limitation de débit / curseur du flux de changements / file de tâches | **Ce n'est pas un cache** : si les tokens sont perdus, tous les utilisateurs sont déconnectés |
| **Nginx** | Proxy inverse, un seul port exposé | Nécessaire uniquement en production ; en développement, on peut accéder directement à l'application |

```
                       ┌──────────────────────────────────────┐
  Panneau admin /admin ─▶│                                      │
  Client WebDAV  ─WebDAV▶│        netdisk (binaire unique Go)    │
  Chargeur TUS      ─TUS─▶│   Entrée unifiée REST · TUS · WebDAV · SSE  │
  Client de synchro bureau  ─SSE──▶│                                      │
                       │   ┌──────────────────────────────┐   │
                       │   │ finalize : unique chemin d'écriture + verrou d'objet   │   │
                       │   │ adressage par contenu → stockage d'objets sur disque local    │   │
                       │   └──────────────────────────────┘   │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(proxy) │
               │  métadonnées   │  │sessions/limite│   │  port unique    │
               └──────────┘  └────────┘   └────────────┘
```

### 2.2 Pourquoi un binaire unique + un front-end intégré

Le front-end du panneau d'administration, après avoir été construit par le dépôt web, voit ses productions **intégrées à la compilation** dans le binaire Go (`internal/webui/dist/`),
et est servi directement par Go via `embed` (`/admin`). Avantage : il suffit de distribuer un seul fichier, sans déployer séparément un site statique front-end sur le serveur.

> ⚠️ Par conséquent, **modifier le front-end impose de recompiler le binaire Go** : d'abord `pnpm build` puis `go build`,
> l'ordre inverse intégrerait d'anciennes productions (la compilation réussit, mais la page est l'ancienne version, sans erreur).

## 3. Architecture en couches (répartition des paquets internal)

Le code est divisé en 5 couches selon les responsabilités ; une couche supérieure ne peut dépendre que d'une couche inférieure (vérifié mécaniquement par `depsguard`) :

```
┌─────────────────────────────────────────────────────────────┐
│ 4. Couche d'entrée   cmd/  (netdisk / migrate / passwd / depsguard …)  │
├─────────────────────────────────────────────────────────────┤
│ 3. Couche d'interface   internal/api/  (gestionnaire HTTP, routage, middlewares)          │
│            internal/webdavfs/ webdavauth/                     │
├─────────────────────────────────────────────────────────────┤
│ 2. Couche métier   usersvc/orgsvc/spacesvc/filesvc/sharesvc/ │
│            uploadsvc/finalize/lifecycle/syncfeed/syncsse/    │
│            patrol/quotareconcile/consistency/credentials/    │
├─────────────────────────────────────────────────────────────┤
│ 1. Domaine/données  repo/(accès aux données) model/(modèles de domaine) objlock/ storage/  │
│            authsvc/ cache/ auth/ apierr/reqctx/middleware/    │
├─────────────────────────────────────────────────────────────┤
│ 0. Socle      config/ db/ migrate/ ratelimit/ namepolicy/      │
│            dirops/ fastupload/ condreq/ webui/ obs/ syncssem  │
└─────────────────────────────────────────────────────────────┘
```

**Discipline (cœur des lignes rouges R-xx)** : les « paquets noyau » tels que `objlock` (verrous), `storage` (stockage d'objets), `model` (modèles de domaine)
**ne doivent en aucun cas dépendre de `internal/api` (couche HTTP)**. La logique métier doit être découplée de HTTP,
afin que les règles métier restent identiques même en changeant de mode d'accès (WebDAV/TUS/appel interne).

## 4. Mécanismes de conception clés

### 4.1 finalize —— unique chemin d'écriture

Tous les téléversements (fragments TUS, WebDAV PUT, multipart direct) convergent finalement vers **la même logique de finalisation** `finalizeUpload()` :
1. Vérifie d'abord le téléversement instantané (si le hachage de contenu existe déjà, on le réutilise directement, pour éviter l'« empoisonnement ») ;
2. Écrit le contenu dans la zone d'objets et obtient l'objet **adressé par contenu** ;
3. Dans une transaction courte, écrit l'entrée de répertoire et le compteur de références ;
4. Écrit le flux de changements (sync_feed) pour déclencher la perception de synchronisation.

Avantage : les trois canaux de téléversement ont un comportement strictement identique, éliminant la faille « un canal contourne une règle ».

### 4.2 Stockage d'objets adressés par contenu

Les objets sont stockés selon leur **hachage de contenu**, avec un chemin de la forme `objects/xx/xx/<sha256>` :
- **Un même contenu n'est stocké qu'une seule fois** (déduplication naturelle) : si deux utilisateurs téléversent le même fichier, une seule copie est présente sur le disque ;
- Le nom de fichier est séparé du contenu : renommer un fichier ne copie pas les données, seules les métadonnées changent ;
- Objet immuable : le contenu finalisé n'est plus réécrit, ce qui élimine la « surcharge silencieuse / dérive de fichier ».

### 4.3 Verrouillage d'objet à double niveau (ADR-2)

Pour éviter qu'une donnée soit corrompue si plusieurs requêtes finalisent/suppriment simultanément le même objet, on introduit un verrou au niveau objet :

| Niveau | Usage | Mode de détention du verrou |
|---|---|---|
| **Verrou de session** | Pendant toute la finalisation, worker de cycle de vie (le véritable chemin d'écriture) | Une connexion de base de données dédiée unique traverse tout le processus ; la détention a un plafond, évitant un trop long secteur critique |
| **Verrou de transaction** | Simple incrément de référence (+1) (copie, partage vers un espace) | Détenu uniquement au sein d'une transaction courte |

La clé de verrou est déterminée par le hachage de l'objet ; l'ordre de verrouillage suit l'ordre croissant des hachages, évitant les interblocages.

### 4.4 Synchronisation à double état (SSE + curseur)

Le client de bureau a besoin de « savoir dès que le distant change » :
- **Poussée SSE** (`/sync/events`) : pousse les changements en temps réel aux clients en ligne (rapide, mais peut perdre des trames) ;
- **Tirage par curseur** (`/changes`) : le client tire de manière incrémentale en fournissant un curseur (`since`) (fiable, solution de repli).

Les deux partagent **le même `change_seq` global** (une table `sync_feed`, numéro séquentiel globalement croissant) ;
le client peut alterner entre les deux et compléter via `/changes` comme référence. Ainsi on évite un balayage périodique intégral tout en ne perdant aucun changement.

### 4.5 Authentification JWT séparée par terminal

Après une connexion réussie, un JWT (HS256) est émis, séparé par terminal :
- Les jetons du **terminal web** (panneau d'administration) et du **terminal desktop** (client de bureau) sont indépendants (R-14),
  évitant qu'une fuite en un endroit n'affecte tout le système ;
- Le jeton d'accès et le jeton de rafraîchissement sont stockés dans Redis, avec support de révocation et de « rafraîchissement en vol unique » (une seule requête de rafraîchissement réussit à un instant donné).

L'édition communautaire ne conserve que la **connexion compte+mot de passe** (`/api/v1/auth/login`).

### 4.6 Sémantique des répertoires et contraintes de nommage

- Profondeur du répertoire ≤ 31 niveaux ET chemin cumulé unitaire ≤ 240 octets (double plafond, configurable) ;
- Doublon insensible à la casse sur le nom de fichier (`lower(name)` + index unique) ;
- Le « déplacement/suppression de sous-arbre » au niveau répertoire dépassant un seuil (1000 lignes par défaut) devient une **tâche asynchrone**, renvoyant un `task_id` que le client interroge.

## 5. Modèle de données (tables principales)

| Table | Signification |
|---|---|
| `users` | Utilisateurs (hachage compte+mot de passe, e-mail, nom affiché, rôle) |
| `organizations` / `departments` / `groups` | Organisation / département / groupe |
| `spaces` | Espaces (espace personnel + espace d'équipe ; inclut le quota `used_bytes`, `last_seq`) |
| `space_members` | Membres de l'espace et permissions |
| `files` | Entrées de répertoire (fichiers et répertoires ; `is_dir`, `parent_id`, `name`, `depth`, `size`) |
| `file_objects` | Objets adressés par contenu (hachage, quatre états : live/pending_delete…, compteur de références `ref_count`) |
| `sync_feed` | Flux de changements (global `change_seq`, rétention 90 jours) |
| `shares` | Liens de partage (token, permissions, expiration) |
| `audit_logs` | Journaux d'audit (actuellement un espace réservé, non persisté dans l'édition communautaire) |

## 6. Conception de la configuration

- **Une seule source de valeurs par défaut** : dans la structure Go `internal/config.Default()` ; supprimer un bloc de configuration ne donne pas une valeur zéro, mais retombe sur la valeur par défaut ;
- **Les secrets ne vont jamais dans le yaml** : `JWT_SECRET`, mot de passe de la base, mot de passe Redis ne passent que par **variables d'environnement** (`NETDISK_*`) ;
- Priorité : défaut Go < fichier de configuration < variables d'environnement ;
- Auto-contrôle au démarrage (`netdisk -config ... -check`) : tout élément illégal est **listé d'un coup** et quitte avec un code non nul, au lieu d'attendre la première requête pour renvoyer 500 ;
- Les durées utilisent une écriture lisible (`5m`/`30s`/`24h`).

## 7. Processus clés

### 7.1 Cycle de vie d'un téléversement (TUS)

```
Client ─PATCH fragments→ zone temporaire(tus-tmp) ─tout transmis→ finalizeUpload()
    finalize : calcule le hachage → contrôle déduplication téléversement instantané → écrit zone d'objets(objects/) → transaction courte écrit files+file_objects
             → écrit sync_feed(change_seq++) → pousse SSE → renvoie X-File-Id terminé
```

### 7.2 Chaîne de traitement d'une requête

```
Nginx ─→ chaîne de middlewares(journalisation/récupération/limitation d'accès/auth) ─→ routage(ServeMux) ─→ handler ─→ service métier ─→ repo(DBA)
```

## 8. Aperçu des points d'entrée

| Entrée | Description |
|---|---|
| `REST /api/v1/*` | Authentification, utilisateurs, départements, espaces, fichiers, répertoires, téléversement, téléchargement, partage, événements de changement |
| `Téléchargement & Range` | Téléchargement en flux, Range au niveau octet / requêtes conditionnelles (If-Match, etc.) |
| `TUS /uploads/*` | Téléversement fragmenté reprenable, réutilisation de ticket, récupération temporaire, niveau d'eau disque (>90% → 507) |
| `WebDAV /dav/*` | WebDAV standard + PUT passe par la finalisation (≤100Mo) |
| `SSE /sync/events` | Poussée de changements en temps réel |
| `curseur /changes` | Tirage incrémental par curseur |
| `GET /metrics` | Indicateurs de surveillance Prometheus (par défaut boucle locale / proxy de confiance uniquement) |
| `GET /healthz` | Vérification de santé |
| `/admin/*` | Panneau d'administration intégré (Go embed) |

## 9. Lignes rouges architecturale (extrait)

- Les paquets noyau ne doivent pas dépendre de la couche HTTP/API (famille R-01) ;
- Ordre de verrouillage global fixe : ligne spaces → ligne files → file_objects (advisory + verrou de ligne, ordre croissant des hachages) ;
- OAuth / clés passent toujours en variables d'environnement, jamais dans le dépôt de versions ;
- La patrouille (patrol) est **lecture seule** : ne répare jamais automatiquement, pour éviter qu'un faux jugement ne s'amplifie en corruption de données ;
- La réconciliation des quotas, la patrouille d'objets et le nettoyage du flux de changements sont tous des tâches d'arrière-plan contrôlées, sans extension sans limite.

## 10. Stack technique

| Couche | Choix |
|---|---|
| Langage | Go 1.26 |
| HTTP | bibliothèque standard `net/http` + chaîne de middlewares `ServeMux` |
| DB | pgx v5 + sqlc + migrations goose (PostgreSQL uniquement) |
| Auth | golang-jwt v5 (HS256) |
| Stockage | stockage d'objets sur disque local (adressage par contenu, le paquet `storage` est extensible à plusieurs backends) |
| Configuration | yaml.v3 + validation stricte des variables d'environnement |
| Déploiement | systemd + Nginx + Prometheus (quatuor sur port unique, sans conteneur) |

---

*Pour le déploiement et l'exploitation, voir `05-INSTALL.md` / `06-OPS.md` / `07-BACKUP.md` ; pour le détail des interfaces, voir `04-API_GUIDE.md`. Pour la liste complète des capacités, voir `01-FEATURE_LIST.md`.*
