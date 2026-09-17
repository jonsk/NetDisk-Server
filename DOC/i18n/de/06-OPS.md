# Server-com Täglicher Betrieb

> Dieser Text beschreibt **wie man nach dem Go-Live verwaltet**: routinemäßige Inspektion, Start/Stopp, Upgrade, Überwachung/Alarmierung, häufige Fehlerbehebung.
> Erstinstallation siehe 05-INSTALL.md, Datenschutz siehe 07-BACKUP.md.

---

## 1. Routinemäßige Inspektion

Im Folgenden eine Inspektions-Checkliste, die man direkt abarbeiten kann; empfohlen täglich/wöchentlich einmal durchgehen.

```sh
# ① Dienst und Abhängigkeiten laufen alle
systemctl is-active postgresql redis-server netdisk nginx

# ② Protokoll auf Anomalien prüfen
journalctl -u netdisk -n 50 --no-pager

# ③ Ob das Backup kürzlich erfolgreich war (wichtig! Details siehe 07-BACKUP.md)
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_backup_last_success_timestamp_seconds'

# ④ Ob die Platte genug Platz hat (über 90% werden Uploads abgelehnt)
df -h /opt/netdisk/data /var/lib/postgresql /var/backups/netdisk
```

| Prüfpunkt | Was gilt als normal | Bei Anomalie |
|---|---|---|
| Alle Dienste active | alle 4 sind active | siehe §6 Fehlerbehebung |
| PG-Daten wachsen, aber auch das WAL-Archiv-Verzeichnis wächst | Archivierung läuft normal weiter | Archivierung hängt = `pg_wal` läuft voll |
| Letztes Backup < 26h | Zeitstempel ist aktuell | sofort Backup-Protokoll prüfen |
| Objekt-Inspektion läuft | `netdisk_object_patrol_last_timestamp_seconds` < 48h her | Inspektion steht = Leck/Verlust unbemerkt |
| Platten-Wasserstand < 90% | — | ≥90% lehnt neue Uploads ab (gibt 507) |

---

## 2. Start-/Stopp-Reihenfolge

> Die Reihenfolge ist wichtig: **Datenbank zuerst hoch, Anwendung danach, Reverse-Proxy zuletzt**; beim Stopp umgekehrt.

```sh
# Start
systemctl start postgresql
systemctl start redis-server
systemctl start netdisk      # führt beim Start automatisch die DB-Migration nach
systemctl start nginx
systemctl is-active postgresql redis-server netdisk nginx

# Stopp (zuerst Nginx, dann die Anwendung, damit laufende Uploads/Downloads sauber auslaufen)
systemctl stop nginx
systemctl stop netdisk
systemctl stop redis-server
systemctl stop postgresql
```

---

## 3. Upgrade und Rollback

### 3.1 netdisk-Binary upgraden (regulär)

```sh
deploy/build-release.sh <neue Version>
scp deploy/dist/netdisk-<neue Version>-linux-amd64 root@<host>:/tmp/
# Empfohlen über install.sh (idempotent, stoppt automatisch alten Prozess → Selbsttest → starten → Versions-Selbstbeweis)
sudo bash deploy/install.sh /tmp/netdisk-<neue Version>-linux-amd64 http://<externe URL>
```

> Rollback = das **vorherige** Binary `install` zurück und neu starten (Migrationen sind vorwärtskompatibel und anhängend, meist kein Zurückrollen der DB nötig).
> Wurde der Dienst durch wiederholte Abstürze ratelimit-abgewürgt (`systemctl start` meldet "Start request repeated too quickly"),
> nach Behebung der Ursache zuerst `systemctl reset-failed netdisk` und dann starten.

### 3.2 PostgreSQL-Großversion upgraden (z. B. 17→18)

> Die lokale Baseline ist 17; will man vor Ort auf eine höhere Hauptversion upgraden, verwendet man `pg_upgrade`. **Vor dem Upgrade muss ein Backup vorhanden sein, dessen Wiederherstellung verifiziert ist**,
> denn wenn `pg_upgrade` fehlschlägt, können sowohl die alte als auch die neue Instanz nicht hochkommen.

```sh
systemctl stop netdisk                       # zuerst die Anwendung stoppen, um Schreiben während des Upgrades zu vermeiden
su - postgres -c "/usr/lib/postgresql/17/bin/pg_dumpall > /var/backups/netdisk/pre-upgrade.sql"
apt-get install -y postgresql-18             # neue Version installieren (PGDG-Quelle)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_upgrade \
  --old-datadir=/var/lib/postgresql/17/main --new-datadir=/var/lib/postgresql/18/main \
  --old-bindir=/usr/lib/postgresql/17/bin --new-bindir=/usr/lib/postgresql/18/bin --check"
# Nach bestandenem --check das --check entfernen und erneut ausführen, dann Statistiken neu sammeln
su - postgres -c "/usr/lib/postgresql/18/bin/vacuumdb --all --analyze-in-stages"
```

