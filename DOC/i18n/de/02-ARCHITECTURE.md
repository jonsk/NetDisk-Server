# Server-com Architektur-Dokumentation

> Version: mit der Server-com Community-Edition
> Verwandt: `DOC/05-INSTALL.md` (Bereitstellung), `DOC/06-OPS.md` (Betrieb), `DOC/07-BACKUP.md` (Sicherung/Wiederherstellung), `DOC/08-TEST.md` (Abnahme), `DOC/04-API_GUIDE.md` (Schnittstellen), `DOC/03-CODE_READING_GUIDE.md` (Code-Lesen)

---

## 1. Überblick und Positionierung

`Server-com` ist ein **serverseitiger Unternehmens-Netzdatenträger (Netdisk)**, geschrieben in **Go** und am Ende zu **einer ausführbaren Datei** (Single-Binary) kompiliert.
Er bietet gemeinsame Team-Bereiche, Multi-Protokoll-Uploads (TUS / WebDAV / multipart), versioniertes Finalisieren, Echtzeit-Synchronisationserkennung,
feingranulare Berechtigungen und Quoten-Verwaltung. Er lässt sich sowohl als privater Netzdatenträger betreiben als auch als zentrale Datei-Drehscheibe eines Unternehmens einsetzen.

Die Community-Edition (Server-com) behält nur **Passwort-Login + Verwaltungs-Backend** und entfernt den Enterprise-WeChat/DingTalk/IdP-Drittanbieter-Login
sowie den H5-Mobilkanal. Sie ist eine funktionale Teilmenge der kommerziellen Edition (Server-Ent).

**Kern-Gedanke: Der gesamte Dienst ist eine „große Kiste", und außen arbeiten vier Komponenten mit ihr zusammen.**

## 2. Gesamtarchitektur

### 2.1 Bereitstellungsbestandteile (Viererset)

