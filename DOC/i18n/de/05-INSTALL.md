# Server-com Installation und Bereitstellung

> Dieser Text beschreibt nur **den kompletten Weg von Null bis zum Go-Live**; routinemäßige Inspektion / Fehlerbehebung siehe 06-OPS.md, Datenschutz (Backup) siehe 07-BACKUP.md.
> Die zum Repository gehörenden Skripte liegen unter `deploy/`; für fast jeden Schritt gibt es fertige Skripte (idempotent, wiederholt ausführbar).
>
> **Jeder Schritt bietet zwei Vorgehensweisen, wovon eine gewählt werden kann**: **① Skript nutzen** (fertige Skripte unter `deploy/`, idempotent, empfohlen) —
> **② Handbefehle nutzen** (völlig ohne Skripte, Befehle einzeln eintippen, gut für Audit und schrittweise Verifikation).
> Beide Vorgehensweisen erzeugen dasselbe Endergebnis; wählen Sie nach Belieben eine, Sie müssen nicht beide ausführen.

---

## 1. Bereitstellungsform und Voraussetzungen

Der gesamte Netzdatenträger ist ein **einzelnes Go-Binary** (`netdisk`), extern arbeiten nur vier Komponenten mit ihm zusammen:

```
Nginx(80/443) ─Reverse-Proxy─▶ netdisk(:8080) ─▶ PostgreSQL 17 (:5432)
                                               ─▶ Redis (:6379)
```

| Komponente | Versionsanforderung | Installationsort |
|---|---|---|
| netdisk | Build-Erzeugnis des Repositories | lokaler Rechner `/opt/netdisk/` |
| PostgreSQL | **mindestens 17** (siehe unten) | lokaler Rechner |
| Redis | 7+ | lokaler Rechner |
| Nginx | empfohlen 1.26+ | lokaler Rechner (Produktion) |

> **Warum wird mindestens PostgreSQL 17 unterstützt?**
> Der Standardwert des Primärschlüssels verwendet `gen_random_uuid()` (siehe Migrationsskript `00001_init.sql`) — das ist eine **ab PG 13 eingebaute** UUID-Generierungsfunktion, daher liegt die Untergrenze nicht an der UUID. Dieses Projekt legt die Mindestunterstützung auf **PG 17** fest (passend zur von gängigen Distributionen mitgelieferten Version), und das Architektur-Dokument ADR-1 stützt sich darauf. Vor Ort laufen auch 15/16 (gen_random_uuid ist kompatibel), aber Archivierungs- und Langzeit-Support-Strategie wird nach 17 gepflegt. Das System unterstützt derzeit nur diese eine Datenbank (keine Anbindung an MySQL usw.).

---

## 2. Installationsabhängigkeiten installieren

> In diesem Schritt werden installiert: Backup-Werkzeug (borg/rsync), PostgreSQL 17, Redis, Nginx.
> Wichtigster Punkt: **Das System-Repository von Debian 13 enthält von sich aus PostgreSQL 17**, was genau der Mindestunterstützungsversion dieses Projekts entspricht — einfach direkt installieren, **keine zusätzliche PGDG-Quelle nötig** (falls das lokale System-Repository eine niedrigere Version hat, z. B. Debian 12 liefert 15 mit, dann wie unten in den Handbefehlen PGDG für 17 hinzufügen).

```sh
# Dem Skript übergeben: installiert postgresql-17/redis/nginx/borg → stoppt Apache, gibt 80/443 frei
sudo bash deploy/provision/01-install-packages.sh
```

Das Skript gibt die Versionsnummer jeder Komponente zur Bestätigung aus und druckt am Ende `INSTALL_DONE`.

> **Ohne Skript, Schritt für Schritt per Handbefehl** (entspricht `01-install-packages.sh`):

```sh
# ① PostgreSQL 17 / Redis / Nginx / Backup-Werkzeug borg installieren
#    (Debian 13 bringt PG17 mit, direkt installieren; PGDG-Quelle nur bei zu
#     niedriger Distro-Version nötig, siehe Hinweis unten)
sudo apt-get update
sudo apt-get install -y postgresql-17 redis-server nginx borgbackup

# ② Apache stoppen, um 80/443 freizugeben (nur falls Apache installiert ist)
sudo systemctl disable --now apache2 2>/dev/null || echo 'Kein Apache, übersprungen'
```

