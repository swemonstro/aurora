# Integrationskontrakt för per-instance presence

Status: normativt Paket 4.5-kontrakt

Detta dokument definierar produktgränsen som ska gälla före och under Paket 5.
`SKA`, `FÅR INTE` och `KRÄVER` är bindande. Beskrivningar märkta **nuläge** är
observerade avvikelser och gör dem inte till godkänd målarkitektur.

Den kanoniska paketordningen finns i
[roadmapen](per-instance-presence-roadmap.md). Domäninvarianter och fattade ADR-
beslut gäller fortsatt enligt [huvuddesignen](per-instance-presence.md) och
[beslutsdokumentet](per-instance-presence-decisions.md).

## Produktlager

### A. Generell produktkärna

Kärnan SKA vara OS-neutral, agentneutral, transportneutral och
deploymentneutral. Den äger generiska observationer, runtimekandidater,
korrelation, instanssemantik och publiceringsgränser. Den FÅR INTE importera
Linux `/proc`, Unix-socket, `SO_PEERCRED`, Claude-/Codexpayloads, systemd eller
lokala installationsvärden.

### B. Plattformsspecifik backend

En backend översätter en plattforms säkra lokala API:n till kärnans kontrakt.
Linux `/proc`, Unix-socket och `SO_PEERCRED` hör hit. Framtida Windows- eller
macOS-backends kan använda andra mekanismer men SKA producera samma generiska
observationer. Paket 4.5 lovar inte att sådana backends finns eller är stödda.

### C. Agentadapter

En agentadapter äger agentens eventnamn, tillåtna payloadfält och
runtimeigenkänning. Claude, Codex, Hermes och framtida agenter ska vara separata
adapters. En adapter får endast producera generiska, sanerade observationer och
FÅR INTE mutera registry genom korrelationsresultat.

### D. Distribution och installation

CLI, configinläsning, processkomposition, tjänstehantering och paketering hör
hit. Lagret väljer konkreta backends och adapters men FÅR INTE omdefiniera
domänidentitet eller korrelationsinvarianter. Observe-only-CLI:n är interna
diagnostikverktyg, inte färdiga produktkommandon.

### E. Lokal deploymentkonfiguration

Maskinnamn, användarsökvägar, UID/GID, IP-adresser, systemd-namn och lokala
startkommandon hör hit. Sådana värden FÅR endast förekomma i tydligt märkt
deploymentdokumentation eller exempel. [Blue1-dokumentet](../deployment/blue1.md)
är lokal evidens och aldrig produktkontrakt.

## Obligatoriska gränser

### ProcessBackend

`ProcessBackend` SKA:

- producera en full snapshot av OS-neutrala `ProcessObservation`-värden;
- tillhandahålla boot- och generationssäker processidentitet;
- samla endast kontraktets allowlistade, innehållsfria metadata;
- hantera plattformsrace konservativt.

`ProcessBackend` FÅR INTE avgöra att en process är Claude, Codex, Hermes eller
annan agent och FÅR INTE bygga agentfamiljer. Plattformens rådata får inte bli
domänfält bara för att en viss backend erbjuder dem.

### AgentRuntimeRecognizer

Varje `AgentRuntimeRecognizer` SKA ägas av en agentadapter. Den:

- tar generiska processobservationer som input;
- känner igen endast den agent som adaptern ansvarar för;
- producerar generiska `RuntimeCandidate`-värden och innehållsfria reason codes;
- lämnar osäkra eller motstridiga fall obundna;
- FÅR INTE använda source/provider/profile som identitet.

Recognizern FÅR INTE läsa eller ta emot prompt, transcript, rå CWD, rå argv,
full kommandorad, terminalinnehåll eller generell miljö. Om en backend behöver
reducera lokal startinformation ska resultatet vara en begränsad, validerad och
opak executable-/launch-identitet; rådata får inte passera backendgränsen.

