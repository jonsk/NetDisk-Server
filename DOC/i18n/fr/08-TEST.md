# Document de test de Server-com

> Objectif : savoir **quels tests existent**, **comment les lancer**, **les résultats réels actuels**, **ce qui nécessite un humain**.
> Liens : `DOC/02-ARCHITECTURE.md` (conception), `DOC/05-INSTALL.md` (installation), `DOC/06-OPS.md` (exploitation), `DOC/07-BACKUP.md` (sauvegarde/restauration)

---

## 1. Aperçu des couches de test

| Couche | Que fait-elle | Qui l'exécute |
|---|---|---|
| Compilation / vérification statique | Garantir la compilation, aucune construction suspecte | Développement / CI, automatique |
| Tests unitaires + intégration | Vérifier les règles métier fonction par fonction / interface par interface | Développement / CI, automatique |
| Porte de discipline architecturale | Garantir le découpage en couches, les lignes rouges, la cohérence documentaire | CI, automatique |
| Porte de couverture | Garantir que les paquets clés ne sont pas à découvert | CI, automatique |
| Sonde de bout en bout | Fumée HTTP réelle sur toute la chaîne | Après déploiement / avant publication, nécessite une machine réelle |
| Référence de performance | Valider liste / synchronisation / débit concurrent dans les seuils | Machine réelle, script reproductible |
| Réception manuelle | Clics UI, chemins ultra-longs, plateformes réelles, etc. | Exploitation / personnel de réception |

> Dans le projet, `cmd/testsgate`, `cmd/depsguard`, `cmd/coveragegate` sont tous des **programmes de porte indépendants**.
> Les commandes locales et les commandes CI sont identiques (la CI utilise les mêmes commandes).

---

## 2. Portes automatisées (une commande par couche)

Exécuter à la racine du dépôt (`Server-com/`) :

| Couche | Commande | Bloque quoi |
|---|---|---|
| Compilation / statique | `go build ./... && go vet ./...` | Erreurs de compilation, constructions suspectes |
| Unitaires + intégration | `NETDISK_TEST_DSN='...' go test ./... -count=1` | Tous les cas Go (y compris PG/Redis réels) |
| Intégration non sautée | `go run ./cmd/testsgate` | Le « faux vert » du type « groupe entier skip faute de DSN alors que CI est tout vert », nomme tout cas sauté |
| Concurrence de données | `go run ./cmd/testsgate -race` | Concurrence de données (nécessite gcc) |
| Architecture et discipline documentaire | `go run ./cmd/depsguard` | Paquet noyau dépendant de la couche HTTP / discipline bureau / lignes rouges / cohérence docs et artefacts d'exploitation |
| Couverture | `go run ./cmd/coveragegate -profile <cover.out> -min 55` | Couverture des paquets clés sous le seuil |
| Sonde de bout en bout | `go run ./cmd/probesmoke -pass <mot de passe>` | Chaîne HTTP réelle entière (téléversement/téléchargement/partage/WebDAV/audit/patrouille…) |

> Les tests d'intégration nécessitent la base `netdisk_test` + Redis. Sans PG/Redis réels, les cas d'intégration sont sautés ;
> le rôle de `testsgate` est précisément de **surveiller « si des cas ont été sautés en cachette »**, évitant le faux vert en CI.

---

## 3. Tests unitaires + intégration (état actuel)

- **1042 cas passent** au total, couvrant 39 paquets / **0 sauté** (revus nommément par `testsgate`).
- Couverture par domaine : jeton d'authentification / finalisation de téléversement / concurrence du verrou d'objet / sémantique des répertoires / changements de synchronisation / partage / réconciliation de quota / patrouille / WebDAV.

**Critères de jugement par couche pour les assertions sensibles aux performances** (ex. fenêtre de verrou de finalisation concurrente P95) :
- Le critère principal utilise la **médiane** (une régression ralentirait chaque détention de verrou) ;
- La queue utilise P95 < 500ms (absorbe les à-coups machine de « tout le paquet en parallèle + base de test partagée »).
- Vérification par l'inverse : dormir artificiellement dans le secteur critique du verrou → la médiane ralentit aussitôt, le cas passe au rouge, ce qui prouve qu'il s'agit d'un « élargissement du secteur critique » et non d'un à-coup.

---

## 4. Sonde de bout en bout (probesmoke)

Une seule commande fait une fumée sur toute la chaîne d'un **service réel en cours d'exécution** (51+ étapes) : connexion → répertoire → téléversement (TUS/WebDAV/multipart) →
téléchargement/Range → partage → événements de synchronisation → audit → patrouille.

```sh
go run ./cmd/probesmoke -base http://127.0.0.1:8080 -user admin -pass '<mot de passe>'
```

> En passant par le proxy inverse, les en-têtes SSE sont consommés par Nginx (`X-Accel-Buffering`), donc **la sonde doit se connecter directement à l'application** (51/51 réussis) ;
> via le proxy inverse, c'est 50/51, et cette différence est expliquée (ce n'est pas un incident).

---

