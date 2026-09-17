# Server-com testausdokumentti

> Lukijakunta: ylläpitäjät / kokemattomat IT-ammattilaiset
> Tavoite: tietää **mitä testejä on**, **miten ne ajetaan**, **nykyiset mitatut tulokset**, **mitkä vaativat manuaalista työtä**.
> Katso myös: `02-ARCHITECTURE.md`, `05-INSTALL.md`, `06-OPS.md`, `07-BACKUP.md`

---

## 1. Testaustasojen yleiskatsaus

| Taso | Mitä tekee | Kuka ajaa |
|---|---|---|
| Käännös / staattinen tarkistus | takaa, että kääntyy, eikä epäilyttäviä rakenteita | kehittäjä / CI, automaattinen |
| Yksikkö- + integraatiotestit | funktio kerrallaan / rajapinta kerrallaan liiketoimintasääntöjen varmennus | kehittäjä / CI, automaattinen |
| Arkkitehtuurin kurinpito-portti | takaa kerrostuksen, punaiset viivat, dokumenttien yhdenmukaisuuden | CI, automaattinen |
| Kattavuusportti | takaa, että avainpaketit eivät ole ilman testejä | CI, automaattinen |
| Päästä päähän -anturi | todellinen HTTP koko ketjun savutesti | käyttöönoton jälkeen / julkaisun edellä, vaatii todellisen koneen |
| Suorituskyvyn perustaso | varmistaa, että listaus / synkronointi / rinnakkaissiirto täyttävät tavoitteet | todellinen kone, toistettava skripti |
| Manuaalinen vastaanotto | käyttöliittymän klikkailu, poikkeuksellisen pitkät polut, todellisen koneen alustat jne. | ylläpito / vastaanottaja |

> Projektissa `cmd/testsgate`, `cmd/depsguard`, `cmd/coveragegate` ovat kaikki **erillisiä porttiohjelmia**.
> Paikallinen komento ja CI-komento ovat sama (CI käyttää samoja komentoja).

---

## 2. Automaattiset portit (yksi komento per taso)

Suorita arkiston juurihakemistossa (`Server-com/`):

| Taso | Komento | Mitä pysäyttää |
|---|---|---|
| Käännös / staattinen | `go build ./... && go vet ./...` | käännösvirheet, epäilyttävät rakenteet |
| Yksikkö + integraatio | `NETDISK_TEST_DSN='...' go test ./... -count=1` | kaikki Go-testitapaukset (mukaan lukien todellinen PG/Redis) |
| Integraatio ei ohitettu | `go run ./cmd/testsgate` | tällainen **vale-vihreä** "DSN puuttuu, koko ryhmä skipataan ja CI on kokonaan vihreä", nimeää kaikki ohitetut testitapaukset |
| Tietokilpailu | `go run ./cmd/testsgate -race` | tietokilpailu (vaatii gcc) |
| Arkkitehtuuri- ja dokumenttikuri | `go run ./cmd/depsguard` | ytimpakkauden riippuvuus HTTP-kerroksesta / työpöytäkuri / punaiset viivat / dokumenttien ja ylläpitotuotteiden yhdenmukaisuus |
| Kattavuus | `go run ./cmd/coveragegate -profile <cover.out> -min 55` | avainpakkausten kattavuus alittaa kynnysarvon |
| Päästä päähän -anturi | `go run ./cmd/probesmoke -pass <口令>` | todellinen HTTP koko ketju (lataus/lataaminen/jako/WebDAV/auditointi/tarkastus…) |

> Integraatiotestit vaativat `netdisk_test`-tietokannan + Redisin. Ilman todellista PG/Redis-integraatiotapaukset ohitetaan; `testsgate`-työkalun tehtävä on **valvoa, "onko jokin testitapaus ohitettu salaa"**, jotta CI:ssä ei esiintyisi vale-vihreää.

---

## 3. Yksikkö- + integraatiotestit (nykytila)