### RuntimeObservationSource

Korrelationsservicen SKA bero på en OS-neutral `RuntimeObservationSource` som
returnerar generiska `RuntimeObservation`-värden. Källan kan i kompositionslagret
bestå av en processbackend plus installerade recognizers.

Korrelationsservicen FÅR INTE importera `internal/linuxprocess`, returnera
`linuxprocess.Sample` eller känna till procdiagnostik. Plattformsdiagnostik ska
rapporteras separat från korrelationsindata.

### HookEventAdapter

En `HookEventAdapter` SKA:

- ägas av respektive agentadapter;
- översätta ett agentspecifikt event till en sanerad generell
  `HookObservation`;
- uttryckligen allowlista de fält som används;
- ignorera eller avvisa förbjudna fält utan att vidarebefordra rå payload;
- vara oberoende av transportbackend.

Transportpaket FÅR INTE importera Claude-, Codex-, Hermes- eller andra
agentpaket. Agentadaptern får använda transportens generiska klientgräns, men
transporten får inte känna till agentens eventmodell.

### Transport

Transportlagret SKA endast bära versionerade generiska observationer och
sanerade svar. Protokollet får ha Unix-socket-, named-pipe- eller annan lokal
backend utan att domänpayloaden byts.

Transportprotokollet FÅR INTE definiera en användare eller principal genom
UID, GID eller PID. Sådana värden får finnas i en plattformsspecifik backend men
får inte serialiseras som påstådd avsändaridentitet.

### Peer authentication

Peerautentisering är backendansvar och SKA ske före payloadbehandling. Den
generella receivern ska endast se ett transportneutralt resultat, exempelvis en
autentiserad principal eller capability med opak lokal referens och explicit
autentiseringsnivå.

Numerisk UID är en Linuxcredential, inte generell användaridentitet. En framtida
named-pipe-backend får använda Windowscredentials utan att imitera UID/GID.

### Processhint-proveniens

PID, starttid eller `RuntimeIdentity` i en payload är ett avsändarpåstående och
SKA initialt behandlas som **overifierat**. Att socketpeeren har samma UID som
servern gör inte påståendet betrott. Hård identitet kräver att mottagaren eller
en betrodd lokal bridge attesterar processgeneration och relation server-side.

Korrelationsindata SKA bära proveniens så att en overifierad hint inte kan ge
`exact`, `strong` eller `would_bind_under_current_threshold`. Paket 5 får inte
skapa hård identitet genom att kopiera payloadens PID/starttid till dagens
`ProcessHint` eller `RuntimeHint`.

`weak`, `ambiguous`, `rejected` och overifierade resultat FÅR ALDRIG mutera
registry. Paket 5 får över huvud taget inte göra registrymutation.

## Agentidentifierare

Den långsiktiga modellen är en validerad namespacad `AgentID`, inte en sluten
Claude-/Codex-enum. Välkända identifierare är:

- `anthropic.claude`
- `openai.codex`
- `nous.hermes`

Ett agent-ID ska bestå av minst två punktseparerade, gemena komponenter. Varje
komponent börjar med `a-z`, får därefter innehålla `a-z`, `0-9` och `-`, får inte
sluta med `-` och är högst 63 tecken. Hela identifieraren är högst 128 tecken.
Namespaceägaren ansvarar för betydelsen. Officiellt stödda agenter får ha
välkända konstanter.

Okända men syntaktiskt giltiga identifierare SKA kunna transporteras och
bevaras av det långsiktiga generiska kontraktet. Automatisk runtimeigenkänning
kräver däremot en explicit installerad recognizer för identifieraren. Avsaknad
av recognizer ger `unsupported`/`unmatched`, aldrig en gissad match.

### Kompatibilitet med nuläget

**Nuläge:** `instancepresence.ToolKind` och v2-schemat accepterar endast
`claude` och `codex`. Paket 4:s protokoll återanvänder `ToolKind`.

