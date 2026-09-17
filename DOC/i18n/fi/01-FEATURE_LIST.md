# Server-com (avoimen lähdekoodin versio) ominaisuusluettelo

> Versio: 2026-09-15 koottu
> Tämä repositorio ei sisällä mitään yrityspalvelulupausta, käytä/muokkaa/levitä avoimen lähdekoodin lisenssin mukaisesti, ota käyttöönotto ja ylläpito omalla vastuullasi.
> Suunnittelun auktoriteetti: `Doc\网盘系统架构设计文档.md`.

---

## I. Repositorion asemointi

| Kohta | Sisältö |
|---|---|
| Nimi | **Server-com** (avoimen lähdekoodin versio / Community) |
| Luonne | verkkolevylinjärjestelmän **taustapalvelun (Go)** avoin julkaisu; sisältö = palvelinpuolen lähdekoodi + käyttöönottomateriaali |
| Teknologiapino | Go / PostgreSQL 17 / Redis / Nginx / paikallinen levytallennus |
| Ulospäin suunnatut kyvyt | REST-rajapinta + TUS jatkuva lataus + WebDAV + SSE reaaliaikainen synkronointi |

**Hakemistot:** `internal/` (ydinkoodi), `cmd/` (komentoriviohjelmat), `deploy/` (käyttöönottomateriaali), `scripts/` (apuskriptit), `DOC/` (dokumentaatio).

> Lukijan huomio: tämä luettelo on järjestetty "`mitä järjestelmä pystyy tekemään`" -periaatteella, jotta kokonaiskuvan saa nopeasti; jos tarvitset syvällistä perehtymistä koodirakenteeseen ja sisääntuloihin, lue saman hakemiston *Code Reading Guide* (`03-CODE_READING_GUIDE.md`).

---

## II. Mitä järjestelmä tarjoaa ulospäin

Server-com on **yritysverkkolevyn taustapalvelu**, joka tarjoaa ulospäin neljää kykyä; asiakasohjelmat (hallintapaneeli / työpöytä / tiedostoprotokollatyökalut) käyttävät niitä verkkolevyn käsittelyyn:

| Kyky | Kuvaus | Ulkopuolinen muoto |
|---|---|---|
| **REST-rajapinta** | kirjautuminen, tiedostojen luominen/poistaminen/muokkaaminen/haku, lataus, jakaminen, taustahallinta | `/api/v1/*` |
| **TUS jatkuva lataus** | suurten tiedostojen sirpaleittainen lataus, verkkokatkaisu voidaan jatkaa | `/tus/*` |
| **WebDAV** | yhteensopiva vakiotiedostoprotokolla, voidaan suoraan liittää verkkolevynä | `/webdav/*` |
| **SSE reaaliaikainen synkronointi** | tiedosto muuttuessa push asiakkaalle, sitten inkrementaalinen haku | `/api/v1/events` |

---

## III. Toimintomoduulien luettelo

### 1) Tilit ja todennus
- **Tunnuksella kirjautuminen**: käyttäjänimi/salasana-kirjautuminen, tukee päivitystunnisteen uusimista ja uloskirjautumista.
- **Tunnistemekanismi**: onnistuneen kirjautumisen jälkeen myönnetään käyttötunniste; tunnisteet jakautuvat **päätepisteisiin** (taustapaneeli / työpöytä), eri päätepisteiden tunnisteiden oikeudet ovat eristettyjä.
- **Tilien hallinta**: taustapaneelissa voi luoda käyttäjiä, ottaa käyttöön/poistaa käytöstä tilejä, säätää rooleja; tukee latauksen/latauksen alas roolien hienorakeista hallintaa.
- **Salasanan turvallisuus**: salasanan vahva tarkistus ja tiivistevarasto; epäonnistuneisiin yrityksiin kohdistuu väliaikainen lukitusstrategia väkivaltaista murtamista vastaan.
- > Huomio: aiempi yritys-WeChat / DingTalk -koodinlukukirjautuminen ja organisaatiosynkronointi on poistettu avoimesta versiosta; tällä hetkellä on jäljellä vain oma tunnus/salasana-kirjautuminen.

### 2) Organisaatio ja tilat
- **Organisaatiorakenne**: tukee osastopuun ylläpitoa (osastojen luominen/poistaminen/muokkaaminen, alipuiden katselu, sulkeuman uudelleenrakennus); osasto on yksi käyttöoikeuksien lähtökohdista.
- **Tilat**: jokaisella käyttäjällä on **henkilökohtainen tila**; voi luoda **tiimien tilan** ja kutsua jäseniä yhteistyöhön.
- **Jäsenhallinta**: tilan jäsenten lisääminen/poistaminen, roolien säätäminen, siirtäminen, hajottaminen; ylläpitäjä voi tehdä tilalle globaalia hallintaa (kiintiö, jäädytys, takaisinotto).

### 3) Tiedostonhallinta
- **Metatietotoiminnot**: tiedostojen/hakemistojen selaus, hakemiston luominen, uudelleennimeäminen, siirtäminen, poistaminen.
- **Lataus alas**: tukee virtailevaa latausta, HTTP Range -jatkolatausta, ehdollisia pyyntöjä (If-Match jne., optimistinen lukko).
- **Pikälataus**: sisällöltään samat tiedostot voivat ohittaa toistuvan latauksen ja käyttää suoraan jo tallennettua objektia uudelleen.
- **Muokkauslukko**: tiedostoon voi asettaa lukon, jotta vältetään useiden käyttäjien yhtäaikainen muokkaus ja keskinäinen päällekirjoitus.
- **Nimeämis- ja polkurajoitteet**: tiedostonimen laillisuus, polun pituus, hakemiston syvyys jne. tarkistetaan palvelinpuolella yhtenäisesti.
- **Kiintiö**: tilan kiintiön hallinta, ylitys estetään; mukana kiintiöiden täsmäytys ja ajautumishälytykset.