> Die Skript-Variante packt diese 3 Schritte in einen einzigen `sudo bash deploy/provision/01-install-packages.sh` und macht zusätzlich „Versionsnummern der Komponenten ausgeben zur Bestätigung + `INSTALL_DONE` ausgeben".

> Hinweis: Die offizielle PGDG-Quelle ist in einigen Regionen langsam; alternativ ein inländisches Mirror verwenden
> (z. B. `https://mirror.nju.edu.cn/postgresql/repos/apt`).

---

## 3. Systemvorbereitung (was dieser Schritt macht)

> Ziel: Einen **nicht einloggfähigen** Dienst-Account anlegen, die Verzeichnisse des Programms anlegen und deren „Besitzer“ (Owner) korrekt setzen, zufällige Passwörter generieren.

```sh
sudo bash deploy/provision/02-provision-base.sh
```

Konkret macht es drei Dinge (alle gegen das `ls`-Ergebnis verifizierbar):

1. **Dienst-Account `netdisk` anlegen**: ein nur zum Ausführen des Dienstes gedachter, nicht einloggfähiger Account
   (mit minimalen Berechtigungen; das Programm liest nur das Binary, schreibt in sein eigenes Datenverzeichnis).
2. **Verzeichnisse anlegen und Besitzer setzen** (am kritischsten; falscher Besitzer führt dazu, dass der Selbsttest beim Dienststart direkt ablehnt):

   | Verzeichnis | Besitzer | Inhalt |
   |---|---|---|
   | `/opt/netdisk/data` | netdisk | Objekt- und Staging-Wurzel (content-adressierte Objekte, TUS-Staging) |
   | `/etc/netdisk` | root:netdisk | Konfigurationsdatei + secrets |
   | `/var/log/netdisk` | netdisk | Anwendungsprotokoll |
   | `/var/lib/netdisk` | netdisk | Backup/Drill-Status-JSON |

3. **Zufällige Passwortdatei `/etc/netdisk/secrets.env` generieren**: einmalig Datenbank-Passwort, JWT-Schlüssel,
   Redis-Passwort usw. generieren (**niemals ins Repository, niemals in die yaml schreiben**), vom nachfolgenden Skript und dem Programm gelesen.

> **Ohne Skript, Schritt für Schritt per Handbefehl** (entspricht den 3 Dingen von `02-provision-base.sh`):

```sh
# ① Nicht einloggfähigen Dienst-Account netdisk anlegen (System-Account, kein Home, Shell sperren)
sudo useradd --system --no-create-home --shell /usr/sbin/nologin netdisk

# ② Verzeichnisse anlegen und Besitzer setzen (falscher Besitzer → Selbsttest beim Start lehnt ab)
sudo install -d -o netdisk -g netdisk -m 0750 /opt/netdisk/data    # Objekt- und Staging-Wurzel
sudo install -d -o root   -g netdisk -m 0750 /etc/netdisk          # Konfiguration + secrets
sudo install -d -o netdisk -g netdisk -m 0750 /var/log/netdisk     # Anwendungsprotokoll
sudo install -d -o netdisk -g netdisk -m 0750 /var/lib/netdisk      # Backup/Drill-Status

# ③ Zufällige Passwortdatei secrets.env generieren (erst Zufallsstring holen, dann schreiben, dann Rechte enger fassen)
DB_PASS=$(openssl rand -base64 24)
REDIS_PASS=$(openssl rand -base64 24)
JWT_SECRET=$(openssl rand -base64 48)
sudo tee /etc/netdisk/secrets.env >/dev/null <<EOF
NETDISK_DB_PASSWORD=$DB_PASS
NETDISK_REDIS_PASSWORD=$REDIS_PASS
NETDISK_JWT_SECRET=$JWT_SECRET
EOF
sudo chown root:netdisk /etc/netdisk/secrets.env
sudo chmod 0640 /etc/netdisk/secrets.env
```