Övergångsmappningen är:

| Legacy `ToolKind` | Namespacad `AgentID` |
| --- | --- |
| `claude` | `anthropic.claude` |
| `codex` | `openai.codex` |

Paket 4.5 ändrar inte wireformatet. En senare migrering SKA vara versionssatt:
den får inte tyst ändra betydelsen av `tool`, kringgå nuvarande strict schema
eller få äldre klienter att acceptera okända värden. Under övergången ska
mappingen ligga i en adapter/projektion. Paket 5 får initialt stödja de två
legacyvärdena, men får inte definiera dem som slutlig generell agentmodell eller
lägga fler slutna agentfall i transportkärnan.

## Host-ID, producer epoch och revision

- **Host-ID** ägs av installationen. Det SKA vara stabilt för samma lokala
  Aurora-installation, opakt och oberoende av hostname, MAC-adress och
  användarsökväg.
- **Producer epoch** ägs av en långlivad agentbridge/collector. En ny epoch
  skapas vid förlorad producentstate eller beslutad omregistrering, inte av
  varje kortlivad hookprocess.
- **Revision** ökar monotont för en logisk hookström inom samma producer epoch.
  Revisioner från olika epochs jämförs inte.
- **Kortlivade hookprocesser** ska lämna event till den långlivade ägaren och
  behöver inte själva upprätthålla permanent revisionsstate.

Exakt lagringsformat, crash recovery och rotation implementeras inte i Paket
4.5. I Paket 5 får den manuellt startade, långlivade receivern/bridgen äga en
in-memory-epoch och monotona revisioner; restart skapar då en ny epoch. Hookens
ingressenvelope ska inte kräva att varje kortlivad hook själv skapar dessa
värden. Paket 5 får inte lösa detta med en per-invocation slump-epoch eller med
väggklockan som revision.

## Konfigurationslager

| Konfiguration | Ägare | Exempel på ansvar |
| --- | --- | --- |
| Produkt | Generell applikationskomposition | aktiverade agents, observe-only-feature flag, host-ID-källa, integritetsläge |
| Plattformsbackend | Respektive backend | proc-root, USER_HZ, boot-ID-källa, plattformslimiter |
| Transport | Transport/transportbackend | protokollimiter, deadline, socket- eller pipeadress, peerpolicy |
| Agentadapter | Respektive adapter | eventmapping, recognizerregler, tillåtna identitetsbevis |
| Lokal deployment | Operatör/deployment | faktiska paths, användare, grupper, IP, tjänstenamn och startkommando |

Produktdefault eller domänkontrakt FÅR INTE innehålla repositorysökvägar,
personliga hemsökvägar, maskinnamn, fasta UID/GID, lokala IP-adresser eller
systemd-enhetsnamn. Sådana värden får endast finnas i märkt deploymenttext som
[Blue1-exemplet](../deployment/blue1.md).

## Produkt-MVP

Första MVP är **Linux-only**. Det betyder att Linux är den enda implementerade
och testade plattformen, inte att Linuxdetaljer får bli kärnkontrakt.

Windows och macOS implementeras inte nu och ska inte beskrivas som stödda. En
framtida backend ska kunna införas utan ändring av domänens processobservation,
runtimekandidat, korrelator eller `HookEventAdapter`. Om en sådan backend visar
att kontraktet saknar en generell förmåga ska den förmågan beslutas som en
versionssatt kärnändring, inte simuleras med Linuxfält.

## Stabila respektive interna gränser

Följande semantik ska behandlas som stabil och kontraktstestas, även om Go-
paketen fortsatt ligger under `internal/`:

- namespacad agentidentifierare och legacyprojektion;
- sanerad `HookObservation`;
- OS-neutral processobservation;
- `RuntimeCandidate` och `RuntimeObservationSource`;
- `AgentRuntimeRecognizer` och `HookEventAdapter`;
- korrelatorns input/output och mutationfrihet;
- publishergränsen mellan domänsnapshot och publiceringsbackend.

