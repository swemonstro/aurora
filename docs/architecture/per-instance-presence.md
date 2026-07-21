# Per-instance presence för Aurora

Status: föreslagen arkitektur

Mål: nästa generations närvaromodell

Kompletterande dokument: [beslut och migrering](per-instance-presence-decisions.md)

## 1. Syfte och produktgräns

Auroras kanoniska enhet ska vara en faktisk, körande AI-agentinstans. Två
`claude`-processer och två `codex`-processer är fyra instanser även om de har samma
verktyg, provider, profil eller status. En instans får aldrig ersätta en annan.

Användarens startkommando förblir `claude` respektive `codex`. Automatisk
processdetektering och verktygens ordinarie hooks är integrationspunkterna. En
wrapper, ett alias eller ett profilspecifikt kommando får inte krävas. Den
befintliga `bin/aurora-codex` är därmed en migrationskälla, inte målarkitekturen.

En fler-pixel-enhet visar normalt en instans per pixel. En LED är en projektion av
samma instansregister och aldrig en alternativ datamodell.

Fler-pixel är huvudprodukten. Standardläge för en LED är ett öppet
produkt-/presentationsbeslut (ADR-12); oavsett val påverkas aldrig den kanoniska
instansmodellen, statusägarskapet eller slottilldelningen.

Arkitekturen har fortsatt status föreslagen. ADR-01–ADR-11 samt ADR-13–ADR-14 är
fattade inom förslaget. Numeriska timeoutvärden är mätparametrar och uttryckligen
öppna produktfrågor gäller bara presentation. Beslutsstatus och senaste
beslutspunkt per migreringspaket finns i det kompletterande dokumentet.

### 1.1 Förankring i nuvarande implementation

I dagens kod:

- lagrar `internal/relay.Store` en senaste snapshot per `source` i minnet;
- ersätter en ny snapshot en äldre med samma `source`;
- returnerar `GET /presence` antingen den enda källan eller
  `source="aurora-aggregate"` med prioriteten
  `error > attention > working > idle`;
- returnerar en tom relay `404 Not Found`;
- aggregerar Claude- och Codex-hookarna sessioner i lokala JSON-filer och
  publicerar bara källans aggregat;
- förlitar sig Codex avslutningsvis på wrapper-trap plus `SessionEnd`, medan
  hookleverans är best effort;
- har snapshot v1 endast `version`, `source`, `state` och `timestamp`.

Det innebär att `source` i dag samtidigt fungerar som integration, profil och
lagringsnyckel. Den sammanblandningen ska inte följa med till v2. V1 ändras inte
under kompatibilitetsperioden.

## 2. Systembild och ansvar

```text
vanligt CLI-kommando
        |
        v
Claude/Codex-processfamilj ---- verktygshook
        |                         |
        v                         v
plattformadapter ----------> lokal presence collector
 (processobservation)          | korrelation, revisioner,
                               | lease och återregistrering
                               v
                         relay/coordinator
                         | kanoniskt register
                         | stabil slotallokering
                         | presentationer
                         +--------+---------+
                                  |
                         ESP: en eller N pixlar
```

Den lokala **presence collectorn** är i första implementationen en
bakgrundsprocess per OS-användare, inte en maskinövergripande tjänst. Den använder
en gemensam kärna och en OS-adapter. Collectorn upptäcker processer innan första
prompten, tar emot lokala hookobservationer,
kopplar dem konservativt och skickar heartbeats/upserts till relayn.

**Relay/coordinatorn** är auktoritativ för distribuerat register, lease-utgång och
global logisk slotallokering inom Aurora-deploymenten. Coordinator är ett logiskt
ansvar i relayprocessen i första implementationen, inte en ny driftsatt tjänst.
Gränssnittet ska hållas separat så
att ansvaret kan flyttas senare utan API-byte.

ESP-enheten är en läsande presentationsklient. Den skapar, slår ihop eller tar
inte bort instanser.

## 3. Kanonisk domänmodell

### 3.1 Typer

| Typ | Obligatoriska fält | Innebörd |
| --- | --- | --- |
| `Instance` | `instance_id`, `tool`, `runtime`, `state`, `slot`, livscykeltider | En faktisk start av Claude eller Codex. |
| `Tool` | `kind`, valfri `version` | Produktfamilj, initialt `claude` eller `codex`. Inte instans-ID. |
| `RuntimeIdentity` | `host_id`, `boot_id`, `root_process`, `started_at` | OS-verifierbar identitet för processfamiljen. |
| `ProcessIdentity` | `pid`, `started_at` | PID plus OS-starttid; PID ensamt återanvänds och är aldrig identitet. |
| `HookBinding` | `tool`, `hook_session_id`, `instance_id`, `bound_at`, `evidence` | Lokal koppling från verktygssession till runtime. |
| `State` | `effective`, `base`, valfri `hook_claim` | Effektivt tillstånd och dess två ägarskikt. `idle` är alltid processbasen. |
| `Slot` | `namespace`, `index`, `assigned_at` | Stabil logisk pixelposition under aktiv livstid. |
| `SourceDescriptor` | `provider`, `profile`, `collector_id` | Ursprung och konfiguration, aldrig lagringsnyckel. |