> Die Feldnamen richten sich nach den tatsächlich vom Programm gelesenen Umgebungsvariablen (siehe `deploy/systemd/secrets.env.example`).
> Die Skript-Variante fasst diese 3 Schritte zu einem einzigen `sudo bash deploy/provision/02-provision-base.sh` zusammen.
> Das Passwort wird nur einmal generiert; bei Rotation diese Datei ändern und den Dienst neu starten.

---

## 4. PostgreSQL konfigurieren

> In diesem Schritt wird PG auf den Zustand „passend für diesen Rechner + sicher“ gebracht: Lauschen nur lokal, Anpassung an wenig RAM,
> WAL-Archivierung einschalten (Voraussetzung für Point-in-Time-Recovery), die dem Programm vorbehaltene Rolle und Datenbank anlegen.

```sh
sudo bash deploy/provision/03-provision-postgresql.sh
```

Der Zweck der jeweiligen Konfiguration (alles nach `/etc/postgresql/17/main/conf.d/` geschrieben):

| Konfiguration | Wert | Warum |
|---|---|---|
| `listen_addresses` | `localhost` | Lauscht nur lokal, der PG-Port **erscheint nicht auf dem Netzwerkadapter** |
| `max_connections` | 100 | Anwendungsseite max. 32 Verbindungen, Puffer für Betrieb/psql |
| `shared_buffers` | 128MB | Anpassung an Rechner mit wenig RAM (sonst frisst der Standardwert für viel RAM den Speicher auf) |
| `archive_mode=on` + `archive_command` | — | **WAL-Archivierung**: Voraussetzung für Point-in-Time-Recovery (PITR); `pg_wal` läuft nicht voll |
| pg_hba | nur Loopback zulassen | verweigert direkte LAN-Verbindungen, nur lokaler Zugriff möglich |

Am Ende geschieht außerdem:
- Anlegen der Rolle `netdisk` und der Datenbank `netdisk` (owner=netdisk);
- Anlegen der `pg_trgm`-Erweiterung (vom Tabellenerstellungs-Skript benötigt; vorab durch Superuser angelegt, um Berechtigungsunterschiede bei der Migration zu vermeiden).

> **Ohne Skript, Schritt für Schritt per Handbefehl** (entspricht `03-provision-postgresql.sh`):

```sh
# ① Tuning + WAL-Archivierung einschalten (nach conf.d schreiben, damit nicht beim nächsten Major-Upgrade überschrieben)
sudo tee -a /etc/postgresql/17/main/conf.d/netdisk.conf >/dev/null <<'EOF'
listen_addresses = 'localhost'
max_connections = 100
shared_buffers = 128MB
archive_mode = on
archive_command = 'test ! -f /var/lib/postgresql/wal_archive/%f && cp %p /var/lib/postgresql/wal_archive/%f'
EOF
sudo install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/wal_archive

# ② Dem Programm vorbehaltene Rolle und Datenbank anlegen (Passwort aus §3 secrets.env)
sudo -u postgres psql <<'EOF'
CREATE ROLE netdisk LOGIN PASSWORD '<Datenbank-Passwort>';
CREATE DATABASE netdisk OWNER netdisk ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
EOF

# ③ pg_trgm-Erweiterung anlegen (von Superuser innerhalb der DB, für die Migration)
sudo -u postgres psql -d netdisk -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'

# ④ Neu starten, damit wirksam (Debian lässt pg_hba standardmäßig nur lokal zu, keine额外e Änderung nötig)
sudo systemctl restart postgresql
```

> Falls die Locale der lokalen Datenbank nicht UTF-8 ist, unbedingt die Methode `TEMPLATE template0` + `LC_COLLATE 'C'` aus Schritt ② zum Anlegen verwenden, um deterministische Byte-Reihenfolge sicherzustellen, nah an der Linux-Produktion.
> Die Skript-Variante fasst die obigen 4 Schritte zu einem einzigen `sudo bash deploy/provision/03-provision-postgresql.sh` zusammen (inkl. Versions-/Verbindungsselbsttest).

