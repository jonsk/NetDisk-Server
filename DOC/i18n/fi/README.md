# NetDisk · Yritystason avoimen lähdekoodin verkkolevyn palvelin

> Itseisännöitävä verkkolevyn taustapalvelu **tiimien tiedostoyhteistyöhön** (Go-yksittäinen binääri).
> Tarjoaa tiimien jaettuja tiloja, TUS / WebDAV / multipart -moniprotokollaisen latauksen, versioidun finalisoinnin,
> reaaliaikaisen synkronointitietoisuuden (SSE + kohdistin), hienorakeisen käyttöoikeuksien ja kiintiöiden hallinnan — voidaan ottaa käyttöön yksityisenä verkkolevynä,
> tai yrityksen tiedostokeskuksena.
>
> **Lisenssi:** [Apache-2.0](../../../LICENSE) · **Muoto:** Yhteisöversio (Server-com)

---

## 🌐 Monikielisyys / Translations

[中文](../../../README.md) | [English](../en/README.md) | [Deutsch](../de/README.md) | [Français](../fr/README.md) | [Suomi](../fi/README.md) | [Русский](../ru/README.md)


---

## ✨ Ominaisuudet

| Ulottuvuus | Kyky |
|---|---|
| 🚀 **Lataus ja finalisointi** | TUS-rajapalat ja keskeytyksistä jatkaminen / WebDAV PUT / multipart — kolme sisääntuloa jakavat saman finalisoinnin polun; sisältöosoitteiset objektit + pikasiirron deduplikaatio + myrkytyksenestotarkistus |
| 🧠 **Objektin elinkaari** | `file_objects` neljä tilaa + objektitason lukko (kaksikerroksinen mutex), kirjoitetaan ensin objekti sitten lyhyt transaktio, estäen hiljaisen ylikirjoituksen ja tiedoston ajautumisen |
| 💾 **Paikallinen objektitallennus** | Sisältöosoitteinen tallennus: objektit tallennetaan sisällön Hashin perusteella paikalliselle levylle, sama sisältö tallennetaan vain kerran (deduplikaatio) |
| 📡 **Synkronointitietoisuus** | SSE-reaaliaikapushaus + `/changes`-kohdistimen kaksitilaisuus (globaali `change_seq`), nollasyklinen skannaus, yhteistyössä työpöytäasiakkaan kanssa |
| 🔐 **Identiteetti ja käyttöoikeudet** | Itse kehitetty tunnus/salasana-kirjautuminen + JWT (HS256), organisaatio/osasto/ryhmä, henkilökohtainen tila / tiimitila, jaettu linkki, kiintiöiden täsmäytys |
| 🗂 **Hakemistosemanttiikka** | Hakemistonsyvyys ≤31 / polun enimmäispituus ≤240B; kirjainkoolla ei ole väliä deduplikaatiossa; MOVE/DELETE-kynnyksen reititys asynkroniseen jonoon |
| 🛡 **Ylläpito ja tietoturva** | Kiintiöiden täsmäytys + objektien tarkastus (vuoto/orko) ja korjaus; objektitason lukko + rajapinnan nopeusrajoitus; auditointi on varattu paikanvaraaja (ei tällä hetkellä tallenneta pysyvästi) |
| 📦 **Ylläpitoikkuna** | `/admin`-ylläpitoikkuna (käyttäjä / organisaatio / tila / kiintiö), tarjotaan Go `embed` -yksittäisestä binääristä |

---

## 🏛 Arkkitehtuurin yleiskatsaus

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

- **PostgreSQL 17（vähimmäistuki）** —— ainoa metatietokoneisto (sisältää WAL-arkistoinnin); perusavain käyttää `gen_random_uuid()`, vaatii PG ≥ 17
- **Redis** —— istunto / nopeusrajoitus / tehtäväjono (`SKIP LOCKED`, ei MQ:ta)
- **Nginx** —— käänteinen välityspalvelin, yksi portti ulospäin
- Ylläpitoikkunan käyttöliittymä kopioidaan `web`-repo:n `pnpm build`:n jälkeen kansioon `internal/webui/dist/`, ja sen tarjoaa **Go `embed`**

---

## 🛠 Teknologiapino

| Kerros | Valinta |
|---|---|
| Kieli | Go 1.26 |
| HTTP | vakiokirjasto `net/http` + `ServeMux` -väliketjut |
| Tietokanta | pgx v5 + sqlc + goose-migraatio |
| Todennus | golang-jwt v5 (HS256) |
| Tallennus | paikallinen levyn objektitallennus (sisältöosoitteinen) |
| Kokoonpano | yaml.v3 + ympäristömuuttujien tiukka validointi |
| Käyttöönotto | systemd + Nginx + Prometheus (yhden portin nelikko) |

---

## 🚀 Pika-aloitus

### Esivaatimukset

- Go 1.26+
- PostgreSQL 17 (tietokannat: `netdisk` / `netdisk_test`, `LC_COLLATE=C`)
- Redis 7+ (`appendonly yes`, `noeviction`)
- Nginx (tuotanto)

### 1. Rakennus

```bash
# Ympäristö (kotimainen kiihdytys + tarkistuksen poiskytkentä)
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off

go build ./...     # käännös
go vet ./...       # staattinen tarkistus
```

> **Huomio (embed):** Jos käyttöliittymän tuote on jo olemassa kansiossa `internal/webui/dist/`, se upotetaan käännöksen yhteydessä;
> käyttöliittymän päivityksen jälkeen on ensin suoritettava `pnpm build` ja sitten `go build`, muuten upotetaan vanha tuote.

