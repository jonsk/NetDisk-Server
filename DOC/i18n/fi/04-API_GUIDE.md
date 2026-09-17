# Server-com API-opas

> Lukijakunta: ylläpitäjät / kokemattomat IT-ammattilaiset (integraattorit / virheenetsintä)
> Määrittelytiedosto: auktoritatiivinen sopimus on `DOC/api/openapi.yaml` (OpenAPI v3); tämä asiakirja on sen **ihmisen luettava johdanto**.
> Katso myös: `02-ARCHITECTURE.md`, `05-INSTALL.md`, `06-OPS.md`, `07-BACKUP.md`

---

## 1. Todennusmalli

Palvelu käyttää todennukseen **JWT (HS256)**; kirjautumisen jälkeen saat `access_token`, jonka lähetät mukana jokaisessa pyynnössä:

```
Authorization: Bearer <access_token>
```

**Tokenit jaettu päihin (R-14)**: web-pään (hallintapaneeli) ja desktop-pään (työpöytäasiakasohjelma) tokenit ovat toisistaan riippumattomia, jotta vuoto yhdessä paikassa ei vaikuta koko järjestelmään. Tokenit / päivitystokenit tallennetaan Redisissä ja tukevat mitätöintiä ja "yksittäistä päivityslentoa".

| Rajapinta | Selitys |
|---|---|
| `POST /api/v1/auth/login` | tili/salasanalogin, palauttaa `access_token` + `refresh_token` |
| `POST /api/v1/auth/refresh` | vaihtaa päivitystokenilla uuden access-tokenin |
| `POST /api/v1/auth/logout` | kirjautuminen ulos, tokenin mitätöinti |
| `GET /api/v1/me` | nykyisen käyttäjän tiedot |
| `GET /api/v1/version` | palvelimen versio |

> Yhteisöversio säilyttää vain **salasanalogon**; WeCom/DingTalk/kolmannen osapuolen IdP-kirjautuminen on poistettu yhteisöversiosta.
> Jos ikkuna ylitetään, palautetaan **429**, ja asiakkaan tulee perääntyä ja yrittää uudelleen (ks. §7).

---

## 2. Yleiset sopimukset

- **Base URL**: `/api/v1`, tuotannossa Nginxin yhden portin kautta ulospäin.
- **Pyyntö/vastaus**: JSON (`Content-Type: application/json`).
- **Sivutus**: listarajapinnat käyttävät `limit` + `after` (keyset-kohdistin) sivutukseen; `limit` yläraja on **999** (ylitys palautetaan oletukseen 200).
- **Lataaminen**: stream-vastaus + tavutason `Range` (200/206/416) + ehdolliset pyyntöotsakkeet (`If-Match`/`If-None-Match`/`If-Range`).
- **Nopeusrajoitus**: jaettu rajapintaperheisiin (login/upload/file_list/file_read/file_write/webdav), kaksi ikkunaa (sekunti/minuutti) ovat voimassa yhtä aikaa ja tiukempi niistä pätee.

---

## 3. REST-rajapintaryhmät

### 3.1 Tiedostot ja hakemistot

| Metodi ja polku | Selitys |
|---|---|
| `GET /api/v1/files?space=<id>&parent_id=&limit=&after=` | hakemiston luettelo (oletus 200 riviä/sivu; tukee keyset-sivutusta) |
| `POST /api/v1/files` | luo tiedosto/hakemistorivi |
| `GET /api/v1/files/dirs` | hakemiston luettelo (vain hakemistot) |
| `GET /api/v1/files/{id}` | tiedoston/hakemiston tiedot |
| `GET /api/v1/files/{id}/content` | lataa sisältö (tukee Rangea) |
| `POST /api/v1/files/{id}/move` | siirrä / nimeä uudelleen |
| `POST /api/v1/files/{id}/copy` | kopioi |
| `POST /api/v1/files/{id}/share-to-space` | kopioi / jaa toiseen tilaan (käyttää objektin viite +1 -polkua) |
| `POST /api/v1/files/{id}/lock` | lukitse / avaa lukko (objektitason lukko) |
| `GET /api/v1/files/{id}/subtree-stats` | alipuun tilastot |