---

## 5. Redis konfigurieren

> In diesem Schritt wird Redis auf den Zustand „Login-Status geht beim Neustart nicht verloren, niemand wird durch Eviction rausgeworfen“ gebracht.

```sh
sudo bash deploy/provision/04-provision-redis.sh
```

In Redis liegen keine gewöhnlichen Cache-Daten, sondern **Token / Ratelimit-Fenster / Sync-Cursor** — gehen sie verloren, bedeutet das „alle Benutzer werden ausgeloggt“.
Daher gibt es **drei harte Anforderungen**, fehlt eine, treten Probleme auf:

| Anforderung | Wert | Fehlt sie, passiert |
|---|---|---|
| Zugriffspasswort setzen | `requirepass <Passwort>` | Login/Ratelimit fallen komplett aus |
| AOF-Persistenz einschalten | `appendonly yes` + `appendfsync everysec` | ein Neustart = alle Benutzer ausgeloggt |
| Schlüssel-Eviction verbieten | `maxmemory-policy noeviction` | Token/Cursor-Schlüssel werden evicted = Benutzer wahllos offline |

Das Skript bindet außerdem das Lauschen an localhost (`bind 127.0.0.1`) und testet nach dem Neustart mit dem Passwort (nicht authentifiziert muss abgelehnt werden, authentifiziert muss PING durchkommen).

> Das Speicherlimit von 96MB ist die Anpassung an wenig RAM; ist es voll, dann führt `noeviction` zu einem **Fehler** statt stiller Schlüssel-Löschung —
> der Fehler deckt das Problem auf, Eviction würde Benutzer nur still offline werfen.

> **Ohne Skript, Schritt für Schritt per Handbefehl** (entspricht `04-provision-redis.sh`):

```sh
# Drei harte Anforderungen + lokale Bindung + RAM-Limit, als Anhängsegment in die Debian-Redis-Konfiguration
sudo tee -a /etc/redis/redis.conf >/dev/null <<EOF
bind 127.0.0.1
requirepass <Redis-Passwort>
appendonly yes
appendfsync everysec
maxmemory-policy noeviction
maxmemory 96mb
EOF
sudo systemctl restart redis-server

# Test: Nicht authentifiziert muss abgelehnt werden, authentifiziert muss PING durchkommen
redis-cli -a <Redis-Passwort> --no-auth-warning PING   # sollte PONG zurückgeben
```

> Keine der drei harten Anforderungen ist entbehrlich (siehe Tabelle oben). Die Skript-Variante `sudo bash deploy/provision/04-provision-redis.sh`
> macht zusätzlich einen Test nach dem Neustart mit dem Passwort (nicht authentifiziert muss abgelehnt werden).

---

## 6. netdisk bauen und bereitstellen

### 6.1 Bauen (Reihenfolge ist wichtig)

```sh
# Umgebung (Inlands-Beschleunigung + Prüfung aus)
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off
cd Server-com
# ⚠ Das Frontend des Verwaltungs-Backends wird zur Compile-Zeit ins Binary embed-det:
#    nach Änderung des Frontends zuerst pnpm build, dann go build — bei vertauschter
#    Reihenfolge wird die alte Seite eingebettet (kompiliert trotzdem, ohne Fehler)
deploy/build-release.sh 1.0.0     # → deploy/dist/netdisk-1.0.0-linux-amd64
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<host>:/tmp/
```

> **Ohne Skript, Schritt für Schritt per Handbefehl** (entspricht `build-release.sh`):
> Die Kernreihenfolge darf nicht vertauscht werden: **zuerst `pnpm build` für das Frontend, dann `go build`** (vertauscht = alte Seite wird embed-det).

```sh
# ① Umgebung (Inlands-Beschleunigung + Prüfung aus)
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off

# ② Zuerst das Frontend des Verwaltungs-Backends bauen (web-Repository) und das Erzeugnis
#    nach Server-com/internal/webui/dist kopieren (für embed)
cd Server-com/web && pnpm install && pnpm build
# Das Erzeugnis wird dann vom embed-Befehl ins Binary gepackt (web/apps/*/dist → internal/webui/dist)

# ③ Server-seitiges Single-Binary kompilieren
cd ../ && go build -o deploy/dist/netdisk-1.0.0-linux-amd64 ./cmd/netdisk

# ④ Auf den Zielrechner kopieren
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<host>:/tmp/
```

