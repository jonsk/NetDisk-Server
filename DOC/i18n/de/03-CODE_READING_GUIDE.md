# Server-com Code-Lese-Leitfaden

> Gilt für: Server-com (Open-Source-Edition / Community) · Go-Server-Einzelbinaire Cloud-Speicher
> Zielgruppe: Entwickler und Betreiber, die diesen Code verstehen, durchsuchen oder weiter entwickeln müssen.
> Dieser Leitfaden erklärt „wie der Code organisiert ist, wie Daten fließen, wo man für Änderung an X suchen muss“, nicht Zeile für Zeile den Algorithmus.
> Modulweise Abnahme und Entwicklungs-Rote-Linien siehe `../../01-FEATURE_LIST.md` im selben Verzeichnis; die autoritative Gestaltung finden Sie unter `DOC/02-ARCHITECTURE.md` (V3.0).

---

## 1. Was dieses Repository ist

`Server-com` ist ein **Backend-Dienst** eines Enterprise-Cloud-Speichers, geschrieben in Go, am Ende kompiliert zu **einer ausführbaren Datei** (Einzelbinaire).

Es bietet nach außen vier Arten von Fähigkeiten:

| Fähigkeit | Beschreibung | Haupteingang |
|---|---|---|
| REST-Verwaltungs-/Datei-Schnittstelle | Backend-Verwaltung, Datei-CRUD, Download, Freigabe | `/api/v1/*` |
| TUS-Resumable-Upload | Chunk-Upload großer Dateien | `/tus/*` |
| WebDAV | Desktop-Einbindungslaufwerk (kompatibel mit Dateiprotokoll) | `/webdav/*` |
| SSE-Echtzeit-Sync | Pusht „Datei hat sich geändert“ an den Client, Client ruft dann Inkrement ab | `/api/v1/events` |

Es ist **nicht** zuständig für: HTTP-Reverse-Proxy (Nginx), Frontend-Seiten (`web`-Repo), Desktop-Client (`desktop`-Repo). Diese drei sind eigenständige Repos oder externe Komponenten.

> Kurz gesagt: dieses Repository = eine Reihe von Go-Paketen + eine Gruppe von Befehlen + Migrationsskripte + Deployment-Material, das am Ende ein `netdisk`-Binaire erzeugt.

---

## 2. Top-Level-Verzeichnisstruktur

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

Leseempfehlung: **zuerst die Start-Montage in `cmd/netdisk/main.go` lesen (§5)**, dann entlang „wie eine Anfrage läuft“ (§6) in `internal/api` gehen, danach bei Bedarf in ein bestimmtes Fachpaket eintauchen.

---

## 3. Befehle (cmd/) —— jeder ist ein eigenständiges kleines Programm

`cmd/` enthält 8 Verzeichnisse, jeweils ein `main`-Programm:

| Befehl | Was es tut | Wann verwendet |
|---|---|---|
| **netdisk** | **Hauptdienst**, der einzige produktionsorientierte Prozess | der, den `systemd` startet |
| **migrate** | Datenbank-Migration | beim Deployment `go run ./cmd/migrate up` |
| **passwd** | Konto-Betrieb: Benutzer anlegen / Passwort zurücksetzen / aktivieren-deaktivieren | manuelle Konto-Operation durch Betreiber |
| **probesmoke** | End-to-End-Sonde: echtes HTTP einmal durchspielen Login→Upload→Download | Selbsttest in der Entwicklung |
| **devdb** | lokale Entwicklungs-/Test-DB-Wartung | vom Entwickler lokal genutzt |
| **depsguard** | Architektur-Gateway: prüft „Kernpaket darf HTTP-Schicht nicht abhängen“ usw. | CI statische Analyse |
| **testsgate** | Integrations-Test-Gateway: bestätigt, dass Tests wirklich liefen | CI |
| **coveragegate** | Coverage-Gateway: Statistik der Anweisungs-Abdeckung pro Paket | CI |

In der Produktion interessieren nur die ersten beiden: `netdisk` und das beim Deployment genutzte `migrate`. Der Rest sind Entwicklungs-/CI-Hilfen.

---

## 4. internal/ Paket-Karte —— wo der Code ist, was er tut

