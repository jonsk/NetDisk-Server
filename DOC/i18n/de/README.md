# NetDisk · Open-Source-Netzdisk-Server-Backend

> Ein selbst gehostetes Netzdisk-Backend für **Team-Dateizusammenarbeit** (Go-Einzelbinary).
> Bietet gemeinsame Team-Bereiche, TUS-/WebDAV-/multipart-Mehrprotokoll-Uploads, versioniertes Finalisieren,
> Echtzeit-Synchronisationserkennung (SSE + Cursor), feingranulare Berechtigungen und Quoten-Verwaltung – sowohl als privater Netzdisk einsetzbar
> als auch als zentrale Datei-Drehscheibe eines Unternehmens.
>
> **Lizenz:** [Apache-2.0](LICENSE) · **Ausprägung:** Community Edition (Server-com)

---

## 🌐 Mehrsprachig / Translations

[中文](../../../README.md) | [English](../en/README.md) | [Deutsch](../de/README.md) | [Français](../fr/README.md) | [Suomi](../fi/README.md) | [Русский](../ru/README.md)


---

## ✨ Funktionen

| Dimension | Fähigkeit |
|---|---|
| 🚀 **Upload & Finalisierung** | TUS-Chunk-Upload mit Fortsetzung / WebDAV PUT / multipart – drei Eingänge teilen denselben Finalisierungspfad; inhaltsadressiertes Objekt + Instant-Upload-Deduplizierung + Anti-Poisoning-Prüfung |
| 🧠 **Objektlebenszyklus** | `file_objects` mit vier Zuständen + objektbezogene Sperre (Zwei-Ebenen-Exklusivität); zuerst Objekt schreiben, dann kurze Transaktion – verhindert stilles Überschreiben und Dateidrift |
| 💾 **Lokaler Objektspeicher** | Inhaltsadressierter Speicher: Objekte werden nach Inhalts-Hash auf lokaler Festplatte abgelegt; gleicher Inhalt wird nur einmal gespeichert (Deduplizierung) |
| 📡 **Synchronisationserkennung** | SSE-Echtzeit-Push + `/changes`-Cursor mit zwei Zuständen (globales `change_seq`), keine periodischen Scans, Zusammenarbeit mit Desktop-Client |
| 🔐 **Identität & Berechtigung** | Eigenes Konto-Passwort-Login + JWT (HS256), Organisation/Abteilung/Gruppe, persönlicher Bereich / Team-Bereich, Freigabelinks, Quota-Abgleich |
| 🗂 **Verzeichnisseemantik** | Verzeichnistiefe ≤31 / Pfad-Budget ≤240B; groß-/kleinschreibungsunempfindliche Duplikaterkennung; MOVE/DELETE-Schwellenwert-Aufteilung über asynchrone Queue |
| 🛡 **Betrieb & Sicherheit** | Quota-Abgleich + Objekt-Inspektion (Leck/Verwaist) Reparatur; objektbezogene Sperre + API-Ratenbegrenzung; Audit ist Platzhalter (derzeit nicht persistiert) |
| 📦 **Verwaltungs-Backend** | `/admin`-Verwaltungs-Backend (Benutzer / Organisation / Bereich / Quota), ausgeliefert als einzelne Go-`embed`-Binary |

---

## 🏛 Architekturüberblick

```
                       ┌──────────────────────────────────────┐
  Web-Verwaltungs-Backend /admin ─▶│                                      │
  WebDAV-Client  ─WebDAV▶│        netdisk (Go-Einzelbinary)          │
  TUS-Uploader      ─TUS─▶│   REST · TUS · WebDAV · SSE einheitlicher Eingang  │
  Desktop-Sync-Client  ─SSE──▶│                                      │
                       │   ┌──────────────────────────────┐   │
                       │   │ finalize einziger Pfad+Sperre │   │
                       │   │ Inhaltsadressierung → lokaler  │   │
                       │   │ Festplatten-Objektspeicher     │   │
                       │   └──────────────────────────────┘   │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(Proxy)│
               │ Metadaten│  │Sitz/Rate│   │  ein Port   │
               └──────────┘  └────────┘   └────────────┘
```

