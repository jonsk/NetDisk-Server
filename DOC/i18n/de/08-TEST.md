# Server-com Test-Dokumentation

> Ziel: Wissen, **welche Tests es gibt**, **wie man sie ausführt**, **wie das aktuelle tatsächliche Ergebnis lautet**, **was manuell bleiben muss**.
> Verwandt: `DOC/02-ARCHITECTURE.md`, `DOC/05-INSTALL.md`, `DOC/06-OPS.md`, `DOC/07-BACKUP.md`

---

## 1. Überblick über die Test-Schichten

| Schicht | Was sie tut | Wer führt aus |
|---|---|---|
| Kompilieren / Statische Prüfung | Stellt sicher, dass es kompiliert und keine verdächtigen Konstrukte gibt | Entwicklung / CI, automatisch |
| Unit- + Integrationstests | Geschäftsregeln funktions-/schnittstellenweise verifizieren | Entwicklung / CI, automatisch |
| Architektur-Disziplin-Gate | Stellt Schichtung, Rotlinien, Dokument-Konsistenz sicher | CI, automatisch |
| Coverage-Gate | Stellt sicher, dass Schlüssel-Pakete nicht ungetestet bleiben | CI, automatisch |
| End-to-End-Sonde | Echte HTTP-Vollständigkeits-Rauchtest | Nach Bereitstellung / vor Release, echte Maschine nötig |
| Performance-Baseline | Verifiziert, dass Liste / Sync / Concurrency-Durchsatz Ziele erreichen | Echte Maschine, wiederholbares Skript |
| Manuelle Abnahme | UI-Klicks, ÜBERLANGE Pfade, echte Plattformen usw. | Betrieb / Abnahmepersonal |

> Im Projekt sind `cmd/testsgate`, `cmd/depsguard`, `cmd/coveragegate` eigenständige Gate-Programme.
> Lokale Befehle und CI-Befehle sind identisch (auch die CI hängt an denselben Befehlen).

---

## 2. Automatisierte Gates (ein Befehl pro Schicht)

Im Repository-Wurzelverzeichnis (`Server-com/`) ausführen:

| Schicht | Befehl | Was er abfängt |
|---|---|---|
| Kompilieren / Statisch | `go build ./... && go vet ./...` | Kompilierfehler, verdächtige Konstrukte |
| Unit + Integration | `NETDISK_TEST_DSN='...' go test ./... -count=1` | Alle Go-Fälle (inkl. echtem PG/Redis) |
| Integration nicht übersprungen | `go run ./cmd/testsgate` | „DSN fehlt, ganze Gruppe skip, aber CI komplett grün" – dieser **Scheingrün**-Fall, nennt jeden übersprungenen Fall |
| Daten-Wettlauf | `go run ./cmd/testsgate -race` | Daten-Wettlauf (benötigt gcc) |
| Architektur- und Dokument-Disziplin | `go run ./cmd/depsguard` | Kernel-Pakete hängen von HTTP-Schicht ab / Desktop-Disziplin / Rotlinien / Dokument- und Betriebs-Artefakt-Konsistenz |
| Coverage | `go run ./cmd/coveragegate -profile <cover.out> -min 55` | Coverage der Schlüssel-Pakete fällt unter Schwelle |
| End-to-End-Sonde | `go run ./cmd/probesmoke -pass <Passwort>` | Echte HTTP-Vollständigkeit (Upload/Download/Freigabe/WebDAV/Audit/Patrouille…) |

> Integrationstests benötigen die `netdisk_test`-Datenbank + Redis. Ohne echtes PG/Redis werden Integrationsfälle übersprungen;
> `testsgate` dient genau dazu, **darauf zu achten, ob Fälle heimlich übersprungen werden**, um Scheingrün in der CI zu vermeiden.

---

## 3. Unit- + Integrationstests (Status quo)

- Vollständige Fälle **1042 bestehen**, abdeckend 39 Pakete / **0 übersprungen** (durch `testsgate` namentlich gegengeprüft).
- Abdeckung nach Domäne: Auth-Token / Upload-Finalisierung / Objekt-Lock-Concurrency / Verzeichnis-Semantik / Sync-Change / Freigabe / Quota-Abgleich / Patrouille / WebDAV.