Die 36 Pakete lassen sich nach Zuständigkeit in fünf Schichten unterteilen. Das Verständnis der Schichtung ist der Schlüssel zum Lesen dieses Repos: **Die HTTP-Schicht macht nur „Anfrage empfangen, Service aufrufen, Antwort zurückgeben“, die Geschäftslogik liegt in der Service-Schicht, das eigentliche Lesen/Schreiben der Datenbank in der Repo-Schicht, ganz unten sind Datenbank/Redis/Festplatte.**

### 4.1 Eingang & HTTP-Schicht (die Sie in diesem Repo zuerst berühren)
| Paket | Zuständigkeit |
|---|---|
| **api** | **Routing-Gesamt-Montage**. Alle URL↔Handler-Funktion-Mappings stehen in `api.go`. Fast alles „welcher Endpoint ruft wen“ lässt sich hier finden |
| **middleware** | Middleware-Kette: Request-ID, Real-IP, strukturierter Fehler, Logging, Crash-Recovery. Die „Sicherheitskontrollpunkte“, die die Anfrage passiert |
| **apierr** | einheitliche Fehler-Kapselung (HTTP-Statuscode + Geschäftscode + chinesischer Text) |
| **webui** | `/admin` Backend-Frontend-Static-Resource-Eingang (in Binaire `embed`det, Nginx hostet nicht mehr) |
| **reqctx** | Werkzeug, das den „aktuell angemeldeten Benutzer“ (Actor) in den Request-Kontext legt |

### 4.2 Authentifizierung & Berechtigung
| Paket | Zuständigkeit |
|---|---|
| **auth** | JWT-Ausstellung/Prüfung (das Token selbst, zustandslos) |
| **authsvc** | Geschäfts-Orchestrierung von Login/Refresh/Logout (DB abfragen, Passwort prüfen, sperren, Token-Versions-Kopplung) |
| **webdavauth** | WebDAV Basic-Auth-Kanal |
| **credentials** | Passwort-Hash (bcrypt-artig) und Stärke-Strategie |

### 4.3 Geschäfts-Service-Schicht (jeweils eine Geschäftsklasse, am lohnendsten im Detail zu lesen)
| Paket | Zuständigkeit |
|---|---|
| **usersvc** | Backend-Benutzerverwaltung (Konto anlegen/aktivieren-deaktivieren/Rolle) |
| **orgsvc** | Organisationsstruktur (Abteilungsbaum) |
| **spacesvc** | Bereiche (persönlicher Bereich/Team-Bereich) und Mitglieder-Kollaboration |
| **filesvc** | Datei-Metadaten (Liste/Umbenennen/Verschieben/Löschen/Verzeichnis erstellen) |
| **uploadsvc** | Upload-Task-Erstellung, Ticket-Prüfung, TUS-Datenebene |
| **fastupload** | „Besitz-Nachweis“-Herausforderung des Instant-Uploads |
| **finalize** | **Finalisierung** —— der einzige Eingang, in dem alle Uploads letztlich geschrieben werden, der wichtigste Schreibpfad im gesamten Netz |
| **sharesvc** | Freigabe-Link (der einzige ausgeloggte Ausgang des Systems) |
| **dirops** | Verzeichnis-Ebene asynchrone Task-Queue (sehr große Verzeichnisoperationen laufen im Hintergrund) |
| **quotareconcile** | Kontingent-Abgleich und Drift-Warnung |
| **patrol** | Objekt-Inspektion: scannt Platte nach Lecks/Waisen-Dateien |

### 4.4 Domänenmodell & Synchronisation
| Paket | Zuständigkeit |
|---|---|
| **model** | Entitäten (struct), die streng mit Datenbanktabellen korrespondieren |
| **namepolicy** | der alleinige Schiedsrichter für Dateinamen (prüft Gültigkeit, Länge, Tiefe) |
| **objlock** | Objekt-Ebene-Sperre (Mutex-Disziplin beim Schreiben derselben Datei, verhindert konkurrierendes Überschreiben) |
| **syncfeed** | Schreiben/Lesen des Änderungsstroms (wer hat was geändert) |
| **syncsse** | SSE-Event-Kanal (Push an Client) |