- Kaikki testitapaukset **1042 läpäisi**, kattaen 39 pakettia / **0 ohitettu** (`testsgate` nimesi ja tarkisti).
- Kattavuuden osa-alueet: todennustokenit / latauksen lopullistaminen / objektilukon rinnakkaisuus / hakemiston semantiikka / synkronoinnin muutokset / jako / kiintiöiden täsmäytys / tarkastus / WebDAV.

**Suorituskykyherkkien väitteiden kerrostetut kriteerit** (esim. rinnakkaisen lopullistamisen lukkoikkunan P95):
- Pääkriteerinä käytetään **mediaania** (regressio hidastaisi jokaista lukon pitämistä);
- Hännässä käytetään P95 < 500 ms (imee sisäänsä "kaikkien pakettien rinnakkain + jaettu testitietokanta" -koneen heilunnan).
- Vastaesimerkin varmennus: kun lukon kriittiseen alueeseen lisätään keinotekoinen sleep → mediaani hidastuu heti ja testitapaus muuttuu punaiseksi, mikä osoittaa, että kyse on "kriittisen alueen levenemisestä" eikä "heitosta".

---

## 4. Päästä päähän -anturi (probesmoke)

Yhdellä komennolla tehdään **käynnissä olevan todellisen palvelun** koko ketjun savutesti (51+ vaihetta): kirjautuminen → hakemisto → lataus (TUS/WebDAV/multipart) → lataaminen/Range → jako → synkronointitapahtumat → auditointi → tarkastus.

```sh
go run ./cmd/probesmoke -base http://127.0.0.1:8080 -user admin -pass '<口令>'
```

> Kun kuljetaan välityspalvelimen kautta, Nginx kuluttaa SSE-otsakkeet (`X-Accel-Buffering`), joten **anturin on oltava suoraan sovellukseen yhdistettynä** (51/51 läpäisee kokonaan); välityspalvelimen kautta tulos on 50/51, ja tämä ero on selitetty (ei ole vika).

---

## 5. Suorituskyvyn perustaso (mitattu 2026-09-12)

Skripti: `deploy/verify/06-perf-baseline.sh` (itsenäinen, toistettava; luvut skriptin mittaamia). Kone: Debian 13, 6 vCPU / ~2 Gt RAM / sekventiaalinen kirjoitus 2723,6 MB/s (paikallinen perustaso).

| Vastaanottokohta | Kynnys | Mitattu | Johtopäätös |
|---|---|---|---|
| 100 000 rivin hakemistolistaus | P95 ≤ 500 ms | **P95 107,2 ms** (oletussivun limit=200) | läpäisi (varaa noin 4,7-kertainen) |
| Etämuutoksen havaitseminen (`/changes` näkyvissä) | P95 ≤ 3 s | **P95 78,1 ms** (multipart-kirjoitus) | läpäisi (varaa noin 37-kertainen) |
| Etämuutoksen havaitseminen (SSE-kehys saapuu) | P95 ≤ 3 s | **P95 80,5 ms** | läpäisi |
| Rinnakkaislataus 8×8MiB | — | **9,66 MB/s**, onnistui 8/8, epäonnistui 0 | läpäisi |
| Rinnakkaislataus 16×8MiB | — | **10,57 MB/s**, onnistui 16/16, epäonnistui 0 | läpäisi (12 kertaa nopeusrajoituksen jälkeen uudelleenyritys) |
| Rinnakkaislataaminen 8/16×8MiB | — | **1944 / 2580 MB/s** | läpäisi (sivuvälimuistin osuma, ei vastaa levyn läpikykyä) |

> **Rehellinen huomautus**: 16 rinnakkaislatauksen yhteydessä `POST /tus` sai 12 kertaa palvelimen **429-nopeusrajoituksen** (`rate_limits.upload=10/s`), ja skripti yritti uudelleen eksponentiaalisella perääntymisellä ja onnistui kaikissa — tämä on **nopeusrajoituksen tarkoituksenmukainen vaikutus**, ei latauksen epäonnistuminen. Jos vastaanotto vaatii "16 rinnakkaista kerralla läpi", on suurennettava `rate_limits.upload` tai vähennettävä rinnakkaisuutta. Lataamisen 2 Gt/s-luokka on "muisti + virtuaalilevy" -nopeus ja osoittaa vain, että latauspolulla ei ole jonotusta eikä virheitä rinnakkaisuuden alla.

