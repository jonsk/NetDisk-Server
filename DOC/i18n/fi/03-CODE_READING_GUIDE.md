# Server-com - koodin lukemisen opas

> Soveltuva versio: Server-com (avoimen lähdekoodin versio / Community) · Go-palvelinpuolen yksi binääri verkkolevystä
> Lukijakunta: insinöörit ja ylläpidon väki, jotka tarvitsevat ymmärtää, selvittää tai jatkokehittää tätä koodia.
> Tämä opas kertoo "miten koodi on järjestetty, miten data virtaa, minne mennä jos haluat muuttaa X", eikä selitä algoritmeja rivi riviltä.
> Moduolikohtainen hyväksyntä ja kehityksen punaiset linjat ovat saman hakemiston tiedostossa `01-FEATURE_LIST.md`; auktoritatiivinen suunnittelu on tiedostossa `D:\WorkSpace\GO\Doc\网盘系统架构设计文档.md` (V3.0).

---

## 1. Mitä tämä repositorio on

`Server-com` on yritysverkkolevyn **taustapalvelu**, kirjoitettu Go:lla, ja lopuksi käännetty **yhdeksi suoritettavaksi tiedostoksi** (yksi binääri).

Se tarjoaa ulospäin neljää kykyä:

| Kyky | Kuvaus | Pääsisääntulo |
|---|---|---|
| REST hallinta/tiedosto-rajapinta | taustahallinta, tiedostojen luominen/poistaminen/muokkaaminen/haku, lataus, jakaminen | `/api/v1/*` |
| TUS jatkuva lataus | suurten tiedostojen sirpaleittainen lataus | `/tus/*` |
| WebDAV | työpöytäliitoslevy (yhteensopiva tiedostoprotokolla) | `/webdav/*` |
| SSE reaaliaikainen synkronointi | push "tiedosto muuttui" asiakkaalle, asiakas hakee sitten inkrementaalisesti | `/api/v1/events` |

Se **ei** vastaa: HTTP-välityspalvelimesta (Nginx), frontend-sivuista (`web`-repositorio), työpöytäasiakkaasta (`desktop`-repositorio). Nämä kolme ovat erillisiä repositorioita tai ulkoisia komponentteja.

> Yhdellä lauseella: tämä repositorio = joukko Go-paketteja + joukko komentoja + migraatioskriptit + käyttöönottomateriaali, jotka lopulta tuottavat `netdisk`-binäärin.

---

## 2. Ylimmän tason hakemistorakenne

```
Server-com/
├── cmd/            命令行程序（8 个），见 §3
├── internal/       核心代码（36 个 Go 包），见 §4
├── deploy/         部署物料：build 脚本 / 配置示例 / systemd / nginx / 备份 / 供给脚本
├── DOC/            仓库说明文档（本指南所在的目录）
├── scripts/        辅助脚本
├── .github/        CI 流水线
├── go.mod / go.sum Go 依赖清单
└── README.md       仓库入口说明
```

Lukusuositus: **katso ensin `cmd/netdisk/main.go`:n käynnistyskokoonpano (§5)**, sitten seuraa "miten yksi pyyntö etenee" (§6) sisään `internal/api`:hin, ja sen jälkeen syvenny tarpeen mukaan johonkin liiketoimintapakettiin.

---

## 3. Komennot (cmd/) —— jokainen on itsenäinen pienohjelma

`cmd/`:n alla on 8 hakemistoa, joista jokainen on oma `main`-ohjelma:

| Komento | Mitä tekee | Milloin käytetään |
|---|---|---|
| **netdisk** | **pääpalvelu**, ainoa tuotantoon suunnattu prosessi | se, jonka `systemd` käynnistää |
| **migrate** | tietokannan migraatio | käyttöönotossa `go run ./cmd/migrate up` |
| **passwd** | tilin ylläpito: käyttäjän luonti / salasanan nollaus / käyttöönotto-poisto | ylläpidon manuaalinen tilin käsittely |
| **probesmoke** | päästä päähän -anturi: todellinen HTTP läpi kirjautuminen → lataus → lataus alas | kehityskauden itsetesti |
| **devdb** | paikallisen kehitys/testitietokannan ylläpito | kehittäjän paikallinen käyttö |
| **depsguard** | arkkitehtuuriportti: tarkistaa "ydinpaketin ei tule riippua HTTP-tasosta" jne. | CI staattinen tarkistus |
| **testsgate** | integraatiotestin portti: varmistaa että testit todella ajettiin | CI |
| **coveragegate** | kattavuusportti: laskee lausetason kattavuuden paketin mukaan | CI |

Tuotantoympäristössä kiinnostavat vain kaksi ensimmäistä: `netdisk` ja käyttöönotossa käytettävä `migrate`. Loput ovat kehitys-/CI-apuja.

---

## 4. internal/ -pakettikartta —— missä koodi on, mitä tekee

36 pakettia voidaan jakaa viiteen tasoon vastuun mukaan. Tasojen ymmärtäminen on avain tämän repositorion lukemiseen: **HTTP-taso vain "vastaanottaa pyynnön, kutsuu palvelua, palauttaa vastauksen", liiketoimintalogiikka on palvelutasossa, todellinen tietokannan luku/kirjoitus on repo-tasossa, ja alimpana on tietokanta/Redis/levy.**

### 4.1 Sisääntulot ja HTTP-taso (ensimmäinen, johon tässä repositoriossa törmäät)
| Paketti | Vastuu |
|---|---|
| **api** | **reitityksen pääkokoonpano**. Kaikki URL↔käsittelijäfunktio -kartoitukset ovat `api.go`:ssa. Lähes kaikki "mikä rajapinta kutsuu ketä" löytyy täältä |
| **middleware** | väliketju: pyyntötunniste, todellinen IP, strukturoitu virhe, loki, kaatumisen palautus. Pyynnön läpikäymät "turvatarkastuspisteet" |
| **apierr** | yhtenäinen virheen kapselointi (HTTP-tilakoodi + liiketoimintakoodi + kiinankielinen teksti) |
| **webui** | `/admin` taustapaneelin frontend-staattisten resurssien sisääntulo (embed-atty binääriin, Nginx ei enää isännöi) |
| **reqctx** | työkalu, joka laittaa "nykyisen kirjautuneen käyttäjän" (Actor) pyyntökontekstiin |

### 4.2 Todennus ja käyttöoikeudet
| Paketti | Vastuu |
|---|---|
| **auth** | JWT:n myöntäminen/tarkistus (itse tunniste, tilaton) |
| **authsvc** | tunnus/salasana-kirjautumisen / uusimisen / uloskirjautumisen liiketoimintaorkestrointi (tietokannan haku, salasanan tarkistus, lukko, token-version linkitys) |
| **webdavauth** | WebDAV:n Basic-todennuskanava |
| **credentials** | salasanan tiiviste (bcrypt-tyyppinen) ja vahvuusstrategia |

### 4.3 Liiketoimintapalvelutaso (jokainen vastaa yhtä liiketoimintaluokkaa, kaikkein syvällisintä luettavaa)
| Paketti | Vastuu |
|---|---|
| **usersvc** | taustan käyttäjähallinta (tilin luonti/käyttöönotto-poisto/rooli) |
| **orgsvc** | organisaatiorakenne (osastopuu) |
| **spacesvc** | tilat (henkilökohtainen tila/tiimien tila) ja jäsenten yhteistyö |
| **filesvc** | tiedostometatiedot (luettelo/uudelleennimeäminen/siirto/poisto/hakemiston luonti) |
| **uploadsvc** | lataustehtävän luonti, tiketin tarkistus, TUS-datapinta |
| **fastupload** | pikälatauksen "haltuunottotodiste" -haaste |
| **finalize** | **lopullistaminen** —— kaikkien latausten lopullinen kirjoitussisääntulo, koko verkon kriittisin kirjoituspolku |
| **sharesvc** | jakolinkki (järjestelmän ainoa kirjautumaton ulostulo) |
| **dirops** | hakemistotason asynkroninen tehtäväjono (erittäin suurten hakemistojen operaatiot taustalla) |
| **quotareconcile** | kiintiöiden täsmäytys ja ajautumishälytykset |
| **patrol** | objektitarkastus: skannaa levyä vuotojen/orphan-tiedostojen löytämiseksi |