### 4.5 Infrastruktur (meistens müssen Sie nur wissen, dass es existiert)
| Paket | Zuständigkeit |
|---|---|
| **config** | Konfigurationsladung (yaml + Umgebungsvariablen) |
| **db** | PostgreSQL-Verbindungspool und Transaktionen |
| **cache** | Redis-Kapselung und Key-Benennungskonvention |
| **ratelimit** | Redis-basiertes Rate-Limit |
| **repo** | SQL-Repository-Schicht —— **der gesamte SQL steht hier**, der Eingang zum „Daten ansehen“ |
| **storage** | lokaler Festplatten-Objektspeicher (inhaltlich adressiert, Dateien nach Hash abgelegt) |
| **migrate** | Datenbank-Versionsmigration (goose) |
| **obs** | strukturiertes Logging (slog) |
| **lifecycle** | Objekt-Lebenszyklus-Zustandsmaschine |
| **condreq** | HTTP-bedingte Anfragen (If-Match/If-None-Match, optimistische Sperre) |
| **webdavfs** | WebDAV-unterstützende zugrundeliegende Datei-Sicht |

> **Lese-Kurzreferenz**: will man „einen bestimmten HTTP-Endpoint“ ändern → `internal/api/api.go` Routing-Zeile + passendes `handlers_*.go` suchen;
> will man „wie die Datenbank liest/schreibt“ prüfen → `internal/repo/` suchen;
> will man „das finale Schreiben auf Platte“ finden → `internal/finalize/` ansehen (einziger Schreibpfad).

---

## 5. Start-Montage —— wie der Dienst „zusammengebaut“ wird

Alles beginnt mit `cmd/netdisk/main.go`. Gemäß der nummerierten Schrittfolge in den Kommentaren ist die Montage-Reihenfolge:

1. **Konfiguration**: Standardwert ← yaml ← Umgebungsvariable (späteres überschreibt früheres)
2. **Validierung**: alle Probleme auf einmal ausgeben, bei Problemen Start verweigern (fail-fast)
3. **PostgreSQL**: Verbindung zur DB, Migration ausführen
4. **Redis**: ohne Verbindung kein Start (Auth/Rate-Limit hängen davon ab)
5. **Token-Verwaltung**: JWT-Ausstellung, Redis, Rate-Limiter, verschiedene Geschäfts-Services initialisieren
6. **HTTP-Dienst**: das von `internal/api` montierte Routing an den Port hängen, mit Lauschen starten

Diese Datei ist der Ort der „Dependency Injection“ —— **wo welche Services mit `new` erzeugt und welche Abhängigkeiten übergeben wurden, steht in dieser einen Datei**. Um zu verstehen, „wie ein bestimmter Service zusammengebaut wird“, lesen Sie sie; um dem System eine Abhängigkeit hinzuzufügen, ändern Sie ebenfalls sie.

---

## 6. Wie eine Anfrage läuft (wer den Fluss versteht, versteht das Repo)

Am Beispiel „Benutzer fordert nach Login die Dateiliste an“, die Kette ist:

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

**Merken Sie sich diesen Satz fett**: `api`-Schicht verwaltet „das äußere Erscheinungsbild“, `repo`-Schicht verwaltet „den Umgang mit der Datenbank“, die Service-Schicht dazwischen verwaltet „die Geschäftsregeln“. Je klarer die Schichtung, desto eher betrifft eine Änderung an einer Stelle nur genau diese Stelle.

---

## 7. Wie die Konfiguration verwaltet wird

Sehen Sie `deploy/config/config.example.yaml` (Konfigurationsbeispiel, die autoritativste Feldliste) sowie `internal/config/config.go` (Ladelogik).

- Priorität der Konfigurationsquellen: **Code-Standardwert < yaml-Datei < Umgebungsvariable**.
- **Sensible Werte (Passwörter, Schlüssel) laufen nur über Umgebungsvariablen, niemals in yaml**. Zum Beispiel `NETDISK_DB_PASSWORD`, `JWT_SECRET`, `NETDISK_REDIS_PASSWORD`.
- Große Konfigurationsblöcke umfassen: `server` (Port/Proxy), `database`, `redis`, `jwt` (Token-Gültigkeit), `policy` (Kontingent/Tiefe/Größenlimit), `patrol` (Inspektion), `webui` (Backend-Präfix), `storage` (Speicher-Stammverzeichnis), `log`, `rate_limits` (Rate-Limit).
- Bei Start wird validiert; ein Rate-Limit komplett 0, nicht beschreibbares Speicherverzeichnis usw. werden abgefangen.