### Toisto

```sh
BASE=http://127.0.0.1:8080 USER=admin PASS='<口令>' \
ROWS=100000 NREP=60 NFEED=30 SAR="8 16" UPFILE=8388608 \
sudo sh deploy/verify/06-perf-baseline.sh | tail -n +1
```

Skriptin loppu tulostaa `===== TS-08 机器可读摘要 =====` (kerta kerralta otokset / histogrammi / kvantiilit / verdict-päätösrivi). Oletuksena tunnistusdata siivotaan automaattisesti; `KEEP=1` säilyttää sen (vianetsinnän käyttöön).

---

## 6. Vastaanoton vertailu ja uudelleenajon paikat

| Vastaanottoalue | Uudelleenajon paikka |
|---|---|
| Yksikkötestin perustaso / kattavuus | `go test ./... -coverprofile=cover.out` → `coveragegate` |
| Integraatiotestit yksi kerrallaan | `go test -tags=integration ./...` |
| Suorituskyvyn perustaso | `deploy/verify/06-perf-baseline.sh` (todellinen kone) |
| Käyttöönotto ja päivitys | `deploy/verify/0[12567]-*.sh` (todellisen koneen skriptit rivi riviltä) |
| Varmuuskopiointi / palautus / PITR | `deploy/backup/*.sh` (harjoitusskriptit + kirjanpito) |
| Valvonta ja hälytykset | `curl /metrics`, `promtool check rules` |
| Objektien yhdenmukaisuus / väliaikaistiedostojen kierrätys | `go test ./internal/storage/ -run 'ReapTemp|MismatchedObjectKeys' -count=1 -v` |
| Tietokilpailu | `go run ./cmd/testsgate -race` |

---

## 7. Edelleen manuaalisesti suoritettavat kohteet (älä tee automaatiossa näennäiskattavuutta)

| Kohde | Miksi automaatio ei kata | Rekisteröintipaikka |
|---|---|---|
| Windows-erittäin pitkä polku käytännössä | vaatii todellisen aseman kirjaimen ja `longPathAware`-ympäristön voimaantulon | asiakkaan vastaanottolista |
| Virustorjuntaohjelman tiedoston lukituksen uudelleenyrityskäyttäytyminen | vaatii todellisen AV-koukun | sama kuin edellä (rehellisesti merkitty "tarkistamaton") |
| Käyttöliittymän klikkailu (kirjautuminen / ilmoitusalue / asetukset / ristiriita / etäselaus / päivitysten tarkistus) | ei käyttöliittymän automaatiota | asiakkaan vastaanottolista |
| 24 h asiakkaan pitkä ajo | kesto ja todellinen vuorovaikutussessio | suoritettava manuaalisesti ennen julkaisua |
| Kolmannen osapuolen harjoitus käsikirjan mukaan (varmuuskopiointi/palautus) | vaatii "toisen henkilön" toimivan käsikirjan mukaan | harjoituskirjanpito |

---

## 8. Kolme testikuria

1. **Ei voi päättää ≠ läpäisi**: tarkistimen kohdatessa "ei voida päättää" on tuomittava epäonnistumiseksi eikä sitä saa kohdella läpäistyneenä.
2. **Vastaesimerkin varmennus on osa väitettä**: jokaiselle avainväitteelle on tehtävä kerran "pilaa koodi → väitteen on muututtava punaiseksi", muuten et tiedä, mitä valvot (arkistossa on jo useita kertoja ilmennyt "pilattu mutta silti vihreä").
3. **Todiste on toistettava**: todiste = komento + todellinen tuloste + kyseinen versio; pelkän komennon lähteettömän tulosteen liittäminen ei ole todiste.
