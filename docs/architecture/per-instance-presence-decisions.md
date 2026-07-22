# Beslut och migrering för per-instance presence

Status: föreslagen arkitektur med fattade och öppna beslut

Huvuddesign: [per-instance presence](per-instance-presence.md)

Kanonisk aktuell paketnumrering:
[roadmap för per-instance presence](per-instance-presence-roadmap.md).
Roadmapen supersedar paketnamn och tidsordning i avsnitt 4 nedan. Fattade ADR-
principer i detta dokument gäller fortsatt om roadmapen inte uttryckligen säger
annat.

## 1. Beslutsklasser

Dokumentet skiljer på fyra kategorier:

- **Fattat arkitekturbeslut:** normativt för implementationen och öppnas endast
  genom ett nytt uttryckligt arkitekturbeslut.
- **Parameter som ska mätas:** ett initialt mätvärde eller en timeout som ska
  kalibreras med observe-only-data.
- **Produkt-/presentationsbeslut:** påverkar hur en enhet visar kanonisk data,
  aldrig själva instansmodellen.
- **Blockerande beslut:** en kvarvarande fråga med angivet senaste migreringspaket
  då den måste vara avgjord.

Hela arkitekturen har fortsatt status föreslagen. Statusen **BESLUTAD** nedan
anger att just den principen är låst inom förslaget.

## 2. Fattade arkitekturbeslut och öppet produktbeslut

| ID | Beslut | Motiv | Status |
| --- | --- | --- | --- |
| ADR-01 | `Instance` är kanonisk enhet; source/provider/profile är metadata. | Förhindrar att parallella körningar skriver över varandra. | **BESLUTAD** |
| ADR-02 | En lokal collector per OS-användare upptäcker processer utan wrapper och tar emot hooks. Ingen maskinövergripande collector i första implementationen. | Bevarar `claude`/`codex` som användargränssnitt och begränsar rättighets- och integritetsytan. | **BESLUTAD** |
| ADR-03 | Runtime key är host + boot + root PID + process-starttid; PID ensam förbjuds. | PID återanvänds och processnamn kan spoofas. | **BESLUTAD** |
| ADR-04 | Hookstatus binds bara när exakt en instans är säkert identifierad. Osäker korrelation uppdaterar aldrig en kandidat. | Fel körning får aldrig uppdateras. Falskt idle är mindre skadligt än fel status på annan instans. | **BESLUTAD** |
| ADR-05 | Processlagret äger närvaro/idle, hooklagret äger aktivitetsclaim och kan rensa den. Effektiv status beräknas. | En poll kan då inte skriva över `working`, `attention` eller `error`. | **BESLUTAD** |
| ADR-06 | Revisioner per ägarskikt och producer epoch styr ordning; klienttid är bara diagnostik. | Hanterar retries, out-of-order och klockskevhet. | **BESLUTAD** |
| ADR-07 | Relayn äger global logisk slottilldelning och durable, stabila slots. | Ger samma placering mellan collectors och klienter samt efter relay-omstart. | **BESLUTAD** |
| ADR-08 | Slotnamespace är logiskt och obegränsat; fysisk pixelkapacitet är presentation. | Alla instanser förblir kanoniska även vid overflow. | **BESLUTAD** |
| ADR-09 | V2 har separat instans-API och presentations-API; `/presence` är legacyprojektion. | Hindrar en-LED-aggregat från att bli domänmodell. | **BESLUTAD** |
| ADR-10 | En tom v2-lista ger HTTP 200 och betyder `sleeping`; offline, 5xx och schemafel är separata klienttillstånd. | Tomt register är ett normalt produkttillstånd. | **BESLUTAD** |
| ADR-11 | Relay använder minimal persistence plus leases och full re-registration. | In-memory ensamt tappar slotstabilitet; persistence ensam kan behålla spökinstanser. | **BESLUTAD** |
| ADR-12 | Standardläge för en LED är ännu inte valt. Fler-pixel är huvudprodukten; en-LED-läget är endast presentation och får aldrig påverka kanonisk modell, status eller slots. | Valet mellan priority, cycle, pinned och summary kräver produkt-/användartest. | **ÖPPET PRODUKT-/PRESENTATIONSBESLUT — före en-LED-v2** |
| ADR-13 | Linux är första adapter; kärna, adapterkontrakt och fixtures är plattformsneutrala. | Ger leveransbar omfattning utan att låsa arkitekturen eller påstå färdigt Windows/macOS-stöd. | **BESLUTAD** |
| ADR-14 | Rå argv, CWD, transcript-path, prompt, terminaloutput och generell miljödata lämnar aldrig klientmaskinen. | Upprätthåller README:s integritetslöfte och minskar läckagerisk. | **BESLUTAD** |

