# Server-com (Open-Source-Edition) Funktionsliste

> Version: 2026-09-15 zusammengestellt
> Dieses Repository bietet keine Unternehmens-Service-Zusicherung, wird gemäß der Open-Source-Lizenz verwendet/modifiziert/redisitribuiert, Bereitstellung und Betrieb selbst getragen.
> Design-Autorität: `DOC/02-ARCHITECTURE.md`.

---

## 1. Repository-Positionierung

| Eintrag | Inhalt |
|---|---|
| Name | **Server-com** (Open-Source-Version / Community) |
| Charakter | Open-Source-Veröffentlichung des **Backend-Dienstes (Go)** des Cloud-Speicher-Systems; Inhalt = Server-Quellcode + Deployment-Material |
| Tech-Stack | Go / PostgreSQL 17 / Redis / Nginx / lokaler Festplattenspeicher |
| Externe Fähigkeiten | REST-Schnittstelle + TUS-Resumable-Upload + WebDAV + SSE-Echtzeit-Sync |

**Verzeichnisse**: `internal/` (Kerncode), `cmd/` (Kommandozeilenprogramme), `deploy/` (Deployment-Material), `scripts/` (Hilfsskripte), `DOC/` (Dokumentation).

> Leserhinweis: Diese Liste ist nach „Was das System kann" organisiert, um zunächst einen Überblick über die Fähigkeiten zu erhalten; für die tiefere Code-Struktur und Einstiegspunkte lesen Sie bitte den „Code-Lese-Leitfaden" im selben Verzeichnis.

---

## 2. Was das System nach außen bietet

Server-com ist ein **Enterprise-Cloud-Speicher-Backend** und bietet nach außen vier Arten von Fähigkeiten, über die Clients (Admin-Backend / Desktop / Dateiprotokoll-Werkzeuge) den Cloud-Speicher bedienen:

| Fähigkeit | Beschreibung | Äußeres Erscheinungsbild |
|---|---|---|
| **REST-Schnittstelle** | Login, Datei-CRUD, Download, Freigabe, Backend-Verwaltung usw. | `/api/v1/*` |
| **TUS-Resumable-Upload** | Chunk-Upload großer Dateien, bei Netzunterbrechung fortsetzbar | `/tus/*` |
| **WebDAV** | kompatibel mit Standard-Dateiprotokoll, direkt als Netzlaufwerk einbindbar | `/webdav/*` |
| **SSE-Echtzeit-Sync** | sobald sich eine Datei ändert, Push an den Client, dann inkrementelles Abrufen | `/api/v1/events` |

---

## 3. Funktionsmodul-Liste

### 1) Konto & Authentifizierung
- **Login mit Benutzername/Passwort**: Anmeldung mit Benutzername/Passwort, unterstützt Token-Verlängerung (Refresh) und Logout.
- **Token-Mechanismus**: nach erfolgreichem Login wird ein Zugriffstoken ausgestellt; Token sind nach **Endpoint** getrennt (Backend / Desktop), die Fähigkeiten verschiedener Endpoints sind isoliert.
- **Kontoverwaltung**: das Backend kann Benutzer anlegen, Konten aktivieren/deaktivieren, Rollen anpassen; unterstützt feingranulare Steuerung von Upload-/Download-Rollen.
- **Passwortsicherheit**: strenge Passwortvalidierung und Hash-Speicherung; bei Fehlversuchen gibt es eine temporäre Sperrstrategie gegen Brute-Force.

### 2) Organisation & Bereiche
- **Organisationsstruktur**: Pflege des Abteilungsbaums (Abteilung anlegen/ändern/löschen, Unterbaum anzeigen, Closure neu aufbauen); die Abteilung ist einer der Eingänge für Berechtigungen.
- **Bereiche**: jeder Benutzer hat einen **persönlichen Bereich**; es kann ein **Team-Bereich** erstellt und Mitglieder zur Kollaboration eingeladen werden.
- **Mitgliederverwaltung**: Hinzufügen/Entfernen von Bereichsmitgliedern, Rollenanpassung, Übertragung, Auflösung; Administratoren können eine globale Governance des Bereichs vornehmen (Kontingent, Einfrieren, Zurückziehen).

### 3) Dateiverwaltung
- **Metadaten-Operationen**: Durchsuchen von Dateien/Verzeichnissen, Verzeichnis erstellen, Umbenennen, Verschieben, Löschen.
- **Download**: unterstützt Streaming-Download, HTTP-Range-Fortsetzung, bedingte Anfragen (If-Match usw., optimistische Sperre).
- **Instant-Upload**: Dateien mit identischem Inhalt können das wiederholte Hochladen überspringen und das bereits gespeicherte Objekt direkt wiederverwenden.
- **Bearbeitungssperre**: Dateien können gesperrt werden, um zu vermeiden, dass mehrere Personen gleichzeitig bearbeiten und sich gegenseitig überschreiben.
- **Namens- & Pfad-Constraints**: Dateinamen-Gültigkeit, Pfadlänge, Verzeichnistiefe usw. werden einheitlich vom Server validiert.
- **Kontingent**: Verwaltung des Bereichs-Kontingents, Sperrung bei Überschreitung; mit Kontingent-Abgleich und Drift-Warnung.