### 6.2 Auf den Zielrechner bereitstellen

```sh
sudo bash deploy/install.sh /tmp/netdisk-1.0.0-linux-amd64 http://<externe URL>
```

`install.sh` ist idempotent und macht: Benutzer/Verzeichnisse anlegen → secrets generieren/wiederverwenden → Binary und Konfiguration installieren →
systemd-Dienst installieren → Selbsttest → starten → **Versions-Selbstbeweis** (bei Erfolg druckt die letzte Zeile `INSTALL_DONE`).

> **Ohne Skript, Schritt für Schritt per Handbefehl** (entspricht `install.sh`; Dienst-Account/Verzeichnisse/Passwort bereits in §3 vorbereitet):

```sh
# ① Binary und Konfiguration ablegen (Besitzer muss stimmen, sonst besteht der Selbsttest nicht)
sudo install -o root -g netdisk -m 0755 /tmp/netdisk-1.0.0-linux-amd64 /opt/netdisk/netdisk
sudo install -o root -g netdisk -m 0644 deploy/config/config.example.yaml /etc/netdisk/config.yaml
# Falls §3 keine secrets generiert hat, eine ergänzen (siehe §3 ③); bereits generierte wiederverwenden, nicht überschreiben

# ② systemd-Unit installieren und starten (die Unit enthält bereits „Selbsttest ExecStartPre → Migration+Start ExecStart“)
sudo install -o root -g root -m 0644 deploy/systemd/netdisk.service /etc/systemd/system/netdisk.service
sudo systemctl daemon-reload
sudo systemctl enable --now netdisk

# ③ Bestätigen, dass es läuft
sudo systemctl is-active netdisk          # → active
sudo systemctl status netdisk -l --no-pager
```

> Wichtig: In `netdisk.service` ist `ExecStart=/opt/netdisk/netdisk -migrate ...` „erst migrieren, dann weiter starten“,
> **es beendet sich nicht** — auch in der manuellen Variante nicht als Einmal-Befehl im Vordergrund warten lassen, sondern systemd verwalten.

### 6.3 Datenbankmigration

Die Migration ist in die systemd-Unit geschrieben und **wird beim Dienststart automatisch ausgeführt**, kein manuelles Ausführen nötig:

```ini
ExecStartPre=/opt/netdisk/netdisk -check -config /etc/netdisk/config.yaml   # Start-Selbsttest
ExecStart=/opt/netdisk/netdisk -migrate -config /etc/netdisk/config.yaml    # erst migrieren, dann weiter starten
```

> Hinweis: Die Semantik von `-migrate` ist „erst Migration ausführen, dann den Dienst weiter starten“, **es beendet sich nicht** —
> im Bereitstellungs-Skript nicht als Vordergrund-Befehl mit Warten behandeln, sonst hängt es für immer.

> Wer **nicht auf die automatische systemd-Migration** vertrauen, sondern vor dem Start separat migrieren möchte, kann goose einmal `up` gegen die Zieldatenbank laufen lassen
> (Migrationsverzeichnis liegt in `internal/migrate/sql`; normalerweise nicht nötig, beim Start automatisch erledigt):

```sh
export NETDISK_DB_DSN='postgres://netdisk:<Datenbank-Passwort>@127.0.0.1:5432/netdisk?sslmode=disable'
goose -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" status   # zuerst Version ansehen
goose -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up        # Migration ausführen
```

> Beide Vorgehensweisen sind äquivalent; solange `-migrate` in der systemd-Unit vorhanden ist, wird beim Start migriert, auch ohne manuelles goose.

---

## 7. Nginx Reverse-Proxy konfigurieren

> Nginx ist der einzige nach außen gerichtete Eingang; netdisk selbst lauscht nur auf 127.0.0.1:8080.