`provider` beskriver integrationsvariant, exempelvis `claude`, `codex-api` eller
`codex-business`. `profile` är en lokalt konfigurerad, icke-hemlig etikett, till
exempel `default` eller `business`. `tool.kind` för båda Codex-profilerna är ändå
`codex`. Flera instanser får ha identisk `SourceDescriptor`.

### 3.2 Exempel på intern instans

```json
{
  "instance_id": "019c7c35-7d21-7ad1-9e52-52e7bd4a1559",
  "tool": { "kind": "codex", "version": "unknown" },
  "source": {
    "provider": "codex-business",
    "profile": "business",
    "collector_id": "col_3Gf9k2"
  },
  "runtime": {
    "host_id": "host_J7nK4w",
    "boot_id": "boot_q13M",
    "root_process": {
      "pid": 18420,
      "started_at": "2026-07-21T09:12:31.447Z"
    }
  },
  "hook": {
    "bound": true,
    "session_ref": "hs_5ba8f1d4"
  },
  "state": {
    "effective": "working",
    "base": "idle",
    "hook_claim": "working",
    "revision": 17,
    "observed_at": "2026-07-21T09:14:02.091Z"
  },
  "slot": { "namespace": "default", "index": 2 },
  "lifecycle": {
    "discovered_at": "2026-07-21T09:12:31.790Z",
    "last_seen_at": "2026-07-21T09:14:03.102Z",
    "lease_expires_at": "2026-07-21T09:14:18.102Z",
    "ended_at": null,
    "slot_released_at": null
  }
}
```

API:t publicerar inte råa runtimefält som standard; exemplet visar den interna
modellen. Publik representation finns i avsnitt 8.

### 3.3 Stabilt och tillfälligt ID

| ID | Stabilitet | Regel |
| --- | --- | --- |
| `instance_id` | Stabilt från upptäckt till tombstone-radering | UUIDv7 skapat av collectorn. Återanvänds aldrig. |
| `host_id` | Stabilt per Aurora-installation på värden | Slump-ID i privat lokal lagring; inte hostname/MAC. |
| `collector_id` | Stabilt per collector-installation | Roteras vid ominstallation eller explicit reset. |
| `boot_id` | Tillfälligt per OS-boot | Adaptergenererat/OS-hämtat, hashat innan fjärröverföring. |
| `pid` | Tillfälligt | Meningsfullt endast ihop med värd, boot och process-starttid. |
| runtime key | Stabilt under processen | `(host_id, boot_id, root_pid, root_started_at)`. |
| hook session-id | Verktygsägt, tillfälligt | Namespacas med `tool` och `collector_id`; får inte bli `instance_id`. |
| transcript reference | Tillfälligt korrelationsbevis | Lokalt HMAC, aldrig rå sökväg i relayn. |
| `slot.index` | Stabilt under aktiv livstid | Kan återanvändas först efter `slot_released_at`. |
| client/producer revision | Monotont inom producer epoch | Inte globalt klockvärde och inte identitet. |

Collectorn persisterar `instance_id` mot runtime key lokalt så att en
collector-omstart inte byter identitet för ännu levande processer. En PID med ny
starttid är alltid en ny instans.

### 3.4 Tillstånd och livscykel

Tillståndsmängden för en aktiv instans är exakt:

- `idle`: processfamiljen finns, men ingen aktuell hookaktivitet pågår;
- `working`: hooken säger att modellen arbetar;
- `attention`: hooken säger att användarinput eller tillstånd behövs;
- `error`: hooken rapporterar ett fel för körningen.

`sleeping` är inte ett instanstillstånd. Det är en presentation när mängden
aktiva instanser är tom. `offline` och protokollfel är transport-/klienttillstånd,
inte instanstillstånd.

Livscykeln är:

```text
discovered/idle -> active/(idle|working|attention|error)
                -> suspect-missing -> ended -> tombstone removed
```

Följande är mätbaselines, inte fattade arkitekturparametrar:

- processpoll: varannan sekund;
- `suspect-missing`: tre på varandra följande missar, alltså normalt sex sekunder;
- runtime-heartbeat till relay: var femte sekund;
- relay-lease: 15 sekunder;
- tombstone: 30 sekunder efter `ended_at` för felsökning och deltaflöden;
- slotten frigörs när instansen övergår till `ended`, efter sexsekundersgracen.

Paket 2 ska mäta poll/missing och ge underlag inför Paket 3. Lease, recovery och
tombstone ska kalibreras före Paket 4. Tiderna ska vara konfigurerbara och mätas
med monotona lokala timers där det går. `observed_at` är diagnostik; ordning
avgörs inte av väggklockor.

## 4. Processdetektering och identitetskoppling

### 4.1 Gemensamt adaptergränssnitt

