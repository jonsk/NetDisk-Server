# NetDisk · Serveur netdisk open source

> Un back-end de netdisk auto-hébergé destiné à la **collaboration sur fichiers d'équipe** (binaire unique Go).
> Fournit des espaces de partage d'équipe, un téléversement multi-protocoles TUS / WebDAV / multipart, une finalisation versionnée,
> une perception de synchronisation en temps réel (SSE + curseur), et une gouvernance fine des droits et des quotas — déployable comme netdisk privé,
> ou utilisé comme hub de fichiers de l'entreprise.
>
> **Licence :** [Apache-2.0](LICENSE) · **Forme :** Édition communautaire (Server-com)

---

## 🌐 Multilingue / Translations

[中文](../../../README.md) | [English](../en/README.md) | [Deutsch](../de/README.md) | [Français](../fr/README.md) | [Suomi](../fi/README.md) | [Русский](../ru/README.md)


---

## ✨ Fonctionnalités

| Dimension | Capacité |
|---|---|
| 🚀 **Téléversement et finalisation** | TUS reprise de téléversement en fragments / WebDAV PUT / multipart : trois entrées partageant le même chemin de finalisation ; objet adressable par contenu + déduplication par téléversement instantané + validation anti-empoisonnement |
| 🧠 **Cycle de vie des objets** | `file_objects` à quatre états + verrouillage au niveau objet (double niveau d'exclusion mutuelle) ; écriture de l'objet suivie d'une courte transaction, évitant tout écrasement silencieux et dérive de fichier |
| 💾 **Stockage d'objets local** | Stockage adressable par contenu : les objets sont stockés sur disque local selon le hash de leur contenu, un seul exemplaire par contenu identique (déduplication) |
| 📡 **Perception de synchronisation** | Push SSE en temps réel + curseur `/changes` à double état (séquence globale `change_seq`), aucune analyse périodique, coopère avec le client de bureau |
| 🔐 **Identité et droits** | Connexion par compte/mot de passe maison + JWT (HS256), organisation/département/groupe, espace personnel / espace d'équipe, lien de partage, rapprochement de quota |
| 🗂 **Sémantique des répertoires** | Profondeur de répertoire ≤31 / budget de chemin ≤240 octets ; détection de doublon insensible à la casse ; bascule de seuil MOVE/DELETE vers file d'attente asynchrone |
| 🛡 **Exploitation et sécurité** | Rapprochement de quota + inspection des objets (fuite/orphelin) et réparation ; verrouillage au niveau objet + limitation de débit des interfaces ; l'audit est un emplacement réservé (non persistant actuellement) |
| 📦 **Panneau d'administration** | Panneau d'administration `/admin` (utilisateur / organisation / espace / quota), servi par un binaire unique Go `embed` |

---

## 🏛 Aperçu de l'architecture

```
                       ┌──────────────────────────────────────┐
  Web admin /admin ─▶  │                                      │
  Client WebDAV  ─WebDAV▶│     netdisk (binaire unique Go)        │
  Chargeur TUS    ─TUS─▶│   REST · TUS · WebDAV · SSE unifiés   │
  Client synchro ─SSE──▶│                                      │
                       │   ┌──────────────────────────────┐   │
                       │   │ finalize : écriture unique   │   │
                       │   │ adressage → stockage local   │   │
                       │   └──────────────────────────────┘   │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(proxy)│
               │métadonnée│  │sess/lim│   │ port unique│
               └──────────┘  └────────┘   └────────────┘
```

- **PostgreSQL 17 (minimum pris en charge)** —— unique moteur de métadonnées (avec archivage WAL) ; la clé primaire utilise `gen_random_uuid()`, nécessite PG ≥ 17
- **Redis** —— sessions / limitation de débit / file de tâches (`SKIP LOCKED`, sans MQ)
- **Nginx** —— proxy inverse, port unique exposé
- Le front du panneau d'administration est copié dans `internal/webui/dist/` après `pnpm build` du dépôt `web`, servi par **Go `embed`**

---

## 🛠 Stack technique

| Couche | Choix |
|---|---|
| Langage | Go 1.26 |
| HTTP | bibliothèque standard `net/http` + chaîne de middlewares `ServeMux` |
| DB | pgx v5 + sqlc + migrations goose |
| Auth | golang-jwt v5 (HS256) |
| Stockage | stockage d'objets sur disque local (adressage par contenu) |
| Config | yaml.v3 + validation stricte des variables d'environnement |
| Déploiement | systemd + Nginx + Prometheus (quatuor sur port unique) |

---

## 🚀 Démarrage rapide

### Prérequis

- Go 1.26+
- PostgreSQL 17+ (minimum 17 ; bases : `netdisk` / `netdisk_test`, `LC_COLLATE=C`)
- Redis 7+ (`appendonly yes`, `noeviction`)
- Nginx (production)

### 1. Construction

```bash
# Environnement (accélérateur CN + vérification désactivée)
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off

go build ./...     # compiler
go vet ./...       # vérification statique
```

> **Remarque (embed) :** si les artefacts front-end existent déjà dans `internal/webui/dist/`, ils seront intégrés à la compilation ;
> après mise à jour du front, exécutez d'abord `pnpm build` puis `go build`, sinon l'ancien artefact sera intégré.

### 2. Migration de la base de données

```bash
go run ./cmd/migrate -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up
# ou utiliser le binaire goose déjà construit
```

### 3. Configuration

Copiez `deploy/config/config.example.yaml` en `config.yaml` et modifiez-le selon vos besoins.
**Toute configuration sensible passe par les variables d'environnement** (`NETDISK_JWT_SECRET` etc., valeur absente/trop courte refusera le démarrage),
jamais écrite dans le yaml —— voir `deploy/systemd/secrets.env.example`.

### 4. Exécution

```bash
go run ./cmd/netdisk
# écoute sur :8080
# /admin   panneau admin  /api/v1/*  REST  /dav/*  WebDAV  /changes  tirage incrémental  /sync/events  SSE
```

---

## 🔌 Capacités des interfaces

| Entrée | Description |
|---|---|
| `REST /api/v1/*` | authentification, utilisateurs, départements, espaces, fichiers, répertoires, téléversement, téléchargement, partage, événements |
| `Téléchargement & Range` | `ServeContent` en streaming ; Range au niveau octet (200/206/416) ; requêtes conditionnelles If-Match/If-None-Match/If-Range |
| `TUS /uploads/*` | fragments, reprise de téléversement, réutilisation de ticket, recyclage de la zone tampon, niveau d'eau disque (>90% → 507) |
| `WebDAV /dav/*` | `x/net/webdav` + interception PUT vers la finalisation (≤100 Mo) ; LOCK sémantique complétée en interne |
| `SSE /sync/events` | push temps réel des changements distants (suppression de l'écho propre) |
| `curseur /changes` | tirage incrémental à double état du curseur, séquence globale `change_seq` |
| Limitation de débit | niveaux file_read / file_write ; 429 à retenter |

---

## 📂 Structure du projet

```
Server-com/
├── cmd/          points d'entrée exécutables
│   ├── netdisk         service principal
│   ├── migrate         migration de base de données
│   ├── passwd         (identifiants administrateur)
│   ├── depsguard      validation mécanique de la discipline d'architecture
│   ├── testsgate      contrôle d'accès des tests d'intégration
│   ├── coveragegate   contrôle de couverture
│   ├── devdb          création de base locale de dev
│   └── probesmoke     sonde de fumée
├── internal/         logique centrale (37 paquets)
│   ├── finalize/      unique chemin d'écriture ★
│   ├── objlock/       verrou objet double niveau (ADR-2)
│   ├── storage/       stockage d'objets sur disque local
│   ├── uploadsvc/     téléversement TUS
│   ├── lifecycle/     worker de cycle de vie des objets
│   ├── syncfeed|syncsse/  perception de synchronisation
│   ├── webdavfs|webdavauth/ WebDAV
│   ├── api/           handler REST
│   ├── patrol/        inspection des objets
│   └── quotareconcile/ rapprochement de quota
├── deploy/          artefacts de déploiement (systemd/Nginx/Prometheus/sauvegarde/provisioning/vérification)
├── scripts/         scripts de build et de génération
├── DOC/             liste des fonctionnalités / architecture / installation / exploitation / sauvegarde / tests / guide API / guide de lecture du code
│   └── api/          contrat OpenAPI openapi.yaml (autoporté par ce dépôt, autorité dans GO\Doc\api)
└── LICENSE          Apache-2.0
```

---

## 🧪 Tests et contrôles

- `go test ./...` —— tests unitaires
- `go test -tags=integration ./...` —— tests d'intégration (nécessite la base `netdisk_test` + Redis)
- `cmd/depsguard` —— validation mécanique des lignes rouges (ex. les paquets noyau ne dépendent pas de la couche HTTP)
- `cmd/testsgate` / `cmd/coveragegate` —— contrôle d'intégration / de couverture
- avant commit, lancez localement : `go build ./...` / `go vet ./...` / `cmd/depsguard`

---

## 🤝 Contribution

Avant de soumettre, assurez-vous que `go build ./...`, `go vet ./...`, `depsguard` sont tous au vert ; les nouveaux comportements respectent les lignes rouges et les ADR du document d'architecture ; ne soumettez aucune clé ou identifiant.

---

## 📄 Licence

Ce projet est publié sous **Apache License 2.0** ([Apache-2.0](LICENSE)).
Vous pouvez librement l'utiliser, le modifier, le redistribuer et l'employer à des fins **commerciales** (y compris des dérivés sous licence fermée),
mais vous devez **conserver l'avis de droit d'auteur et de licence d'origine** et indiquer les modifications dans les fichiers modifiés.
Ce projet est fourni **en l'état** (AS IS), **sans aucune garantie expresse ou implicite** ; le déploiement, l'exploitation et la conformité relèvent de la responsabilité de l'utilisateur.

### Relation avec l'édition commerciale

`Server-com` (ce dépôt, édition communautaire) ne comporte aucun engagement de service d'entreprise, ni aucun identifiant ou configuration confidentielle.

---

*La liste complète des fonctionnalités se trouve dans [`01-FEATURE_LIST.md`](01-FEATURE_LIST.md) ; pour le déploiement et l'exploitation, voir [`05-INSTALL.md`](05-INSTALL.md) · [`06-OPS.md`](06-OPS.md) · [`07-BACKUP.md`](07-BACKUP.md) ; pour le détail des interfaces, voir [`04-API_GUIDE.md`](04-API_GUIDE.md).*