## 5. Référence de performance (mesurée le 2026-09-12)

Script : `deploy/verify/06-perf-baseline.sh` (autonome, reproductible, chiffres produits par le script en mesure réelle). Machine : Debian 13,
6 vCPU / ~2Go RAM / écriture séquentielle 2723,6 Mo/s (base machine locale).

| Point d'acceptation | Seuil | Mesuré | Conclusion |
|---|---|---|---|
| Liste de répertoire 100 000 lignes | P95 ≤ 500ms | **P95 107,2 ms** (limite de page par défaut=200) | Réussi (marge ~4,7×) |
| Perception changement distant (`/changes` visible) | P95 ≤ 3s | **P95 78,1 ms** (écriture multipart) | Réussi (marge ~37×) |
| Perception changement distant (trame SSE arrive) | P95 ≤ 3s | **P95 80,5 ms** | Réussi |
| Téléversement concurrent 8×8Mo | — | **9,66 Mo/s**, succès 8/8, échec 0 | Réussi |
| Téléversement concurrent 16×8Mo | — | **10,57 Mo/s**, succès 16/16, échec 0 | Réussi (12 fois limité puis réessayé) |
| Téléchargement concurrent 8/16×8Mo | — | **1944 / 2580 Mo/s** | Réussi (hit cache page, ne pas prendre pour débit disque) |

> **Explication honnête** : lors du téléversement concurrent 16, `POST /tus` a été limité **429** par le serveur 12 fois (`rate_limits.upload=10/s`),
> le script a tout réessayé avec repli et a réussi — c'est la **limitation qui s'applique conformément à la conception**, pas un échec de téléversement. Si l'acceptation exige « 16 concurrents réussis du premier coup »,
> il faut augmenter `rate_limits.upload` ou réduire la concurrence. Le téléchargement à ~2 Go/s est une vitesse « mémoire + disque virtuel », indiquant seulement qu'en concurrence le chemin de téléchargement ne fait ni file d'attente ni erreur.

### Mode de reproduction

```sh
BASE=http://127.0.0.1:8080 USER=admin PASS='<mot de passe>' \
ROWS=100000 NREP=60 NFEED=30 SAR="8 16" UPFILE=8388608 \
sudo sh deploy/verify/06-perf-baseline.sh | tail -n +1
```

Le script affiche à la fin (échantillons par passage / histogramme / quantiles / ligne de verdict).
Par défaut, les données de sondage sont nettoyées automatiquement ; `KEEP=1` permet de les conserver (pour le dépannage).

---

## 6. Entrées de contrôle d'acceptation et de relance

| Domaine d'acceptation | Entrée de relance |
|---|---|
| Référence unitaire / couverture | `go test ./... -coverprofile=cover.out` → `coveragegate` |
| Tests d'intégration un par un | `go test -tags=integration ./...` |
| Référence de performance | `deploy/verify/06-perf-baseline.sh` (machine réelle) |
| Déploiement et mise à niveau | `deploy/verify/0[12567]-*.sh` (scripts machine réelle un par un) |
| Sauvegarde / restauration / PITR | `deploy/backup/*.sh` (scripts d'exercice + registre) |
| Surveillance et alertes | `curl /metrics`, `promtool check rules` |
| Cohérence d'objets / récupération fichiers temporaires | `go test ./internal/storage/ -run 'ReapTemp|MismatchedObjectKeys' -count=1 -v` |
| Concurrence de données | `go run ./cmd/testsgate -race` |

---

## 7. Éléments nécessitant encore une exécution manuelle (ne pas prétendre les couvrir en automatisation)

| Élément | Pourquoi l'automatisation ne couvre pas | Où enregistrer |
|---|---|---|
| Chemin ultra-long sur Windows sur le terrain | Nécessite un vrai lecteur et l'environnement où `longPathAware` s'active | Liste de réception client |
| Comportement de réessai de l'antivirus verrouillant les fichiers | Nécessite un vrai AV accroché | Idem (marquer honnêtement « non vérifié ») |
| Clics d'interface (connexion/zone de notification/paramètres/conflit/navigation distante/vérification mise à jour) | Pas d'automatisation UI | Liste de réception client |
| Longue exécution client 24h | Durée et session d'interaction réelle | Exécution manuelle avant publication |
| Exercice tiers selon manuel (sauvegarde/restauration) | Nécessite « une autre personne » opérant selon le manuel | Registre d'exercice |

---

## 8. Trois disciplines de test

1. **Ne pas pouvoir juger ≠ réussi** : un vérificateur devant « impossible à juger » doit juger l'échec, pas le traiter comme réussi.
2. **La vérification par l'inverse fait partie de l'assertion** : chaque assertion clé doit faire une fois « casser le code → l'assertion doit passer au rouge »,
   sinon on ne sait pas quoi surveiller (il est déjà arrivé plusieurs fois dans le dépôt que « cassé, ça reste vert »).
3. **La preuve doit être reproductible** : preuve = commande + sortie réelle + version du moment ; coller une sortie sans source de commande ne compte pas comme preuve.