Kärnan, kontraktstesterna och processfixtures ska vara plattformsneutrala. Linux
är den första adaptern, inte en del av kärnans domänkontrakt. Kärnan ska bero på
följande konceptuella gränssnitt:

```text
ProcessAdapter
  Snapshot() -> ProcessObservation[]
  WatchExits(callback) -> optional subscription
  BootIdentity() -> opaque boot identity

ProcessObservation
  pid, ppid, started_at
  executable_identity
  process_group_or_job
  os_session
  terminal_identity?
  cwd_fingerprint?
  owner_identity
```

Adaptern returnerar endast metadata som kärnan behöver. Exakta kommandorader och
miljöblock får inte lämnas vidare. Kärnan klassificerar kända executable-identiteter
och bygger processfamiljer.

### 4.2 Upptäckt före första prompten

1. Adaptern observerar en ny process som matchar en verifierad Claude- eller
   Codex-executable.
2. Kärnan går uppåt och nedåt i processträdet och väljer ett `runtime root`.
3. För Codex kollapsas en Node-launcher och dess native barn till samma familj när
   härstamning, starttidsintervall och processgrupp/OS-session stämmer.
4. Om runtime key inte redan finns skapar collectorn ett nytt UUIDv7, basstatus
   `idle` och lokal registry-post.
5. Instansen registreras och får en slot innan någon hook måste ha körts.

Två starter i olika terminaler får olika root-processer/starttider och därmed
olika instanser. Att de råkar ha samma arbetskatalog eller profil påverkar inte
identiteten.

Executable-detektering ska i första hand använda kanonisk executable-identitet
(signerad binär, inode/file-ID och verifierad sökväg enligt plattform), inte bara
processnamnet `claude` eller `codex`.

### 4.3 Koppling av en senare hook

Hookmottagaren kör lokalt och lägger till processkontext som verktyget självt inte
skickar. Kandidater poängsätts, men endast deterministiska eller tillräckligt
starka kombinationer får bindas.

Prioritetsordning för bevis:

1. Befintlig bindning `(collector_id, tool, hook_session_id) -> instance_id`, om
   runtime fortfarande är samma generation.
2. Hookprocessens verifierade ancestor chain träffar exakt en medlem i en känd
   runtimefamilj.
3. Exakt processgrupp/job object eller OS-session plus rätt verktyg och kompatibel
   starttid träffar exakt en kandidat.
4. Unik TTY/ConPTY/session tillsammans med tidsfönster och rätt verktyg.
5. Arbetskatalogfingerprint, transcript-HMAC och profil används endast som
   kompletterande skiljekriterier.

PID, PPID och processgrupp är värdefulla bara när starttid/generation kontrolleras.
TTY, arbetskatalog, transcript och miljö är aldrig ensamma tillräckliga: parallella
körningar delar ofta dessa värden.

När en bindning lyckas persisteras den lokalt. Hook-session-id och rå
transcript-path skickas inte till relay; API:t kan exponera en irreversibel lokal
sessionreferens för diagnostik.

### 4.4 Osäkra matchningar

Följande regel är invariant, oberoende av den bindningströskel som kalibreras med
observe-only-data:

1. Processdetektering får skapa en `idle`-instans före första prompten.
2. Hookstatus får endast bindas till exakt en säkert identifierad instans.
3. Om bindning inte är säker visas processinstansen fortsatt som `idle`, ingen
   kandidat uppdateras och ett innehållsfritt diagnostikmått ökas.

Felaktig koppling är värre än tillfälligt utebliven hookstatus. Om två kandidater
återstår eller bara svaga signaler finns ska collectorn:

- behålla observationen i en kort, lokal `unbound hook`-kö;
- inte ändra någon existerande instans;
- försöka igen när nästa processnapshot eller hookevent ger mer bevis;
- logga strukturerad orsak utan prompt, rå sökväg eller miljövärde;
- låta kandidaterna fortsätta visas som `idle`;
- kassera observationen efter en konfigurerad tidsgräns och öka ett diagnostiskt
  mätvärde.

Ingen "närmaste PID", "senast startad" eller samma-CWD-fallback får uppdatera en
instans. En hook-only provisional instans ska inte publiceras om en tvetydig
processmängd finns, eftersom det både dubbelräknar och kan få en egen felaktig
slot. I ett kort race där ingen process ännu syns får observationen vänta till
nästa poll; om processen aldrig kan observeras rapporteras integrationsdegradering,
inte en påhittad säker identitet.

### 4.5 Linux

Första implementationen kan använda `/proc` för PID/PPID, startticks, session-ID,
processgrupp, executable-länk, ägare och vid behov TTY. Boot-ID kan läsas lokalt.
Exit kan upptäckas med pidfd där kärnan tillåter, med polling som alltid tillgänglig
säkerhetsmekanism. `/proc/<pid>/stat`-starttid måste ingå för att skydda mot
PID-återanvändning.

