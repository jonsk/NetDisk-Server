# Liste des fonctionnalités de Server-com (édition open source)

> Version : 2026-09-15, compilée
> Ce dépôt ne comporte aucun engagement de service d'entreprise, utilise/modifie/redistribue selon la licence open source, déploiement et exploitation à votre charge.
> Autorité de conception : `02-ARCHITECTURE.md`.

---

## I. Positionnement du dépôt

| Élément | Contenu |
|---|---|
| Nom | **Server-com** (version open source / Community) |
| Nature | Distribution open source du **service backend (Go)** du système de netdisk ; contenu = code source du serveur + matériel de déploiement |
| Stack technique | Go / PostgreSQL 17 / Redis / Nginx / stockage sur disque local |
| Capacités externes | Interface REST + téléversement TUS par reprise sur coupure + WebDAV + synchronisation SSE en temps réel |

**Répertoires** : `internal/` (code cœur), `cmd/` (programmes en ligne de commande), `deploy/` (matériel de déploiement), `scripts/` (scripts auxiliaires), `DOC/` (documentation).

> Note au lecteur : cette liste est organisée autour de « ce que le système peut faire », pour faciliter la compréhension globale des capacités ; pour approfondir la structure du code et les points d'entrée, lisez le *Guide de lecture du code* (03-CODE_READING_GUIDE.md) du même répertoire.

---

## II. Que fournit le système vers l'extérieur