### 4.4 Toimialamallit ja synkronointi
| Paketti | Vastuu |
|---|---|
| **model** | tietokantatauluihin tiukasti vastaavat entiteetit (struct) |
| **namepolicy** | tiedostonimen ainoa tuomari (tarkistaa laillisuuden, pituuden, syvyyden) |
| **objlock** | objektitason lukko (samaa tiedostoa kirjoitettaessa mutex-kuri, estää rinnakkaisen päällekirjoituksen) |
| **syncfeed** | muutosvirran (kuka muutti mitä) luku/kirjoitus |
| **syncsse** | SSE-tapahtumakanava (push asiakkaille) |

### 4.5 Infrastruktuuri (useimmiten riittää tietää, että se on olemassa)
| Paketti | Vastuu |
|---|---|
| **config** | kokoonpanon lataus (yaml + ympäristömuuttujat) |
| **db** | PostgreSQL-yhteyspooli ja transaktio |
| **cache** | Redis-kääre ja avainten nimeämiskäytäntö |
| **ratelimit** | Redis-pohjainen nopeusrajoitus |
| **repo** | SQL-varastotasoo —— **kaikki SQL on täällä**, "dataa katsotaan" täältä |
| **storage** | paikallisen levyn objektitallennus (sisältöosoitteinen, tiedostot tallennetaan tiivisteen mukaan) |
| **migrate** | tietokannan version migraatio (goose) |
| **obs** | strukturoitu loki (slog) |
| **lifecycle** | objektin elinkaaritila-automaatti |
| **condreq** | HTTP-ehdolliset pyynnöt (If-Match/If-None-Match, optimistinen lukko) |
| **webdavfs** | WebDAV-tuen alimman tason tiedostonäkymä |

> **Lukemisen pikahaun**: jos haluat muuttaa "jotain HTTP-rajapintaa" → etsi `internal/api/api.go`:n reititysrivi + vastaava `handlers_*.go`;
> jos haluat tarkistaa "miten tietokantaa luetaan/kirjoitetaan" → etsi `internal/repo/`;
> jos etsit "latauksen lopullista levylle kirjoitusta" → katso `internal/finalize/` (ainoa kirjoituspolku).

---

## 5. Käynnistyskokoonpano —— miten palvelu "kootaan" yhteen

Kaikki alkaa `cmd/netdisk/main.go`:sta. Kommenttien numeroidun vaiheen mukaan kokoonpanojärjestys on:

1. **Kokoonpano**: oletusarvot ← yaml ← ympäristömuuttujat (jälkimmäinen korvaa aiemman)
2. **Tarkistus**: tulostaa kaikki ongelmat kerralla, jos ongelma niin käynnistyminen estetään (fail-fast)
3. **PostgreSQL**: yhdistä tietokantaan, aja migraatio
4. **Redis**: jos ei saada yhteyttä, käynnistyminen estetään (todennus/nopeusrajoitus riippuvat siitä)
5. **Tunnisteiden hallinta**: alusta JWT-myöntäminen, Redis, nopeusrajoitin, erilaiset liiketoimintapalvelut
6. **HTTP-palvelu**: ripusta `internal/api`:n kokoama reititys porttiin, käynnistä kuuntelu

Tämä tiedosto on "riippuvuusinjektion" paikka —— **missä on new:llä luotu mitkä palvelut ja välitetty mitkä riippuvuudet, kaikki on tässä yhdessä tiedostossa**. Jos haluat ymmärtää "miten jokin palvelu on koottu", lue se; jos haluat lisätä järjestelmään riippuvuuden, muokkaa myös sitä.

