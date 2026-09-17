# Server-com varmuuskopiointi ja palautus

> Ydinhyöty yhdellä lauseella: **mihin tahansa hetkeen voidaan palauttaa "tietokanta + objektit" samanaikaisesti**.
> Mukana tulevat skriptit ovat `deploy/backup/`-hakemistossa, ja ne ajetaan automaattisesti systemd-ajastimilla.

---

## 1. Varmuuskopioinnin tavoite ja strategia

Verkkolevyssä on kaksi tietotyyppiä, **jotka on voitava palauttaa yhdessä samaan hetkeen**:

1. **Metatiedot** (käyttäjät/tilat/hakemistorivit/kiintiöt PostgreSQL:ssä…);
2. **Objektitiedostot** (todellinen sisältö, tallennettu sisällön tiivisteen mukaan paikalliselle levylle).

Vain toisen varmuuskopioiminen ilman toista johtaa sekavaan tilaan, jossa "tiedosto on olemassa mutta tietokanta osoittaa toiseen objektiin" tai "tiedosto on kokonaan 0 tavua".

| Tieto | Tapa | Tiheys | Säilytys |
|---|---|---|---|
| PostgreSQL | `pg_dump -Fc` (päivittäinen koko tietokannan varmuuskopiointi) + **WAL-arkistointi** (ajankohtaisen palautuksen toteutus) | koko tietokanta päivittäin 02:30; WAL reaaliaikainen | dump 14 päivää |
| Objektihakemisto `/opt/netdisk/data` | borg (päällekkäisyyden poistava inkrementaalinen) | **4 tunnin välein** | 7 pv / 4 vk / 6 kk |
| Redis (AOF) | menee objektihakemiston mukana varmuuskopioon | sama kuin yllä | sama kuin yllä |

Kaksi ajastinta laukaisee automaattisesti (molemmat ajetaan `deploy/backup/netdisk-backup.sh`-skriptillä):

```sh
netdisk-backup.sh full      # päivittäin: WAL-itsetarkistus + objektien inkrementointi + PG koko tietokanta + lukemisen tarkistus + säilytyksen siivous
netdisk-backup.sh objects   # 4 tunnin välein: vain objektihakemiston inkrementointi
```

### Kolme "vikaantumisen estävää" suunnitteluratkaisua varmuuskopioinnissa

1. **Varmuuskopioinnin jälkeen luetaan heti takaisin**: `pg_restore --list <dump>` ei aukea → katsotaan epäonnistumiseksi — vain kirjoitettu mutta tarkistamaton varmuuskopio on vain "tiedosto, jonka luulet olevan olemassa".
2. **WAL-arkistoinnin tarkistus katsoo "uusia epäonnistumisia" eikä "onko nolla"**: `failed_count` on kumulatiivinen arvo, jota ei koskaan nollata.
   "Onko nolla" -tarkistus muuttaisi kerran tapahtuneen historian epäonnistumisen "varmuuskopio epäonnistuu ikuisesti" -tilaksi. Nyt on muutettu tallentamaan lähtöarvo ja hälytetään vain **inkrementistä**.
3. **Tilan JSON:n atomisoiva kirjoitus** (väliaikainen tiedosto + `mv`): estää Prometheusia saamasta puolikasta tiedostoa.
   "Varmuuskopion tila tuntematon" ja "varmuuskopiointi epäonnistui" ovat kaksi täysin eri onnettomuutta.

---

## 2. Voidaanko varmuuskopiosta todella palauttaa? — Palautusharjoitus

> Varmuuskopio ≠ voi palauttaa. Todellinen varmennus on **palautusharjoitus**: kerran neljännesvuodessa, `deploy/backup/netdisk-restore-drill.sh`.

Harjoitus tekee neljä asiaa, **minkä tahansa epäonnistuminen tarkoittaa harjoituksen epäonnistumista**:

1. Palauta viimeisin varmuuskopio **erilliseen väliaikaistietokantaan** (`netdisk_drill_<päivämäärä>`, **ei koskaan koske tuotantotietokantaan**);
2. **Avaintaulujen rivimäärien vertailu** tuotannon vs. palautetun tietokannan (users/spaces/files/file_objects/space_members/sync_feed/audit_logs);
3. **Objektihakemiston uudelleensoitto**: palauta varmuuskopiosta väliaikaishakemistoon ja vertaa jokaisen elävän objektin sisältötiivistettä (sha256);
4. Kirjoita harjoituksen aikaleima, ja yli **90 päivää harjoittelematta** aiheuttaa hälytyksen.

```sh
sudo /opt/netdisk/bin/netdisk-restore-drill.sh     # kirjataan tiedostoon /var/backups/netdisk/logs/drill-<päivämäärä>.log
```

> On oltava vähintään yksi elävä objekti, muuten harjoitus voi vain varmistaa "arkisto voidaan purkaa", ei "sisältö voidaan soittaa uudelleen"
> (nolla tavua oleva tiedosto on "olemassa" samalla tavalla). Voit luoda sellaisen WebDAV-suoralla latauksella:
> `curl -u admin:<salasana> -T <paikallinen tiedosto> http://127.0.0.1:8080/webdav/<space_id>/<tiedostonimi>`
> (polun **on sisällettävä space_id**).

---

## 3. Ajankohtainen palautus (PITR): palautus tiettyyn hetkeen

> Edellytys: `archive_mode=on` + `archive_command` toimii normaalisti (määritetty asennuksessa), ja **on olemassa perusfyysinen varmuuskopio**, josta voidaan lähteä liikkeelle.

```sh
# ① Ota perusvarmuuskopio (fyysinen varmuuskopio, joka yhdessä WAL:n kanssa mahdollistaa "paluun menneisyyteen")
su - postgres -c "/usr/lib/postgresql/17/bin/pg_basebackup -D /var/backups/netdisk/base -Ft -z -X fetch"

# ② Palauta "erilliseen instanssihakemistoon" (älä korvaa tuotantodataa)
install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/17/restore
tar -xzf /var/backups/netdisk/base/base.tar.gz -C /var/lib/postgresql/17/restore

# ③ Kirjoita palautuskohde (palauta hetkeen 2026-09-12 19:53)
cat >> /var/lib/postgresql/17/restore/postgresql.auto.conf <<'EOF'
restore_command = 'cp /var/lib/postgresql/wal_archive/%f %p'
recovery_target_time = '2026-09-12 19:53:00+08'
recovery_target_action = 'promote'
EOF
touch /var/lib/postgresql/17/restore/recovery.signal
chown -R postgres:postgres /var/lib/postgresql/17/restore

# ④ Käynnistä palautusinstanssi toisessa portissa (rinnakkain tuotannon kanssa, ei kosketa tuotantoa)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_ctl -D /var/lib/postgresql/17/restore \
  -o '-p 5433' -l /tmp/pitr.log start"
su - postgres -c "psql -p 5433 -Atc 'SELECT count(*) FROM files' netdisk"
```

**PITR-harjoitusskripti** (`netdisk-pitr-drill.sh`) automatisoi tämän ja todella "tekee aikamatkan",
ja kriteeri ei ole "tietokanta nousee", vaan **kohdehetken jälkeiset tiedot eivät saa olla olemassa**:
Lisää `before`(T0) → ota fyysinen varmuuskopio → kirjaa kohdehetki T → lisää `after`(T1>T) →
palauta varmuuskopiosta hetkeen T → väitä: tietokannassa `before` on olemassa, `after` **ei ole olemassa**.

Käytännön keskeiset kohdat:
- Perusvarmuuskopiossa **plain-muoto** (`-Fp`, joka putoaa suoraan käyttökelpoiseksi datakansioksi) on vaivattomampi kuin `-Ft` (pakatut paketit);
- Palautusinstanssin `max_connections` ja muiden parametrien on oltava **≥ pääkannan**, muuten PG kieltäytyy suoraan palautumasta;
- Harjoitus on tehtävä **replikalla**: promote-komennon jälkeen kyseinen hakemisto on kirjoitettu, eikä se enää ole "sen hetken varmuuskopio",
  perusvarmuuskopio on tuote, ja harjoitus voi koskettaa vain sen kopiota.

