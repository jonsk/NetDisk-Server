# Guide de lecture du code de Server-com

> Version applicable : Server-com (édition open source / Community) · backend Go en binaire unique de stockage cloud
> Public visé : Ingénieurs et opérations ayant besoin de comprendre, dépanner ou refaire du développement sur ce code.
> Ce guide explique « comment le code est organisé, comment les données circulent, où aller pour modifier X », sans détailler chaque algorithme ligne par ligne.
> La réception par module et les lignes rouges de développement se trouvent dans `01-功能清单.md` du même répertoire ; la conception autorisée est dans `D:\WorkSpace\GO\Doc\网盘系统架构设计文档.md` (V3.0).

---

## 1. Ce qu'est ce dépôt

`Server-com` est un **service backend** de stockage cloud d'entreprise, écrit en Go, compilé finalement en **un seul fichier exécutable** (binaire unique).

Il fournit quatre types de capacités vers l'extérieur :

| Capacité | Description | Entrée principale |
|---|---|---|
| Interface de gestion/fichiers REST | gestion back-office, CRUD fichiers, téléchargement, partage | `/api/v1/*` |
| Téléversement TUS par reprise sur coupure | téléversement de fichiers volumineux par fragments | `/tus/*` |
| WebDAV | disque monté côté bureau (compatible protocole de fichiers) | `/webdav/*` |
| Synchronisation SSE en temps réel | pousse « le fichier a changé » au client, qui récupère ensuite l'incrément | `/api/v1/events` |

Il ne gère **pas** : le proxy inverse HTTP (Nginx), les pages front-end (dépôt `web`), le client de bureau (dépôt `desktop`). Ces trois éléments sont des dépôts indépendants ou des composants externes.

> En un mot : ce dépôt = un tas de paquets Go + un ensemble de commandes + scripts de migration + matériel de déploiement, produisant finalement un binaire `netdisk`.

---

## 2. Structure des répertoires de niveau supérieur

```
Server-com/
├── cmd/            命令行程序（8 个），见 §3
├── internal/       核心代码（36 个 Go 包），见 §4
├── deploy/         部署物料：build 脚本 / 配置示例 / systemd / nginx / 备份 / 供给脚本
├── DOC/            仓库说明文档（本指南所在的目录）
├── scripts/        辅助脚本
├── .github/        CI 流水线
├── go.mod / go.sum Go 依赖清单
└── README.md       仓库入口说明
```

Conseil de lecture : **commencez par l'assemblage de démarrage de `cmd/netdisk/main.go` (§5)**, puis suivez « comment se déroule une requête » (§6) jusqu'à `internal/api`, et approfondissez ensuite un paquet métier selon vos besoins.

---

## 3. Commandes (cmd/) —— chacune est un petit programme indépendant

`cmd/` contient 8 répertoires, chacun étant un programme `main` :

| Commande | Fonction | Quand l'utiliser |
|---|---|---|
| **netdisk** | **service principal**, unique processus en production | celui démarré par `systemd` |
| **migrate** | migration de base de données | au déploiement `go run ./cmd/migrate up` |
| **passwd** | exploitation de compte : créer utilisateur / réinitialiser mot de passe / activer-suspendre | opération manuelle d'exploitation sur les comptes |
| **probesmoke** | sonde de bout en bout : vrai HTTP enchaînant connexion → téléversement → téléchargement | autotest en phase de développement |
| **devdb** | maintenance de la base de développement/test locale | usage local des développeurs |
| **depsguard** | barrière d'architecture : vérifie « les paquets noyau ne doivent pas dépendre de la couche HTTP », etc. | vérification statique CI |
| **testsgate** | barrière de tests d'intégration : confirme que les tests ont réellement tourné | CI |
| **coveragegate** | barrière de couverture : statistiques de couverture par instruction et par paquet | CI |

En production, seuls les deux premiers comptent : `netdisk` et `migrate` utilisé au déploiement. Les autres sont des auxiliaires développement/CI.

