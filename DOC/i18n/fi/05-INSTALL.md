# Server-com asennus ja käyttöönotto

> Tämä asiakirja käsittelee vain **asennuksen alusta tuotantokäyttöön**; päivittäinen tarkastus / vianetsintä ks. `06-OPS.md`, tietosuojaus ks. `07-BACKUP.md`.
> Tämän repositorion mukana tulevat skriptit ovat `deploy/`-hakemistossa, ja lähes kaikille vaiheille on valmiit skriptit (idempotentit, toistettavissa).

---

## 1. Käyttöönottomuoto ja esivaatimukset

Koko verkkolevy on **yksi Go-binääri** (`netdisk`), ja sen ulkopuolella on vain neljä komponenttia, jotka tukevat sitä:

```
Nginx(80/443) ─反代─▶ netdisk(:8080) ─▶ PostgreSQL 17 (:5432)
                                      ─▶ Redis (:6379)
```

| Komponentti | Versiovaatimus | Missä asennettuna |
|---|---|---|
| netdisk | repositorion koontituote | paikallinen `/opt/netdisk/` |
| PostgreSQL | **vähintään 17** (ks. alla) | paikallinen |
| Redis | 7+ | paikallinen |
| Nginx | suositus 1.26+ | paikallinen (tuotanto) |

> **Miksi PostgreSQL 17 on vähimmäisvaatimus?**
> Perusavaimen oletusarvo käyttää `gen_random_uuid()`-funktiota (ks. migraatioskripti `00001_init.sql`) — se on **PostgreSQL 13:sta lähtien sisäänrakennettu** UUID-funktio. Versioraja ei siis määräydy UUID:n luonnista. Tämä projekti asettaa vähimmäistukiversioksi **PG 17** (tasattuna valtavirran jakeluversioihin); myös arkkitehtuuridokumentin ADR-1 noudattaa tätä. 15/16-ympäristöt toimivat myös (gen_random_uuid on yhteensopiva), mutta arkistointi-/pitkäaikaistukipolitiikkaa ylläpidetään versiossa 17. Järjestelmä tukee tällä hetkellä vain tätä yhtä tietokantaa (ei yhdistä MySQL:iin tms.).

---

## 2. Riippuvuusohjelmistojen asennus

> Tässä vaiheessa asennetaan: varmuuskopiointityökalut (borg/rsync), PostgreSQL 17, Redis, Nginx.
> Avainkohta: **Debian 13:n oma varasto toimittaa täsmälleen PostgreSQL 17:n**, mikä täyttää tämän projektin vähimmäisvaatimuksen — asenna suoraan, **PGDG-lähdettä ei tarvita** (vain jos jakelun varastoversio on vanhempi, esim. Debian 12 versiolla 15, lisää PGDG alla olevilla manuaalisilla komennoilla saadaksesi 17).

```sh
# Hoidetaan skriptillä: asenna postgresql-17/redis/nginx/borg → poista Apache käytöstä vapauttaaksesi 80/443
sudo bash deploy/provision/01-install-packages.sh
```

Skripti tulostaa jokaisen komponentin version vahvistukseksi ja lopuksi tulostaa `INSTALL_DONE`.

> **Ei skriptiä — vaihe vaiheelta manuaalisilla komennoilla** (vastaa `01-install-packages.sh`):

```sh
# ① Asenna PostgreSQL 17 / Redis / Nginx / varmuuskopiointityökalu borg
#    (Debian 13 tuo PG17:n, asenna suoraan; PGDG-lähde vain jos jakelun
#     varastoversio on liian vanha — huomautus alla)
sudo apt-get update
sudo apt-get install -y postgresql-17 redis-server nginx borgbackup

# ② Poista Apache käytöstä vapauttaaksesi 80/443 (vain jos Apache on asennettu)
sudo systemctl disable --now apache2 2>/dev/null || echo 'Ei Apachea, ohitettu'
```

> Skripti kokoaa nämä vaiheet komentoon `sudo bash deploy/provision/01-install-packages.sh` ja tulostaa lisäksi versiot sekä `INSTALL_DONE`.

> Vihje: PGDG:n virallinen lähde on joissakin alueissa hidas, voit käyttää kotimaista peiliä
> (esim. `https://mirror.nju.edu.cn/postgresql/repos/apt`).

---

## 3. Järjestelmän valmistelu (mitä tämä vaihe tekee)

> Tavoite: luoda **ei-kirjautuva** palvelukäyttäjä, luoda ohjelman hakemistot ja asettaa niiden "omistaja" oikein, luoda satunnaiset salasanat.

```sh
sudo bash deploy/provision/02-provision-base.sh
```

Mitä se tarkalleen tekee (kaikki voidaan varmistaa `ls`-tuloksia vastaan):