**Geschichtete Kriterien für performance-sensible Assertions** (z. B. P95 des Concurrency-Finalisierungs-Lock-Fensters):
- Hauptkriterium nutzt den **Median** (eine Regression lässt jede Lock-Haltezeit langsamer werden);
- Der Schwanz nutzt P95 < 500ms (saugt das „ganzes Paket parallel + gemeinsame Test-DB"-Maschinenflackern auf).
- Gegenbeleg-Validierung: Künstliches sleep im kritischen Lock-Abschnitt → Median wird sofort langsamer, Fall wird rot, was zeigt, dass es „kritischer Abschnitt breiter wurde" und nicht „Flackern" ist.

---

## 4. End-to-End-Sonde (probesmoke)

Ein Befehl führt gegen den **laufenden echten Dienst** einen Vollständigkeits-Rauchtest (51+ Schritte) durch: Login → Verzeichnis → Upload (TUS/WebDAV/multipart) →
Download/Range → Freigabe → Sync-Ereignis → Audit → Patrouille.

```sh
go run ./cmd/probesmoke -base http://127.0.0.1:8080 -user admin -pass '<Passwort>'
```

> Über den Reverse-Proxy wird der SSE-Header von Nginx konsumiert (`X-Accel-Buffering`), daher **sollte die Sonde direkt an die Anwendung gehen** (51/51 voll bestehen);
> über den Reverse-Proxy sind es 50/51, und dieser Unterschied ist erklärt (kein Fehler).

---

## 5. Performance-Baseline (gemessen am 2026-09-12)

Skript: `deploy/verify/06-perf-baseline.sh` (selbstständig, wiederholbar, Zahlen kommen als tatsächliche Ausgabe des Skripts). Maschine: Debian 13,
6 vCPU / ~2GB RAM / sequenziell Schreiben 2723.6 MB/s (lokale Baseline).

| Abnahmepunkt | Schwelle | Gemessen | Ergebnis |
|---|---|---|---|
| 100.000-Zeilen-Verzeichnisliste | P95 ≤ 500ms | **P95 107.2 ms** (Standard-Seite limit=200) | Bestanden (Puffer ca. 4,7×) |
| Remote-Change-Erkennung (`/changes` sichtbar) | P95 ≤ 3s | **P95 78.1 ms** (multipart-Schreiben) | Bestanden (Puffer ca. 37×) |
| Remote-Change-Erkennung (SSE-Frame angekommen) | P95 ≤ 3s | **P95 80.5 ms** | Bestanden |
| Concurrency-Upload 8×8MiB | — | **9.66 MB/s**, erfolgreich 8/8, fehlgeschlagen 0 | Bestanden |
| Concurrency-Upload 16×8MiB | — | **10.57 MB/s**, erfolgreich 16/16, fehlgeschlagen 0 | Bestanden (12× durch Ratelimit dann Retry) |
| Concurrency-Download 8/16×8MiB | — | **1944 / 2580 MB/s** | Bestanden (Page-Cache-Treffer, nicht als Plattendurchsatz werten) |

> **Ehrlich gesagt**: Bei 16 Concurrent-Uploads erhielt `POST /tus` 12-mal einen serverseitigen **429 Ratelimit** (`rate_limits.upload=10/s`),
> das Skript hat nach Backoff erneut versucht und alle erfolgreich abgeschlossen – das ist **Ratelimit wie vorgesehen wirksam**, kein Upload-Fehler. Wenn die Abnahme „16 Concurrent auf einmal voll bestehen" fordert,
> muss `rate_limits.upload` erhöht oder die Concurrency gesenkt werden. Der Download im 2GB/s-Bereich ist „Speicher + virtuelles Laufwerk"-Geschwindigkeit und zeigt nur, dass der Download-Pfad unter Concurrency keine Queue und keine Fehler hat.

### Reproduktionsweise

```sh
BASE=http://127.0.0.1:8080 USER=admin PASS='<Passwort>' \
ROWS=100000 NREP=60 NFEED=30 SAR="8 16" UPFILE=8388608 \
sudo sh deploy/verify/06-perf-baseline.sh | tail -n +1
```

Das Skript gibt am Ende `===== TS-08 maschinenlesbare Zusammenfassung =====` aus (einzelne Stichproben / Histogramm / Quantile / verdict-Zeile). Standardmäßig werden die Sondierungsdaten automatisch bereinigt; `KEEP=1` bewahrt sie (für Fehlersuche).

---

## 6. Abnahme-Gegenüberstellung und Wiederhol-Einstiege

| Abnahme-Domäne | Wiederhol-Einstieg |
|---|---|
| Unit-Baseline / Coverage | `go test ./... -coverprofile=cover.out` → `coveragegate` |
| Integrationstest einzeln | `go test -tags=integration ./...` |
| Performance-Baseline | `deploy/verify/06-perf-baseline.sh` (echte Maschine) |
| Bereitstellung und Upgrade | `deploy/verify/0[12567]-*.sh` (echte Maschinen-Skripte einzeln) |
| Backup / Restore / PITR | `deploy/backup/*.sh` (Übungs-Skript + Kontenbuch) |
| Überwachung und Alarmierung | `curl /metrics`, `promtool check rules` |
| Objekt-Konsistenz / Temp-Datei-Recycling | `go test ./internal/storage/ -run 'ReapTemp|MismatchedObjectKeys' -count=1 -v` |
| Daten-Wettlauf | `go run ./cmd/testsgate -race` |

---

## 7. Weiterhin manuell auszuführende Punkte (nicht in Automatisierung als abgedeckt vortäuschen)

| Punkt | Warum Automatisierung nicht abdeckt | Erfassungsort |
|---|---|---|
| Windows-ÜBERLANGER-Pfad vor Ort | Benötigt echtes Laufwerkskennzeichen und wirksame `longPathAware`-Umgebung | Client-Abnahmeliste |
| Retry-Verhalten von Virenscanner-Dateisperre | Benötigt echten AV-Hook | Ebenda (ehrlich als „nicht verifiziert" vermerkt) |
| UI-Klicks (Login/Tray/Einstellungen/Konflikt/Remote-Browsen/Update prüfen) | Keine UI-Automatisierung | Client-Abnahmeliste |
| 24h-Client-Dauerlauf | Dauer und echte Interaktions-Session | Vor Release manuell ausführen |
| Dritte übt nach Handbuch (Backup/Restore) | Benötigt „eine andere Person", die nach Handbuch agiert | Übungs-Kontenbuch |

---

## 8. Drei Test-Disziplinen

1. **Nicht entscheidbar ≠ bestanden**: Trifft der Prüfer auf „nicht entscheidbar", muss er Fehlschlag bewerten, darf es nicht als bestanden durchgehen lassen.
2. **Gegenrichtungs-Validierung ist Teil der Assertion**: Jede kritische Assertion muss einmal „Code kaputt machen → Assertion muss rot werden" durchlaufen,
   sonst weiß man nicht, worauf man achtet (im Repo mehrfach „kaputt, trotzdem grün" vorgekommen).
3. **Beweis muss wiederholbar sein**: Beweis = Befehl + echte Ausgabe + Version zum Zeitpunkt; ein Stück Ausgabe ohne Befehlsherkunft zählt nicht als Beweis.