---

## 4. Objektitiedostojen uudelleensoitto (erikseen tehtynä)

```sh
export BORG_REPO=/var/backups/netdisk/borg BORG_PASSPHRASE=$(cat /etc/netdisk/borg.passphrase)
borg list --last 3 "$BORG_REPO"                  # katso viimeisimmät muutama
cd /tmp/restore && borg extract "$BORG_REPO::full-2026-09-12T19:53:38"
# Palautettu muodossa /tmp/restore/opt/netdisk/data/objects/xx/yy/<sha256>
```

> **Objektien ja tietokannan on oltava peräisin samasta hetkestä**: vain objektien soittaminen uudelleen ilman tietokantaa = "tiedosto on olemassa, mutta tietokanta osoittaa toiseen objektiin";
> vain tietokannan soittaminen uudelleen ilman objekteja = "tiedostot ovat kokonaan 0 tavua". Harjoitusskripti sitoo ne yhteen juuri tämän estämiseksi.

---

## 5. Redis:n AOF

Redisissä on kirjautumistilat ja kohdistimet (**ei välimuisti**, pysyvyyden "kolme ehdotonta vaatimusta" on esitetty kohdassa `05-INSTALL.md` §5). Tämän osion keskeinen kohta on varmuuskopioida myös AOF:

```sh
redis-cli -a <salasana> --no-auth-warning BGREWRITEAOF      # käsin laukaistu AOF-uudelleenkirjoitus
ls -l /var/lib/redis/appendonlydir/                     # AOF ja manifesti putoavat tänne
```

Varmuuskopiointitapa: menee objektihakemiston mukana borgiin, tai päivittäin `BGREWRITEAOF`:n jälkeen erikseen `cp`. **AOF:n katoaminen → kaikki käyttäjät kirjautuvat uudelleen** (data ei katoa).

---

## 6. Vastaavat hälytykset

Varmuuskopiointiin/palautukseen vahvasti liittyvät hälytykset (säännöt ks. `deploy/prometheus/netdisk-alerts.yml`, mittarien kriteerit ks. `06-OPS.md` §4):

- `NetdiskBackupStale` / `NetdiskObjectsBackupStale`: koko tietokannan/objektien varmuuskopiointi ei onnistunut odotetulla taajuudella;
- `NetdiskWalArchiveFailing`: WAL-arkistointi epäonnistuu jatkuvasti, `pg_wal` täyttää levyn;
- `NetdiskRestoreDrillOverdue`: edellisestä harjoituksesta > 90 päivää, on aika tehdä palautusharjoitus.

---

## 7. Tällä hetkellä tekemättä (rehellisesti merkitty)

- **Viikoittainen perusfyysinen varmuuskopio** (PITR:n "lähtökohta"). Nykyinen `netdisk-backup.sh` tekee vain `pg_dump` (looginen varmuuskopio),
  eikä tuota fyysistä perusvarmuuskopiota — joten "paluu menneisyyteen" -alue on rajoittunut käsiharjoituksen jättämään perusvarmuuskopioon.
  Suositellaan lisättäväksi ennen käyttöönottoa **viikoittainen `pg_basebackup`-ajastin** (säilytä 4 kappaletta).
- **Etäkopio**. Tällä hetkellä kaikki varmuuskopion kopiot ovat samalla levyllä, **levyn rikkoutuminen = data ja varmuuskopio katoavat yhdessä**,
  mikä on nykyisen ratkaisun suurin yksittäinen vika-apiste. Suositellaan lisättäväksi borg-etävarastoa tai objektitallennuksen kopiota, joka synkronoi varmuuskopiot koneen ulkopuolelle.