> Betreiber-Tipp: Konfiguration ändern → yaml oder Umgebungsvariable bearbeiten → `netdisk` neu starten; **Frontend ändern erfordert Neukompilierung des Go-Binaries** (da die Seite `embed`det ist), nur der Prozess-Neustart reicht nicht.

---

## 8. Datenbank —— Kern-Tabellen-Kurzreferenz

Migrationen liegen in `internal/migrate/sql/` (13 versionierte Skripte, in das Binaire eingebettet). Kern-Geschäftstabellen:

| Tabelle | Speichert |
|---|---|
| `users` | Benutzerkonto |
| `spaces` | persönlicher/Team-Bereich |
| `space_members` | Bereichsmitgliedschaft |
| `groups` / `group_members` | Team und Mitglieder |
| `departments` / `department_closure` / `user_departments` | Organisationsstruktur (Abteilungsbaum + Closure-Tabelle) |
| `files` | Datei-/Verzeichnis-Metadaten |
| `file_objects` | das tatsächliche Objekt der Datei (inhaltlich adressiert) |
| `uploads` | Upload-Task (TUS-Session) |
| `shares` | Freigabe-Link |
| `file_locks` | Datei-Bearbeitungssperre |
| `refresh_tokens` | ständiger Login-Zustand (Refresh-Token) |
| `sync_feed` / `sync_cursors` | Änderungsstrom und Cursor der Synchronisation |
| `dir_op_tasks` | Verzeichnis-Ebene asynchrone Tasks |

> Hinweis: `audit_logs`, `idp_providers`, `idp_sync_state`, `user_idp_bindings`, `user_sso_bindings` sind **historische Tabellen** aus frühen Funktionen (Audit, Identity-Source-Anbindung), die der Code aktuell nicht mehr liest/schreibt; die Migrationshistorie darf nicht beliebig geändert werden, normale Beibehaltung genügt.

---

## 9. HTTP-Schnittstellen-Überblick

Alle Routen sind in `internal/api/api.go` zentralisiert. Nach Funktion gruppiert (Auth-Details weggelassen):

| Gruppe | Beispielpfad | Beschreibung |
|---|---|---|
| Gesundheit / Version | `GET /healthz`, `GET /api/v1/version` | Liveness, Version-Aushandlung (ohne Login) |
| Auth | `POST /api/v1/auth/login` / `refresh` / `logout` | Login mit Benutzername/Passwort und Token-Verlängerung |
| Meine Info | `GET /api/v1/me` | aktuell angemeldeter Benutzer |
| Datei | `GET /api/v1/files`, `GET /api/v1/files/{id}/content` | Liste, Download (Range unterstützt) |
| Datei-Schreiboperation | `PATCH /api/v1/files/{id}`, `DELETE /api/v1/files/{id}`, `POST /api/v1/files/dirs` | Umbenennen, Löschen, Verzeichnis erstellen |
| Upload | `POST /api/v1/upload/create`, `/tus/*` | Upload-Task erstellen + TUS-Chunk-Upload |
| Instant-Upload | `POST /api/v1/upload/{id}/finish` | Finalisierung nach bestandenem Besitz-Nachweis |
| Bearbeitungssperre | `POST/GET/DELETE /api/v1/files/{id}/lock` | Datei-Kollaborationssperre |
| Freigabe | `POST /api/v1/shares`, `GET /api/v1/shares/{token}/meta` | Freigabe-Link (Download ohne Login) |
| Bereich | `GET /api/v1/spaces`, `POST /api/v1/spaces` | meine Bereiche, Bereich erstellen |
| Organisations-/Benutzerverwaltung | `/api/v1/admin/departments*`, `/api/v1/admin/users*` | Backend-Verwaltung (Admin nötig) |
| Synchronisation | `GET /api/v1/events` (SSE), `GET /api/v1/changes` | Echtzeit-Push + inkrementelles Abrufen |
| WebDAV | `/webdav/*` (PROPFIND/PUT/COPY/MOVE/LOCK usw.) | Desktop-Einbindungslaufwerk |
| Task | `GET /api/v1/tasks/{id}` | Fortschrittsabfrage der Verzeichnis-Ebene asynchroner Tasks |

> Will man „in welcher Datei ein Endpoint implementiert ist“ finden: `api.go` schreibt mit `mux.Handle("METHOD /path", ...d.handleXxx...)`; `handleXxx` ist die Implementierungsfunktion, meist in `handlers_*.go` im selben Verzeichnis.