> Hakemistason "alipuun siirto/poisto", joka ylittää kynnysarvon (oletus 1000 riviä), muunnetaan **asynkroniseksi tehtäväksi**: `POST` palauttaa `task_id`:n, ja asiakas kysyy edistymistä `GET /api/v1/tasks/{id}`.

### 3.2 Lataus (multipart-suora lataus)

| Metodi ja polku | Selitys |
|---|---|
| `POST /api/v1/upload/create` | luo lataus (varaa kiintiön, palauttaa lataustokenin) |
| `POST /api/v1/upload/simple` | multipart-suora lataus (pienet tiedostot; yksi kirjoitus lopullistaa) |
| `POST /api/v1/upload/{id}/finish` | valmista / lopullista |
| `GET/PATCH /api/v1/upload/{id}` | kysely / jatka latausta |

> Suuret tiedostot kulkevat **TUS**-protokollan kautta (ks. §4).

### 3.3 Muutokset ja synkronointi

| Metodi ja polku | Selitys |
|---|---|
| `GET /api/v1/changes?since=<cursor>` | kohdistimen inkrementaalinen haku (kaksitilaisen "luotettava" kanava) |
| `GET /api/v1/changes/head` | hae nykyinen uusin kohdistin |
| `GET /api/v1/sync/cursors` | hallitse synkronointikohdistimia |
| `GET /sync/events` | SSE-reaaliaikapush (kaksitilaisen "reaaliaikainen" kanava) |

> Molemmat jakavat saman globaalin `change_seq`:n: SSE voi pudottaa ruutuja, mutta voit täydentää hakea `/changes`-kohdistimella, mikä takaa ettei mitään jää saamatta.

### 3.4 Jako

| Metodi ja polku | Selitys |
|---|---|
| `POST /api/v1/shares` | luo jakolinkki |
| `GET /api/v1/shares` | minun luomani jakolinkit |
| `DELETE /api/v1/shares/{id}` | peru jakov |
| `GET /api/v1/shares/{token}/meta` | jakamisen metatiedot (laskeutumissivua varten) |
| `GET /api/v1/shares/{token}/download` | lataa jakotokenilla |

### 3.5 Tilat

| Metodi ja polku | Selitys |
|---|---|
| `POST /api/v1/spaces` | luo tila (henkilökohtainen tila / tiimitila) |
| `GET /api/v1/spaces/{id}` | tilan tiedot (sisältää kiintiön) |
| `POST /api/v1/spaces/{id}/transfer` | siirrä tila toiselle |
| `GET /api/v1/spaces/{id}/members` | jäsenluettelo |
| `PUT/DELETE /api/v1/spaces/{id}/members/{userId}` | lisää / poista jäsen |
| `POST /api/v1/spaces/{id}/leave` | poistu tilasta |

### 3.6 Hallintapaneeli (vain super_admin)

| Metodi ja polku | Selitys |
|---|---|
| `POST/GET /api/v1/admin/departments` | osastojen hallinta |
| `PUT/DELETE /api/v1/admin/departments/{id}` | osaston muokkaus / poisto |
| `POST /api/v1/admin/spaces/{id}/freeze` | jäädytä tila (pysäytä synkronointi) |
| `GET /api/v1/audit/logs` | auditointiloki (yhteisöversiossa varattu paikka) |

> Käyttäjän / organisaation / tilan / kiintiön hallintapaneelin käyttöliittymän tarjoaa Go `embed` (`/admin/*`).
> Lisäksi: käyttäjähallintaan liittyvät rajapinnat (käyttäjän luonti, roolin muutos, kiintiön asetus) kulkevat `/api/v1/admin/*` (katso täysi openapi).

---

## 4. TUS-osioitu lataus (suuret tiedostot)

Työpöytäasiakasohjelmille / suurille tiedostoille tarkoitettu jatkuvuuslatausprotokolla (`deploy`-hakemiston `cmd/probesmoke` on referenstoteutus):

| Vaihe | Metodi | Avainotsake |
|---|---|---|
| Luo tehtävä | `POST /tus` | `Tus-Resumable: 1.0.0`, `Upload-Length`, `Upload-Metadata: filename <base64>`, `Authorization: Bearer` |
| Lähetä osio | `PATCH /tus/{id}` | `Tus-Resumable`, `Upload-Offset`, `X-Upload-Token`, `Content-Type: application/offset+octet-stream` |
| Valmista | viimeinen osio | vastaus `200` + `Upload-Complete: true` + `X-File-Id` lopullistaa |

