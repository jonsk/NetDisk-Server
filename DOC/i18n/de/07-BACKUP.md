# Server-com Backup und Wiederherstellung

> Kernziel in einem Satz: **jederzeit „Datenbank + Objekte" auf denselben Zeitpunkt wiederherstellen können**.
> Begleitende Skripte liegen unter `deploy/backup/`, automatisch durch systemd-Timer gesteuert.

---

## 1. Backup-Ziel und -Strategie

Das Netzdisk hat zwei Arten von Daten, die **zusammen auf denselben Zeitpunkt wiederherstellbar sein müssen**:

1. **Metadaten** (Benutzer/Bereiche/Verzeichniseinträge/Quoten… in PostgreSQL);
2. **Objektdateien** (der eigentliche Inhalt, nach Inhalts-Hash lokal auf Platte gespeichert).

Nur eines davon zu sichern und nicht das andere führt zu einem verfälschten Zustand „Datei existiert, aber die DB zeigt auf ein anderes Objekt" oder „Datei komplett 0 Byte".

| Daten | Verfahren | Frequenz | Aufbewahrung |
|---|---|---|---|
| PostgreSQL | `pg_dump -Fc` (tägliches Full-Backup der ganzen DB) + **WAL-Archivierung** (ermöglicht Point-in-Time-Recovery) | ganze DB täglich 02:30; WAL in Echtzeit | dump 14 Tage |
| Objektverzeichnis `/opt/netdisk/data` | borg (dedupliziert, inkrementell) | **alle 4 Stunden** | 7 Tage / 4 Wochen / 6 Monate |
| Redis (AOF) | zusammen mit dem Objektverzeichnis ins Backup | wie oben | wie oben |

Zwei Timer lösen automatisch aus (beide über `deploy/backup/netdisk-backup.sh` gesteuert):

```sh
netdisk-backup.sh full      # täglich: WAL-Selbsttest + Objekt-Inkrement + PG-Full + Leseverifizierung + Aufbewahrungsbereinigung
netdisk-backup.sh objects   # alle 4 Stunden: nur Objektverzeichnis-Inkrement
```

### Drei „Ausfallschutz"-Designs in der Backup-Umsetzung

1. **Sofortige Leseverifizierung nach dem Backup**: `pg_restore --list <dump>` lässt sich nicht öffnen = Fehlschlag – ein Backup, das nur geschrieben aber nicht verifiziert wird, ist nur eine Datei, „von der Sie glauben, dass sie existiert".
2. **WAL-Archivierung prüft „neue Fehler" statt „ob null"**: `failed_count` ist ein kumulierter Wert, der nie zurückgesetzt wird.
   „Ob null" zu prüfen würde einen historischen einmaligen Fehler zu „Backup ab sofort immer fehlgeschlagen" machen. Nun wird eine Baseline erfasst und nur über das **Inkrement** alarmiert.
3. **Atomares Schreiben der Status-JSON** (Temp-Datei + `mv`): verhindert, dass Prometheus eine halb geschriebene Datei erwischt.
   „Backup-Status unbekannt" und „Backup fehlgeschlagen" sind zwei völlig verschiedene Vorfälle.

---

## 2. Kann das Backup wirklich wiederhergestellt werden? —— Wiederherstellungsübung

> Backup ≠ wiederherstellbar. Die echte Verifizierung ist die **Wiederherstellungsübung**: vierteljährlich, `deploy/backup/netdisk-restore-drill.sh`.

Die Übung macht vier Dinge, **jeder einzelne Fehlschlag zählt als Übungsfehlschlag**:

1. Das neueste Backup in eine **eigenständige temporäre DB** wiederherstellen (`netdisk_drill_<Datum>`, **niemals die Produktions-DB anfassen**);
2. **Zeilenzahl-Vergleich der Schlüsseltabellen** Produktion vs. Wiederherstellungs-DB (users/spaces/files/file_objects/space_members/sync_feed/audit_logs);
3. **Objektverzeichnis-Replay**: aus dem Backup in ein temporäres Verzeichnis wiederherstellen, für jedes überlebende Objekt den Inhalts-Hash (sha256) einzeln vergleichen;
4. Übungszeitpunkt schreiben; bei **> 90 Tagen ohne Übung** Alarm.

```sh
sudo /opt/netdisk/bin/netdisk-restore-drill.sh     # protokolliert in /var/backups/netdisk/logs/drill-<Datum>.log
```