---

## 4. Überwachung und Alarmierung

- **Expose-Punkt**: `GET /metrics` (Prometheus-Textformat, **selbst implementierter** Collector).
  Standardmäßig nur **Loopback / vertrauenswürdiger Proxy** dürfen abgreifen; für Cross-Machine-Scrape `NETDISK_METRICS_TOKEN` konfigurieren und `Authorization: Bearer <token>` mitgeben.
- **Scrape**: `deploy/prometheus/prometheus.yml` (Standardintervall 30s).
- **Alarmregeln**: `deploy/prometheus/netdisk-alerts.yml` (ein Dutzend), Syntax mit `promtool check rules` prüfen.

**Die wichtigsten Metriken im Auge behalten** (Bedeutung + Kriterium):

| Metrik | Bedeutung | Wann handeln |
|---|---|---|
| `netdisk_process_resident_memory_bytes` | residenter Speicher des Prozesses | nähert sich dem Limit (320M) → wird von systemd getötet und neu gestartet |
| `netdisk_disk_used_percent` | Platten-Wasserstand des Objektbereichs | ≥90% lehnt Uploads ab; `>85` zuerst vorwarnen |
| `netdisk_backup_last_success_timestamp_seconds` | Zeitpunkt des letzten erfolgreichen Backups | `now()-es > 26h` → Alarm (critical) |
| `netdisk_object_missing_total` | Anzahl Objekte, die in der DB sind, aber auf Platte fehlen | **≥1 bedeutet Daten nicht lesbar** (critical) |
| `netdisk_object_leak_bytes` | auf Platte vorhanden, aber in der DB nicht (belegt Platz) | >64MiB = Platte wächst nur, schrumpft nie |
| `netdisk_quota_drift_bytes` | Differenz zwischen Quota-Buchhaltung und echtem Verbrauch | >1MiB = Quota-Berechnung vermutlich fehlerhaft |
| `netdisk_restore_drill_last_timestamp_seconds` | Zeitpunkt der letzten Wiederherstellungsübung | >90 Tage ohne Übung → Alarm |

> **Wichtiges Prinzip**: Eine Metrik, die nicht gelesen werden kann, **liefert keinen Sample**, statt 0 zu melden (0 würde den Alarm lügen lassen).
> Beispielsweise verschwinden bei fehlgeschlagener Plattenkapazitäts-Erkennung die drei Kapazitätskurven komplett – das ist ein normales „nicht lesbar", kein voller Plattenspeicher.

---

## 5. Netzwerksicherheit und Berechtigungspunkte

- `/metrics` gibt **401** zurück als **beabsichtigtetes Verhalten**: es darf nur Loopback/vertrauenswürdiger Proxy abgreifen. Cross-Machine-Scrape muss Bearer-Token mitführen.
- Schlüssel kommen niemals in yaml / das Repository, existieren nur in `/etc/netdisk/secrets.env` (Rotation = diese Datei ändern + Dienst neu starten).
- PostgreSQL lauscht nur Loopback, Redis lauscht nur Loopback: die Ports dieser beiden Dienste **dürfen nicht auf dem Netzwerkadapter erscheinen**.

---

## 6. Häufige Fehlerbehebung (runbook)

Jeder Punkt ist nach **Symptom → zuerst prüfen → häufige Ursache und Maßnahme** geschrieben, Befehle direkt abklatschbar.

### 6.1 Dienst startet nicht

```sh
systemctl status netdisk -l
journalctl -u netdisk -n 60 --no-pager
```

| Symptom | Bedeutung | Maßnahme |
|---|---|---|
| `ExecStartPre ... status=1/FAILURE` | **Start-Selbsttest hat abgefangen** (Konfig/Verzeichnis/Port/Abhängigkeit) | die einmal vollständig aufgelistete Problemliste ansehen und Punkt für Punkt beheben |
| `active (running)` aber Anfrage 502 | Dienst läuft, aber Lausch-Adresse und Nginx-Ziel stimmen nicht überein | `http_addr` in der Konfiguration mit dem Nginx-upstream abgleichen |
| `Start request repeated too quickly` | Absturzschleife hat Ratelimit ausgelöst | nach Behebung der Ursache `systemctl reset-failed netdisk` |

Zwei häufige Selbsttest-Fehler:
- **Verzeichnis nicht schreibbar**: `chown -R netdisk:netdisk /opt/netdisk/data` (wurden `objects/`/`tus-tmp/` jemals per Hand als root angelegt, gehören sie root).
- **Read-only-Dateisystem**: der Dienst ist durch `ProtectSystem=strict` strikt eingeschränkt – wurde ein Pfad in den secrets geändert, muss **synchron die `ReadWritePaths` der systemd-Unit** geändert werden, damit der neue Pfad hinzukommt.

### 6.2 Upload/Download steckt bei 0%