## 3. Bindningsinvariant och svaga identiteter

Följande invariant är beslutad:

1. Processdetektering får skapa en `idle`-instans före första prompten.
2. Hookstatus får endast bindas till exakt en säkert identifierad instans.
3. Om bindningen inte är säker fortsätter processinstansen visas som `idle`, ingen
   kandidat muteras och ett innehållsfritt diagnostikmått ökas.

Följande genvägar ska uttryckligen avvisas:

- `source` som instansnyckel: alla `codex-api`-körningar kolliderar;
- hook session-id som generell runtimeidentitet: det finns inte före första hooken,
  är verktygsägt och saknar ensam processbevis;
- PID utan starttid: kan peka på en helt annan process efter återanvändning;
- senaste startade process: race mellan terminaler ger slumpmässig felkoppling;
- samma arbetskatalog: flera körningar arbetar ofta i samma repository;
- samma TTY: multiplexer, barnprocesser och verktygsimplementation kan dela eller
  förändra terminalrelationen;
- processnamn: enkelt att imitera och otillräckligt för Node/native-familjer;
- transcript-path som fjärrnyckel: läcker filsysteminformation och bevisar inte
  ensam vilken process som lever.

En felkoppling kan visa att fel kundprofil behöver input, hålla en avslutad
körnings pixel i error eller låta ett event avsluta granninstansen. Därför är
designens avsiktliga failure mode "status saknas och korrelationsfel syns i
diagnostik", aldrig "gissa och mutera".

## 4. Migreringsplan

> **Historisk plan:** Paketnumreringen i detta avsnitt beskriver den ursprungliga
> migreringsplanen och är inte längre aktuell. Den får inte användas för att
> namnge nya leveranser. Se den
> [kanoniska roadmapen](per-instance-presence-roadmap.md) för implementerade
> Paket 0–4, Paket 4.5 och det planerade Paket 5. ADR-besluten och tekniska
> säkerhetskraven nedan förblir relevanta även när deras gamla paketnummer har
> supersedats.

Varje arbetspaket ska kunna verifieras och rullas tillbaka utan att kräva att
nästa paket redan finns.

### Historiskt steg 0: kontrakt och fixtures

- Definiera plattformsneutrala domän-, adapter-, korrelations- och
  coordinatorgränssnitt.
- Gör de fattade ADR-invarianterna exekverbara som kontraktstester, särskilt att
  osäker korrelation aldrig muterar en kandidat.
- Frys v1:s nuvarande tester: snapshotformat, 404 vid tom store, source-delete och
  prioritetsaggregat.
- Lägg OS-neutrala fixtures för processfamiljer, PID-återanvändning, parallella
  starter och tvetydig korrelation.
- Definiera en innehållsfri mätplan för korrelationssignaler, falska
  positiva/negativa kandidater och process-exitfall.
- Definiera JSON Schema/OpenAPI för v2 och felkoder.

Paket 0 ska inte låsa den slutliga automatiska bindningströskeln. Verifiering:
gränssnitt, invariants och fixtures kan testas utan daemon, verkliga processer
eller ESP, och mätplanen innehåller inga förbjudna datafält.

### Historiskt steg 1: domänkärna och in-memory v2 bakom feature flag

- Inför `Instance`, skilda runtime/hookclaims, revisioner och lifecycle state.
- Implementera korrelations- och slotcoordinatorgränssnitt som rena komponenter.
- Lägg v2-read/write handlers bakom avstängd feature flag.
- Låt v1-koden vara orörd och parallell.

Verifiering: två instanser med samma provider förblir separata; processpoll kan
inte sänka hookstate; slotar kompakteras inte; stale revision ger konflikt.

### Historiskt steg 2: Linux processadapter och collector, observe-only

- Implementera samma-användare-observation via `/proc`, starttid och processfamilj.
- Klassificera Claude samt Codex Node/native utan att kräva wrapper.
- Kör collectorn i observe-only: logga opaka ID:n och korrelationskvalitet, men
  skriv ännu inte produktionsstatus.
- Samla underlag för signalstyrka och möjlig automatisk bindning utan att mutera
  någon produktionsinstans.
- Mät falska positiva/negativa fall vid Ctrl+C, krasch, SSH-bortfall och
  terminaldöd.