1. **Luodaan palvelukäyttäjä `netdisk`**: käyttäjä, jota käytetään vain palvelun ajamiseen eikä se voi kirjautua sisään
   (sille annetaan vähimmäisoikeudet; ohjelma lukee vain binääriä ja kirjoittaa omaan datakansioonsa).
2. **Luodaan hakemistot ja asetetaan omistaja** (kriittisin kohta; väärin asetettu omistaja saa palvelun käynnistyksen itsetarkistuksen hylkäämään käynnistyksen suoraan):

   | Hakemisto | Omistaja | Mitä sisältää |
   |---|---|---|
   | `/opt/netdisk/data` | netdisk | objektien ja väliaikaissäilön juuri (sisältöosoitteiset objektit, TUS-väliaikaissäilö) |
   | `/etc/netdisk` | root:netdisk | kokoonpanotiedostot + salaisuudet |
   | `/var/log/netdisk` | netdisk | sovelluslokit |
   | `/var/lib/netdisk` | netdisk | varmuuskopion / harjoituksen tilan JSON |

3. **Luodaan satunnainen salasanatiedosto `/etc/netdisk/secrets.env`**: kerran luodaan tietokannan salasana, JWT-salaisuus,
   Redis-salasana jne. (**ei missään tapauksessa versionhallintaan eikä yaml-tiedostoon**), ja myöhemmät skriptit ja ohjelma lukevat sen.

> Salasanat luodaan vain kerran; jos niitä halutaan vaihtaa, muokkaa tätä tiedostoa ja käynnistä palvelu uudelleen.

---

## 4. PostgreSQL:n kokoonpano

> Tässä vaiheessa PG asetetaan "sopivaksi tälle koneelle + turvalliseksi" tilaan: kuuntelu vain paikallisesti, pienen muistin mukautus,
> WAL-arkistoinnin käyttöönotto (ajankohtaisen palautuksen edellytys), ohjelman oman roolin ja tietokannan luonti.

```sh
sudo bash deploy/provision/03-provision-postgresql.sh
```

Kunkin kokoonpanon tarkoitus (kaikki kirjoitettiin tiedostoon `/etc/postgresql/17/main/conf.d/`):

| Kokoonpano | Arvo | Miksi |
|---|---|---|
| `listen_addresses` | `localhost` | kuunnellaan vain paikallisesti, PG-portti **ei näy verkkokortilla** |
| `max_connections` | 100 | sovelluspuoli enintään 32 yhteyttä, varaa tilaa ylläpidolle/psql:lle |
| `shared_buffers` | 128MB | pienen muistin koneen mukautus (muuten oletusarvo ison muistin mukaan syö muistin loppuun) |
| `archive_mode=on` + `archive_command` | — | **WAL-arkistointi**: ajankohtaisen palautuksen (PITR) edellytys; `pg_wal` ei täytä levyä |
| pg_hba | salli vain loopback | hylkää lähiverkon suorat yhteydet, vain paikallinen käyttö sallitaan |

Lopuksi se myös:
- Luo roolin `netdisk` ja tietokannan `netdisk` (owner=netdisk);
- Luo `pg_trgm`-laajennuksen (taulujen luontiskripti vaatii sen; luodaan etukäteen pääkäyttäjänä, jotta migraation aikaisilta oikeuseroilta vältytään).

---

## 5. Redis:n kokoonpano

> Tässä vaiheessa Redis asetetaan tilaan, jossa "uudelleenkäynnistys ei kadota kirjautumistilaa eikä se poista ketään automaattisesti".

```sh
sudo bash deploy/provision/04-provision-redis.sh
```

Redisissä ei tallenneta tavallista välimuistia, vaan **tokenit / nopeusrajoitusikkunat / synkronointikohdistimet** — jos ne katoavat, se vastaa "kaikkien käyttäjien kirjautumista ulos".
Siksi on **kolme ehdotonta vaatimusta**, ja yhdenkin puuttuminen aiheuttaa ongelman:

| Vaatimus | Arvo | Mitä tapahtuu ilman |
|---|---|---|
| Aseta käyttöoikeussalasana | `requirepass <salasana>` | kirjautuminen / nopeusrajoitus lakkaavat kokonaan toimimasta |
| Ota AOF-pysyvyys käyttöön | `appendonly yes` + `appendfsync everysec` | yksi uudelleenkäynnistys = kaikki käyttäjät kirjataan ulos |
| Kielle turvaluonti | `maxmemory-policy noeviction` | tokeni-/kohdistinavaimet poistetaan = käyttäjiä potkitaan ulos satunnaisesti |

Skripti myös: sitoo kuuntelun paikalliselle (`bind 127.0.0.1`), ja uudelleenkäynnistyksen jälkeen testaa salasanalla (todentamaton on hylättävä, todentanut saa PING-vastauksen).

> Muistin ylärajan asettaminen 96 Mt on pienen muistin koneen mukautus; kun yläraja täyttyy, koska käytössä on `noeviction`, se **palauttaa virheen** sen sijaan, että se hiljaa poistaisi avaimia —
> virhe paljastaa ongelman, kun taas turvaluonti vain hiljaa potkisi ihmisiä ulos.