`internal/publish.Publisher` är i nuläget en stabil v1-gräns för
`presence.Snapshot`, inte ett per-instance-v2-publisherkontrakt. Paket 5 ska inte
använda eller utöka den. En framtida per-instance-publisher ska få ett eget
generiskt, transportneutralt kontrakt före relayintegration.

Följande ska förbli interna implementationer:

- proc-parser och proc-filsystemsläsare;
- Unix-socket-, `SO_PEERCRED`- och framtida named-pipe-implementation;
- scoring- och global matchningsalgoritm;
- replaycache;
- registryimplementation och låsning;
- observe/correlate/local-server/local-client som diagnostik-CLI:n.

Stabil semantik betyder inte att interna Go-paket blir ett externt Go-API.
Maskinläsbara wirekontrakt ska versionssättas separat.

## Nulägesavvikelser och refaktorbeslut

Varje avvikelse nedan är verifierad i baslinjen `0e19843`. Paket 4.5 gör ingen
kodflytt eftersom en isolerad deländring skulle skapa dubbla eller halvfärdiga
kontrakt.

| Refaktor | Fil och symbol/ansvar | Avvikelse | Klassificering | Krav på åtgärd |
| --- | --- | --- | --- | --- |
| A. OS-neutral runtimekälla | `internal/localhooktransport/service.go`, `SnapshotSource` och `CorrelationService` | Servicen importerar `linuxprocess` och tar `linuxprocess.Sample`. | **obligatorisk under Paket 5 före verklig hookanslutning** | Inför `RuntimeObservationSource`; Linuxkompositionen gör snapshot + recognition utanför servicen innan hookanslutning. |
| B. Agentadapters ur transport | `internal/localhooktransport/adapters.go`, `ClaudeObservation` och `CodexObservation` | Transportpaketet importerar båda agentpaketen. | **obligatorisk under Paket 5 före verklig hookanslutning** | Flytta mapping till agentägda paket innan hookanslutning; transporten tar endast generell observation. |
| C. Recognizers ur Linuxbackend | `internal/linuxprocess/classify.go`, `buildFamilies` i `families.go`, agentkoder i `types.go` | `/proc`-backend klassificerar Claude/Codex och bygger agentfamiljer. | **obligatorisk under Paket 5 före verklig hookanslutning** | Behåll procinsamling i backend; flytta identifiering/familjeregler till agentägda recognizers före verklig mätning. |
| D. Transportneutral principal | `internal/localhooktransport/types.go`, `PeerIdentity`; `auth_linux.go` | Det generella authgränssnittet uttrycks som UID/GID/PID. | **kan skjutas upp** | Behåll internt för Linux-MVP; inför generell principal innan andra transportbackends eller publikt authkontrakt. |
| E. Configdelning | `internal/localhooktransport/config.go`, `Config` | Socketpath, receiverlimiter, servicegräns och replay finns i samma typ. | **kan skjutas upp** | Paket 5 får endast komponera befintlig config; dela före produktinstallation eller ny backend. |
| F. Agent-ID-migrering | `internal/instancepresence/domain.go`, `ToolKind.Validate`; `api/v2/presence.schema.json`, `ToolKind` | Kontraktet är stängt till Claude/Codex. | **kan skjutas upp** | Använd dokumenterad legacy-mapping i Paket 5; gör senare en samlad versionssatt domän-, transport- och schemamigrering. |
| G. Registry från wiretyper | `internal/instanceregistry/registry.go`, mutationsmetoder; `projection.go` | Registry tar och returnerar `presencev2`-typer. | **utanför nuvarande scope** | Lös före aktivt v2-API eller alternativ publisher, inte i observe-only Paket 5. |

