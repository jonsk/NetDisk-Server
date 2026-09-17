# Server-com arkkitehtuurisuunnittelun asiakirja

> Versio: seuraa Server-com-yhteisöversiota
> Lukijakunta: ylläpitäjät / kokemattomat IT-ammattilaiset (moduulikohtaisen lukujärjestyksen osalta ks. 《代码阅读指南》)
> Liittyvä: `05-INSTALL.md`, `06-OPS.md`, `07-BACKUP.md`, `08-TEST.md`, `04-API_GUIDE.md`, `03-CODE_READING_GUIDE.md`

---

## 1. Yleiskatsaus ja asema

`Server-com` on **yritystason verkkolevyn palvelinpuoli**, joka on kirjoitettu kielellä **Go** ja joka lopulta käännetään **yhdeksi suoritettavaksi tiedostoksi** (yksi binääri).
Se tarjoaa tiimien jaettuja tiloja, moniprotokollaisen latauksen (TUS / WebDAV / multipart), versioidun lopullistamisen, reaaliaikaisen synkronoinnin havainnoinnin, hienorakeisen oikeudenhallinnan ja kiintiöiden hallinnan, ja se voidaan ottaa käyttöön joko yksityisenä verkkolevynä tai yrityksen tiedon keskuksena.

Yhteisöversio (Server-com) säilyttää vain **salasanalogon + hallintapaneelin**, ja siitä on poistettu WeCom-/DingTalk-/IdP-kolmannen osapuolen kirjautumiset ja H5-mobiilikanava; se on kaupallisen version (Server-Ent) toiminnallinen osajoukko.

**Ydinajatus: koko palvelu on yksi "iso laatikko", ja sen ulkopuolella on neljä komponenttia, jotka tukevat sitä.**

## 2. Kokonaisarkkitehtuuri

### 2.1 Käyttöönoton kokoonpano (neljä osaa)

| Komponentti | Tehtävä | Huomautukset |
|---|---|---|
| **netdisk** (tämän arkiston tuote) | vastaanottaa HTTP-pyyntöjä, käsitellä liiketoimintaa, lukea/kirjoittaa metatietoja ja objekteja | sama binääri sisältää REST / WebDAV / TUS / SSE / upotetun hallintapaneelin |
| **PostgreSQL 17 (vähimmäistuki)** | ainoa metatietokone (kaikki "hakemistorivit / käyttäjät / tilat / kiintiöt" ovat tässä) | sisältää WAL-arkistoinnin ajankohtaista palautusta varten. **Vähintään 17**: perusavaimen oletusarvo käyttää `gen_random_uuid()` (sisäänrakennettu PG 13:sta), joten alaraja ei määräydy UUID:stä; vähintään 17 (tasattuna valtavirran jakeluversioihin). Versiovaatimukset ks. `05-INSTALL.md` §1 |
| **Redis** | istunnot (token) / nopeusrajoitusikkunat / muutosvirran kohdistimet / tehtäväjono | **ei ole välimuisti**; jos tokenit katoavat, kaikki kirjautuvat ulos |
| **Nginx** | käänteinen välityspalvelin, yksi portti ulospäin | tarvitaan vain tuotannossa; kehityksessä sovellukseen voi käyttää suoraan |

```
                       ┌──────────────────────────────────────┐
  Web 管理后台 /admin ─▶│                                      │
  WebDAV 客户端  ─WebDAV▶│        netdisk (Go 单二进制)          │
  TUS 上传器      ─TUS─▶│   REST · TUS · WebDAV · SSE 统一入口  │
  桌面同步客户端  ─SSE──▶│                                      │
                       │   ┌──────────────────────────────┐   │
                       │   │ finalize 唯一写路径 + 对象锁   │   │
                       │   │ 内容寻址 → 本地磁盘对象存储     │   │
                       │   └──────────────────────────────┘   │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(反代) │
               │  元数据   │  │会话/限速│   │  单端口    │
               └──────────┘  └────────┘   └────────────┘
```

### 2.2 Miksi yksi binääri + upotettu käyttöliittymä

Hallintapaneelin käyttöliittymän frontendi rakentaa web-arkisto, minkä jälkeen tuote **upotetaan käännösaikana** Go-binääriin (`internal/webui/dist/`), ja Go tarjoaa sen suoraan `embed`-ominaisuudella (`/admin`). Hyöty: riittää, että jaetaan yksi tiedosto, eikä palvelimelle tarvitse erikseen ottaa käyttöön frontendiin staattista sivustoa.