---

## 6. netdiskin kääntäminen ja käyttöönotto

### 6.1 Kääntäminen (järjestys on tärkeä)

```sh
# Ympäristö (kotimainen kiihdytys + tarkistuksen poiskytkentä)
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off
cd Server-com
# ⚠ Hallintapaneelin frontendi käännetään upoksi Go-binääriin käännösaikana: jos frontendiä on muutettu, on ensin tehtävä pnpm build ja sitten go build,
#    väärä järjestys upottaa vanhan sivun (kääntyy, mutta ei virhettä)
deploy/build-release.sh 1.0.0     # → deploy/dist/netdisk-1.0.0-linux-amd64
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<host>:/tmp/
```

### 6.2 Käyttöönotto kohdekoneelle

```sh
sudo bash deploy/install.sh /tmp/netdisk-1.0.0-linux-amd64 http://<ulompi URL>
```

`install.sh` on idempotentti ja tekee: luo käyttäjä/hakemistot → luo/käyttää uudelleen secrets → asentaa binäärin ja kokoonpanon →
asentaa systemd-palvelun → itsetarkistus → käynnistys → **version itsetodistus** (onnistuessaan tulostaa viimeisenä rivinä `INSTALL_DONE`).

### 6.3 Tietokantamigraatio

Migraatio on kirjoitettu systemd-yksikköön ja **suoritetaan automaattisesti palvelun käynnistyksen yhteydessä**, eikä sitä tarvitse ajaa käsin:

```ini
ExecStartPre=/opt/netdisk/netdisk -check -config /etc/netdisk/config.yaml   # käynnistyksen itsetarkistus
ExecStart=/opt/netdisk/netdisk -migrate -config /etc/netdisk/config.yaml    # ensin migraatio, sitten käynnistyksen jatkaminen
```

> Huom: `-migrate`-parametrin semantiikka on "aja ensin migraatio ja jatka sitten palvelun käynnistystä", **se ei poistu** —
> älä odota sitä edustaprosessina käyttöönottoskriptissä, muuten se jumiutuu ikuisesti.

---

## 7. Nginx-käänteisen välityspalvelimen kokoonpano

> Nginx on ainoa ulospäin oleva sisääntulo, ja netdisk itse kuuntelee vain 127.0.0.1:8080.

```sh
# apply: asentaa sivuston/välityspalvelun otsakkeet/virityksen; verify: käyttää nginx -T (voimassa oleva kokoonpano) rivi riviltä tarkistaakseen
sudo bash deploy/nginx/apply.sh
sudo bash deploy/nginx/verify.sh
```

**Muutama helposti huomiotta jäävä sudenkuoppa** (skripti on käsitellyt ne, tässä selitys syistä):
- Lataus-/TUS-rajapinnoissa on oltava pois päältä `proxy_request_buffering`, muuten pyynnön runko putoaa ensin Nginxin väliaikaiseen levylle,
  ja jatkuvuuslataus sekä edistyminen eivät enää ole todellisia;
- Latausrajapinnossa pois päältä `proxy_buffering`, SSE-rajapinnossa pois päältä `proxy_buffering` ja suurennetaan `proxy_read_timeout`.

---

## 8. Käyttöönoton jälkeinen vastaanotto

```sh
# 1) kaikki sisääntulot tavoitettavissa
curl -s -o /dev/null -w '%{http_code}\n' http://<host>/admin/    # 200「网盘管理后台」
curl -s http://127.0.0.1:8080/healthz                            # 200

# 2) Ensimmäinen ylläpitäjä luodaan automaattisesti "tietokannan alustuksen" yhteydessä
#    Käyttäjänimi admin, aloitussalasana admin123 (ohitettavissa NETDISK_BOOTSTRAP_ADMIN_PASSWORD:llä ennen ensimmäistä käynnistystä)
#    Vaihda salasana heti ensimmäisen kirjautumisen jälkeen (suositus):
/opt/netdisk/bin/passwd -config /etc/netdisk/config.yaml \
  -username admin -role super_admin -prompt

# 3) päästä päähän -anturi: yhdistä suoraan sovellukseen (älä kulje välityspalvelimen kautta — Nginx kuluttaa SSE-vastausotsakkeet)
/opt/netdisk/bin/probesmoke -base http://127.0.0.1:8080 -user admin -pass admin123
```

---

## 9. Mitä tehdä asennuksen jälkeen

- Tee **ensimmäinen varmuuskopio** ja suorita yksi palautusharjoitus (ks. `07-BACKUP.md`), varmista "voidaanko palauttaa" ennen kuin otat järjestelmän viralliseen käyttöön;
- Määritä valvonta haarukoimaan `/metrics` ja hälytysäännöt (ks. `06-OPS.md`).