---

## 6. Miten yksi pyyntö etenee (virtauksen ymmärtäminen on tämän repositorion ymmärtämistä)

Otetaan esimerkiksi "käyttäjä kirjautuu ja pyytää tiedostoluetteloa", ketju on:

```
        ┌────────────────────────────────────────────┐
        │  Nginx：HTTPS 终结、反代、来源 IP 透传       │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  middleware 中间件链（internal/middleware） │
        │  requestID → realIP → structuredErrors →   │
        │  logging → recoverer（崩溃兜底）            │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  api 路由（internal/api/api.go）            │
        │  鉴权(auth) → 限速(ratelimit) → handler    │
        └────────────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  handlers_*.go  解析参数、调用服务          │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  业务服务层（filesvc/uploadsvc/...）         │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  repo 层：「唯一 SQL 的地方」→ PostgreSQL    │
        │  storage 层：读写磁盘对象                    │
        └────────────────────────────────────────────┘
```

**Laukalla muista tämä lause**: `api`-taso hallitsee "ulospäin näkyvää muotoa", `repo`-taso hallitsee "tietokannan käsittelyä", välimmäinen palvelutaso hallitsee "liiketoimintasääntöjä". Mitä selkeämpi tasoero, sitä enemmän yhden paikan muuttaminen vaikuttaa vain siihen yhteen paikkaan.

---

## 7. Miten kokoonpanoa hallitaan

Katso `deploy/config/config.example.yaml` (kokoonpanoesimerkki, auktoritatiivisin kenttäluettelo) sekä `internal/config/config.go` (latauslogiikka).

- Kokoonpanolähteen prioriteetti: **koodin oletusarvo < yaml-tiedosto < ympäristömuuttuja**.
- **Arkaluonteiset kohdat (salasanat, avaimet) kulkevat vain ympäristömuuttujien kautta, eivät koskaan yaml:iin**. Esimerkiksi `NETDISK_DB_PASSWORD`, `JWT_SECRET`, `NETDISK_REDIS_PASSWORD`.
- Kokoonpanon suuret lohkot sisältävät: `server` (portti/välitys), `database`, `redis`, `jwt` (tunnisteen voimassaolo), `policy` (kiintiö/syvyys/kokoraja), `patrol` (tarkastus), `webui` (taustan etuliite), `storage` (tallennuksen juurihakemisto), `log`, `rate_limits` (nopeusrajoitus).
- Käynnistyksessä tarkistetaan; jos jokin nopeusrajoitus on kokonaan 0, tallennushakemisto ei ole kirjoitettavissa jne., se estetään.

> Ylläpidon vihje: muuta kokoonpanoa → muokkaa yaml tai ympäristömuuttujaa → käynnistä `netdisk` uudelleen; **frontendin muuttaminen vaatii Go-binäärin uudelleenkäännöksen** (koska sivu on embed-atty sisään), pelkkä prosessin uudelleenkäynnistys ei riitä.

---

## 8. Tietokanta —— ydin taulujen pikahaku

Migraatio on `internal/migrate/sql/`:ssa (13 versioitua skriptiä, upotettu binääriin). Ydintoimintataulut:

| Taulu | Mitä tallentaa |
|---|---|
| `users` | käyttäjätilit |
| `spaces` | henkilökohtainen/tiimien tila |
| `space_members` | tilan jäsensuhde |
| `groups` / `group_members` | tiimi ja jäsenet |
| `departments` / `department_closure` / `user_departments` | organisaatiorakenne (osastopuu + sulkeumatyyppinen taulu) |
| `files` | tiedosto/hakemistometatiedot |
| `file_objects` | tiedoston todellinen objekti (sisältöosoitteinen) |
| `uploads` | lataustehtävä (TUS-istunto) |
| `shares` | jakolinkki |
| `file_locks` | tiedoston muokkauslukko |
| `refresh_tokens` | pysyvä kirjautumistila (refresh token) |
| `sync_feed` / `sync_cursors` | synkronoinnin muutosvirta ja kohdistimet |
| `dir_op_tasks` | hakemistotason asynkroninen tehtävä |

