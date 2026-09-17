# Server-com ylläpito

> Tämä asiakirja kertoo **miten hallita järjestelmää tuotantokäytön jälkeen**: rutiinitarkastus, käynnistys/pysäytys, päivitys, valvonta ja hälytykset, yleiset vianetsinnät.
> Ensimmäinen asennus ks. `05-INSTALL.md`, tietosuojaus ks. `07-BACKUP.md`.

---

## 1. Rutiinitarkastus

Alla on tarkastuslista, jota voi suoraan noudattaa; suositellaan käytävän läpi päivittäin/viikoittain.

```sh
# ① palvelu ja riippuvuudet ovat käynnissä
systemctl is-active postgresql redis-server netdisk nginx

# ② katso ovatko lokeissa poikkeamia
journalctl -u netdisk -n 50 --no-pager

# ③ onko varmuuskopio onnistunut äskettäin (kriittistä! ks. tarkemmin `07-BACKUP.md`)
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_backup_last_success_timestamp_seconds'

# ④ riittävätkö levytilat (yli 90 % = lataus hylätään)
df -h /opt/netdisk/data /var/lib/postgresql /var/backups/netdisk
```

| Tarkistuskohde | Mikä on normaalia | Poikkeustilanteessa |
|---|---|---|
| Kaikki palvelut active | 4 kpl ovat active | ks. §6 vianetsintä |
| PG-data kasvaa mutta myös WAL-arkistohakemisto kasvaa | arkistointi etenee normaalisti | arkistoinnin tukkiutuminen = `pg_wal` täyttää levyn |
| Viimeisin varmuuskopio < 26 h | aikaleima on tuore | tarkista varmuuskopioloki heti |
| Objektitarkastus on käynnissä | `netdisk_object_patrol_last_timestamp_seconds` alle 48 h vanha | tarkastuksen pysähtyminen = vuoto/katoaminen ilman havaintoa |
| Levyn vesitaso < 90 % | — | ≥90 % hylkää uudet lataukset (palauttaa 507) |

---

## 2. Käynnistys / pysäytysjärjestys

> Järjestyksellä on väliä: **tietokanta ensin, sovellus sen jälkeen, käänteinen välityspalvelin viimeisenä**; pysäytyksessä päinvastoin.

```sh
# Käynnistys
systemctl start postgresql
systemctl start redis-server
systemctl start netdisk      # käynnistyksessä täydennetään tietokantamigraatio automaattisesti
systemctl start nginx
systemctl is-active postgresql redis-server netdisk nginx

# Pysäytys (pysäytä ensin Nginx, sitten sovellus, jotta keskeneräiset lataukset/lataamiset tyhjenevät sulavasti)
systemctl stop nginx
systemctl stop netdisk
systemctl stop redis-server
systemctl stop postgresql
```

---

## 3. Päivitys ja takaisinkaanto

### 3.1 netdisk-binäärin päivitys (tavanomainen)

```sh
deploy/build-release.sh <uusi versio>
scp deploy/dist/netdisk-<uusi versio>-linux-amd64 root@<host>:/tmp/
# Suositus kulkea install.sh:n kautta (idempotentti, pysäyttää vanhan prosessin automaattisesti → itsetarkistus → käynnistys → version itsetodistus)
sudo bash deploy/install.sh /tmp/netdisk-<uusi versio>-linux-amd64 http://<ulompi URL>
```

> Takaisinkaanto = asenna **edellinen** binääri takaisin ja käynnistä uudelleen (migraatio on eteenpäin yhteensopiva ja lisäävä, yleensä tietokantaa ei tarvitse peruuttaa).
> Jos palvelu on pysäytetty nopeusrajoituksen vuoksi toistuvien kaatumisten takia (`systemctl start` ilmoittaa "Start request repeated too quickly"),
> korjaa juurisyyn jälkeen ensin `systemctl reset-failed netdisk` ja käynnistä sitten.

### 3.2 PostgreSQL:n suuren version päivitys (esim. 17→18)

> Paikallinen perusversio on 18; jos ympäristössä on aiempi versio (15/16) ja halutaan päivittää, käytä `pg_upgrade`. **Ennen päivitystä on oltava kerran varmuuskopio, jonka tiedetään olevan palautettavissa**,
> koska `pg_upgrade`-epäonnistumisen tapauksessa kumpikin, vanha ja uusi instanssi, saattavat jäädä käynnistymättä.

```sh
systemctl stop netdisk                       # pysäytä ensin sovellus, jotta kirjoituksia ei tapahdu päivityksen aikana
su - postgres -c "/usr/lib/postgresql/17/bin/pg_dumpall > /var/backups/netdisk/pre-upgrade.sql"
apt-get install -y postgresql-18             # asenna uusi versio (PGDG-lähde)
su - postgres -c "/usr/lib/postgresql/18/bin/pg_upgrade \
  --old-datadir=/var/lib/postgresql/17/main --new-datadir=/var/lib/postgresql/18/main \
  --old-bindir=/usr/lib/postgresql/17/bin --new-bindir=/usr/lib/postgresql/18/bin --check"
# Kun --check onnistuu, poista --check ja aja uudelleen, sitten kerää tilastot uudelleen
su - postgres -c "/usr/lib/postgresql/18/bin/vacuumdb --all --analyze-in-stages"
```