- **Jatkuvuuslataus**: asiakas ilmoittaa `Upload-Offset`, ja palvelin jatkaa tuosta siirtymästä;
- **Ticketin uudelleenkäyttö**: latauslippua voi käyttää uudelleen ennen lopullistamista, idempotentti;
- **Levyn vesitaso**: väliaikaissäilön käyttö ≥ kynnysarvon (oletus 90 %) palauttaa **507 Insufficient Storage**;
- **Väliaikaisten tiedostojen kierrätys**: aikakatkaistut keskeneräiset väliaikaistiedostot kierrättää taustatehtävä (noin 10 minuutin välein).

---

## 5. WebDAV

Polun etuliite `/dav/*`, perustuu vakioon `x/net/webdav`, yleisimmin käytetty "verkkoaseman kartoitukseen / resurssienhallintaan":

- **PUT kulkee lopullistamispolun kautta**: ≤100 Mt lopullistetaan suoraan (ylärajan ylittyessä ohjataan TUS:iin);
- **LOCK-semantiiikka on täydennetty itse** (vakiokirjasto on vain muistipohjainen toteutus);
- Range / ehdolliset pyynnöt ovat samat kuin REST:ssä;
- Hakemistoselaus lähettää useita pyyntöjä kerralla → vastaavasti `rate_limits.webdav` on asetettu melko leveäksi (60/s).

---

## 6. Terveys / valvonta

| Polku | Selitys |
|---|---|
| `GET /healthz` | elossaolotarkistus (200) |
| `GET /metrics` | Prometheus-mittarit; oletuksena vain loopback / luotettava välityspalvelin, koneiden väliseen tarvitaan `NETDISK_METRICS_TOKEN` Bearer |
| `GET /api/v1/version` | versio |

---

## 7. Tilakoodit ja semantiikka

| Tilakoodi | Semantiikka | Selitys |
|---|---|---|
| 200 / 201 | onnistuminen | lopullistamistyyppinen onnistuminen sisältää `X-File-Id` |
| 206 / 416 | osittainen sisältö / alueen ylitys | lataamisen Range |
| 400 / 422 | parametrivirhe | validointi epäonnistui |
| 401 | todentamaton | tokeni puuttuu / vanhentunut |
| 403 / 401 | oikeudet rajoitettu | **keskeytä tilan synkronointi, älä koskaan poista paikallista** (asiakkaan toiminta) |
| 404 / 410 | ei ole olemassa / poistettu | 410 = objekti / tila on siirretty tai poistettu |
| 409 | ristiriita | version ristiriita, nimen kaksoiskappale (kirjainkoolla ei merkitystä) |
| 429 | nopeusrajoitus | vaatii perääntymistä ja uudelleenyritystä |
| 500 | palvelinvirhe | ks. ylläpitodokumentin vikatilannekäsittely |
| 507 | tallennustila loppu | levyn vesitaso saavuttaa kynnysarvon |

**Asiakkaan toiminta 429-koodiin**: projektin kokemus on, että "429:n perääntyminen ja uudelleenyritys on pakollista" — mittauksissa tunnistustiedostojen siivouksessa `DELETE` osui `file_write`-nopeusrajoitukseen, ja ilman perääntymistä **poistettiin hiljaisesti liian vähän**; latauksen tehtävän luonnin osuessa nopeusrajoitukseen ilman perääntymistä ilmenee "tiedosto ei koskaan lataudu".

---

## 8. Suhde määrittelytiedostoon

- Tämä asiakirja on ihmisen luettava johdanto; **auktoritatiivinen sopimus** on koneellisesti luettava `DOC/api/openapi.yaml`.
- web-arkisto tekee sen pohjalta frontend-sopimuksen tarkistuksen (`pnpm check:api`); työpöytäpää käyttää gen-csharp-työkalua asiakaskoodin luomiseen.
- Kun sopimusta muutetaan, `web`- ja `desktop`-haaroilla on kummallakin oma vendored-kopionsa, joka on **kolmessa paikassa synkronoitava**.