> ⚠️ Siksi **jos frontendiä muutetaan, Go-binääri on käännettävä uudelleen**: ensin `pnpm build`, sitten `go build`.
> Jos järjestys on päinvastainen, vanha tuote upotetaan (kääntäminen onnistuu, mutta sivu on vanha eikä virhettä ilmene).

## 3. Kerroksittainen arkkitehtuuri (internal-pakkausten jako)

Koodi jaetaan viiteen kerrokseen vastuualueen mukaan, ja ylempi kerros saa riippua vain alemmasta (mekaanisesti tarkistettu `depsguard`-työkalulla):

```
┌─────────────────────────────────────────────────────────────┐
│ 4. 入口层   cmd/  (netdisk / migrate / passwd / depsguard …)  │
├─────────────────────────────────────────────────────────────┤
│ 3. 接口层   internal/api/  (HTTP handler、路由、中间件)          │
│            internal/webdavfs/ webdavauth/                     │
├─────────────────────────────────────────────────────────────┤
│ 2. 业务层   usersvc/orgsvc/spacesvc/filesvc/sharesvc/ │
│            uploadsvc/finalize/lifecycle/syncfeed/syncsse/    │
│            patrol/quotareconcile/consistency/credentials/    │
├─────────────────────────────────────────────────────────────┤
│ 1. 领域/数据  repo/(数据访问) model/(领域模型) objlock/ storage/  │
│            authsvc/ cache/ auth/ apierr/reqctx/middleware/    │
├─────────────────────────────────────────────────────────────┤
│ 0. 底座      config/ db/ migrate/ ratelimit/ namepolicy/      │
│            dirops/ fastupload/ condreq/ webui/ obs/ syncssem  │
└─────────────────────────────────────────────────────────────┘
```

**Kuri (punaiset viivat R-xx:n ydin)**: ytimpakkausten kuten `objlock` (lukitus), `storage` (objektivarasto), `model` (toimialueen malli) **ei missään tapauksessa saa riippua `internal/api`-paketista (HTTP-kerros)**. Liiketoimintalogiikan on oltava irrotettu HTTP:stä, jotta liiketoimintäsäännöt eivät muutu, vaikka käyttötapa vaihtuu (WebDAV / TUS / sisäinen kutsu).

## 4. Ydinsuunnittelun mekanismit

### 4.1 finalize —— ainoa kirjoituspolku

Kaikki lataukset (TUS-osiointi, WebDAV PUT, multipart-suora lataus) **ohjautuvat lopulta samaan lopullistamislogiikkaan** `finalizeUpload()`:
1. Ensin tarkistetaan pikakopiointi (jos sisältöhajautus on jo olemassa, käytetään sitä suoraan estääkseen "myrkytyksen");
2. Sisältö kirjoitetaan objektialueelle ja haetaan **sisältöosoitteinen** objekti;
3. Lyhyessä transaktiossa kirjoitetaan hakemistorivi ja viittauksen laskuri;
4. Kirjoitetaan muutosvirta (sync_feed) laukaisten synkronoinnin havainnointi.

Hyöty: kolmen latauskanavan käyttäytyminen on täsmälleen sama, mikä poistaa aukon "jokin kanava kiertää jonkin säännön".

### 4.2 Sisältöosoitteinen objektivarasto

Objektit tallennetaan **sisältöhajautuksen** mukaan, polku on muotoa `objects/xx/xx/<sha256>`:
- **Sama sisältö tallennetaan vain kerran** (luonnollinen päällekkäisyyden poisto): kun kaksi käyttäjää lataa saman tiedoston, levyllä on vain yksi kopio;
- Tiedostonimi ja sisältö on erotettu toisistaan: nimen muuttaminen ei kopioi dataa, vaan muuttaa metatietoja;
- Muuttumaton objekti: lopullistamisen jälkeistä sisältöä ei enää ylikirjoiteta, mikä estää "hiljaisen päällekirjoituksen / tiedoston valumisen".

### 4.3 Objektitason kaksikerroksinen lukko (ADR-2)

Estääkseen useiden pyyntöjen yhtäaikaisen lopullistamisen/poistamisen samalle objektille aiheuttaman tietojen sekaannuksen, otettiin käyttöön objektitason lukko:

| Taso | Käyttötarkoitus | Lukon hallintatapa |
|---|---|---|
| **Istuntotason lukko** | koko lopullistaminen, elinkaarityöntekijä (todellinen kirjoituspolku) | yksi omistettu tietokantayhteys läpi koko ajan, lukolla on yläraja (katto), joka estää kriittisen alueen ylimitoituksen |
| **Transaktioperustainen lukko** | pelkkä viittaus +1 (kopiointi, jakaminen tilaan) | pidetään vain yhden lyhyen transaktion ajan |

Lukon avain määräytyy objektin hajautuksen perusteella, ja lukitusjärjestys on hajautuksen nousevassa järjestyksessä, mikä estää livähtämisen (deadlock).

### 4.4 Kaksitilainen synkronointi (SSE + kohdistin)

Työpöytäasiakasohjelman tarvitsee "kun etäpää muuttuu, minä tiedän heti":
- **SSE-push** (`/sync/events`): lähettää muutokset reaaliajassa online-asiakkaille (nopea, mutta voi pudottaa ruutuja);
- **Kohdistimen haku** (`/changes`): asiakas hakee inkrementaalisesti kohdistimella (`since`) (luotettava, vararahasto).

Molemmat jakavat **saman globaalin `change_seq`:n** (yksi `sync_feed`-taulu, globaalisti kasvava järjestysnumero), ja asiakas voi käyttää niitä vuorotellen ja täydentää hakea `/changes`-pohjalta. Tämä välttää jaksonittaisen täyden skannauksen ja estää muutosten katoamisen.

### 4.5 JWT-kirjautuminen päittäin (kahtia jaettu)

Kirjautumisen onnistuessa myönnetään JWT (HS256), jaettuna kahteen päähän:
- **web-pää** (hallintapaneeli) ja **desktop-pää** (työpöytäasiakasohjelma) tokeneilla on erilliset (R-14), jotta vuoto yhdessä paikassa ei vaikuta koko järjestelmään;
- Käyttö- ja päivitystokenit tallennetaan Redisissä, ja ne tukevat mitätöintiä ja "yksittäistä päivityslentoa" (vain yksi päivityspyyntö onnistuu samanaikaisesti).

Yhteisöversio säilyttää vain **tili/salasanalogon** (`/api/v1/auth/login`); kolmannen osapuolen OAuth (WeCom/DingTalk/IdP) on poistettu.

### 4.6 Hakemiston semantiikka ja nimeämisrajoitukset

- Hakemiston syvyys ≤ 31 tasoa ja yksittäisen kertyneen polun pituus ≤ 240 tavua (kaksi ylärajaa, määritettävissä);
- Tiedostonimen kirjainkoolla ei ole merkitystä kaksoiskappaleiden tunnistamisessa (`lower(name)` + yksilöllinen indeksi);
- Hakemistason "alipuun siirto/poisto", joka ylittää kynnysarvon (oletus 1000 riviä), muunnetaan **asynkroniseksi tehtäväksi**, joka palauttaa `task_id`:n asiakkaan kyselyä varten.

## 5. Tietomalli (ydintaulut)

| Taulu | Merkitys |
|---|---|
| `users` | käyttäjä (tili/salasana-hajautus, sähköposti, näyttönimi, rooli) |
| `organizations` / `departments` / `groups` | organisaatio / osasto / ryhmä |
| `spaces` | tila (henkilökohtainen tila + tiimitila; sisältää kiintiön `used_bytes`, `last_seq`) |
| `space_members` | tilan jäsenet ja oikeudet |
| `files` | hakemistorivi (tiedostot ja hakemistot; `is_dir`, `parent_id`, `name`, `depth`, `size`) |
| `file_objects` | sisältöosoitteinen objekti (hajautus, neljä tilaa: live/pending_delete…, viitelaskuri `ref_count`) |
| `sync_feed` | muutosvirta (globaali `change_seq`, 90 päivän säilytys) |
| `shares` | jakolinkki (token, oikeudet, vanheneminen) |
| `audit_logs` | auditointiloki (tällä hetkellä varattu paikka, ei pysyväistetä yhteisöversiossa) |

## 6. Kokoonpanosuunnittelu