Hookhjälparens ancestor chain är normalt det starkaste beviset. Om en hook
startas asynkront eller reparentas krävs kombinationen processgrupp/session/TTY;
annars lämnas den obunden. Codex Node- och native-processer grupperas bara när den
verifierade ancestryn visar samma launchfamilj. Ett likadant processnamn bredvid
är en separat kandidat.

Ctrl+C, krasch, tappad SSH-session och försvunnen terminal fångas genom att
runtimefamiljen inte längre innehåller en levande relevant process. `SessionEnd`
kan korta gracen men är aldrig enda dödssignalen.

### 4.6 Windows och macOS

Samma domänmodell används, men signalerna översätts av adaptern:

- Windows använder PID + creation time, parent relation via systemprocess-API,
  session-ID och om möjligt Job Objects/ConPTY. Executable file identity och
  signer/path-policy ersätter `/proc`. Åtkomst till andra säkerhetskontexter kan
  vara begränsad; per-user-collectorn får endast observera sin OS-användare i
  första implementationen.
- macOS använder `libproc`/system-API för PID, parent, processstart, processgrupp,
  session och TTY. Process-exit kan bevakas med plattformsmekanism där tillåtet,
  med polling som fallback. Hardened Runtime, sandbox och rättigheter kan minska
  synlig metadata.

Linuxstöd innebär inte automatiskt färdigt Windows- eller macOS-stöd. Adapter-
kontraktstester, fixtures och plattformsspecifika integrationstester krävs innan
respektive plattform deklareras stödd.

## 5. Statusägarskap, ordning och stale-hantering

### 5.1 Två ägarskikt

Processdetektorn äger:

- att runtime finns;
- `base = idle` medan den finns;
- heartbeat, `last_seen_at`, missing/ended och återregistrering.

Hooks äger:

- `hook_claim = working | attention | error`;
- hookrevision och explicit övergång mellan hooklägen;
- att rensa sin claim vid `Stop`, idle-notis eller motsvarande. Ett hookevent
  med betydelsen idle skriver alltså inte en konkurrerande idle-status, utan
  lämnar tillbaka effektiv status till processlagrets basläge.

Effektivt tillstånd beräknas så här:

```text
om runtime inte är levande: ingen aktiv instans
annars om en giltig hook_claim finns: hook_claim
annars: idle
```

En processpoll skriver alltså bara närvaro och basstatus. Den får aldrig skriva
`effective` direkt och kan därför aldrig sänka aktuell `working`, `attention`
eller `error` till `idle`. Hookens explicita `Stop`, idle-notis eller motsvarande
får däremot rensa `hook_claim`, så att basstatusen åter blir effektiv.

En hook-claim ersätts eller rensas av ett nyare relevant hookevent och upphör vid
ny runtimegeneration eller avslutad instans. Den ska inte försvinna bara för att
en godtycklig kort hooktimer löper ut;
det skulle göra långt arbete falskt idle. Runtime-leasen skyddar mot en collector
som försvinner. En separat, konservativ maxålder för hookclaims kan senare införas
per verktyg först när verkliga eventgarantier är mätta.

### 5.2 Revisioner och idempotens

Varje collectorstart har ett slumpmässigt `producer_epoch`. För varje instans
underhåller collectorn två monotona sekvenser:

- `runtime_revision` för discovery/heartbeat/end;
- `hook_revision` för bundna hookevent.

Relay accepterar en mutation bara när `(producer_epoch, revision)` är nyare för
rätt ägarskikt. Upprepning med samma idempotency key och samma body är OK; samma
nyckel med annan body är konflikt. Äldre revision ger `409 Conflict` och ändrar
ingenting. En ny epoch måste först göra en verifierad re-registration mot samma
runtime key och får inte återuppliva en tombstone med annan process-starttid.

Clientens `observed_at` sparas för diagnostik. Relay sätter `received_at` och
lease-deadline från egen klocka. Väggklockstimestamps avgör aldrig write-order.

Om runtime-leasen löper ut blir instansen `suspect` och avslutas efter en kort
relay-grace, såvida collectorn inte återregistrerar den. Detta hanterar tappat nät,
collector-krasch och abrupt maskinförlust utan `SessionEnd`.

Det finns en fundamental partitionstradeoff: relayn kan inte veta om en tyst
fjärrcollector är död eller om processen lever bakom ett nätavbrott. Den kan inte
både återanvända stale slots snabbt och garantera samma slot efter ett obegränsat
avbrott. Den föreslagna bounded-recovery-semantiken låter Aurora avsluta den
observerade instanslivscykeln efter tidsgränsen. Om samma OS-process återkommer
senare måste policyn för sen reactivation vara beslutad; relayn får aldrig tyst
flytta den till en upptagen slot eller skriva över en ny instans.

## 6. Slotmodell för NeoPixels

### 6.1 Logiska slots

Relayns inbyggda coordinator äger den globala logiska slottilldelningen, initialt
i namespace `default`. Vid ny instans tilldelas lägsta lediga icke-negativa index.
Indexet ändras inte så länge instansen är aktiv.

När en instans har passerat missing-gracen:

1. `ended_at` sätts;
2. slotten släpps och `slot_released_at` sätts;
3. övriga instanser behåller sina index;
4. nästa nya instans får lägsta lediga index.

Om slots `0, 1, 2` används och instans 1 avslutas ligger instanserna på `0` och
`2`. Nästa instans får `1`; ingen kompaktering sker.

Coordinatorn hör hemma i relayn eftersom flera värdar och flera verktyg måste
samordnas. Klientägd allokering ger olika placering per ESP och kollisioner.
Collectorägd allokering kan inte lösa globala kollisioner. En separat tjänst är
onödig i nuläget men coordinator-gränssnittet bör vara frikopplat från HTTP.

### 6.2 Fler instanser än pixlar

Logiska slots är inte begränsade av en enskild enhets kapacitet. En fyrpixelsenhet
visar slot `0..3`; aktiva instanser i slot `4+` är `overflow`. API-svaret anger
`active_count`, `visible_count` och `overflow_count`. Enheten ska ge en separat,
konfigurerbar overflow-indikation, exempelvis en kort vit markör i slutet av varje
visningscykel, utan att låtsas att de fyra pixlarna visar alla sex samtidigt.

En kapacitetsmedveten presentationsendpoint kan cykla overflowinstanser, men får
inte skriva tillbaka nya slotnummer. Den kanoniska listan behåller alla sex.

### 6.3 Omstarter

- ESP-omstart: enheten hämtar en full snapshot och återställer samma slotbild.
- Kort nätavbrott: enheten kan behålla senaste bild med tydlig stale-indikation
  under en begränsad tid, därefter kommunikationsfel.
- Relay-omstart: minimal durable state återläser aktiva instance/runtime keys och
  slotallokeringar. Poster markeras `recovering` tills collector återregistrerat
  dem. Slots hålls under en recovery window på 30 sekunder.
- Om persistence saknas kan korrekt identitet återskapas från collectorn, men
  slotordning kan ändras. Därför räcker helt in-memory inte för produktkravet om
  stabil placering över relay-omstart.

## 7. Relay och lagring

Relayns nuvarande helt in-memory-map är lämplig för v1-experimentet men inte för
v2:s restart- och slotsemantik. Rekommendationen är lätt lokal persistence, till
exempel en atomiskt uppdaterad fil eller embedded databas, med:

- `instance_id`, hashad runtime key och source descriptor;
- senaste accepterade revisioner/epoch;
- slotnamespace/index;
- lifecycle- och leasefält;
- tombstones under retentionfönstret.

Ingen prompt, rå argv/kommandorad, CWD/arbetskatalog, transcript-path,
terminaloutput eller generell miljödata lagras.
Durable data är en cache av koordinationsstate, inte sanningen om processliv.
Efter omstart måste varje collector återregistrera levande instanser. Icke
bekräftade poster avslutas efter recovery window och deras slots frigörs.

Lease/heartbeat och processpoll är primära säkerhetsmekanismer. Hookarnas
`SessionEnd` är en optimering. Om en collector tappar relaykontakt köar den endast
senaste runtime- och hookrevision per instans i begränsad lokal state och gör full
re-registration vid återkomst. Den får inte återspela gamla hookevent över ett
nyare tillstånd.

## 8. Versionerat API

### 8.1 Läsmodell

Det kanoniska API:t är `GET /api/v2/presence/instances`. En tom v2-lista ger enligt
fattade ADR-10 alltid `200 OK` och betyder `sleeping`; den är inte ett fel. Svaret
är en full snapshot och kan kompletteras med ETag eller senare ett deltaflöde.

#### Inga instanser

```json
{
  "api_version": 2,
  "generated_at": "2026-07-21T10:00:00Z",
  "presence": "sleeping",
  "instances": [],
  "slots": {
    "namespace": "default",
    "active_count": 0
  }
}
```

#### En Claude-instans

```json
{
  "api_version": 2,
  "generated_at": "2026-07-21T10:01:00Z",
  "presence": "active",
  "instances": [
    {
      "instance_id": "019c7c35-7d21-7ad1-9e52-52e7bd4a1559",
      "tool": "claude",
      "provider": "claude",
      "profile": "default",
      "state": "idle",
      "slot": 0,
      "revision": 3,
      "discovered_at": "2026-07-21T10:00:41Z",
      "state_changed_at": "2026-07-21T10:00:41Z",
      "lease_expires_at": "2026-07-21T10:01:15Z"
    }
  ],
  "slots": {
    "namespace": "default",
    "active_count": 1
  }
}
```

#### Flera Claude-/Codex-instanser med olika status