---

## 10. Ein paar Schlüssel-Designs, vor dem Lesen des Codes ein Konzept

Damit auch weniger erfahrene Leser nicht den Faden verlieren, zunächst diese vier mentalen Modelle:

1. **JWT nach Endpoint getrennt**: Token sind nach `web` (Backend)/`desktop` (Desktop) usw. Endpoints getrennt, **Backend-Token darf keine Desktop-Fähigkeit bedienen**, und umgekehrt. Das Token enthält keine Berechtigungen, Berechtigungen werden immer serverseitig entschieden.
2. **TUS-Resumable-Upload + einzelner Schreibpfad**: alle Uploads (TUS-Chunk, WebDAV PUT, Instant-Upload) fließen letztlich in diesen einen „Finalisierungs“-Eingang `finalize`, um sicherzustellen, dass das Schreiben von Dateien nur einen einzigen Pfad hat und nicht gegeneinander kämpft.
3. **Synchronisations-Doppelkanal**: der Server pusht über SSE „es gibt eine Änderung“; der Client ruft über den `/changes`-Cursor die konkreten Änderungen **ab**. Dass ein Push verloren geht, ist unkritisch, das Abrufen ist autoritativ. Der Cursor nutzt eine globale Auto-Inkrement-Nummer zur Ausrichtung.
4. **Inhaltlich adressierter Speicher**: `storage` legt Objekte nach dem Hash des Dateiinhalts ab, identischer Inhalt wird nur einmal gespeichert (Deduplizierung), der Dateiname ist nur Metadaten.

---

## 11. Drittanbieter-Abhängigkeiten (go.mod) sehr schlank

Nach dem „Slimming“ wurden die Abhängigkeiten stark reduziert, derzeit sind die Kern-Abhängigkeiten nur:

- **pgx** (PostgreSQL-Treiber), **goose** (Migration)
- **go-redis** (Redis-Client)
- **golang-jwt/v5** (JWT)
- **miniredis** (nur für Tests), **yaml.v3** (Konfiguration)

Keine Message-Queue, kein schweres Framework, kein Multi-Backend-Speicher-SDK (lokale Festplatte ist das einzige Backend). Das ist freundlich für die Fehlersuche: auf dem Stack gibt es nicht zu viele „unüberschaubare Abhängigkeiten“.

---

## 12. Build / Run / Deployment Kurznotiz

```bash
# lokaler Build (Go 1.26+ nötig)
go build ./...            # alles kompilieren (Fehler prüfen)
go vet ./...              # statische Analyse
go run ./cmd/depsguard    # Architektur-Gateway (auch in CI)

# das echte Deployment-Artefakt (erledigt deploy/build-release.sh)
# Reihenfolge wichtig: zuerst Frontend bauen (web) → dann go build zum Einzelbinaire → paketieren
```

- Einzelbinaire wird von `systemd` verwaltet, Nginx als Reverse-Proxy (`deploy/nginx/`).
- Detailliertes Deployment siehe `deploy/README.md`.
- Die Haupt-Programmlogik direkt lesen: `cmd/netdisk/main.go`.

---

## 13. Wo man anfängt zu lesen (für Erst-Einsteiger)

1. `cmd/netdisk/main.go` —— die Montage-Reihenfolge ansehen (in 5 Minuten das Globale verstehen)
2. `internal/api/api.go` —— alle Routen ansehen (in 5 Minuten wissen, welche Endpoints das System hat)
3. `deploy/config/config.example.yaml` —— welche Konfigurations-Knöpfe es gibt
4. Wählen Sie ein Geschäft, das Sie am meisten interessiert: Datei-Upload verfolgen Sie `finalize`, Organisationsstruktur verfolgen Sie `orgsvc`, Synchronisation verfolgen Sie `syncfeed`/`syncsse`
5. Ein echtes Problem debuggen: von `obs`-Log / einem Endpoint-Fehler → zurück zu `handlers_*.go` → `repo/*.go` den SQL ansehen

> Goldene Regel: **Trifft man auf „wofür ist das“, lies den Kommentar am Kopf der betreffenden Paket-Datei** —— jedes Paket/jede Funktion in diesem Repo hat umfangreiche chinesische Design-Kommentare, eine „lebendige Dokumentation“.