Inga kodrefaktorer krävs i Paket 4.5. A–C är obligatoriska första steg i Paket 5
och ska vara klara innan en verklig hook ansluts eller Paket 5 godkänns. Refaktor
D bör göras under en framtida transportbackend, E under produktifiering och F
som ett eget kompatibilitetspaket. Ingen av dem får användas som argument för
att lägga nya plattforms- eller agentfall i dagens kärntyper.

## Paket 5: bindande avgränsning

### Mål

Paket 5 ansluter verkliga Claude- och Codexevent till den manuellt startade
observe-only-receivern. Anslutningen styrs av en explicit feature flag vars
default är av. Agentadaptern skickar en enda sanerad observation best-effort och
v1-flödet fortsätter oberoende.

### Tillåtet

- agentägda `HookEventAdapter`-implementationer;
- refaktorer A–C ovan;
- bounded lokal sändning via generisk klientgräns;
- en sanerad ingressmodell där den långlivade receivern/bridgen tillför epoch,
  revision och attesterad proveniens före Paket 3-korrelation;
- explicit produkt- och agentconfig utan lokala hårdkodningar;
- lokal mätning och märkta parallellfall;
- innehållsfri diagnostik för leverans och korrelation.

### Förbjudet

- registry-, slot-, hookclaim- eller runtimemutation;
- persistence, durable retry eller bakgrundskö;
- relay- eller v2-publicering;
- automatisk bindning;
- TCP, fjärrtransport, installation, daemon eller systemdaktivering;
- beroende från v1-publicering till observe-only-sändningen;
- prompt, transcript, CWD, argv, kommandorad, terminalinnehåll eller generell
  miljö i transport, logg eller fixture;
- lokala paths, maskinnamn, UID/GID, IP eller serviceenhetsnamn i generell kod.

### Acceptanskriterier

1. Feature flag av ger bit-identiskt eller semantiskt oförändrat v1-beteende.
2. Saknad receiver, authnekande, malformed svar och timeout påverkar inte v1.
3. Generell kod innehåller ingen lokal deploymentkonfiguration.
4. Transportpaketet importerar inga agentpaket.
5. Korrelationsservicen importerar inte `linuxprocess`.
6. Agentklassificering ägs inte av `ProcessBackend`.
7. Overifierade processhints kan inte bli hård identitet eller `would_bind`.
8. Faktiska parallella Claude- och Codexsessioner kan observeras och märkas
   utan innehållsdata.
9. Inga förbjudna data skickas eller loggas.
10. Ingen kodväg kan mutera registry, slots, relay eller v2-state.
11. Rollback består enbart av att stänga feature flag; v1 kräver ingen migration.
12. Receivern startas manuellt och inget installeras eller aktiveras automatiskt.
13. Epoch och revision ägs av den långlivade bridgen; kortlivade hooks skapar
    inte egna epochs eller väggklockebaserade revisioner.

Att Paket 5 passerar dessa kriterier godkänner inte automatisk bindning. Ett
senare mutationspaket kräver separat beslut, mätdata, replaypolicy, feature flag,
rollback och end-to-end-bevis för registryisolering.

## Öppna produktbeslut

Inget produktägarbeslut blockerar Paket 4.5 eller ett strikt observe-only Paket
5. Följande beslut ska tas först när motsvarande produktfunktion planeras:

- accepterad false-positive-gräns och krav på verifierad hard identity före
  någon bindningsmutation;
- driftmodell, lagring och återställning för host-ID, producer epoch och revision
  innan en långlivad bridge installeras som produkt;
- tidpunkt och kompatibilitetsperiod för wiremigrering från legacy `ToolKind`
  till namespacad `AgentID`;
- vilka ytterligare agent-ID:n som ska ha officiellt installerade recognizers;
- v1-kompatibilitetsperiod och presentationsbeslut enligt befintliga ADR:n.

Dessa frågor får inte lösas implicit genom lokala defaults i Paket 5.