```json
{
  "api_version": 2,
  "generated_at": "2026-07-21T10:05:00Z",
  "presence": "active",
  "instances": [
    {
      "instance_id": "019c7c35-a001-7000-8000-000000000001",
      "tool": "claude",
      "provider": "claude",
      "profile": "default",
      "state": "attention",
      "slot": 0,
      "revision": 11,
      "discovered_at": "2026-07-21T09:55:00Z",
      "state_changed_at": "2026-07-21T10:04:58Z",
      "lease_expires_at": "2026-07-21T10:05:15Z"
    },
    {
      "instance_id": "019c7c35-a002-7000-8000-000000000002",
      "tool": "codex",
      "provider": "codex-api",
      "profile": "default",
      "state": "working",
      "slot": 1,
      "revision": 8,
      "discovered_at": "2026-07-21T09:57:00Z",
      "state_changed_at": "2026-07-21T10:04:40Z",
      "lease_expires_at": "2026-07-21T10:05:14Z"
    },
    {
      "instance_id": "019c7c35-a003-7000-8000-000000000003",
      "tool": "codex",
      "provider": "codex-business",
      "profile": "business",
      "state": "error",
      "slot": 2,
      "revision": 6,
      "discovered_at": "2026-07-21T09:59:00Z",
      "state_changed_at": "2026-07-21T10:04:20Z",
      "lease_expires_at": "2026-07-21T10:05:13Z"
    }
  ],
  "slots": {
    "namespace": "default",
    "active_count": 3
  }
}
```

#### Sex instanser och fyra tillgängliga pixlar

Det kanoniska anropet returnerar fortfarande alla sex och känner inte till
enhetens kapacitet:

```json
{
  "api_version": 2,
  "generated_at": "2026-07-21T10:10:00Z",
  "presence": "active",
  "instances": [
    { "instance_id": "inst_1", "tool": "claude", "provider": "claude", "profile": "default", "state": "working", "slot": 0 },
    { "instance_id": "inst_2", "tool": "codex", "provider": "codex-api", "profile": "default", "state": "idle", "slot": 1 },
    { "instance_id": "inst_3", "tool": "claude", "provider": "claude", "profile": "default", "state": "attention", "slot": 2 },
    { "instance_id": "inst_4", "tool": "codex", "provider": "codex-business", "profile": "business", "state": "working", "slot": 3 },
    { "instance_id": "inst_5", "tool": "codex", "provider": "codex-api", "profile": "default", "state": "error", "slot": 4 },
    { "instance_id": "inst_6", "tool": "claude", "provider": "claude", "profile": "default", "state": "idle", "slot": 5 }
  ],
  "slots": {
    "namespace": "default",
    "active_count": 6
  }
}
```

De korta `inst_N` används bara för läsbarhet i exemplet; produktion använder
UUIDv7. Enheten hämtar separat
`GET /api/v2/presence/presentation?mode=slots&pixel_capacity=4`:

```json
{
  "api_version": 2,
  "mode": "slots",
  "pixel_capacity": 4,
  "pixels": [
    { "pixel": 0, "instance_id": "inst_1", "state": "working" },
    { "pixel": 1, "instance_id": "inst_2", "state": "idle" },
    { "pixel": 2, "instance_id": "inst_3", "state": "attention" },
    { "pixel": 3, "instance_id": "inst_4", "state": "working" }
  ],
  "active_count": 6,
  "visible_count": 4,
  "overflow_count": 2,
  "overflow_instance_ids": ["inst_5", "inst_6"]
}
```

### 8.2 Skrivmodell

Endast autentiserade collectors får skriva. Rekommenderade operationer:

- `POST /api/v2/presence/instances`: registrera ny runtime; klienten skickar
  `instance_id`, source descriptor, hashad runtime key och `runtime_revision`.
  Samma idempotency key är säker att repetera. Svar `201`, eller `200` vid
  identisk återregistrering.
- `PATCH /api/v2/presence/instances/{instance_id}/runtime`: heartbeat, process
  seen/missing eller explicit end. Endast runtime-rollen får skriva denna yta.
- `PATCH /api/v2/presence/instances/{instance_id}/hook-state`: nytt bundet
  hookläge med `hook_revision`. Endast hook/collector-rollen får skriva ytan.
  `working`, `attention` och `error` sätter claim; `idle` är en wire-level
  bekvämlighetsåtgärd som rensar claim och faller tillbaka till runtime-basens
  `idle`.
- `DELETE /api/v2/presence/instances/{instance_id}`: administrativ eller
  verifierad slutstädning; normal exit uttrycks i runtime-PATCH så att revision
  och tombstone bevaras.

Exempel på hookmutation:

```json
{
  "producer_epoch": "01J2EPOCHF5KQ9X9P",
  "hook_revision": 12,
  "state": "attention",
  "observed_at": "2026-07-21T10:04:58Z",
  "idempotency_key": "01J2EVENT9YR6B7QH"
}
```

Servern svarar med den accepterade instansrevisionen och effektiva statusen.
Fälten `source`, `provider` och `profile` får aldrig användas som URL-nyckel för
mutation; endast `instance_id` adresserar instansen.

### 8.3 Presentation och v1-kompatibilitet

`GET /api/v2/presence/instances` är kanoniskt. En-LED-vyer ligger separat, till
exempel:

```text
GET /api/v2/presence/presentation?mode=cycle&pixel_capacity=1
```