---

## 4. Carte des paquets internal/ —— où est le code, que fait-il

Les 36 paquets se répartissent en cinq couches selon leur responsabilité. Comprendre le découpage en couches est la clé de lecture du dépôt : **la couche HTTP ne fait que « recevoir la requête, appeler le service, renvoyer la réponse », la logique métier est dans la couche service, et c'est la couche repo qui lit/écrit réellement la base, au plus bas la base/Redis/disque.**

### 4.1 Entrée et couche HTTP (ce que vous rencontrez en premier dans ce dépôt)
| Paquet | Responsabilité |
|---|---|
| **api** | **assemblage global du routage**. Toute la correspondance URL↔fonction de traitement est dans `api.go`. Presque tout « quel interface appelle qui » se trouve ici |
| **middleware** | chaîne de middleware : ID de requête, IP réelle, erreurs structurées, journalisation, récupération sur crash. Les « postes de contrôle » traversés par la requête |
| **apierr** | encapsulation d'erreur unifiée (code statut HTTP + code métier + texte en chinois) |
| **webui** | entrée des ressources statiques du front du back-office `/admin` (embed dans le binaire, Nginx ne les héberge plus) |
| **reqctx** | outil plaçant « l'utilisateur connecté actuel » (Actor) dans le contexte de la requête |

### 4.2 Authentification et permissions
| Paquet | Responsabilité |
|---|---|
| **auth** | émission/validation JWT (le jeton lui-même, sans état) |
| **authsvc** | orchestration métier de la connexion par identifiant/mot de passe / rafraîchissement / déconnexion (requête base, vérification mot de passe, verrouillage, liaison version de token) |
| **webdavauth** | canal d'authentification Basic de WebDAV |
| **credentials** | Hash du mot de passe (type bcrypt) et politique de robustesse |