Verifiering: fixtures plus riktiga parallella starter; inga kommandorader,
sökvägar eller miljövärden förekommer i logg eller nättrafik.

### Historiskt steg 3: lokal hook-IPC och säker bindning

- Lås den automatiska bindningströskeln från Paket 2:s observe-only-underlag
  innan Paket 3 börjar.
- Ändra hookarnas transport till collectorn och bevara best effort mot verktyget.
- Bind befintliga Claude/Codex-sessionevent till upptäckta runtimeinstanser.
- Karantänsätt tvetydiga events och exponera endast räknad diagnostik.
- Behåll nuvarande source-publicering parallellt för v1 under perioden.

Verifiering: två parallella Claude och två parallella Codex i samma CWD/olika och
samma terminalmiljö; ingen avsiktligt tvetydig hook får mutera en kandidat.

Det här paketet får inte kräva `bin/aurora-codex`. Wrappern kan finnas kvar som
legacy under övergången, men vanliga `codex` måste ge full processlivscykel.

### Historiskt steg 4: durable relay, leases och v2 shadow traffic

- Lägg minimal atomisk persistence, recovery window och collector
  re-registration.
- Aktivera v2-writes som shadow state medan `/presence` fortfarande läser v1.
- Jämför legacyaggregat med en v2-priorityprojektion i metrics utan att exponera
  innehåll.
- Testa relay-kill/restart med tre levande instanser och abrupt collector-förlust.

Verifiering: samma instance-ID och slots återkommer efter relayrestart; ej
återregistrerade poster försvinner efter lease/recovery; ingen tombstone
återupplivas med återanvänd PID.

### Historiskt steg 5: fler-pixel-ESP på v2

- Implementera full snapshot, slotrendering, overflow och strikt schemavalidering.
- Skilj sleeping, offline, 5xx, 4xx och invalid-data visuellt.
- Behåll befintlig firmware på `/presence` för kompatibilitet.

Verifiering: 0, 1, 4 och 6 instanser på en fyrpixelsenhet; ESP-omstart behåller
placering; avslutad mitteninstans flyttar inte andra pixlar.

### Historiskt steg 6: v1 som projektion och en-LED-v2

- Låt `GET /presence` projicera v2 med exakt dagens format och prioritet.
- Isolera legacy POST/DELETE så att source-writes aldrig blir v2-instanser.
- Lägg v2 presentation modes och implementera det då fattade ADR-12-valet utan
  att ändra kanonisk instansmodell.
- Publicera kompatibilitetsperiod, mät legacytrafik och definiera borttagningsvillkor.

Verifiering: dagens relay/agent-integrationstester fortsätter passera; gammal ESP
ser ingen wire-förändring; ny en-LED-ESP ändrar aldrig kanonisk state.

### Historiskt steg 7: plattformsadaptrar

- Implementera och validera macOS-adaptern mot samma kontraktsfixtures.
- Implementera och validera Windows-adaptern med creation time,
  session/Job Object/ConPTY där tillgängligt.
- Märk en plattform stödd först efter exit-, parallellitets-, permissions- och
  integritetstester.

Windows och macOS är arkitekturellt inrymda från paket 0 men inte levererade bara
för att Linux-adaptern finns.

### Historiskt steg 8: avveckla legacy

- Ta bort wrapperkravet och sedan wrappern först när processdetekteringens
  acceptanskriterier är uppfyllda i stödda miljöer.
- Sluta skriva source-bucket när alla producers använder v2.
- Avveckla v1-endpoints först efter annonserad period och verifierat noll legacybruk,
  eller behåll read-only projektion om kostnaden är låg.

## 5. Kompatibilitetsperiod

Perioden bör vara minst två firmware-releaser och en dokumenterad relay-release,
men kalenderlängden är ett öppet produkt-/driftbeslut. Under perioden:

- gammal ESP läser oförändrat `GET /presence`;
- ny ESP läser v2 och kan skilja sleeping från kommunikationsfel;
- gamla hook/agent-writes går till legacy-bucket;
- nya collectors skriver per instans;
- legacy och v2 får inte korsskriva varandras kanoniska poster.

Om samma verkliga körning syns i båda modellerna används v2 för nya klienter och
legacy endast för v1-projektionen; dubbelräkning undviks genom separata
presentationsvägar, inte genom osäker sourcebaserad deduplicering.

## 6. Öppna frågor med beslutspunkt

### 6.1 Blockerande arkitektur- och protokollbeslut