| Komponente | Funktion | Anmerkung |
|---|---|---|
| **netdisk** (Erzeugnis dieses Repositories) | Nimmt HTTP-Anfragen entgegen, verarbeitet die Geschäftslogik, liest/schreibt Metadaten und Objekte | Dasselbe Binary trägt REST / WebDAV / TUS / SSE / eingebettetes Verwaltungs-Backend |
| **PostgreSQL 17 (mindestens unterstützt)** | Einzige Metadaten-Engine (alle „Verzeichniseinträge/Benutzer/Bereiche/Quoten" liegen hier) | Enthält WAL-Archivierung für Point-in-Time-Recovery. **Minimum 17**: Der Primärschlüssel-Standardwert nutzt `gen_random_uuid()` (seit PG 13 eingebaut), die Untergrenze liegt also nicht an der UUID; Minimum 17 (passend zu gängigen Distro-Versionen). Versionsanforderungen siehe 05-INSTALL.md §1 |
| **Redis** | Sitzungen (Token) / Ratelimit-Fenster / Change-Stream-Cursor / Task-Queue | **Kein Cache** – gehen die Token verloren, werden alle abgemeldet |
| **Nginx** | Reverse-Proxy, ein einzelner nach außen gerichteter Port | Nur in Produktion nötig; in der Entwicklung ist der direkte Zugriff auf die Anwendung möglich |

```
                       ┌──────────────────────────────────────┐
  Web-Verwaltungs-Backend /admin ─▶│                                      │
  WebDAV-Client  ─WebDAV▶│        netdisk (Go Single-Binary)          │
  TUS-Uploader      ─TUS─▶│   REST · TUS · WebDAV · SSE einheitlicher Eingang  │
  Desktop-Sync-Client  ─SSE──▶│                                      │
                       │   ┌──────────────────────────────┐   │
                       │   │ finalize einziger Schreibpfad + Objekt-Lock   │   │
                       │   │ Content-Addressing → lokaler Platten-Objektspeicher     │   │
                       │   └──────────────────────────────┘   │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(Reverse-Proxy) │
               │   Metadaten   │  │Sitzung/Ratelimit│   │  einzelner Port    │
               └──────────┘  └────────┘   └────────────┘
```

### 2.2 Warum Single-Binary + eingebettetes Frontend

Das Frontend des Verwaltungs-Backends wird vom web-Repository gebaut; das Erzeugnis wird **zur Compile-Zeit in das Go-Binary** eingebettet (`internal/webui/dist/`),
und von Go über `embed` direkt ausgeliefert (`/admin`). Vorteil: Die Auslieferung einer einzigen Datei genügt, ohne dass das Frontend-Static-Site separat auf dem Server bereitgestellt werden muss.

> ⚠️ Daher muss **bei einer Änderung des Frontends das Go-Binary neu kompiliert werden**: zuerst `pnpm build`, dann `go build`,
> bei vertauschter Reihenfolge wird das alte Erzeugnis eingebettet (es kompiliert, die Seite ist aber die alte Version – und es gibt keinen Fehler).

## 3. Schichtenarchitektur (Einteilung der internal-Pakete)

Der Code ist nach Verantwortung in 5 Schichten unterteilt; obere Schichten dürfen nur von unteren abhängen (mechanisch geprüft durch `depsguard`):

```
┌─────────────────────────────────────────────────────────────┐
│ 4. Eingangs-Schicht   cmd/  (netdisk / migrate / passwd / depsguard …)  │
├─────────────────────────────────────────────────────────────┤
│ 3. Schnittstellen-Schicht   internal/api/  (HTTP-Handler, Routing, Middleware)          │
│            internal/webdavfs/ webdavauth/                     │
├─────────────────────────────────────────────────────────────┤
│ 2. Geschäfts-Schicht   usersvc/orgsvc/spacesvc/filesvc/sharesvc/ │
│            uploadsvc/finalize/lifecycle/syncfeed/syncsse/    │
│            patrol/quotareconcile/consistency/credentials/    │
├─────────────────────────────────────────────────────────────┤
│ 1. Domäne/Daten  repo/(Datenzugriff) model/(Domänenmodell) objlock/ storage/  │
│            authsvc/ cache/ auth/ apierr/reqctx/middleware/    │
├─────────────────────────────────────────────────────────────┤
│ 0. Fundament      config/ db/ migrate/ ratelimit/ namepolicy/      │
│            dirops/ fastupload/ condreq/ webui/ obs/ syncssem  │
└─────────────────────────────────────────────────────────────┘
```

**Disziplin (Kern der R-xx-Rotlinien)**: „Kernel-Pakete“ wie `objlock` (Lock), `storage` (Objektspeicher), `model` (Domänenmodell) dürfen **niemals von `internal/api` (HTTP-Schicht) abhängen**. Die Geschäftslogik muss von HTTP entkoppelt sein,
damit sich die Geschäftsregeln selbst dann nicht ändern, wenn die Zugangsart gewechselt wird (WebDAV/TUS/interne Aufrufe).

## 4. Kern-Design-Mechanismen

### 4.1 finalize —— einziger Schreibpfad

Alle Uploads (TUS-Chunks, WebDAV PUT, multipart-Direktupload) münden schließlich **in dieselbe Finalisierungslogik** `finalizeUpload()`:
1. Zuerst Sekunden-Upload prüfen (bei vorhandenem Inhalts-Hash direkt wiederverwenden, gegen „Vergiftung");
2. Den Inhalt in den Objektbereich schreiben und das **content-adressierte** Objekt erhalten;
3. In einer kurzen Transaktion den Verzeichniseintrag und den Referenzzähler schreiben;
4. Den Change-Stream (sync_feed) schreiben, um die Synchronisationserkennung auszulösen.

Vorteil: Die drei Upload-Kanäle verhalten sich völlig identisch, was Lücken der Art „ein Kanal umgeht eine Regel" ausschließt.

### 4.2 Content-adressierter Objektspeicher

Objekte werden nach **Inhalts-Hash** abgelegt, Pfadform `objects/xx/xx/<sha256>`:
- **Gleicher Inhalt wird nur einmal gespeichert** (natürliche Deduplizierung): Laden zwei Benutzer dieselbe Datei hoch, liegt auf der Platte nur eine Kopie;
- Trennung von Dateiname und Inhalt: Eine Umbenennung kopiert keine Daten, sondern ändert Metadaten;
- Unveränderliche Objekte: Nach der Finalisierung wird der Inhalt nicht mehr überschrieben, was „stilles Überschreiben/Datei-Drift" ausschließt.

### 4.3 Objekt-Level-Doppelschloss (ADR-2)

Um zu verhindern, dass mehrere Anfragen gleichzeitig dasselbe Objekt finalisieren/löschen und dabei Daten verfälschen, wird ein Objekt-Level-Lock eingeführt:

| Ebene | Verwendung | Art des Lock-Haltens |
|---|---|---|
| **Sitzungs-Level-Lock** | Gesamter Finalisierungslauf, Lifecycle-Worker (der eigentliche Schreibpfad) | Eine einzelne dedizierte Datenbankverbindung durchgehend; das Lock-Halten hat ein festes Maximum (Deckel), um zu lange kritische Abschnitte zu vermeiden |
| **Transaktions-Level-Lock** | Reine Referenz +1 (Kopie, Freigabe in Bereich) | Nur innerhalb einer kurzen Transaktion gehalten |

Der Lock-Schlüssel wird durch den Objekt-Hash bestimmt; die Sperrreihenfolge erfolgt aufsteigend nach Hash, um Deadlocks zu vermeiden.

### 4.4 Dualer Synchronisationszustand (SSE + Cursor)

Der Desktop-Client benötigt „sobald sich remote etwas ändert, weiß ich es":
- **SSE-Push** (`/sync/events`): Änderungen in Echtzeit an online Clients pushen (schnell, kann aber Frames verlieren);
- **Cursor-Pull** (`/changes`): Client zieht mit Cursor (`since`) inkrementell nach (zuverlässig, als Rückfall).

Beide nutzen **denselben globalen `change_seq`** (eine `sync_feed`-Tabelle mit global aufsteigender Nummer),
der Client kann beide abwechselnd nutzen und sich mit `/changes` als Maßstab nachziehen. So entfällt die periodische Vollscan-Suche, ohne dass Änderungen verloren gehen.

### 4.5 JWT-zweiteilige Authentifizierung

Nach erfolgreichem Login wird ein JWT (HS256) ausgestellt, zweiteilig:
- **web-Seite** (Verwaltungs-Backend) und **desktop-Seite** (Desktop-Client) haben unabhängige Token (R-14),
  damit sich ein Leck an einer Stelle nicht auf den ganzen Körper auswirkt;
- Access- und Refresh-Token liegen in Redis, unterstützen Widerruf und „Single-Flight-Refresh" (nur ein Refresh-Request pro Moment erfolgreich).

Die Community-Edition behält nur **Konto-Passwort-Login** (`/api/v1/auth/login`); das Drittanbieter-OAuth (WeCom/DingTalk/IdP) wurde entfernt.

### 4.6 Verzeichnis-Semantik und Namensbeschränkungen

- Verzeichnistiefe ≤ 31 Ebenen und kumulierter Einzelpfad ≤ 240 Byte (doppeltes Limit, konfigurierbar);
- Dateinamens-Dubletten-Erkennung case-insensitive (`lower(name)` + Unique-Index);
- Verzeichnis-Ebene „Unterbaum verschieben/löschen" über Schwellenwert (Standard 1000 Zeilen) wird zu **asynchronem Task**, der `task_id` zurückgibt, damit der Client pollt.

## 5. Datenmodell (Kern-Tabellen)

| Tabelle | Bedeutung |
|---|---|
| `users` | Benutzer (Konto-Passwort-Hash, E-Mail, Anzeigename, Rolle) |
| `organizations` / `departments` / `groups` | Organisation / Abteilung / Gruppe |
| `spaces` | Bereiche (persönlicher Bereich + Team-Bereich; enthält Quota `used_bytes`, `last_seq`) |
| `space_members` | Bereichsmitglieder und Berechtigungen |
| `files` | Verzeichniseinträge (Dateien und Verzeichnisse; `is_dir`, `parent_id`, `name`, `depth`, `size`) |
| `file_objects` | Content-adressierte Objekte (Hash, vier Zustände: live/pending_delete…, Referenzzähler `ref_count`) |
| `sync_feed` | Change-Stream (globaler `change_seq`, 90 Tage Aufbewahrung) |
| `shares` | Freigabe-Links (Token, Berechtigung, Ablauf) |
| `audit_logs` | Audit-Log (derzeit Platzhalter, in der Community-Edition nicht persistiert) |

## 6. Konfigurations-Design

- **Standardwerte existieren nur einmal**: in der Go-Struktur `internal/config.Default()`; das Löschen eines Konfigurationsabschnitts fällt nicht auf Null zurück, sondern greift auf den Standard zurück;
- **secret kommt niemals in yaml**: `JWT_SECRET`, Datenbank-Passwort, Redis-Passwort laufen nur über **Umgebungsvariablen** (`NETDISK_*`);
- Priorität: Go-Standard < Konfigurationsdatei < Umgebungsvariable;
- Start-Selbsttest (`netdisk -config ... -check`): Jeder ungültige Eintrag wird **vollständig auf einmal** aufgelistet und mit Exit-Code ≠ 0 beendet, statt beim ersten Request mit 500 zu antworten;
- Zeiträume in lesbarer Schreibweise (`5m`/`30s`/`24h`).

## 7. Schlüsselabläufe

### 7.1 Lebenszyklus eines Uploads (TUS)

```
Client ─PATCH Chunk→ Staging-Bereich(tus-tmp) ─vollständig übertragen→ finalizeUpload()
    finalize: Hash berechnen → Sekunden-Upload-Dedup-Prüfung → Objektbereich schreiben(objects/) → kurze Transaktion files+file_objects schreiben
             → sync_feed schreiben(change_seq++) → SSE pushen → X-File-Id zurückgeben, fertig
```

### 7.2 Verarbeitungskette einer Anfrage

```
Nginx ─→ Middleware-Kette(Log/Recovery/Ratelimit/Auth) ─→ Routing(ServeMux) ─→ Handler ─→ Geschäfts-Service ─→ repo(DBA)
```

## 8. Überblick der Schnittstellen-Eingänge

| Eingang | Beschreibung |
|---|---|
| `REST /api/v1/*` | Authentifizierung, Benutzer, Abteilung, Bereich, Datei, Verzeichnis, Upload, Download, Freigabe, Change-Ereignisse |
| `Download & Range` | Streaming-Download, byte-genaues Range / Conditional Request (If-Match usw.) |
| `TUS /uploads/*` | Chunk-Checkpoint-Upload, Ticket-Wiederverwendung, Staging-Recycling, Platten-Wasserstand (>90% → 507) |
| `WebDAV /dav/*` | Standard WebDAV + PUT läuft über Finalisierung (≤100MB) |
| `SSE /sync/events` | Echtzeit-Change-Push |
| `Cursor /changes` | Inkrementelles Cursor-Pull |
| `GET /metrics` | Prometheus-Überwachungsmetriken (Standard nur Loopback/vertrauenswürdiger Proxy) |
| `GET /healthz` | Health-Check |
| `/admin/*` | Eingebettetes Verwaltungs-Backend (Go embed) |

## 9. Architektur-Rotlinien (Auszug)

- Kernel-Pakete dürfen nicht von der HTTP/API-Schicht abhängen (R-01-Familie);
- Globale Sperrreihenfolge fest: spaces-Zeile → files-Zeile → file_objects (advisory + Zeilensperre, Hash aufsteigend);
- OAuth / Schlüssel laufen ausschließlich über Umgebungsvariablen, nicht im Repository;
- Patrouille (patrol) ist **nur lesend**: sie repariert niemals automatisch, um zu verhindern, dass eine Fehlentscheidung zu Datenkorruption eskaliert;
- Quota-Abgleich, Objekt-Patrouille und Change-Stream-Bereinigung sind allesamt kontrollierte Hintergrund-Tasks, ohne unbegrenzte Ausweitung.

## 10. Technologie-Stack

| Schicht | Auswahl |
|---|---|
| Sprache | Go 1.26 |
| HTTP | Standardbibliothek `net/http` + `ServeMux` Middleware-Kette |
| DB | pgx v5 + sqlc + goose Migration (nur PostgreSQL) |
| Auth | golang-jwt v5 (HS256) |
| Speicher | Lokaler Platten-Objektspeicher (content-adressiert, `storage`-Paket mehrfach erweiterbar) |
| Konfiguration | yaml.v3 + starke Validierung der Umgebungsvariablen |
| Bereitstellung | systemd + Nginx + Prometheus (Single-Binary-Viererset, keine Container) |

---

*Bereitstellung/Betrieb siehe `DOC/05-INSTALL.md` / `DOC/06-OPS.md` / `DOC/07-BACKUP.md`; Schnittstellendetails siehe `DOC/04-API_GUIDE.md`.*