- **PostgreSQL 17 (mindestens unterstützt)** —— einzige Metadaten-Engine (inkl. WAL-Archivierung); Primärschlüssel nutzt `gen_random_uuid()`, benötigt PG ≥ 17
- **Redis** —— Sitzung / Ratenbegrenzung / Task-Queue (`SKIP LOCKED`, keine MQ)
- **Nginx** —— Reverse-Proxy, einzelner Port nach außen
- Das Frontend des Verwaltungs-Backends wird nach `pnpm build` im `web`-Repo nach `internal/webui/dist/` kopiert und über **Go `embed`** ausgeliefert

---

## 🛠 Technologie-Stack

| Ebene | Auswahl |
|---|---|
| Sprache | Go 1.26 |
| HTTP | Standardbibliothek `net/http` + `ServeMux` Middleware-Kette |
| DB | pgx v5 + sqlc + goose Migration |
| Auth | golang-jwt v5 (HS256) |
| Speicher | Lokaler Festplatten-Objektspeicher (inhaltsadressiert) |
| Konfiguration | yaml.v3 + strenge Validierung von Umgebungsvariablen |
| Bereitstellung | systemd + Nginx + Prometheus (Viererset, ein Port) |

---

## 🚀 Schnellstart

### Voraussetzungen

- Go 1.26+
- PostgreSQL 17+ (mindestens 17; Datenbanken: `netdisk` / `netdisk_test`, `LC_COLLATE=C`)
- Redis 7+ (`appendonly yes`, `noeviction`)
- Nginx (Produktion)

### 1. Build

```bash
# Umgebung (Inlands-Beschleunigung + Prüfung aus)
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off

go build ./...     # kompilieren
go vet ./...       # statische Analyse
```

> **Hinweis (embed):** Wenn Frontend-Artefakte bereits in `internal/webui/dist/` vorhanden sind, werden sie beim Kompilieren eingebettet;
> nach Aktualisierung des Frontends zuerst `pnpm build`, dann `go build`, sonst werden alte Artefakte eingebettet.

### 2. Datenbankmigration

```bash
go run ./cmd/migrate -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up
# oder die bereits gebaute goose-Binary verwenden
```

### 3. Konfiguration

Kopieren Sie `deploy/config/config.example.yaml` nach `config.yaml` und passen Sie es nach Bedarf an.
**Alle geheimen Konfigurationen erfolgen ausschließlich über Umgebungsvariablen** (`NETDISK_JWT_SECRET` usw., Standard/zu kurz führt zur Verweigerung des Starts),
niemals in die yaml schreiben —— Details siehe `deploy/systemd/secrets.env.example`.

### 4. Ausführen

```bash
go run ./cmd/netdisk
# hört auf :8080
# /admin   Verwaltungs-Backend  /api/v1/*  REST  /dav/*  WebDAV  /changes  inkrementelles Pull  /sync/events  SSE
```

---

## 🔌 API-Fähigkeiten

| Eingang | Beschreibung |
|---|---|
| `REST /api/v1/*` | Authentifizierung, Benutzer, Abteilung, Bereich, Datei, Verzeichnis, Upload, Download, Freigabe, Ereignis |
| `Download & Range` | Streaming-`ServeContent`; byte-genaues Range (200/206/416); If-Match/If-None-Match/If-Range bedingte Anfragen |
| `TUS /uploads/*` | Chunks, Fortsetzung, Ticket-Wiederverwendung, Staging-Recycling, Platten-Wasserstand (>90% → 507) |
| `WebDAV /dav/*` | `x/net/webdav` + PUT-Interception über Finalisierung (≤100MB); LOCK-Semantik selbst implementiert |
| `SSE /sync/events` | Echtzeit-Push entfernter Änderungen (Selbst-Echo-Unterdrückung) |
| `Cursor /changes` | inkrementelles Pull mit zwei Cursor-Zuständen, globales `change_seq` |
| Ratenbegrenzung | file_read / file_write gestaffelt; 429 erfordert Retry |

---

## 📂 Projektstruktur