> Es muss mindestens ein überlebendes Objekt übrig bleiben, sonst kann die Übung nur „Archiv lässt sich entpacken", aber nicht „Inhalt lässt sich replayen" verifizieren
> (eine wiederhergestellte 0-Byte-Datei „existiert" trotzdem). Man kann eines per WebDAV-Direktupload erzeugen:
> `curl -u admin:<Passwort> -T <lokale Datei> http://127.0.0.1:8080/webdav/<space_id>/<Dateiname>`
> (Pfad **muss die space_id enthalten**).

---

## 3. Point-in-Time-Recovery (PITR): auf einen Zeitpunkt wiederherstellen

> Voraussetzung: `archive_mode=on` + `archive_command` funktioniert normal (bei der Installation konfiguriert) und **ein physisches Basis-Backup** als Startpunkt vorhanden.

```sh
# ① Ein Basis-Backup holen (physisches Backup, erst mit WAL „zurück in der Zeit" möglich)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_basebackup -D /var/backups/netdisk/base -Ft -z -X fetch"

# ② In ein „eigenständiges Instanz-Verzeichnis" wiederherstellen (Produktionsdaten nicht überschreiben)
install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/17/restore
tar -xzf /var/backups/netdisk/base/base.tar.gz -C /var/lib/postgresql/17/restore

# ③ Wiederherstellungsziel schreiben (wiederherstellen auf den Zeitpunkt 2026-09-12 19:53)
cat >> /var/lib/postgresql/17/restore/postgresql.auto.conf <<'EOF'
restore_command = 'cp /var/lib/postgresql/wal_archive/%f %p'
recovery_target_time = '2026-09-12 19:53:00+08'
recovery_target_action = 'promote'
EOF
touch /var/lib/postgresql/17/restore/recovery.signal
chown -R postgres:postgres /var/lib/postgresql/17/restore

# ④ Wiederherstellungsinstanz auf einem anderen Port starten (existiert parallel zur Produktion, rührt Produktion nicht an)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_ctl -D /var/lib/postgresql/17/restore \
  -o '-p 5433' -l /tmp/pitr.log start"
su - postgres -c "psql -p 5433 -Atc 'SELECT count(*) FROM files' netdisk"
```

Das **PITR-Übungs-Skript** (`netdisk-pitr-drill.sh`) automatisiert diesen Ablauf und macht wirklich „eine Zeitreise",
das Kriterium ist nicht „DB lässt sich starten", sondern **Daten nach dem Zielzeitpunkt dürfen nicht vorhanden sein**:
`before`(T0) einfügen → physisches Backup holen → Zielzeitpunkt T aufzeichnen → `after`(T1>T) einfügen →
aus Backup auf T wiederherstellen → Behauptung: in der DB existiert `before`, `after` **existiert nicht**.

Praktische Punkte:
- Basis-Backup im **plain-Format** (`-Fp`, direkt als nutzbares Datenverzeichnis) ist bequemer als `-Ft` (komprimiertes Paket);
- Die Parameter der Wiederherstellungsinstanz wie `max_connections` **müssen ≥ denen der Haupt-DB** sein, sonst verweigert PG die Wiederherstellung direkt;
- Die Übung muss auf einem **Replica** stattfinden: nach promote wurde jenes Verzeichnis beschrieben und ist nicht mehr „das Backup jenes Moments",
  das physische Basis-Backup ist ein Artefakt, die Übung darf nur dessen Kopie verändern.

---

## 4. Objektdatei-Replay (einzeln durchführen)

```sh
export BORG_REPO=/var/backups/netdisk/borg BORG_PASSPHRASE=$(cat /etc/netdisk/borg.passphrase)
borg list --last 3 "$BORG_REPO"                  # letzte paar ansehen
cd /tmp/restore && borg extract "$BORG_REPO::full-2026-09-12T19:53:38"
# Wiederhergestellt etwa als /tmp/restore/opt/netdisk/data/objects/xx/yy/<sha256>
```

> **Objekt und DB müssen vom selben Zeitpunkt stammen**: nur Objekte replayen ohne DB = „Datei existiert, aber DB zeigt auf anderes Objekt";
> nur DB replayen ohne Objekte = „Datei komplett 0 Byte". Das Übungs-Skript bindet beide zusammen, genau um dies zu verhindern.

---

## 5. Redis AOF

In Redis liegen Login-Status und Cursor (**kein Cache**, die „drei harten Anforderungen" für zwingend nötige Persistenz siehe 05-INSTALL.md §5). Der Kernpunkt dieses Abschnitts ist, auch das AOF mit zu sichern:

```sh
redis-cli -a <Passwort> --no-auth-warning BGREWRITEAOF      # manuell einmal AOF-Rewrite auslösen
ls -l /var/lib/redis/appendonlydir/                     # AOF und Manifest landen hier
```

Sicherungsart: zusammen mit dem Objektverzeichnis in borg, oder täglich nach `BGREWRITEAOF` separat per `cp`. **AOF verloren → alle Benutzer neu anmelden** (Daten gehen nicht verloren).

---

## 6. Zugehörige Alarme

Mit Backup/Wiederherstellung stark verwandte Alarme (Regeln siehe `deploy/prometheus/netdisk-alerts.yml`, Metrik-Kriterien im Detail siehe 06-OPS.md §4):

- `NetdiskBackupStale` / `NetdiskObjectsBackupStale`: Full-Backup/Objekt-Backup überschritt die erwartete Frequenz ohne Erfolg;
- `NetdiskWalArchiveFailing`: WAL-Archivierung dauerhaft fehlgeschlagen, `pg_wal` läuft voll;
- `NetdiskRestoreDrillOverdue`: seit letzter Übung > 90 Tage, es ist Zeit für eine Wiederherstellungsübung.

---

## 7. Derzeit noch nicht erledigt (ehrlich vermerkt)

- **Wöchentliches physisches Basis-Backup** (der „Startpunkt" für PITR). Das aktuelle `netdisk-backup.sh` macht nur `pg_dump` (logisches Backup),
  erzeugt kein physisches Basis-Backup – daher ist der Bereich, in dem man „zurück in der Zeit" kann, auf das bei der manuellen Übung hinterlassene Basis-Backup beschränkt.
  Empfehlung: vor dem Go-Live einen **wöchentlichen `pg_basebackup`-Timer** hinzufügen (4 Kopien aufbewahren).
- **Geografisch getrennte Replica**. Derzeit liegen alle Backup-Replicas auf derselben Platte, **Plattencrash = Daten und Backup gemeinsam weg**,
  das ist derzeit der größte Single-Point-of-Failure. Empfehlung: borg-Remote-Repository oder Objektspeicher-Replica hinzufügen, um Backups aus dem lokalen Rechner herauszusynchronisieren.
