# Server-com API-Leitfaden

> Spezifikationsdatei: Der autoritative Vertrag liegt in `DOC/api/openapi.yaml` (OpenAPI v3); dieser Text ist dessen **menschenlesbare Einführung**.
> Verwandt: `DOC/02-ARCHITECTURE.md`, `DOC/05-INSTALL.md`, `DOC/06-OPS.md`, `DOC/07-BACKUP.md`

---

## 1. Authentifizierungsmodell

Der Dienst nutzt **JWT (HS256)** zur Authentifizierung; nach dem Login erhält man ein `access_token`, das fortan jede Anfrage mitführt:

```
Authorization: Bearer <access_token>
```

**Token nach Endgerät getrennt (R-14)**: Token der web-Seite (Verwaltungs-Backend) und der desktop-Seite (Desktop-Client) sind unabhängig voneinander,
um zu verhindern, dass ein Leck an einer Stelle den ganzen Körper betrifft. Token/Refresh-Token liegen in Redis, unterstützen Widerruf und „Single-Flight-Refresh".

| Schnittstelle | Beschreibung |
|---|---|
| `POST /api/v1/auth/login` | Konto-Passwort-Login, gibt `access_token` + `refresh_token` zurück |
| `POST /api/v1/auth/refresh` | Erneuert Access-Token mit Refresh-Token |
| `POST /api/v1/auth/logout` | Logout, widerruft Token |
| `GET /api/v1/me` | Aktuelle Benutzerinformationen |
| `GET /api/v1/version` | Server-Version |

> Die Community-Edition behält nur **Passwort-Login**; Enterprise-WeChat/DingTalk/Drittanbieter-IdP-Login wurden in der Community-Edition entfernt.
> Überschreitet man das Fenster, kommt **429** zurück; der Client sollte mit Backoff erneut versuchen (siehe §7).

---

## 2. Allgemeine Konventionen

- **Base URL**: `/api/v1`, in Produktion über Nginx einzelner Port nach außen.
- **Request/Response**: JSON (`Content-Type: application/json`).
- **Paginierung**: Listen-Schnittstellen nutzen `limit` + `after` (keyset-Cursor) zum Blättern; `limit` Obergrenze **999**
  (darüber wird auf Standard 200 zurückgefahren).
- **Download**: Streaming-Rückgabe + byte-genaues `Range` (200/206/416) + Conditional-Request-Header (`If-Match`/`If-None-Match`/`If-Range`).
- **Ratelimit**: Nach Schnittstellen-Familie (login/upload/file_list/file_read/file_write/webdav), zwei Fenster (Sekunde/Minute) wirken gleichzeitig, der strengere gewinnt.

---

## 3. REST-Schnittstellengruppen

### 3.1 Dateien und Verzeichnisse

| Methode und Pfad | Beschreibung |
|---|---|
| `GET /api/v1/files?space=<id>&parent_id=&limit=&after=` | Verzeichnis auflisten (Standard 200 Einträge/Seite; keyset-Blättern unterstützt) |
| `POST /api/v1/files` | Datei-/Verzeichniseintrag anlegen |
| `GET /api/v1/files/dirs` | Verzeichnis auflisten (verzeichnis-spezifisch) |
| `GET /api/v1/files/{id}` | Datei-/Verzeichnisdetail |
| `GET /api/v1/files/{id}/content` | Inhalt herunterladen (Range unterstützt) |
| `POST /api/v1/files/{id}/move` | Verschieben/Umbenennen |
| `POST /api/v1/files/{id}/copy` | Kopieren |
| `POST /api/v1/files/{id}/share-to-space` | In anderen Bereich kopieren/freigeben (läuft über die Referenz-+1-Pfad des Objekts) |
| `POST /api/v1/files/{id}/lock` | Lock setzen/aufheben (Objekt-Level-Lock) |
| `GET /api/v1/files/{id}/subtree-stats` | Unterbaum-Statistik |

> Verzeichnis-Ebene „Unterbaum verschieben/löschen" über Schwellenwert (Standard 1000 Zeilen) wird zu **asynchronem Task**:
> `POST` gibt `task_id` zurück, der Client pollt `GET /api/v1/tasks/{id}` für den Fortschritt.