### 4.3 Couche de service métier (chacun correspond à une catégorie métier, la plus digne d'être lue en détail)
| Paquet | Responsabilité |
|---|---|
| **usersvc** | gestion des utilisateurs back-office (création/suspension/rôle) |
| **orgsvc** | structure organisationnelle (arbre des départements) |
| **spacesvc** | espaces (espace personnel/espace d'équipe) et collaboration des membres |
| **filesvc** | métadonnées des fichiers (liste/renommage/déplacement/suppression/création de répertoire) |
| **uploadsvc** | création de tâche de téléversement, validation de ticket, plan de données TUS |
| **fastupload** | défi de « preuve de possession » pour le transfert instantané |
| **finalize** | **finalisation** —— unique entrée d'écriture finale de tout téléversement, chemin d'écriture le plus critique du réseau |
| **sharesvc** | lien de partage (unique sortie sans connexion du système) |
| **dirops** | file de tâches asynchrones au niveau répertoire (opérations sur très gros répertoires en arrière-plan) |
| **quotareconcile** | réconciliation de quota et alerte de dérive |
| **patrol** | inspection des objets : analyse du disque à la recherche de fuites/fichiers orphelins |

### 4.4 Modèle de domaine et synchronisation
| Paquet | Responsabilité |
|---|---|
| **model** | entités (struct) correspondant strictement aux tables de la base |
| **namepolicy** | arbitre unique du nom de fichier (validation de légalité, longueur, profondeur) |
| **objlock** | verrou au niveau objet (discipline d'exclusion mutuelle lors de l'écriture du même fichier, empêchant l'écrasement concurrent) |
| **syncfeed** | lecture/écriture du flux de changements (qui a modifié quoi) |
| **syncsse** | canal d'événements SSE (poussés au client) |

### 4.5 Infrastructure (la plupart du temps il suffit de savoir qu'elle existe)
| Paquet | Responsabilité |
|---|---|
| **config** | chargement de configuration (yaml + variables d'environnement) |
| **db** | pool de connexions PostgreSQL et transactions |
| **cache** | encapsulation Redis et convention de nommage des clés |
| **ratelimit** | limitation de débit basée sur Redis |
| **repo** | couche de dépôt SQL —— **tout le SQL est ici**, entrée pour « voir les données » |
| **storage** | stockage objet sur disque local (adressé par contenu, fichiers stockés selon le Hash) |
| **migrate** | migration de version de base (goose) |
| **obs** | journalisation structurée (slog) |
| **lifecycle** | machine à états du cycle de vie des objets |
| **condreq** | requêtes conditionnelles HTTP (If-Match/If-None-Match, verrouillage optimiste) |
| **webdavfs** | vue de fichiers sous-jacente prise en charge par WebDAV |

> **Aide-mémoire de lecture** : pour modifier « une interface HTTP » → cherchez la ligne de routage dans `internal/api/api.go` + le `handlers_*.go` correspondant ;
> pour consulter « comment la base est lue/écrite » → cherchez `internal/repo/` ;
> pour trouver « l'écriture finale sur disque du téléversement » → regardez `internal/finalize/` (unique chemin d'écriture).

---

## 5. Assemblage de démarrage —— comment le service est « assemblé »

Tout commence dans `cmd/netdisk/main.go`. Selon les étapes numérotées dans les commentaires, l'ordre d'assemblage est :

1. **Configuration** : valeurs par défaut ← yaml ← variables d'environnement (le suivant écrase le précédent)
2. **Validation** : impression unique de tous les problèmes, refus de démarrer s'il y en a (fail-fast)
3. **PostgreSQL** : connexion à la base, exécution des migrations
4. **Redis** : refus de démarrer si connexion impossible (auth/limitation de débit en dépendent)
5. **Gestion des jetons** : initialisation émission JWT, Redis, limiteur de débit, divers services métier
6. **Service HTTP** : montage du routage assemblé par `internal/api` sur le port, démarrage de l'écoute

Ce fichier est le lieu de « l'injection de dépendances » —— **où sont instanciés quels services, quelles dépendances sont passées, tout est dans ce seul fichier**. Pour comprendre « comment un service est assemblé », lisez-le ; pour ajouter une dépendance au système, modifiez-le aussi.

---

## 6. Comment se déroule une requête (comprendre la circulation, c'est comprendre le dépôt)

Prenons l'exemple « l'utilisateur connecté demande la liste des fichiers », la chaîne est :

```
        ┌────────────────────────────────────────────┐
        │  Nginx：HTTPS 终结、反代、来源 IP 透传       │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  middleware 中间件链（internal/middleware） │
        │  requestID → realIP → structuredErrors →   │
        │  logging → recoverer（崩溃兜底）            │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  api 路由（internal/api/api.go）            │
        │  鉴权(auth) → 限速(ratelimit) → handler    │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  handlers_*.go  解析参数、调用服务          │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  业务服务层（filesvc/uploadsvc/...）         │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  repo 层：「唯一 SQL 的地方」→ PostgreSQL    │
        │  storage 层：读写磁盘对象                    │
        └────────────────────────────────────────────┘
```

**Retenez cette phrase en gras** : `la couche api gère « l'apparence externe », `repo` gère « les échanges avec la base », la couche service intermédiaire gère « les règles métier ». Plus le découpage en couches est clair, plus modifier un endroit n'en affecte qu'un seul.

---

## 7. Comment la configuration est gérée

Voyez `deploy/config/config.example.yaml` (exemple de configuration, liste de champs la plus autorisée), ainsi que `internal/config/config.go` (logique de chargement).

- Priorité des sources de configuration : **valeur par défaut du code < fichier yaml < variables d'environnement**.
- **Les éléments sensibles (mots de passe, clés) passent uniquement par les variables d'environnement, jamais dans le yaml**. Par exemple `NETDISK_DB_PASSWORD`, `JWT_SECRET`, `NETDISK_REDIS_PASSWORD`.
- Les grands blocs de configuration incluent : `server` (port/proxy), `database`, `redis`, `jwt` (validité du jeton), `policy` (quota/profondeur/limite de taille), `patrol` (inspection), `webui` (préfixe back-office), `storage` (répertoire racine de stockage), `log`, `rate_limits` (limitation de débit).
- Validation au démarrage ; un blocage si par exemple une limitation de débit est à 0, ou le répertoire de stockage non inscriptible.

> Astuce opérations : modifier la configuration → éditez le yaml ou les variables d'environnement → redémarrez `netdisk` ; **modifier le front nécessite de recompiler le binaire Go** (car les pages sont embed), un simple redémarrage du processus ne suffit pas.

---

## 8. Base de données —— aperçu des tables cœur

Les migrations sont dans `internal/migrate/sql/` (13 scripts versionnés, intégrés au binaire). Tables métier cœur :

| Table | Stocke quoi |
|---|---|
| `users` | compte utilisateur |
| `spaces` | espace personnel/d'équipe |
| `space_members` | relation des membres d'espace |
| `groups` / `group_members` | équipe et membres |
| `departments` / `department_closure` / `user_departments` | structure organisationnelle (arbre des départements + table de clôture) |
| `files` | métadonnées des fichiers/répertoires |
| `file_objects` | objet réel du fichier (adressé par contenu) |
| `uploads` | tâche de téléversement (session TUS) |
| `shares` | lien de partage |
| `file_locks` | verrou d'édition de fichier |
| `refresh_tokens` | état de connexion résident (refresh token) |
| `sync_feed` / `sync_cursors` | flux de changement et curseur de synchronisation |
| `dir_op_tasks` | tâche asynchrone au niveau répertoire |

> Note : les tables `audit_logs`, `idp_providers`, `idp_sync_state`, `user_idp_bindings`, `user_sso_bindings` sont des **tables historiques** laissées par des fonctionnalités antérieures (audit, connexion aux sources d'identité) ; le code ne les lit/écrit plus actuellement ; l'historique de migration ne doit pas être modifié à la légère, conservez-les normalement.

---

## 9. Aperçu des interfaces HTTP

Tout le routage est centralisé dans `internal/api/api.go`. Groupé par fonction (détails d'authentification omis) :

| Groupe | Chemin exemple | Description |
|---|---|---|
| Santé / version | `GET /healthz`, `GET /api/v1/version` | sonde de vie, négociation de version (sans connexion) |
| Authentification | `POST /api/v1/auth/login` / `refresh` / `logout` | connexion par identifiant/mot de passe et renouvellement de jeton |
| Mes informations | `GET /api/v1/me` | utilisateur connecté actuel |
| Fichiers | `GET /api/v1/files`, `GET /api/v1/files/{id}/content` | liste, téléchargement (Range supporté) |
| Écriture fichiers | `PATCH /api/v1/files/{id}`, `DELETE /api/v1/files/{id}`, `POST /api/v1/files/dirs` | renommage, suppression, création de répertoire |
| Téléversement | `POST /api/v1/upload/create`, `/tus/*` | création de tâche de téléversement + TUS par fragments |
| Transfert instantané | `POST /api/v1/upload/{id}/finish` | finalisation par transfert instantané après passage de la preuve de possession |
| Verrou d'édition | `POST/GET/DELETE /api/v1/files/{id}/lock` | verrou d'édition collaborative de fichier |
| Partage | `POST /api/v1/shares`, `GET /api/v1/shares/{token}/meta` | lien de partage (téléchargement sans connexion) |
| Espace | `GET /api/v1/spaces`, `POST /api/v1/spaces` | mon espace, création d'espace |
| Gestion org/utilisateurs | `/api/v1/admin/departments*`, `/api/v1/admin/users*` | gestion back-office (admin requis) |
| Synchronisation | `GET /api/v1/events` (SSE), `GET /api/v1/changes` | push en temps réel + récupération incrémentale |
| WebDAV | `/webdav/*` (PROPFIND/PUT/COPY/MOVE/LOCK, etc.) | disque monté de bureau |
| Tâche | `GET /api/v1/tasks/{id}` | requête de progression de tâche asynchrone au niveau répertoire |

> Pour « trouver dans quel fichier une interface est implémentée » : `api.go` écrit via `mux.Handle("METHOD /path", ...d.handleXxx...)`, `handleXxx` est la fonction d'implémentation, généralement dans le `handlers_*.go` du même répertoire.

---

## 10. Quelques conceptions clés, à avoir en tête avant de lire le code

Pour que les lecteurs non expérimentés ne soient pas perdus, établissez d'abord ces quatre modèles mentaux :

1. **JWT par point de terminaison** : les jetons sont partitionnés par points de terminaison `web` (back-office) / `desktop` (bureau), **le jeton back-office ne peut pas opérer les capacités bureau**, et vice versa. Le jeton n'écrit pas les permissions, les permissions sont toujours décidées côté serveur.
2. **TUS par reprise sur coupure + chemin d'écriture unique** : tout téléversement (fragment TUS, WebDAV PUT, transfert instantané) aboutit finalement à cette seule entrée de « finalisation » `finalize`, garantissant qu'il n'y a qu'un seul chemin d'écriture pour un fichier, sans conflit.
3. **Synchronisation à double canal** : le serveur pousse « il y a un changement » via SSE ; le client récupère ensuite les modifications concrètes via le curseur `/changes`. Perte du push sans crainte, la récupération est autorisée. Le curseur s'aligne via un numéro global auto-incrémenté.
4. **Stockage adressé par contenu** : `storage` stocke les objets selon le Hash du contenu du fichier, un même contenu n'est stocké qu'une fois (déduplication), le nom de fichier n'étant que métadonnée.

---

## 11. Dépendances tierces (go.mod) très légères

Après `slimming`, les dépendances sont fortement réduites, les dépendances cœur actuelles sont seulement :

- **pgx** (pilote PostgreSQL), **goose** (migration)
- **go-redis** (client Redis)
- **golang-jwt/v5** (JWT)
- **miniredis** (tests uniquement), **yaml.v3** (configuration)

Pas de file de messages, pas de framework lourd, pas de SDK de stockage multi-backend (le disque local est le seul backend). C'est très favorable au dépannage : pas trop de « dépendances illisibles » sur la pile.

---

## 12. Mémo de construction / exécution / déploiement

```bash
# Construction locale (Go 1.26+ requis)
go build ./...            # compile tout (vérifie s'il y a erreur)
go vet ./...              # vérification statique
go run ./cmd/depsguard    # barrière d'architecture (aussi en CI)

# Le vrai artefact de déploiement (deploy/build-release.sh fait cela)
# L'ordre compte : d'abord build du front (web) → puis go build du binaire unique → packaging
```

- Le binaire unique est géré par `systemd`, Nginx fait proxy inverse (`deploy/nginx/`).
- Déploiement détaillé voir `deploy/README.md`.
- Lisez directement la logique d'écriture de l'entrée principale : `cmd/netdisk/main.go`.

---

## 13. Par où commencer la lecture (pour un premier contact)

1. `cmd/netdisk/main.go` —— voyez l'ordre d'assemblage (comprendre le global en 5 min)
2. `internal/api/api.go` —— voyez tout le routage (savoir quelles interfaces le système a en 5 min)
3. `deploy/config/config.example.yaml` —— voyez quels boutons de configuration existent
4. Choisissez un métier qui vous préoccupe le plus : téléversement de fichier → poursuivez `finalize`, structure organisationnelle → poursuivez `orgsvc`, synchronisation → poursuivez `syncfeed`/`syncsse`
5. Traitez un vrai problème : depuis le journal `obs` / l'erreur d'une interface → retour à `handlers_*.go` → `repo/*.go` pour voir le SQL

> Principe d'or : **face à « à quoi sert ceci », lisez le commentaire en tête du fichier de ce paquet** —— chaque paquet/fonction de ce dépôt porte de nombreux commentaires de conception en chinois, constituant une « documentation vivante ».