- **Oletusarvoja on vain yksi kappale**: ne ovat Go-rakenteessa `internal/config.Default()`; kokoonpano-osan poistaminen ei muutu nolla-arvoksi, vaan palaa oletukseen;
- **salauskäytännöt eivät koskaan mene yaml-tiedostoon**: `JWT_SECRET`, tietokannan salasana ja Redis-salasana kulkevat vain **ympäristömuuttujien** kautta (`NETDISK_*`);
- Prioriteetti: Go-oletus < kokoonpanotiedosto < ympäristömuuttuja;
- Käynnistyksen itsetarkistus (`netdisk -config ... -check`): kaikki virheelliset kohdat listataan **kerralla** ja poistutaan nollasta poikkeavalla koodilla sen sijaan, että odotettaisiin ensimmäistä pyyntöä ja palautettaisiin 500;
- Kestot käyttävät luettavaa kirjoitusmuotoa (`5m`/`30s`/`24h`).

## 7. Avainprosessit

### 7.1 Yhden latauksen (TUS) elinkaari

```
客户端 ─PATCH 分片→ 暂存区(tus-tmp) ─全部传完→ finalizeUpload()
    finalize: 计算哈希 → 秒传去重检查 → 写对象区(objects/) → 短事务写 files+file_objects
             → 写 sync_feed(change_seq++) → 推 SSE → 返回 X-File-Id 完成
```

### 7.2 Yhden pyynnön käsittelyketju

```
Nginx ─→ 中间件链(日志/恢复/限速/认证) ─→ 路由(ServeMux) ─→ handler ─→ 业务 service ─→ repo(DBA)
```

## 8. Rajapintojen tuloväylien yleiskatsaus

| Tuloväylä | Selitys |
|---|---|
| `REST /api/v1/*` | todennus, käyttäjät, osastot, tilat, tiedostot, hakemistot, lataus, lataaminen, jako, muutostapahtumat |
| `Lataus & Range` | stream-lataus, tavutason Range / ehdolliset pyynnöt (If-Match jne.) |
| `TUS /uploads/*` | osioitu jatkuvuuslataus, ticketin uudelleenkäyttö, väliaikaisten tiedostojen kierrätys, levyn vesitaso (>90% → 507) |
| `WebDAV /dav/*` | standardi WebDAV + PUT kulkee lopullistamisen kautta (≤100MB) |
| `SSE /sync/events` | reaaliaikainen muutospush |
| `kohdistin /changes` | kohdistimen inkrementaalinen haku |
| `GET /metrics` | Prometheus-valvontamittarit (oletuksena vain loopback / luotettava välityspalvelin) |
| `GET /healthz` | terveydentarkistus |
| `/admin/*` | upotettu hallintapaneeli (Go embed) |

## 9. Arkkitehtuurin punaiset viivat (valikoima)

- Ytimpakkausten ei tule riippua HTTP/API-kerroksesta (R-01-perhe);
- Globaali lukitusjärjestys on kiinteä: spaces-rivi → files-rivi → file_objects (advisory + rivilukko, hajautus nousevassa järjestyksessä);
- OAuth / salaisuudet menevät poikkeuksetta ympäristömuuttujiin eivätkä päädy versionhallintaan;
- Tarkastus (patrol) on **vain luettava**: se ei koskaan korjaa automaattisesti, jotta virheellinen tulkinta ei paisu tietojen vioittumiseksi;
- Kiintiöiden täsmäytys, objektien tarkastus ja muutosvirran siivous ovat kaikki taustalla ohjattuja tehtäviä, eikä niitä laajenneta rajoittamattomasti.

## 10. Teknologiapino

| Kerros | Valinta |
|---|---|
| Kieli | Go 1.26 |
| HTTP | vakiokirjasto `net/http` + `ServeMux` middleware-ketju |
| DB | pgx v5 + sqlc + goose-migraatio (vain PostgreSQL) |
| Todennus | golang-jwt v5 (HS256) |
| Tallennus | paikallinen levyn objektivarasto (sisältöosoitteinen, `storage`-paketti laajennettavissa useisiin taustoihin) |
| Kokoonpano | yaml.v3 + ympäristömuuttujien tiukka tarkistus |
| Käyttöönotto | systemd + Nginx + Prometheus (yhden portin neljä osaa, ilman kontteja) |

---

*Käyttöönoton ja ylläpidon osalta ks. `05-INSTALL.md` / `06-OPS.md` / `07-BACKUP.md`; rajapintojen yksityiskohdat ks. `04-API_GUIDE.md`.*