### 4) Lataus ja kirjoitus
- **TUS jatkuva lataus**: suurten tiedostojen sirpaleittainen lataus, tukee keskeytyksestä jatkamista ja rinnakkaisia sirpaleita.
- **Yhtenäinen kirjoituspolku**: kaikki lataukset (sirpaleet, WebDAV levylle kirjoitus) päätyvät lopulta samaan lopullistamissisääntuloon, mikä takaa "sama tiedosto vain yhdellä kirjoituslähteellä", luonnostaan välttäen rinnakkaisen päällekirjoituksen.
- **Sisältöosoitteinen tallennus**: tiedostot tallennetaan sisällön tiivisteen mukaan, sama sisältö tallennetaan vain kerran (deduplikaatio).
- **Elinkaari**: objektilla on selkeä tilasiirtymä kirjoituksesta "poistettavissa/kierrätettävissä", jotta estetään keskeneräisten tiedostojen vahingossa poistaminen.

### 5) WebDAV ja jakaminen
- **WebDAV**: tarjoaa vakio-WebDAV-protokollalla tiedostojen luku/kirjoitus, hakemisto-operaatiot, kopiointi/siirto, lukitus jne., yhteensopiva työpöytäliitoslevyn kanssa.
- **Jakolinkki**: tiedostosta/hakemistosta voi luoda jakolinkin (kirjautumattomasti käytettävissä/ladattavissa), joka on järjestelmän **ainoa kirjautumaton ulostulo**; voi asettaa vanhentumisen tai peruuttaa milloin tahansa.

### 6) Reaaliaikainen synkronointi
- **SSE-push**: kun tiedosto muuttuu, seuraa reaaliaikaisesti online-asiakkaille pitkällä yhteydellä.
- **Inkrementaalinen haku**: asiakas hakee muutokset inkrementaalisesti kohdistimen avulla; vaikka push menetettäisiin, lopullinen yhdenmukaisuus ei kärsi (haku on auktoritatiivinen polku).
- Käyttötapaus: työpöytäpuolen hakemiston ja pilven kahdensuuntainen synkronointi.

### 7) Ylläpaneeli ja tietoturva
- **Ylläpaneeli**: `/admin` tarjoaa taustahallintasivun (käyttäjät, organisaatio, tilan hallinta).
- **Käyttöoikeuksien hallinta**: `web`-päätepisteen tunniste rajoittaa taustakäytön; H5/työpöytätunnisteet eivät pääse taustaan.
- **Nopeusrajoitus**: kirjautumisen, latauksen, tiedoston luvun jne. rajapintaperheillä itsenäinen nopeusrajoitus, muodostaen kaksikerroksisen suojan Nginxin kanssa; estää väkivaltaista murtamista ja rajapintojen spämmäystä.

### 8) Tallennus ja ylläpito
- **Objektitarkastus**: taustapaneeli skannaa levyä säännöllisesti, löytää objektivuotoja (roskaa ilman viittausta) ja käänteiset orvot (viittaus ilman tiedostoa).
- **Kiintiöiden täsmäytys**: vertaa säännöllisesti "looginen käyttö vs. todellinen käyttö"; liian suuren ajautumisen yhteydessä hälytys ja automaattinen takaisinkirjoituskorjaus.
- **Käyttöönottomuoto**: yksi binääri + systemd + Nginx; varmuuskopiointi/palautus-, varmennus- ja skriptit toimitetaan repositorion mukana.
- **Tallennusmuoto**: oletuksena paikallinen levy (sisältöosoitteinen); vain paikallinen tallennus, ei sisällä kolmannen osapuolen objektitallennustaustajärjestelmiä.

---

## IV. Käyttöönotto- ja suoritusmuoto

- **Yksi binääri**: koko palvelu käännetään yhdeksi suoritettavaksi tiedostoksi, jota systemd hallinnoi; Nginx toimii käänteisenä välityspalvelimena ja HTTPS:nä.
- **Esivaatimukset**: PostgreSQL 17 + Redis (todennus/nopeusrajoitus riippuvuus, ei käynnisty jos ei saatavilla).
- **Kokoonpano**: yaml-tiedosto + ympäristömuuttujat; **salasanat/avaimet kulkevat vain ympäristömuuttujien kautta**, eivät koskaan yaml-tiedostoon tai versionhallintaan.
- **Tietokannan migraatio**: palvelu sisältää migraatioskriptit, jotka suoritetaan automaattisesti käyttöönoton yhteydessä.
- **Ylläpaneelin frontend**: on `embed`-attu binääriin, erillistä frontend-hakemistoa ei tarvitse käyttöönottaa.

---

*Tämä luettelo on kykyjen yleiskatsaus, jotta näet ensin "mitä järjestelmä pystyy tekemään"; koodirakenne ja sisääntulot ovat saman hakemiston *Code Reading Guide* -oppaassa (`03-CODE_READING_GUIDE.md`). Ennen avoimen version käyttöä tarkista ja vahvista se itse.*