Nuvarande `GET /presence`, `POST /presence` och
`DELETE /presence?source=...` behålls oförändrade under en uttalad
kompatibilitetsperiod. `GET /presence` projicerar v2-registret till dagens JSON
och prioritet när v2 är aktivt. Tomt register fortsätter initialt ge 404 för att
inte ändra befintligt kontrakt. Därför kan gammal firmware inte skilja sleeping
från fel; den distinktionen blir möjlig först när den läser v2.

V1-writes placeras i en separat legacy-bucket och får inte skapa påstått säkra
per-instance-poster. De kan bidra till legacy-presentationen under migreringen.
Det förhindrar att ett source-namn felaktigt blir en instansidentitet.

## 9. En-LED-presentation

En LED är ett sekundärt presentationsläge; fler-pixel är huvudprodukten. ADR-12,
valet av standardläge för en LED, är öppet till Paket 6. Lägena nedan är
produktalternativ, inte konkurrerande domänmodeller.

Alla lägen läser en immutable snapshot av instansregistret:

- **Priority mode:** visar högsta `error > attention > working > idle`, med
  deterministisk tie-break på lägsta slot. Detta motsvarar v1 bäst men döljer alla
  lägre prioriterade instanser.
- **Cycle mode:** cyklar stabilt i slotordning med kort dwell. `error` och
  `attention` får omedelbar preemption/tydligare puls, därefter fortsätter cykeln.
  Det gör fler aktiva instanser synliga men är ännu inte valt som standard.
- **Pinned source/instance mode:** en explicit `instance_id` visar exakt en
  körning. En provider/profile-pin väljer deterministiskt lägsta aktiva slot och
  måste indikera om flera matchar; den får inte låtsas vara en unik instans.
- **Summary mode:** kodar antal och typer, exempelvis en kort räkningssekvens per
  status. Det är kompakt men kräver att användaren lär sig ett signalspråk.

Alla lägen förlorar simultan spatial information jämfört med flera pixlar.
Priority kan dölja arbete permanent, cycle visar inte samtidighet, pin döljer allt
annat och summary tappar direkt koppling mellan körning och status. Valet lagras
som enhetspresentation och ändrar aldrig instanser, status eller slots.

## 10. Sleeping, offline och fel på ESP

ESP håller dessa tillstånd isär:

| Fall | HTTP/protokoll | Presentation |
| --- | --- | --- |
| Relay nåbar, inga instanser | V2 `200`, `presence=sleeping`, tom lista | Neutral sleeping, exempelvis släckt eller svag neutral andning. |
| Relay nåbar, giltiga instanser | V2 `200`, validerad lista | Rendera instanser/presentation. |
| Kommunikationsfel | Timeout, DNS/TCP/TLS-fel | Separat offline-mönster; använd inte senaste data som om den vore aktuell. |
| Serverfel | HTTP 5xx | Distinkt service-fel, med backoff. |
| Ogiltig data | 2xx men schema/version/JSON fel | Protokollfel; behåll senast goda snapshot endast under kort stale-grace. |
| Klientfel | 4xx | Konfigurations-/authfel, inte sleeping. |

ESP ska validera `api_version`, obligatoriska fält, unika instance-ID:n och unika
aktiva slots innan rendering. `generated_at` kan hjälpa diagnostik men ersätter
inte lokal requestålder. Vid nätfel får senast goda bild markeras stale under
exempelvis 15 sekunder; därefter visas offline. Ett tomt, giltigt register nollställer
stale-data omedelbart och visar sleeping.

## 11. Säkerhet och integritet

### 11.1 Dataminimering

Till relayn får endast följande allowlistade metadata lämna klientmaskinen:

- opaka `instance_id`, `host_id`, `collector_id` och hashad boot/runtime key;
- tool, tillåten provider/profile-etikett och eventuell grov tool-version;
- state, slotrelaterad serverdata, revisioner och livscykeltider;
- aggregerade diagnostikkoder utan innehåll.

Följande får aldrig lämna klientmaskinen eller loggas centralt:

- rå argv eller full kommandorad;
- prompt, svar, terminaloutput eller källkod;
- rå CWD/arbetskatalog, filnamn eller transcript-path;
- generell miljödata, inklusive miljövariabelnamn och -värden;
- API-nycklar, tokens, cookies eller andra credentials.

CWD och transcript-path får behandlas kortvarigt lokalt för korrelation. Om ett
jämförelsevärde måste persisteras används HMAC med en maskinlokal nyckel och
kort retention. Generell miljödata får varken samlas in för fjärrbruk eller lämna
maskinen; eventuella lokala klassificeringssignaler måste definieras specifikt och
får inte publiceras som rå miljö.

### 11.2 Hotmodell

Angripare kan starta en process med namnet `codex`, skicka spoofade HTTP-writes,
återspela en gammal revision eller försöka korsuppdatera en annan användares
instans.

Motmedel:

- verifiera executable identity och processägare, inte bara namn;
- kör collectorn med minsta rättighet och tillåt den i första implementationen
  endast att observera sin egen OS-användarkontext;