---

## 4. Valvonta ja hälytykset

- **Paljastuspiste**: `GET /metrics` (Prometheus-tekstiformaatti, **itse kehitetty** kerääjä).
  Oletuksena sallitaan vain **paikallinen loopback / luotettava välityspalvelin**; koneiden väliseen hakuun on asetettava `NETDISK_METRICS_TOKEN` ja lähetettävä `Authorization: Bearer <token>`.
- **Haarukointi**: `deploy/prometheus/prometheus.yml` (oletus 30 s väli).
- **Hälytysäännöt**: `deploy/prometheus/netdisk-alerts.yml` (kymmenkunta kappaletta), tarkista syntaksi `promtool check rules`.

**Tärkeimmät seurattavat mittarit** (merkitys + kriteeri):

| Mittari | Merkitys | Milloin toimitaan |
|---|---|---|
| `netdisk_process_resident_memory_bytes` | prosessin asuintodennäköisyysmuisti | lähellä ylärajaa (320M) = systemd tappaa ja käynnistää uudelleen |
| `netdisk_disk_used_percent` | objektialueen levyn vesitaso | ≥90 % hylkää lataukset; `>85` ensin esihälytys |
| `netdisk_backup_last_success_timestamp_seconds` | viimeisimmän onnistuneen varmuuskopioinnin hetki | `now()-se > 26h` → hälytys (critical) |
| `netdisk_object_missing_total` | tietokannassa olevien mutta levyllä puuttuvien objektien määrä | **≥1 tarkoittaa dataa ei voida lukea** (critical) |
| `netdisk_object_leak_bytes` | levyllä olevat mutta tietokannassa puuttuvat (vie tilaa) | >64MiB = levy kasvaa vain |
| `netdisk_quota_drift_bytes` | kiintiön kirjanpidon ja todellisen käytön välinen ero | >1MiB = kiintiölaskennassa epäilty vika |
| `netdisk_restore_drill_last_timestamp_seconds` | viimeisimmän palautusharjoituksen hetki | >90 päivää harjoittelematta → hälytys |

> **Tärkeä periaate**: lukukelvoton mittari **ei tuota näytettä** sen sijaan, että se ilmoittaisi 0 (0 ilmoittaminen saisi hälytyksen valehtelemaan).
> Esimerkiksi kun levyn kapasiteetin tunnistus epäonnistuu, kolme kapasiteettikäyrää katoavat kokonaan — tämä on normaalia "lukukelvottomuutta", ei levy on täynnä.

---

## 5. Verkkoturvallisuuden ja käyttöoikeuksien keskeiset kohdat

- `/metrics` palauttaa **401** on **suunniteltu käyttäytyminen**: se sallii vain loopback/luotettavan välityspalvelimen. Koneiden välinen haku vaatii Bearer-tokenin.
- Salaisuudet eivät koskaan mene yaml-tiedostoon / versionhallintaan, vaan ovat vain `/etc/netdisk/secrets.env` (vaihto = muokkaa tätä tiedostoa + käynnistä palvelu uudelleen).
- PostgreSQL kuuntelee vain loopbackia, Redis kuuntelee vain loopbackia: näiden kahden palvelun porttien **ei tule näkyä verkkokortilla**.

---

## 6. Yleiset vianetsinnät (runbook)

Jokainen on kirjoitettu muodossa **oire → mitä katsoa ensin → yleiset syyt ja toimenpiteet**, ja komennot voidaan kopioida suoraan.

### 6.1 Palvelu ei käynnisty

```sh
systemctl status netdisk -l
journalctl -u netdisk -n 60 --no-pager
```

| Oire | Merkitys | Toimenpide |
|---|---|---|
| `ExecStartPre ... status=1/FAILURE` | **käynnistyksen itsetarkistus pysäyttää** (kokoonpano/hakemisto/portti/riippuvuus) | katso sen kerralla listaamat ongelmat ja korjaa ne yksitellen |
| `active (running)` mutta pyyntö 502 | palvelu on päällä, mutta kuunteluosoite ei täsmää Nginxin osoittamaan | vertaa kokoonpanon `http_addr` ja Nginxin upstreamia |
| `Start request repeated too quickly` | toistuva kaatuminen laukaisee nopeusrajoituksen | korjaa juurisyyn jälkeen `systemctl reset-failed netdisk` |

Kaksi yleistä itsetarkistusvirhettä:
- **Hakemisto ei ole kirjoitettavissa**: `chown -R netdisk:netdisk /opt/netdisk/data` (jos `objects/`, `tus-tmp/` on luotu aiemmin root-käyttäjänä, niiden omistaja on root).
- **Vain luku -tiedostojärjestelmä**: palvelu on rajoitettu `ProtectSystem=strict` -asetuksella — jos olet muuttanut secrets-tiedostossa olevaa polkua, on **vastaavasti muutettava systemd-yksikön `ReadWritePaths`** ja lisättävä uusi polku sinne.

