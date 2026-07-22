# Integrationskontrakt för per-instance presence

Status: normativt integrationskontrakt för Paket 4.5–6

Detta dokument definierar produktgränsen som gäller efter Paket 5 och under
Paket 6.
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
  transportneutral ingress;
- uttryckligen allowlista de fält som används;
- ignorera eller avvisa förbjudna fält utan att vidarebefordra rå payload;
- vara oberoende av transportbackend.

Transportpaket FÅR INTE importera Claude-, Codex-, Hermes- eller andra
agentpaket. Agentpaket och hookadaptern FÅR INTE importera
`localhooktransport`. Cmd-lagret ska vara composition root och projicera den
neutrala ingressen till transportens generiska klientgräns.

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
`exact`, `strong` eller `would_bind_under_current_threshold`. Paket 6 får inte
skapa hård identitet genom att kopiera payloadens PID/starttid till dagens
`ProcessHint` eller `RuntimeHint`.

`weak`, `ambiguous`, `rejected` och overifierade resultat FÅR ALDRIG mutera
registry. Paket 6 får över huvud taget inte göra registrymutation.

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
mappingen ligga i en adapter/projektion. Paket 6 får initialt stödja de två
legacyvärdena, men får inte definiera dem som slutlig generell agentmodell eller
lägga fler slutna agentfall i transportkärnan.

## Host-ID, producer epoch och revision

- **Host-ID** ägs av installationen. Det SKA vara stabilt för samma lokala
  Aurora-installation, opakt och oberoende av hostname, MAC-adress och
  användarsökväg.
- **Producer epoch** ägs av den långlivade mottagare som producerar interna
  hookobservationer. I Paket 6 är detta presence-servern. En ny epoch skapas vid
  serverstart, inte av varje kortlivad hookprocess.
- **Revision** ökar monotont för en logisk hookström inom samma producer epoch.
  Revisioner från olika epochs jämförs inte.
- **Kortlivade hookprocesser** ska lämna event till den långlivade ägaren och
  behöver inte själva upprätthålla permanent revisionsstate.

Paket 6 låser ägarskapet: den manuellt startade, långlivade presence-servern
äger en kryptografiskt slumpad in-memory-epoch per serverstart, monotona
revisioner per `(tool, hook_session_ref)` och slutlig observationstid. Revision
och `ObservedAt` tilldelas vid samma atomiska acceptpunkt efter peer-auth,
strikt frame/decode/validering samt replay-, conflict- och kapacitetskontroll,
men före runtimeobservation och korrelation. Avvisade requests får ingetdera.
Restart skapar en ny epoch. Hookens ingress innehåller inte dessa värden. Paket
6 får inte lösa detta med per-invocation-epoch, väggklockerevision, dummyvärden
som skrivs över eller genom att återanvända legacyagenternas statefiler.

Sequencing- och replaystate är bounded och endast in-memory. Exakta bounds,
overflow-, ended- och replayregler finns i
[Paket 6-kontraktet](per-instance-presence-package-6.md).

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
`presence.Snapshot`, inte ett per-instance-v2-publisherkontrakt. Paket 6 ska inte
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

## Paket 5: integrerade refaktorer

Refaktorerna A–C är implementerade och integrerade i `main` vid `0b0fc65`:

| Refaktor | Integrerat resultat |
| --- | --- |
| A. OS-neutral runtimekälla | Korrelationsservicen konsumerar `RuntimeObservationSource`; Linuxsnapshot och recognition komponeras utanför servicen. |
| B. Agentadapters ur transport | Claude- och Codexmapping ägs av agentpaketen; hookadaptern är transportneutral och transporten agentneutral. |
| C. Recognizers ur Linuxbackend | Linuxbackenden samlar processdata; agentrecognizers och familjebildning ligger i agentägda respektive OS-neutrala lager. |

Paket 5 anslöt ingen verklig hook och ändrade inte wireformat, registry, v1,
relay, persistence, installation eller produktion. Se
[Paket 5](per-instance-presence-package-5.md).

Följande senare refaktorer är fortsatt separata:

| Refaktor | Klassificering |
| --- | --- |
| Transportneutral principal före annan transportbackend eller stabilt auth-API | uppskjuten |
| Delning av klient-, server-, receiver- och protokollconfig | tillåten i Paket 6 endast där den bounded klienten kräver det |
| Migrering från legacy `ToolKind` till namespacad AgentID | eget versionssatt kompatibilitetspaket |
| Registry bort från wiretyper | före aktivt v2-API eller Paket 8, inte i Paket 6 |

## Paket 6: bindande avgränsning

Paket 6 ansluter verkliga Claude- och Codexevent genom den separata versionerade
operationen `ingest_hook_event`. Den kortlivade hookprocessen skickar endast
`tool`, `hook_session_ref` och `lifecycle`. Den långlivade presence-servern
tillför epoch, revision och slutlig observationstid före korrelation.

Det fullständiga ingress-, sequencing-, idempotens-, klient-, socket- och
exitkontraktet finns i
[Paket 6](per-instance-presence-package-6.md) och är bindande.

Paket 6 får:

- införa den minimala transportneutrala ingressmodellen och en versionerad lokal
  operation;
- hårdgöra den generella lokala klienten;
- hålla bounded sequencing och replay endast i serverminne;
- komponera agentadapter och transport i hookkommandona bakom en avstängd
  feature flag;
- utföra innehållsfri diagnostik, manuell verifiering och observe-only soak.

Paket 6 får inte:

- mutera registry, slots, hookclaims eller runtimestate;
- persistera sequencing-, replay- eller presence-state eller införa durable
  retry/bakgrundskö;
- använda `Publisher`, relay eller v2-publicering;
- skapa automatisk bindning;
- ändra v1:s state, publicering eller exitbeteende;
- installera, daemonisera eller systemdaktivera server eller hooks;
- transportera prompt, transcript, CWD, argv, kommandorad, tool input,
  terminalinnehåll, generell miljö eller fri metadata;
- skapa epoch/revision i kortlivade hooks eller skicka dummyvärden.

Godkänt Paket 6 godkänner inte bindning eller mutation. Säker binding policy
och korrelationslivscykel hör till Paket 7; registry-/slotmutation hör till
Paket 8.

## Öppna produktbeslut efter Paket 6

Följande ska beslutas före motsvarande framtida funktion:

- accepterad false-positive-gräns och verifierad hard identity före Paket 7;
- starkare cross-process- och cross-restart-deduplicering före eller tillsammans
  med Paket 8;
- driftmodell och persistent recovery innan en bridge installeras som produkt;
- kompatibilitetsperiod för migrering från legacy `ToolKind` till AgentID;
- ytterligare officiellt installerade agentrecognizers;
- v1-kompatibilitetsperiod och presentation enligt befintliga ADR:n.

Dessa frågor får inte lösas implicit genom lokala defaults i Paket 6.