- autentisera collectors med per-installationscredential och TLS utanför strikt
  betrodd loopback/LAN;
- auktorisera varje write mot ägd `collector_id` och `instance_id`;
- använd producer epoch, monotona revisioner, idempotency keys och korta leases;
- bind hook-IPC lokalt med OS-ACL/Unix socket-rättigheter och verifiera peer;
- rate-limita registreringar och mutationer;
- gör source/profile till visningsmetadata, aldrig behörighetsgrund eller nyckel.

En lokal process under samma komprometterade användarkonto kan fortfarande
imitera ett verktyg eller anropa hook-IPC. Aurora är en indikator, inte en
säkerhetsgräns. Designen ska förhindra oavsiktlig och fjärr spoofing samt göra
lokal imitation svårare, men får inte lova attestering som OS:et inte erbjuder.

## 12. Sekvensflöden

### 12.1 Claude startas, väntar, arbetar, behöver input, blir idle, avslutas

```text
Tid  Processadapter     Collector          Claude-hook       Relay/slot
t0   ser Claude root -> skapar instans A ------------------> A idle, slot 0
t1   processen lever -> runtime heartbeat ----------------> A förblir idle
t2                                      UserPromptSubmit --> bind till A,
                                                              A working
t3                                      AskUserQuestion ---> A attention
t4                                      PostToolUse -------> A working
t5                                      Stop/idle_prompt --> A idle
t6   root borta ------> missing 1..3
t7                     end revision ----------------------> A ended,
                                                              slot 0 fri
t8                                                           tombstone tas bort
```

Processpollerna mellan t2 och t5 uppdaterar bara runtime-leasen och skriver
aldrig över hookläget. Vid t5 rensar hooken sin aktivitetsclaim; processlagrets
basläge `idle` blir då åter synligt.

### 12.2 Två Codex-körningar parallellt

```text
Terminal 1: Codex Node P100 -> native P110 = runtimefamilj A -> slot 0
Terminal 2: Codex Node P200 -> native P210 = runtimefamilj B -> slot 1

Hook session S1 -- ancestor/processgrupp A --> bind S1->A --> working
Hook session S2 -- ancestor/processgrupp B --> bind S2->B --> attention

Resultat: A/slot0=working, B/slot1=attention.
Samma provider "codex-api" orsakar ingen ersättning.
```

Om S1 och S2 bara delar CWD är det inte bevis för någon bindning. Processfamilj,
startgeneration och unik OS-kontext måste avgöra.

### 12.3 Process kraschar utan SessionEnd

```text
t0  A=working; runtime- och hookrevision är aktuella
t1  processen kraschar; ingen hook levereras
t2  poll miss 1: A suspect internt, pixel behålls kort
t4  poll miss 2
t6  poll miss 3: collector skickar end
t6  relay sätter ended_at och släpper slotten
t36 tombstone tas bort
```

Om även collectorn eller datorn försvinner löper relay-leasen ut och utför
motsvarande stale-avslutning. `SessionEnd` behövs inte.

### 12.4 Relay startas om medan tre instanser kör

```text
t0  Durable state: A/0, B/1, C/2
t1  Relay startar, läser poster som recovering och håller slots i 30 s
t2  Collectorn reconnectar och gör full re-registration med runtime keys,
    instance-ID:n, producer epoch och senaste revisioner
t3  Relay verifierar A, B, C; samma slots och hookclaims gäller
t30 Eventuella ej återregistrerade poster avslutas och slots släpps
```

En tom nystartad relay får inte omedelbart meddela sleeping om durable recovery
visar kandidater; v2 kan ange `presence=recovering` under det korta fönstret.
ESP visar då stale/recovery, inte felaktigt tomt register.

### 12.5 Fyra pixlar men sex instanser

```text
Relay: A/0 B/1 C/2 D/3 E/4 F/5 (alla kanoniskt aktiva)
ESP:   pixel0=A, pixel1=B, pixel2=C, pixel3=D, overflow_count=2
```

E och F finns kvar i API:t och behåller slots 4 och 5. Enheten visar sin
overflow-indikation eller en separat presentationscykel. Om B avslutas flyttas
inte C–F; slot 1 blir fri. Nästa instans G får slot 1.

## 13. Observability och verifierbarhet

Implementationen bör exponera innehållsfria mätvärden: antal aktiva instanser,
unbound/ambiguous hooks, lease expirations, stale mutationer, slot overflow och
re-registration latency. Loggar ska använda opaka ID:n och reason codes.

Kontraktstester ska särskilt bevisa:

- att samma source/provider kan ha flera instanser;
- att en processpoll inte sänker hookstatus;
- att gamla revisioner och återanvänd PID avvisas;
- att osäker korrelation inte muterar någon kandidat;
- att slotar inte kompakteras och överlever restart;
- att tom v2-lista är sleeping medan transport/schema/5xx är fel;
- att v1-projektionen behåller nuvarande wire-format och prioritet.