### 4) Upload & Schreiben
- **TUS-Resumable-Upload**: Chunk-Upload großer Dateien, unterstützt Fortsetzung nach Unterbrechung, parallele Chunks.
- **Einheitlicher Schreibpfad**: alle Uploads (Chunks, WebDAV-Schreiben auf Platte) fließen letztlich in denselben Finalisierungs-Eingang, um sicherzustellen, dass „eine Datei nur eine einzige Schreibquelle hat" und konkurrierendes Überschreiben von Natur aus vermieden wird.
- **Inhaltlich adressierter Speicher**: Dateien werden nach Inhalts-Hash abgelegt, identischer Inhalt wird nur einmal gespeichert (Deduplizierung).
- **Lebenszyklus**: Objekte durchlaufen von der Speicherung bis „löschbar/recycelbar" einen eindeutigen Zustandsfluss, um das versehentliche Löschen laufender Dateien zu vermeiden.

### 5) WebDAV & Freigabe
- **WebDAV**: bietet über das Standard-WebDAV-Protokoll Dateilesen/-schreiben, Verzeichnisoperationen, Kopieren/Verschieben, Sperren usw., kompatibel mit Desktop-Einbindungslaufwerken.
- **Freigabe-Link**: für Dateien/Verzeichnisse kann ein Freigabe-Link erzeugt werden (zugänglich/downloadbar ohne Login), dies ist der **einzige ausgeloggte Ausgang** des Systems; Ablauf kann gesetzt oder jederzeit widerrufen werden.

### 6) Echtzeit-Synchronisation
- **SSE-Push**: wenn sich eine Datei ändert, wird sie über eine Long-Connection in Echtzeit an Online-Clients gepusht.
- **Inkrementelles Abrufen**: Clients rufen Änderungen über einen Cursor inkrementell ab; selbst wenn ein Push verloren geht, ist die finale Konsistenz nicht beeinträchtigt (das Abrufen ist der autoritative Pfad).
- Anwendungsszenario: bidirektionale Synchronisation von Desktop-Verzeichnis und Cloud.

### 7) Admin-Backend & Sicherheit
- **Admin-Backend**: `/admin` bietet die Backend-Verwaltungsseite (Benutzer, Organisation, Bereichs-Governance).
- **Zugriffskontrolle**: das `web`-Endpoint-Token beschränkt den Backend-Zugriff; H5-/Desktop-Token dürfen nicht ins Backend.
- **Rate-Limit**: Login-, Upload-, Datei-Lese-Interface-Familien haben unabhängige Limits und bilden mit Nginx einen Zwei-Ebenen-Schutz; verhindert Brute-Force und Interface-Abruf.

### 8) Speicher & Betrieb
- **Objekt-Inspektion**: das Backend scannt regelmäßig die Festplatte und findet Objekt-Lecks (Müll ohne Referenz) sowie reverse Waisen (Referenz ohne Datei).
- **Kontingent-Abgleich**: regelmäßiger Vergleich von „logischer Nutzung vs. tatsächlicher Belegung", bei zu großer Drift Warnung und optional automatisches Zurückschreiben zur Korrektur.
- **Deployment-Form**: Einzelbinaire + systemd + Nginx; Backup/Restore-, Verifikationsskripte werden mit dem Repo geliefert.
- **Speicherform**: standardmäßig lokale Festplatte (inhaltlich adressiert); nur lokaler Speicher, keine Backends für Drittanbieter-Objektspeicher.

---

## 4. Deployment- & Betriebsform

- **Einzelbinaire**: der gesamte Dienst wird zu einer ausführbaren Datei kompiliert und von systemd verwaltet; Nginx übernimmt Reverse-Proxy und HTTPS.
- **Voraussetzungen**: PostgreSQL 17 + Redis (Auth/Rate-Limit-Abhängigkeit, bei Nichtverfügbarkeit kein Start).
- **Konfiguration**: yaml-Datei + Umgebungsvariablen; **Passwörter/Schlüssel laufen nur über Umgebungsvariablen**, niemals in yaml oder ins Versions-Repository geschrieben.
- **Datenbank-Migration**: der Dienst enthält Migrationsskripte, die beim Deployment automatisch ausgeführt werden.
- **Admin-Backend-Frontend**: bereits in das Binaire `embed`det, keine separate Frontend-Verzeichnis-Bereitstellung nötig.