```sh
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_(http|tus|sse)'
tail -f /var/log/nginx/netdisk.access.log | grep -E 'rt=|urt='
```

- `urt=` (Upstream-Zeit) viel kleiner als `rt=` (Gesamtzeit) → die Zeit hängt in Nginx → prüfen, ob TUS/WebDAV `proxy_request_buffering` ausgeschaltet hat (nicht aus = Request-Body landet zuerst auf Nginxs Temp-Platte, Fortschritt und Fortsetzung sind nicht echt).
- Download langsam → `proxy_buffering off` der Download-Schnittstelle prüfen.
- SSE-Ereignis-Verzögerung von Dutzenden Sekunden → Nginx puffert den Event-Stream → bestätigen, dass SSE-Schnittstelle `proxy_buffering off` + `proxy_read_timeout 86400s` hat.
  (Diese drei Punkte werden alle in `deploy/nginx/verify.sh` behauptet.)

### 6.3 Alle abgemeldet / sofort abgemeldet nach Login

```sh
redis-cli -a <Passwort> --no-auth-warning CONFIG GET appendonly appendfsync maxmemory-policy
```

- Ist eines nicht erfüllt, ist es abnormal – die **drei harten Anforderungen** (`requirepass` / `appendonly yes`+`everysec` / `maxmemory-policy noeviction`)
  und jeweils deren Fehl-Folge siehe 05-INSTALL.md §5, Punkt für Punkt abgleichen und nach der Reparatur Redis neu starten.
- Merken: In Redis liegt **kein Cache**, verloren gehen Login-Status und Cursor, nicht Dateidaten, und es beeinflusst auch nicht bereits auf Platte geschriebene Dateien.

### 6.4 Backup lief nicht erfolgreich

```sh
systemctl status netdisk-backup.service
journalctl -u netdisk-backup -n 40 --no-pager
cat /var/lib/netdisk/backup-status.json
su - postgres -c "psql -Atc 'SELECT archived_count, failed_count FROM pg_stat_archiver' netdisk"
ls -l /var/lib/postgresql/wal_archive | tail -3
```

- **WAL-Archivierung fehlgeschlagen** → zu 90% ist das Archiv-Verzeichnis unerreichbar: das Archiv-Verzeichnis **darf nicht** unter `/var/lib/netdisk` liegen
  (das ist `netdisk:netdisk 0750`, der postgres-Benutzer hat keine Durchlauf-Berechtigung), es sollte unter `/var/lib/postgresql/wal_archive`
  liegen (postgres' eigener Verzeichnisbaum).
- **`pg_dump` Permission denied** → das Dump-Verzeichnis muss **postgres** gehören (es läuft als postgres-Identität).

### 6.5 Objekt-Inspektions-Alarm

Die Auditierung ist **nur lesend** und repariert niemals automatisch – **vor Klärung der Ursache keine Objekte von Hand löschen, kein Aufräumskript ausführen**.
Beim `object_missing`-Alarm das Objekt-Replay gemäß 07-BACKUP.md durchführen, um die Ursache einzugrenzen.

### 6.6 Platte wird voll

```sh
du -sh /opt/netdisk/data /var/backups/netdisk /var/lib/postgresql/wal_archive /var/log
```

Drei Stellen wachsen: Objektverzeichnis, Backup, WAL-Archiv. **Nicht** das WAL-Archiv-Verzeichnis löschen, um Platz zu schaffen
(das ist die gesamte Fähigkeit zur Point-in-Time-Recovery). Reihenfolge: zuerst das Backup aus dem Rechner heraussynchronisieren, dann lokale alte dumps löschen (Skript behält 14 Tage),
dann erwägen, dass die 24h-Recycle-Fenster der Anwendung die Tombstones gelöschter Objekte aufräumen (**nicht von Hand löschen**).

### 6.7 „Hab nichts gemacht, geht trotzdem nicht" – zeitsparendste Suche

```sh
ss -tlnp | grep -E '80|8080|5432|6379'      # 1. Wer lauscht, wo wird gelauscht
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz   # 2. Reverse-Proxy umgehen, direkt
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1/admin/         # 3. Dann über den Reverse-Proxy
journalctl -u netdisk -n 20 --no-pager      # 4. Anwendungsprotokoll ansehen (wird client_ip durchgereicht?)
nginx -T | grep -A5 'location /tus'         # 5. wirksame Proxy-Konfiguration ansehen (nicht die Datei auf der Platte)
```

Der Unterschied zwischen Schritt 2 und 3 grenzt das Problem sofort auf „Anwendung" oder „Reverse-Proxy" ein;
bei Schritt 4, wenn `client_ip` `127.0.0.1` anzeigt, der tatsächliche Client aber auf einem anderen Rechner ist, bedeutet das, dass `X-Real-IP` nicht durchgereicht oder der vertrauenswürdige Proxy falsch konfiguriert ist.