> Huomio: `audit_logs`, `idp_providers`, `idp_sync_state`, `user_idp_bindings`, `user_sso_bindings` nämä taulut ovat aikaisten ominaisuuksien (auditointi, identiteettilähteen liittäminen) jättämiä **historiallisia tauluja**, joiden koodi ei tällä hetkellä enää lue/kirjoita niitä; migraatiohistoriaa ei saa muuttaa mielivaltaisesti, normaalisti säilytetään.

---

## 9. HTTP-rajapintojen yleiskatsaus

Kaikki reitit ovat `internal/api/api.go`:ssa. Ryhmitelty toiminnon mukaan (todennustiedot jätetty pois):

| Ryhmä | Esimerkkipolku | Kuvaus |
|---|---|---|
| Terveys / versio | `GET /healthz`, `GET /api/v1/version` | elävyystarkistus, versioneuvottelu (ei vaadi kirjautumista) |
| Todennus | `POST /api/v1/auth/login` / `refresh` / `logout` | tunnus/salasana-kirjautuminen ja tunnisteen uusiminen |
| Omat tiedot | `GET /api/v1/me` | nykyinen kirjautunut käyttäjä |
| Tiedosto | `GET /api/v1/files`, `GET /api/v1/files/{id}/content` | luettelo, lataus alas (tukee Range) |
| Tiedoston kirjoitusoperaatiot | `PATCH /api/v1/files/{id}`, `DELETE /api/v1/files/{id}`, `POST /api/v1/files/dirs` | uudelleennimeäminen, poistaminen, hakemiston luonti |
| Lataus | `POST /api/v1/upload/create`, `/tus/*` | lataustehtävän luonti + TUS-sirpaleiden lataus |
| Pikälataus | `POST /api/v1/upload/{id}/finish` | haltuunottotodisteen läpi mentyä pikälatauksen lopullistaminen |
| Muokkauslukko | `POST/GET/DELETE /api/v1/files/{id}/lock` | tiedoston yhteismuokkauslukko |
| Jakaminen | `POST /api/v1/shares`, `GET /api/v1/shares/{token}/meta` | jakolinkki (kirjautumaton lataus alas) |
| Tila | `GET /api/v1/spaces`, `POST /api/v1/spaces` | oma tila, tilan luonti |
| Organisaatio/käyttäjähallinta | `/api/v1/admin/departments*`, `/api/v1/admin/users*` | taustahallinta (vaatii ylläpitäjän) |
| Synkronointi | `GET /api/v1/events` (SSE), `GET /api/v1/changes` | reaaliaikainen push + inkrementaalinen haku |
| WebDAV | `/webdav/*` (PROPFIND/PUT/COPY/MOVE/LOCK jne.) | työpöytäliitoslevy |
| Tehtävä | `GET /api/v1/tasks/{id}` | hakemistotason asynkronisen tehtävän edistymisen kysely |

> Jos haluat "löytää missä tiedostossa jokin rajapinta on toteutettu": `api.go` kirjoittaa `mux.Handle("METHOD /path", ...d.handleXxx...)`, `handleXxx` on toteutusfunktio, yleensä saman hakemiston `handlers_*.go`:ssa.

---

## 10. Muutama keskeinen suunnittelu, joista on hyvä olla käsitys ennen koodin lukemista

Jotta ei-kokeneempi lukija ei hukkuisi, luodaan ensin nämä neljä mielikuvamallia:

1. **JWT päätepisteittäin**: tunnisteet jakautuvat `web` (tausta) / `desktop` (työpöytä) päätepisteisiin, **taustatunniste ei voi käsitellä työpöytäkykyjä**, ja päinvastoin. Tunnisteeseen ei kirjoiteta oikeuksia, oikeudet päätetään aina palvelinpuolella.
2. **TUS jatkuva lataus + yksi kirjoituspolku**: kaikki lataukset (TUS-sirpaleet, WebDAV PUT, pikälataus) päätyvät lopulta tähän yhteen `finalize` "lopullistamis"-sisääntuloon, mikä takaa, että tiedoston kirjoituksella on vain yksi polku eikä ne kilpaile keskenään.
3. **Synkronoinnin kaksoiskanava**: palvelin pushaa SSE:llä "on muutos"; asiakas hakee sitten `/changes`-kohdistimella **todelliset muutokset**. Vaikka push katoaisi, siitä ei ole haittaa, haku on auktoritatiivinen. Kohdistin käyttää globaalia automaattisesti kasvavaa numeroa linjaukseen.
4. **Sisältöosoitteinen tallennus**: `storage` tallentaa objektit tiedoston sisällön tiivisteen mukaan, sama sisältö tallennetaan vain kerran (deduplikaatio), tiedostonimi on vain metatieto.

---

## 11. Kolmannen osapuolen riippuvuudet (go.mod) ovat hyvin tiiviit

`slimming`:n jälkeen riippuvuudet ovat vähentyneet merkittävästi, tällä hetkellä ydinriippuvuudet ovat vain:

- **pgx** (PostgreSQL-ajuri), **goose** (migraatio)
- **go-redis** (Redis-asiakas)
- **golang-jwt/v5** (JWT)
- **miniredis** (vain testikäyttöön), **yaml.v3** (kokoonpano)

Ei viestijonoa, ei raskasta kehystä, ei usean taustajärjestelmän tallennus-SDK:ta (paikallinen levy on ainoa taustajärjestelmä). Tämä on erittäin ystävällistä vianetsinnän kannalta: pinossa ei ole liian monta "lukematonta riippuvuutta".

---

## 12. Käännös / suoritus / käyttöönotto - muistilista

```bash
# Paikallinen käännös (vaatii Go 1.26+)
go build ./...            # käännä kaikki (tarkista onko virheitä)
go vet ./...              # staattinen tarkistus
go run ./cmd/depsguard    # arkkitehtuuriportti (myös CI ajaa)

# Todellinen käyttöönottotuote (deploy/build-release.sh tekee tämän)
# Järjestys on tärkeä: ensin käännä frontend (web) → sitten go build yksi binääri → paketoi
```

- Yksi binääri on `systemd`:n hallinnassa, Nginx toimii käänteisenä välityspalvelimena (`deploy/nginx/`).
- Yksityiskohtainen käyttöönotto on `deploy/README.md`:ssa.
- Katso suoraan pääohjelman sisääntulon kirjoituslogiikkaa: `cmd/netdisk/main.go`.

---

## 13. Mistä aloittaa lukeminen (ensimmäistä kertaa aloittavalle)

1. `cmd/netdisk/main.go` —— katso kokoonpanojärjestys (ymmärrät kokonaisuuden 5 minuutissa)
2. `internal/api/api.go` —— katso kaikki reitit (ymmärrät mitä rajapintoja järjestelmällä on 5 minuutissa)
3. `deploy/config/config.example.yaml` —— katso mitä kokoonpanon nupit ovat
4. Valitse jokin sinua eniten kiinnostava liiketoimi: tiedoston lataus niin seuraa `finalize`, organisaatiorakenne niin seuraa `orgsvc`, synkronointi niin seuraa `syncfeed`/`syncsse`
5. Selvitä todellinen ongelma: `obs`-lokista / jonkin rajapinnan virheestä → takaisin `handlers_*.go` → `repo/*.go` katso SQL

> Kultainen sääntö: **kun mietit "mitä tämä tekee", lue sen pakettitiedoston yläosan kommentti** —— tässä repositoriossa jokaisessa paketissa/funktiossa on runsaasti kiinankielisiä suunnittelukommentteja, se on "elävä dokumentti".
