# Guide d'API de Server-com

> Fichier de spécification : le contrat faisant autorité est `DOC/api/openapi.yaml` (OpenAPI v3) ; ce document en est le **guide de lecture humain**.
> Liens : `DOC/02-ARCHITECTURE.md` (principes), `DOC/05-INSTALL.md` (installation), `DOC/06-OPS.md` (exploitation), `DOC/07-BACKUP.md` (sauvegarde/restauration)

---

## 1. Modèle d'authentification

Le service utilise **JWT (HS256)** pour l'authentification ; après la connexion on obtient `access_token`, à présenter sur chaque requête suivante :

```
Authorization: Bearer <access_token>
```

**Jetons séparés par terminal (R-14)** : les jetons du terminal web (panneau d'administration) et du terminal desktop (client de bureau) sont indépendants,
évitant qu'une fuite en un endroit n'affecte tout le système. Le jeton / jeton de rafraîchissement est stocké dans Redis, avec support de révocation et de « rafraîchissement en vol unique ».

| Interface | Description |
|---|---|
| `POST /api/v1/auth/login` | Connexion compte+mot de passe, renvoie `access_token` + `refresh_token` |
| `POST /api/v1/auth/refresh` | Renouvelle le jeton access avec le jeton de rafraîchissement |
| `POST /api/v1/auth/logout` | Déconnexion, révocation du jeton |
| `GET /api/v1/me` | Informations sur l'utilisateur courant |
| `GET /api/v1/version` | Version du serveur |

> L'édition communautaire ne conserve que la **connexion par mot de passe** ; la connexion via WeCom/DingTalk/IdP tiers a été retirée de l'édition communautaire.
> Dépasseant la fenêtre renvoie **429**, le client doit réessayer avec repli (voir §7).

---

## 2. Conventions générales

- **Base URL** : `/api/v1`, exposée en production via le port unique Nginx.
- **Requête/réponse** : JSON (`Content-Type: application/json`).
- **Pagination** : les interfaces de liste utilisent `limit` + `after` (curseur keyset) pour tourner les pages ; `limit` plafonné à **999**
  (au-delà, repli à la valeur par défaut 200).
- **Téléchargement** : retour en flux + `Range` au niveau octet (200/206/416) + en-têtes de requête conditionnelle (`If-Match`/`If-None-Match`/`If-Range`).
- **Limitation de débit** : par famille d'interface (login/upload/file_list/file_read/file_write/webdav), deux fenêtres (seconde/minute) simultanément, le plus strict s'applique.

---

## 3. Groupes d'interfaces REST

### 3.1 Fichiers et répertoires

| Méthode et chemin | Description |
|---|---|
| `GET /api/v1/files?space=<id>&parent_id=&limit=&after=` | Lister le répertoire (200 entrées/page par défaut ; pagination keyset supportée) |
| `POST /api/v1/files` | Créer une entrée fichier/répertoire |
| `GET /api/v1/files/dirs` | Lister les répertoires (répertoires uniquement) |
| `GET /api/v1/files/{id}` | Détail fichier/répertoire |
| `GET /api/v1/files/{id}/content` | Télécharger le contenu (Range supporté) |
| `POST /api/v1/files/{id}/move` | Déplacer/renommer |
| `POST /api/v1/files/{id}/copy` | Copier |
| `POST /api/v1/files/{id}/share-to-space` | Copier/partager vers un autre espace (emprunte le chemin référence +1 de l'objet) |
| `POST /api/v1/files/{id}/lock` | Verrouiller/déverrouiller (verrou au niveau objet) |
| `GET /api/v1/files/{id}/subtree-stats` | Statistiques de sous-arbre |

> Le « déplacement/suppression de sous-arbre » au niveau répertoire dépassant un seuil (1000 lignes par défaut) devient une **tâche asynchrone** :
> `POST` renvoie `task_id`, le client interroge `GET /api/v1/tasks/{id}` pour suivre l'avancement.

### 3.2 Téléversement (multipart direct)

| Méthode et chemin | Description |
|---|---|
| `POST /api/v1/upload/create` | Créer un téléversement (réserve le quota, renvoie un jeton de téléversement) |
| `POST /api/v1/upload/simple` | Multipart direct (petit fichier ; écriture unique = finalisation) |
| `POST /api/v1/upload/{id}/finish` | Terminer/finaliser |
| `GET/PATCH /api/v1/upload/{id}` | Interroger/reprendre |

> Les gros fichiers passent par le protocole **TUS** (voir §4).

### 3.3 Changements et synchronisation

| Méthode et chemin | Description |
|---|---|
| `GET /api/v1/changes?since=<cursor>` | Tirage incrémental par curseur (canal « fiable » des deux états) |
| `GET /api/v1/changes/head` | Obtenir le curseur le plus récent |
| `GET /api/v1/sync/cursors` | Gérer les curseurs de synchronisation |
| `GET /sync/events` | Poussée temps réel SSE (canal « temps réel » des deux états) |

> Les deux partagent le même `change_seq` global : SSE peut perdre des trames, mais on peut compléter par `/changes` selon le curseur, garantissant qu'aucun n'est manqué.

### 3.4 Partage

| Méthode et chemin | Description |
|---|---|
| `POST /api/v1/shares` | Créer un lien de partage |
| `GET /api/v1/shares` | Liste de mes partages créés |
| `DELETE /api/v1/shares/{id}` | Révoquer le partage |
| `GET /api/v1/shares/{token}/meta` | Métadonnées du partage (pour la page d'accueil) |
| `GET /api/v1/shares/{token}/download` | Télécharger selon le token de partage |

### 3.5 Espaces

| Méthode et chemin | Description |
|---|---|
| `POST /api/v1/spaces` | Créer un espace (espace personnel / espace d'équipe) |
| `GET /api/v1/spaces/{id}` | Détail de l'espace (inclut le quota) |
| `POST /api/v1/spaces/{id}/transfer` | Transférer l'espace |
| `GET /api/v1/spaces/{id}/members` | Liste des membres |
| `PUT/DELETE /api/v1/spaces/{id}/members/{userId}` | Ajouter / retirer un membre |
| `POST /api/v1/spaces/{id}/leave` | Quitter l'espace |

### 3.6 Panneau d'administration (super_admin uniquement)

| Méthode et chemin | Description |
|---|---|
| `POST/GET /api/v1/admin/departments` | Gestion des départements |
| `PUT/DELETE /api/v1/admin/departments/{id}` | Modifier / supprimer un département |
| `POST /api/v1/admin/spaces/{id}/freeze` | Geler l'espace (arrêter la synchronisation) |
| `GET /api/v1/audit/logs` | Journaux d'audit (espace réservé dans l'édition communautaire) |

> L'interface d'administration des utilisateurs/organisations/espaces/quotas est fournie par Go `embed` (`/admin/*`).
> Par ailleurs : les interfaces de gestion des utilisateurs (créer un utilisateur, modifier le rôle, configurer le quota) passent par `/api/v1/admin/*` (voir l'intégralité d'openapi).

---

## 4. Téléversement fragmenté TUS (gros fichiers)

Protocole de reprise pour clients de bureau / gros fichiers (`cmd/probesmoke` dans `deploy` sert d'implémentation de référence) :

| Étape | Méthode | En-têtes clés |
|---|---|---|
| Créer la tâche | `POST /tus` | `Tus-Resumable: 1.0.0`, `Upload-Length`, `Upload-Metadata: filename <base64>`, `Authorization: Bearer` |
| Envoyer un fragment | `PATCH /tus/{id}` | `Tus-Resumable`, `Upload-Offset`, `X-Upload-Token`, `Content-Type: application/offset+octet-stream` |
| Terminer | Dernier fragment | Réponse `200` + `Upload-Complete: true` + `X-File-Id` = finalisation |

- **Reprise** : le client signale `Upload-Offset`, le serveur continue depuis cet offset ;
- **Réutilisation de ticket** : le ticket de téléversement peut être réutilisé avant la finalisation, idempotent ;
- **Niveau d'eau disque** : l'usage de la zone temporaire ≥ seuil (90 % par défaut) renvoie **507 Insufficient Storage** ;
- **Récupération temporaire** : les fichiers temporaires non terminés sont récupérés par une tâche d'arrière-plan (environ un tour toutes les 10 minutes).

---

## 5. WebDAV

Préfixe de chemin `/dav/*`, basé sur le `x/net/webdav` standard, le plus souvent utilisé pour « mapper un lecteur réseau / l'explorateur de fichiers » :

- **PUT passe par la finalisation** : ≤100Mo finalisé directement (au-delà, invitation à passer par TUS) ;
- **Sémantique LOCK complétée maison** (la bibliothèque standard n'a qu'une implémentation mémoire) ;
- Range / requêtes conditionnelles de même source que REST ;
- Le parcours de répertoire envoie plusieurs requêtes d'un coup → correspond à un `rate_limits.webdav` plutôt large (60/s).

---

## 6. Santé / surveillance

| Chemin | Description |
|---|---|
| `GET /healthz` | Vérification de vie (200) |
| `GET /metrics` | Indicateurs Prometheus ; par défaut boucle locale / proxy de confiance uniquement, inter-machine nécessite `NETDISK_METRICS_TOKEN` Bearer |
| `GET /api/v1/version` | Version |

---

## 7. Codes d'état et sémantique

| Code d'état | Sémantique | Description |
|---|---|---|
| 200 / 201 | Succès | Les finalisations réussies portent `X-File-Id` |
| 206 / 416 | Contenu partiel / hors plage | Range de téléchargement |
| 400 / 422 | Erreur de paramètre | Échec de validation |
| 401 | Non authentifié | Jeton manquant/expiré |
| 403 / 401 | Accès restreint | **Suspendre la synchronisation de cet espace, ne jamais supprimer en local** (comportement client) |
| 404 / 410 | Inexistant / retiré | 410 = objet/espace migré ou supprimé |
| 409 | Conflit | Conflit de version, doublon de nom (jugement insensible à la casse) |
| 429 | Limitation de débit | Réessai avec repli requis |
| 500 | Erreur serveur | Voir le traitement des incidents dans 06-OPS.md |
| 507 | Espace de stockage insuffisant | Niveau d'eau disque atteint le seuil |

**Traitement de 429 par le client** : l'expérience du projet est que « le réessai avec repli sur 429 est obligatoire » — lors du nettoyage des fichiers de sonde, un `DELETE` a heurté la limitation `file_write`,
sans repli cela aurait **supprimé silencieusement moins d'éléments** ; si la création de tâche de téléversement heurte la limitation sans repli, cela se manifeste par « le fichier ne finit jamais de monter ».

---

## 8. Relation avec le fichier de spécification

- Ce document est un guide de lecture humain ; le **contrat faisant autorité** est le `DOC/api/openapi.yaml` lisible machine.
- Le dépôt web effectue la validation de contrat front-end dessus (`pnpm check:api`) ; le client de bureau utilise gen-csharp pour générer le code client.
- Lors d'un changement de contrat, `web` et `desktop` ont chacun une copie vendored à synchroniser en **trois endroits**.