```
Server-com/
├── cmd/          ausführbare Einstiegspunkte
│   ├── netdisk         Hauptdienst
│   ├── migrate         Datenbankmigration
│   ├── passwd         (Administrator-Anmeldedaten)
│   ├── depsguard       Architektur-Disziplin mechanische Prüfung
│   ├── testsgate       Integrationstest-Gate
│   ├── coveragegate    Coverage-Gate
│   ├── devdb           lokale Entwicklungs-DB erstellen
│   └── probesmoke      Smoke-Test-Sonde
├── internal/         Kernlogik (37 Pakete)
│   ├── finalize/       einziger Schreibpfad ★
│   ├── objlock/        objektbezogene Zwei-Ebenen-Sperre (ADR-2)
│   ├── storage/        lokaler Festplatten-Objektspeicher
│   ├── uploadsvc/     TUS-Upload
│   ├── lifecycle/     Objektlebenszyklus-Worker
│   ├── syncfeed|syncsse/   Synchronisationserkennung
│   ├── webdavfs|webdavauth/ WebDAV
│   ├── api/           REST-Handler
│   ├── patrol/        Objekt-Inspektion
│   └── quotareconcile/ Quota-Abgleich
├── deploy/          Deployment-Material (systemd/Nginx/Prometheus/Backup/Provisionierung/Verifikation)
├── scripts/         Build- und Generierungsskripte
├── DOC/             Funktionsliste / Architektur / Installation / Betrieb / Backup / Test / API-Leitfaden / Code-Lese-Leitfaden
│   └── api/          OpenAPI-Vertrag openapi.yaml (im Repo gepflegt, Autorität in GO\Doc\api)
└── LICENSE          Apache-2.0
```

---

## 🧪 Tests & Gates

- `go test ./...` —— Unit-Tests
- `go test -tags=integration ./...` —— Integrationstests (benötigt `netdisk_test`-Datenbank + Redis)
- `cmd/depsguard` —— mechanische Prüfung der Roten Linien (z. B. Kernel-Pakete hängen nicht von HTTP-Schicht ab)
- `cmd/testsgate` / `cmd/coveragegate` —— Gate für Integration/Coverage
- Vor dem Commit lokal empfohlen: `go build ./...` / `go vet ./...` / `cmd/depsguard`

---

## 🤝 Beitragen

Stellen Sie vor dem Einreichen sicher, dass `go build ./...`, `go vet ./...`, `depsguard` grün sind; neues Verhalten hält die Roten Linien und ADRs des Architektur-Dokuments V3.0 ein; reichen Sie keine Schlüssel oder Anmeldedaten ein.

---

## 📄 Lizenz

Dieses Projekt wird unter der **Apache License 2.0** ([Apache-2.0](LICENSE)) veröffentlicht.
Sie dürfen es frei nutzen, modifizieren, verteilen und für **kommerzielle** Zwecke verwenden (einschließlich abgeleiteter Closed-Source-Produkte),
müssen jedoch **ursprüngliche Urheberrechts- und Lizenzhinweise beibehalten** und Änderungen in geänderten Dateien kenntlich machen.
Dieses Projekt wird **wie besehen** (AS IS) bereitgestellt, **ohne ausdrückliche oder stillschweigende Gewährleistung**; Verantwortung für Deployment, Betrieb und Compliance liegt beim Nutzer.

### Verhältnis zur kommerziellen Edition

`Server-com` (dieses Repo, Community Edition) beinhaltet keine Unternehmens-Service-Zusicherungen und enthält keine Anmeldedaten oder privaten Konfigurationen.

---

*Vollständige Funktionsliste siehe [`DOC/01-FEATURE_LIST.md`](DOC/01-FEATURE_LIST.md); Bereitstellung und Betrieb siehe [`DOC/05-INSTALL.md`](DOC/05-INSTALL.md) · [`DOC/06-OPS.md`](DOC/06-OPS.md) · [`DOC/07-BACKUP.md`](DOC/07-BACKUP.md); Schnittstellendetails siehe [`DOC/04-API_GUIDE.md`](DOC/04-API_GUIDE.md).*