Server-com est un **backend de netdisk d'entreprise**, fournissant quatre types de capacités vers l'extérieur, par lesquelles les clients (panneau d'administration / client de bureau / outils de protocole de fichiers) opèrent le netdisk :

| Capacité | Description | Apparence externe |
|---|---|---|
| **Interface REST** | connexion, CRUD fichiers, téléchargement, partage, gestion back-office, etc. | `/api/v1/*` |
| **TUS par reprise sur coupure** | téléversement de fichiers volumineux par fragments, reprise possible en cas d'interruption réseau | `/tus/*` |
| **WebDAV** | compatible avec le protocole de fichiers standard, peut être monté directement comme disque réseau | `/webdav/*` |
| **Synchronisation SSE en temps réel** | dès qu'un fichier change, on le pousse au client, puis récupération incrémentale | `/api/v1/events` |

---

## III. Liste des modules fonctionnels

### 1) Compte et authentification
- **Connexion par identifiant/mot de passe** : connexion par nom d'utilisateur/mot de passe, avec renouvellement par jeton de rafraîchissement et déconnexion.
- **Mécanisme de jeton** : après connexion réussie, un jeton d'accès est émis ; les jetons sont partitionnés par **point de terminaison** (back-office / bureau), les capacités des différents points de terminaison étant isolées.
- **Gestion des comptes** : le back-office peut créer des utilisateurs, activer/suspendre des comptes, ajuster les rôles ; contrôle granulaire du téléversement/téléchargement par rôle pris en charge.
- **Sécurité des mots de passe** : validation stricte du mot de passe et stockage par Hash ; stratégie de verrouillage temporaire en cas d'échecs pour prévenir le brute-force.

### 2) Organisation et espaces
- **Structure organisationnelle** : maintenance de l'arbre des départements (ajout/suppression/modification de départements, consultation du sous-arbre, reconstruction de la clôture) ; le département est l'une des entrées des permissions.
- **Espaces** : chaque utilisateur dispose d'un **espace personnel** ; peut créer un **espace d'équipe** et inviter des membres à collaborer.
- **Gestion des membres** : ajout/suppression de membres de l'espace, ajustement des rôles, transfert, dissolution ; l'administrateur peut gouverner l'espace globalement (quota, gel, révocation).

### 3) Gestion des fichiers
- **Opérations de métadonnées** : navigation, création de répertoire, renommage, déplacement, suppression de fichiers/répertoires.
- **Téléchargement** : téléchargement en flux, reprise sur coupure HTTP Range, requêtes conditionnelles (If-Match, etc., verrouillage optimiste).
- **Transfert instantané** : les fichiers de contenu identique peuvent sauter le téléversement répété et réutiliser directement l'objet déjà stocké.
- **Verrou d'édition** : un fichier peut être verrouillé pour éviter que plusieurs personnes ne s'écrasent mutuellement en l'éditant en même temps.
- **Contraintes de nommage et de chemin** : légalité du nom de fichier, longueur du chemin, profondeur du répertoire, etc. sont validées de façon uniforme par le serveur.
- **Quota** : gestion du quota d'espace, blocage au dépassement ; avec réconciliation de quota et alerte de dérive.

### 4) Téléversement et écriture
- **TUS par reprise sur coupure** : téléversement de fichiers volumineux par fragments, avec reprise sur interruption et fragments concurrents.
- **Chemin d'écriture unifié** : tous les téléversements (fragments, écriture WebDAV sur disque) aboutissent finalement à la même entrée de finalisation, garantissant « un seul source d'écriture pour un même fichier », évitant naturellement l'écrasement concurrent.
- **Stockage adressé par contenu** : les fichiers sont stockés selon le Hash de leur contenu, un même contenu n'est stocké qu'une fois (déduplication).
- **Cycle de vie** : l'objet présente une transition d'état claire de l'écriture à « supprimable/récupérable », évitant la suppression accidentelle d'un fichier en cours.

### 5) WebDAV et partage
- **WebDAV** : fournit en protocole WebDAV standard la lecture/écriture de fichiers, opérations de répertoire, copie/déplacement, verrouillage, etc., compatible avec le montage de disque de bureau.
- **Lien de partage** : fichiers/répertoires peuvent générer un lien de partage (accessible/téléchargeable sans connexion), unique **sortie sans connexion** du système ; peut être défini avec expiration ou révoqué à tout moment.

### 6) Synchronisation en temps réel
- **Push SSE** : lorsqu'un fichier change, il est poussé en temps réel au client en ligne via une connexion longue.
- **Récupération incrémentale** : le client récupère les modifications par curseur de façon incrémentale ; même si le push est perdu, la cohérence finale n'est pas affectée (la récupération est le chemin autorisé).
- Cas d'usage : synchronisation bidirectionnelle du répertoire du bureau et du cloud.

### 7) Back-office et sécurité
- **Back-office** : `/admin` fournit la page de gestion back-office (utilisateurs, organisation, gouvernance des espaces).
- **Contrôle des permissions** : le jeton du point de terminaison `web` limite l'accès au back-office ; les jetons H5/bureau ne peuvent pas y entrer.
- **Limitation de débit** : les familles d'interfaces comme connexion, téléversement, lecture de fichier ont une limitation indépendante, formant une double protection avec Nginx ; prévient le brute-force et le bourrage d'interface.

### 8) Stockage et exploitation
- **Inspection des objets** : analyse périodique du disque par le back-office, détectant les fuites d'objets (déchet sans référence) et les orphelins inversés (référence sans fichier).
- **Réconciliation de quota** : comparaison périodique « usage logique vs occupation réelle », alerte en cas de dérive trop grande et réécriture correctrice automatique possible.
- **Forme de déploiement** : binaire unique + systemd + Nginx ; scripts de sauvegarde/restauration et de validation fournis avec le dépôt.
- **Forme de stockage** : disque local par défaut (adressé par contenu) ; stockage local uniquement, sans backend de stockage objet tiers.

---

## IV. Forme de déploiement et d'exécution

- **Binaire unique** : l'ensemble du service est compilé en un fichier exécutable, géré par systemd ; Nginx fait proxy inverse et HTTPS.
- **Dépendances préalables** : PostgreSQL 17 + Redis (dépendances auth/limitation de débit, refus de démarrage s'ils sont indisponibles).
- **Configuration** : fichier yaml + variables d'environnement ; **mots de passe/clés uniquement par variables d'environnement**, jamais écrits dans le yaml ni versionnés.
- **Migration de base** : le service intègre les scripts de migration, exécutés automatiquement au déploiement.
- **Front du back-office** : déjà `embed` dans le binaire, aucun répertoire front séparé à déployer.