| Fråga | Vad återstår | Måste vara avgjort senast |
| --- | --- | --- |
| Runtime root-regler | Verifierade executable-identiteter och kända Claude-/Codex-Node/native-topologier för Linux; processnamn ensamt är otillräckligt. | Före verklig hookanslutning |
| Automatisk bindningströskel | Vilka kombinationer av ancestor, processgrupp, session, TTY och starttid som efter observe-only-mätning får räknas som exakt en säker instans. | Före bindningsmutation |
| V2 write-schema och auth-roller | Exakta requestfält, idempotensfel och maskinell separation mellan runtime- och hookägarskap. | Före aktivt v2-write-API |
| Tillåten fjärrmetadata | Allowlist för opaka ID:n, provider/profile och eventuell grov version. De data som förbjuds av ADR-14 är inte öppna för omprövning. | Före aktiv v2-publicering |
| Credential/bootstrap | Hur en per-user-collector autentiseras mot relay på loopback, betrott LAN och andra nät. | Före collector–relay-integration |
| Persistenceformat | Embedded/atomiskt format, schemaevolution och crash consistency för beslutad minimal persistence. | Före durable relaystate |
| Slotnamespace | Om global `default` räcker eller om en deployment behöver flera namngivna globala layouter; relayn förblir ägare. | Före aktiv global slottilldelning |
| Sen återkomst efter lease-expiry | Maximal reservation och reactivation-policy utan att skriva över en ny instans. | Före durable relaystate |
| Kompatibilitetsperiod | Kalenderlängd, releasevillkor och mätbart villkor för att avveckla legacy-writes/read. | Före v1-avveckling |

### 6.2 Parametrar som ska mätas

| Parameter/fråga | Underlag | Måste vara kalibrerad/avgjord senast |
| --- | --- | --- |
| Pollintervall och antal missar | Observe-only-adaptern mäter exitlatens och falska missing vid Ctrl+C, krasch, SSH- och terminalbortfall. | Före collector-livscykel |
| Automatisk bindningströskel | Observe-only-korrelation mäter signalernas entydighet utan produktionsmutationer. | Före bindningsmutation |
| Relay-lease och recovery window | Disconnect-/restarttester och önskad stale-tid. | Före durable relaystate |
| Tombstone-retention | Restart-, delta- och supportbehov. | Före durable relaystate |
| Behov av delta-/streaming-API | Snapshotstorlek, instansantal och enheternas nätbudget. | Före klient som kräver delta/streaming |
| Säker insamling av `tool.version` | Adaptermätning utan argv eller annan ADR-14-data; `unknown` förblir giltigt. | Före publicering av tool-version |

Mätning får justera numeriska parametrar men aldrig luckra upp invarianten att en
osäker hook inte muterar någon kandidat.

### 6.3 Produkt- och presentationsbeslut

| Fråga | Avgränsning | Måste vara avgjord senast |
| --- | --- | --- |
| ADR-12: standard för en LED | Välj mellan priority, cycle, pinned eller summary och eventuell akut preemption. Fler-pixel är huvudprodukt; valet ändrar inte kanonisk data. | Före en-LED-v2 |
| Overflow-animation | Hur en enhet med färre pixlar än instanser signalerar overflow. | Före fler-pixel-v2 |
| Sleeping/offline/felfärger | Visuell och tillgänglighetsmässig kodning; semantisk åtskillnad är redan beslutad. | Före v2-firmware |
| Cycle dwell och preemption | Behövs endast om ADR-12 väljer cycle eller en valbar cycle-mode. | Före cycle-presentation |

## 7. Acceptanskriterier för arkitekturen

Respektive paket är redo när dess blockerande frågor i avsnitt 6 är avgjorda.
Följande ska kunna uttryckas som automatiska tester:

- sex samtidiga instanser med samma/delade providers ger sex unika instance-ID:n;
- start upptäcks som idle före hook och kräver inget ändrat användarkommando;
- varje hook uppdaterar exakt sin processinstans eller ingen alls;
- Ctrl+C, krasch, SSH-bortfall och terminaldöd frigör slot utan `SessionEnd`;
- processpoll kan inte skriva över aktuell hookstatus;
- relayn återstartar med tre levande instanser utan slotbyte;
- fyra pixlar och sex instanser ger fyra stabila synliga slots plus overflow två;
- sleeping kan inte förväxlas med transport-, server- eller datafel i v2;
- nättrafik och central state saknar prompt, argv, CWD, transcript-path och secrets;
- gamla `GET /presence` behåller sitt nuvarande kontrakt under hela perioden.