### 3.2 Upload (multipart Direktupload)

| Methode und Pfad | Beschreibung |
|---|---|
| `POST /api/v1/upload/create` | Upload anlegen (reserviert Kontingent, gibt Upload-Token zurück) |
| `POST /api/v1/upload/simple` | multipart Direktupload (kleine Datei; einmal Schreiben = finalisiert) |
| `POST /api/v1/upload/{id}/finish` | Abschließen/Finalisieren |
| `GET/PATCH /api/v1/upload/{id}` | Abfragen/Fortsetzen |

> Große Dateien laufen über das **TUS**-Protokoll (siehe §4).

### 3.3 Änderungen und Synchronisation

| Methode und Pfad | Beschreibung |
|---|---|
| `GET /api/v1/changes?since=<cursor>` | Inkrementelles Cursor-Pull (der „zuverlässige" Kanal des Dual-Zustands) |
| `GET /api/v1/changes/head` | Aktuellen neuesten Cursor holen |
| `GET /api/v1/sync/cursors` | Sync-Cursor verwalten |
| `GET /sync/events` | SSE-Echtzeit-Push (der „Echtzeit"-Kanal des Dual-Zustands) |

> Beide nutzen denselben globalen `change_seq`: SSE kann Frames verlieren, aber man kann mit `/changes` nach Cursor nachziehen, um Vollständigkeit zu garantieren.

### 3.4 Freigaben

| Methode und Pfad | Beschreibung |
|---|---|
| `POST /api/v1/shares` | Freigabe-Link erstellen |
| `GET /api/v1/shares` | Liste meiner erstellten Freigaben |
| `DELETE /api/v1/shares/{id}` | Freigabe widerrufen |
| `GET /api/v1/shares/{token}/meta` | Freigabe-Metainfo (für Landing-Page) |
| `GET /api/v1/shares/{token}/download` | Per Freigabe-Token herunterladen |

### 3.5 Bereiche (Spaces)

| Methode und Pfad | Beschreibung |
|---|---|
| `POST /api/v1/spaces` | Bereich anlegen (persönlicher Bereich / Team-Bereich) |
| `GET /api/v1/spaces/{id}` | Bereichsdetail (inkl. Quota) |
| `POST /api/v1/spaces/{id}/transfer` | Bereich übertragen |
| `GET /api/v1/spaces/{id}/members` | Mitgliederliste |
| `PUT/DELETE /api/v1/spaces/{id}/members/{userId}` | Mitglied hinzufügen / entfernen |
| `POST /api/v1/spaces/{id}/leave` | Bereich verlassen |

### 3.6 Verwaltungs-Backend (nur super_admin)

| Methode und Pfad | Beschreibung |
|---|---|
| `POST/GET /api/v1/admin/departments` | Abteilungsverwaltung |
| `PUT/DELETE /api/v1/admin/departments/{id}` | Abteilung ändern / löschen |
| `POST /api/v1/admin/spaces/{id}/freeze` | Bereich einfrieren (Sync stoppen) |
| `GET /api/v1/audit/logs` | Audit-Log (in Community-Edition Platzhalter) |

> Die Verwaltungs-Backend-Oberfläche für Benutzer/Organisation/Bereich/Quota wird von Go `embed` ausgeliefert (`/admin/*`).
> Ferner: Benutzerverwaltungs-Schnittstellen (Benutzer anlegen, Rolle ändern, Quota konfigurieren) laufen über `/api/v1/admin/*` (siehe vollständiges openapi).

---

## 4. TUS-Chunk-Upload (große Dateien)

Checkpoint-fähiges Fortsetzungsprotokoll für Desktop-Client / große Dateien (`deploy` enthält `cmd/probesmoke` als Referenz-Implementierung):

| Schritt | Methode | Schlüssel-Header |
|---|---|---|
| Task anlegen | `POST /tus` | `Tus-Resumable: 1.0.0`, `Upload-Length`, `Upload-Metadata: filename <base64>`, `Authorization: Bearer` |
| Chunk übertragen | `PATCH /tus/{id}` | `Tus-Resumable`, `Upload-Offset`, `X-Upload-Token`, `Content-Type: application/offset+octet-stream` |
| Abschließen | Letzter Chunk | Antwort `200` + `Upload-Complete: true` + `X-File-Id` bedeutet finalisiert |

- **Checkpoint-Fortsetzung**: Client meldet `Upload-Offset`, Server setzt von diesem Offset fort;
- **Ticket-Wiederverwendung**: Upload-Ticket ist vor der Finalisierung wiederverwendbar, idempotent;
- **Platten-Wasserstand**: Staging-Bereichsauslastung ≥ Schwelle (Standard 90%) gibt **507 Insufficient Storage** zurück;
- **Staging-Recycling**: Nicht rechtzeitig abgeschlossene Staging-Dateien werden von einem Hintergrund-Task (ca. alle 10 Minuten) eingesammelt.

---

## 5. WebDAV

Pfad-Präfix `/dav/*`, basierend auf dem Standard `x/net/webdav`, am häufigsten genutzt für „Netzlaufwerk zuordnen / Explorer":

- **PUT läuft über Finalisierungspfad**: ≤100MB direkt finalisiert (darüber Hinweis auf TUS);
- **LOCK-Semantik selbst nachgerüstet** (Standardbibliothek nur In-Memory-Implementierung);
- Range / Conditional-Request wie bei REST gleicher Herkunft;
- Verzeichnis-Browsing schickt auf einmal mehrere Requests → dafür ist `rate_limits.webdav` großzügig konfiguriert (60/s).

---

## 6. Health / Überwachung

| Pfad | Beschreibung |
|---|---|
| `GET /healthz` | Liveness-Check (200) |
| `GET /metrics` | Prometheus-Metriken; Standard nur Loopback / vertrauenswürdiger Proxy, cross-machine benötigt `NETDISK_METRICS_TOKEN` Bearer |
| `GET /api/v1/version` | Version |

---

## 7. Statuscodes und Semantik

| Statuscode | Semantik | Beschreibung |
|---|---|---|
| 200 / 201 | Erfolg | Finalisierungs-Erfolg trägt `X-File-Id` |
| 206 / 416 | Teilinhalt / Bereichsverletzung | Download-Range |
| 400 / 422 | Parameterfehler | Validierung fehlgeschlagen |
| 401 | Nicht authentifiziert | Token fehlt/abgelaufen |
| 403 / 401 | Berechtigung eingeschränkt | **Sync dieses Bereichs pausieren, lokal nie löschen** (Client-Verhalten) |
| 404 / 410 | nicht vorhanden / entfernt | 410 = Objekt/Bereich migriert oder gelöscht |
| 409 | Konflikt | Versionskonflikt, doppelter Name (case-insensitive-Dublettenprüfung) |
| 429 | Ratelimit | Backoff-Retry nötig |
| 500 | Serverfehler | siehe Störungsbehebung in 06-OPS.md |
| 507 | Speicherplatz unzureichend | Platten-Wasserstand erreicht Schwelle |

**Client-Umgang mit 429**: Die Projekterfahrung lautet „429-Backoff-Retry ist zwingend" – beim tatsächlichen Aufräumen der Sonder-Dateien traf `DELETE` auf das `file_write`-Ratelimit,
und ohne Backoff würde **still zu wenig gelöscht**; trifft der Upload-Task-Anlage das Ratelimit ohne Backoff, äußert sich das als „Datei lädt nie hoch".

---

## 8. Verhältnis zur Spezifikationsdatei

- Dieser Text ist die menschenlesbare Einführung; **autoritativer Vertrag** ist die maschinenlesbare `DOC/api/openapi.yaml`.
- Das web-Repository führt darauf basierend die Frontend-Vertragsprüfung durch (`pnpm check:api`); der Desktop nutzt gen-csharp zur Client-Code-Generierung.
- Bei Vertragsänderung gibt es je eine vendored Kopie in `web` und `desktop`, die an **drei Stellen synchron** gehalten werden müssen.