### 2. Tietokantamigraatio

```bash
go run ./cmd/migrate -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up
# tai käytä valmiiksi käännettyä goose-binääriä
```

### 3. Kokoonpano

Kopioi `deploy/config/config.example.yaml` nimellä `config.yaml` ja muokkaa tarpeen mukaan.
**Kaikki salausasetukset kulkevat ympäristömuuttujien kautta** (`NETDISK_JWT_SECRET` jne., oletusarvo tai liian lyhyt hylätään käynnistyksen yhteydessä),
ei missään tapauksessa yaml-tiedostoon —— katso tarkemmin `deploy/systemd/secrets.env.example`.

### 4. Käynnistys

```bash
go run ./cmd/netdisk
# 监听 :8080
# /admin   管理后台  /api/v1/*  REST  /dav/*  WebDAV  /changes  增量拉取  /sync/events  SSE
```

---

## 🔌 Rajapintakyvyt

| Sisääntulo | Kuvaus |
|---|---|
| `REST /api/v1/*` | todennus, käyttäjät, osastot, tilat, tiedostot, hakemistot, lataus, lataaminen, jakaminen, tapahtumat |
| `Lataus & Range` | virtaileva `ServeContent`; tavutason Range (200/206/416); If-Match/If-None-Match/If-Range -ehtopyynnöt |
| `TUS /uploads/*` | palat, keskeytyksistä jatkaminen, lipun (ticket) uudelleenkäyttö, väliaikaistilan kierrätys, levyn vedenpinta (>90 % → 507) |
| `WebDAV /dav/*` | `x/net/webdav` + PUT-kaappaus finalisoinnin kautta (≤100 Mt); LOCK-semanttiikan itsekehitys täydentää |
| `SSE /sync/events` | etämuutosten reaaliaikapushaus (omien kaikujen esto) |
| `kohdistin /changes` | kohdistimen kaksitilainen inkrementaalinen haku, globaali `change_seq` |
| Nopeusrajoitus | file_read / file_write -tasot; 429 on yritettävä uudelleen |

---

## 📂 Projektirakenne

```
Server-com/
├── cmd/          可执行入口
│   ├── netdisk        主服务
│   ├── migrate        数据库迁移
│   ├── passwd         （管理员凭据）
│   ├── depsguard      架构纪律机械校验
│   ├── testsgate      集成测试门禁
│   ├── coveragegate   覆盖率门禁
│   ├── devdb          本地开发建库
│   └── probesmoke     冒烟探针
├── internal/         核心逻辑（37 个包）
│   ├── finalize/      唯一写路径 ★
│   ├── objlock/       对象级双层锁（ADR-2）
│   ├── storage/       本地磁盘对象存储
│   ├── uploadsvc/     TUS 上传
│   ├── lifecycle/     对象生命周期 worker
│   ├── syncfeed|syncsse/  同步感知
│   ├── webdavfs|webdavauth/ WebDAV
│   ├── api/           REST handler
│   ├── patrol/        对象巡检
│   └── quotareconcile/ 配额对账
├── deploy/          部署物料（systemd/Nginx/Prometheus/备份/供给/验证）
├── scripts/         构建与生成脚本
├── DOC/             功能清单 / 架构 / 安装部署 / 日常运维 / 备份与恢复 / 测试 / API 指南 / 代码阅读指南
└── LICENSE          Apache-2.0
```

---

## 🧪 Testaus ja portit

- `go test ./...` —— yksikkötestit
- `go test -tags=integration ./...` —— integraatiotestit (vaatii `netdisk_test` -tietokannan + Redis)
- `cmd/depsguard` —— punaisen linjan koneellinen tarkistus (esim. ydinpaketti ei riipu HTTP-kerroksesta)
- `cmd/testsgate` / `cmd/coveragegate` —— integraatio-/kattavuusportit
- Ennen commit:a on suositeltavaa ajaa paikallisesti: `go build ./...` / `go vet ./...` / `cmd/depsguard`

---

## 🤝 Avustaminen

Ennen commit:a varmista, että `go build ./...`, `go vet ./...`, `depsguard` ovat kaikki vihreitä; uudet toiminnot noudattavat arkkitehtuuridokumentin V3.0 punaisia linjoja ja ADR:ää; älä lähetä mitään salaisuuksia tai tunnuksia.

---

## 📄 Lisenssi

Tämä projekti julkaistaan **Apache License 2.0** -lisenssillä ([Apache-2.0](../../../LICENSE)).
Voit vapaasti käyttää, muokata, levittää ja käyttää sitä **kaupallisiin** tarkoituksiin (mukaan lukien suljetun lähdekoodin johdannaiset),
mutta sinun on **säilytettävä alkuperäinen tekijänoikeus ja lisenssijulistus** ja merkittävä muutokset muokatuissa tiedostoissa.
Tämä projekti toimitetaan **sellaisena kuin se on** (AS IS), **ilman minkäänlaista nimenomaista tai implisiittistä takuuta**; käyttöönotto-, ylläpito- ja vaatimustenmukaisuusvastuu on käyttäjällä.

### Suhde kaupalliseen versioon

`Server-com` (tämä repo, yhteisöversio) ei sisällä mitään yrityspalvelulupausta eikä mitään tunnuksia tai yksityisiä kokoonpanoja.

---

*Katso täydellinen ominaisuusluettelo: [`DOC/01-功能清单.md`](../../01-功能清单.md).*