### 6.2 Lataus/lataaminen jumittuu 0 %:iin

```sh
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_(http|tus|sse)'
tail -f /var/log/nginx/netdisk.access.log | grep -E 'rt=|urt='
```

- `urt=` (ylävirran kesto) on paljon pienempi kuin `rt=` (kokonaiskesto) → aika kuluu Nginxissä → tarkista, onko TUS/WebDAV:ssä **pois päältä `proxy_request_buffering`** (pois ollessa pyynnön runko putoaa ensin Nginxin väliaikaiseen levylle, eikä edistyminen tai jatkuvuus ole todellista).
- Hidas lataaminen → tarkista latausrajapinnan `proxy_buffering off`.
- SSE-tapahtumien viive kymmeniä sekunteja → Nginx puskuroi tapahtumavirran → varmista, että SSE-rajapinnassa on `proxy_buffering off` + `proxy_read_timeout 86400s`.
  (Nämä kolme on `deploy/nginx/verify.sh`-skriptissä väitetty.)

### 6.3 Kaikki kirjataan ulos / kirjautuminen katkeaa heti

```sh
redis-cli -a <salasana> --no-auth-warning CONFIG GET appendonly appendfsync maxmemory-policy
```

- Mikä tahansa näistä, joka ei täyty, on poikkeama — **kolme ehdotonta vaatimusta** (`requirepass` / `appendonly yes`+`everysec` / `maxmemory-policy noeviction`)
  ja niiden puuttumisen seuraukset on esitetty taulukossa kohdassa `05-INSTALL.md` §5; korjaa ne rivi riviltä vertailun mukaan ja käynnistä Redis uudelleen.
- Muista: Redisissä **ei ole välimuistia**; kadonneet ovat kirjautumistila ja kohdistimet, eivätkä ne vaikuta jo levylle kirjoitettuihin tiedostoihin.

### 6.4 Varmuuskopio ei onnistunut

```sh
systemctl status netdisk-backup.service
journalctl -u netdisk-backup -n 40 --no-pager
cat /var/lib/netdisk/backup-status.json
su - postgres -c "psql -Atc 'SELECT archived_count, failed_count FROM pg_stat_archiver' netdisk"
ls -l /var/lib/postgresql/wal_archive | tail -3
```

- **WAL-arkistointi epäonnistui** → yhdeksän kertaa kymmenestä arkistohakemisto ei ole saavutettavissa: arkistohakemistoa **ei saa** sijoittaa `/var/lib/netdisk` alle
  (se on `netdisk:netdisk 0750`, eikä postgres-käyttäjällä ole läpikulkuoikeutta), vaan se on sijoitettava `/var/lib/postgresql/wal_archive`
  (postgresin omaan hakemistopuuhun).
- **`pg_dump` Permission denied** → dump-hakemiston on oltava **postgres**-käyttäjän omistama (se suoritetaan postgres-käyttäjänä).

### 6.5 Objektitarkastuksen hälytys

Auditointi on **vain luku** -tilassa, eikä se koskaan korjaa automaattisesti — **älä poista objekteja käsin tai aja siivousskriptiä ennen kuin olet selvittänyt syyn**.
`object_missing`-hälytyksen yhteydessä paikanna se tekemällä objektien uudelleensoitto kohdan `07-BACKUP.md` mukaisesti.

### 6.6 Levy täyttymässä

```sh
du -sh /opt/netdisk/data /var/backups/netdisk /var/lib/postgresql/wal_archive /var/log
```

Kolme paikkaa kasvavat: objektihakemisto, varmuuskopiot, WAL-arkistointi. **Älä** poista WAL-arkistohakemistoa tilan vapauttamiseksi
(se on ajankohtaisen palautuksen kyvyn kokonaistuotto). Järjestys: synkronoi ensin varmuuskopiot koneen ulkopuolelle ja poista vasta sen jälkeen paikalliset vanhat dumpit (skripti säilyttää 14 päivää),
ja harkitse sitten sovelluksen 24 h kierrätysikkunan käyttöä poistettujen objektien hautakivien siivoamiseen (**älä poista käsin**).

### 6.7 "En tehnyt mitään, mutta yhteys ei toimi" — tehokkain selvitys

```sh
ss -tlnp | grep -E '80|8080|5432|6379'      # 1. kuka kuuntelee, missä kuunnellaan
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz   # 2. ohita välityspalvelin, yhdistä suoraan
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1/admin/         # 3. sitten välityspalvelimen kautta
journalctl -u netdisk -n 20 --no-pager      # 4. katso sovelluslokia (välitetäänkö client_ip)
nginx -T | grep -A5 'location /tus'         # 5. katso välityspalvelimen "voimassa oleva" kokoonpano (ei levylle kirjoitettua tiedostoa)
```

Vaiheiden 2 ja 3 ero paikantaa ongelman välittömästi joko "sovellukseen" tai "välityspalvelimeen";
jos vaiheessa 4 `client_ip` näyttää `127.0.0.1`:ltä mutta todellinen asiakas on toisella koneella, se tarkoittaa, että `X-Real-IP` ei välitä tai luotettavan välityspalvelimen kokoonpano on väärin.