```sh
# apply: Site/Proxy-Header/Tuning installieren; verify: mit nginx -T (wirksame Konfiguration) Punkt für Punkt prüfen
sudo bash deploy/nginx/apply.sh
sudo bash deploy/nginx/verify.sh
```

**Einige leicht zu übersehende Fallstricke** (im Skript bereits behandelt, hier die Ursache):
- Upload/TUS-Schnittstelle muss `proxy_request_buffering` ausschalten, sonst landet der Request-Body zuerst auf Nginxs Temp-Platte,
  und Fortsetzung (Resume) sowie Fortschritt sind nicht mehr echt;
- Download-Schnittstelle schaltet `proxy_buffering` aus, SSE-Schnittstelle schaltet `proxy_buffering` aus und erhöht `proxy_read_timeout`.

> **Ohne Skript, Schritt für Schritt per Handbefehl** (entspricht `apply.sh` + `verify.sh`):

```sh
# ① Site-Konfiguration schreiben (minimal lauffähig, wichtige Zeilen markiert)
sudo tee /etc/nginx/sites-available/netdisk >/dev/null <<'EOF'
server {
    listen 80;
    server_name <externe Domain>;

    # Allgemeiner Reverse-Proxy: echte IP durchreichen
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }

    # TUS-Upload: request_buffering aus (damit Fortsetzung/Fortschritt echt sind)
    location /uploads/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering off;
    }

    # WebDAV: ebenfalls request_buffering aus
    location /dav/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering off;
        proxy_buffering off;
    }

    # SSE: buffering aus + Lese-Timeout erhöhen
    location /sync/events {
        proxy_pass http://127.0.0.1:8080;
        proxy_buffering off;
        proxy_read_timeout 86400s;
    }
}
EOF

# ② Site aktivieren, Syntax prüfen, neu laden
sudo ln -sf /etc/nginx/sites-available/netdisk /etc/nginx/sites-enabled/
sudo nginx -t                                  # erst weiter, wenn Syntax korrekt
sudo systemctl reload nginx

# ③ Ersatz für verify.sh: die wirksame Konfiguration auf Schlüsselpunkte prüfen (nicht die Datei auf der Platte ansehen)
sudo nginx -T | grep -E 'proxy_(request_)?buffering|proxy_read_timeout'
```

> Beim Handschreiben wird am ehesten eine der drei `proxy_*`-Puffer-Einstellungen oben vergessen; ohne sie verhält sich Upload/Fortsetzung/SSE anormal.
> Die Skript-Variante prüft mit `deploy/nginx/verify.sh` basierend auf `nginx -T` diese Punkte einzeln per Assertion, bereits für Sie verifiziert.

---

## 8. Abnahme nach der Bereitstellung

```sh
# 1) Alle Eingänge erreichbar
curl -s -o /dev/null -w '%{http_code}\n' http://<host>/admin/    # 200 „Netzdisk-Verwaltungs-Backend“
curl -s http://127.0.0.1:8080/healthz                            # 200

# 2) Der anfängliche Administrator wurde beim „Datenbank initialisieren“ automatisch angelegt
#    Benutzername admin, anfängliches Passwort admin123 (vor dem ersten Start mit NETDISK_BOOTSTRAP_ADMIN_PASSWORD überschreibbar)
#    Passwort nach dem Login bitte sofort ändern (empfohlen):
/opt/netdisk/bin/passwd -config /etc/netdisk/config.yaml \
  -username admin -role super_admin -prompt

# 3) End-to-End-Sonde: direkt gegen die Anwendung (nicht über den Proxy — SSE-Antwort-Header werden von Nginx konsumiert)
/opt/netdisk/bin/probesmoke -base http://127.0.0.1:8080 -user admin -pass admin123
```

---

## 9. Nächste Schritte nach Abschluss der Installation

- Ein **initiales Backup** erstellen und eine Wiederherstellungsübung durchführen (siehe 07-BACKUP.md), um vor dem offiziellen Einsatz zu bestätigen, dass „Wiederherstellung möglich“ ist;
- Monitoring zum Abgreifen von `/metrics` und Alarmregeln konfigurieren (siehe 06-OPS.md).
