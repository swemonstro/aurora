# Paket 6: verklig lokal hook-ingress, fortsatt observe-only

Status: normativ design; planerad, inte implementerad, integrerad eller aktiv i
produktion

Paket 6 ansluter kortlivade Claude- och Codexhookprocesser till den manuellt
startade lokala presence-servern. Anslutningen är avstängd som default,
best-effort och strikt observe-only. Paketet skapar ingen automatisk bindning
och muterar eller publicerar ingen presence-state.

## 1. Versionerad ingressoperation

Paket 6 inför en separat operation med arbetsnamnet `ingest_hook_event`.
Operationen tillhör ingressprotokoll version 2. Paket 4:s befintliga
`correlate_observation` i protokoll version 1 behålls som ett separat
diagnostikkontrakt och får inte användas med dummy-epoch eller dummyrevision.

Envelopeformen är:

```json
{
  "protocol_version": 2,
  "operation": "ingest_hook_event",
  "request_id": "opaque-random-request-id",
  "payload": {
    "tool": "claude",
    "hook_session_ref": "opaque-agent-session-ref",
    "lifecycle": "active"
  }
}
```

`request_id` är ett opakt, kryptografiskt slumpat transport-ID med minst 128
bitars entropi. Det ska skapas en gång per leveransförsök, får inte härledas
från payload, session-ID, tid, path eller annat agentinnehåll och får inte
loggas av hookkommandot. Om CSPRNG:n inte kan leverera entropi ska den lokala
sändningen hoppas över best-effort; ett svagt eller deterministiskt fallback-ID
är förbjudet.

Ingresspayloaden innehåller exakt:

| Fält | Betydelse |
| --- | --- |
| `tool` | Befintlig legacy-`ToolKind`, endast `claude` eller `codex` i Paket 6. |
| `hook_session_ref` | Opak, icke-tom agentsessionsreferens; inte runtime- eller instance-ID. |
| `lifecycle` | En av `active`, `idle` eller `ended`. |

Den transportneutrala in-memory-typen ska heta
`hookadapter.IngressObservation` och ha just dessa tre fält. Den befintliga metadataförsedda
`hookadapter.Observation` får inte användas genom att fyllas med temporära
epoch-, revisions- eller tidsvärden.

`event_kind` utelämnas. Lifecyclemappingen är agentadapterns ansvar och servern
behöver inte eventnamnet för korrelation eller generell diagnostik.
`event_fingerprint` utelämnas eftersom Claude-/Codexeventen inte ger ett
universellt stabilt, innehållsfritt provider-event-ID. Ett lokalt fingeravtryck
skulle riskera att bli falsk global identitet eller deduplicera legitima
upprepade event.

Ingressen får inte innehålla:

- `ProducerEpoch`, `Revision` eller slutlig `ObservedAt`;
- `ProcessHint`, `RuntimeHint` eller `ParentOrRootPIDHint`;
- host-ID, boot-ID, processgrupp, OS-session eller terminalfingerprint;
- PID, process-starttid eller annan payloadpåstådd processidentitet;
- CWD, argv, kommandorad, prompt, transcript eller tool input;
- fri metadata, miljövariabler eller rå agenteventpayload.

Okända fält, trailing JSON, tom payload och ogiltiga enum-/identitetsvärden ska
avvisas före sequencing. Ingressoperationen är ett eget wirekontrakt och får
inte implementeras genom att fylla dagens `HookObservation` med dummyvärden som
servern senare skriver över.

Requestframen får vara högst 8 KiB och responseframen högst 4 KiB, inklusive
JSON-envelope. Identitetsgränserna ska vara minst lika strikta som det befintliga
lokala protokollets. Servern ska avkoda exakt ett JSON-värde med unknown-fields-
kontroll och får inte lagra rå requestbody efter validering.

Responseformen är:

```json
{
  "protocol_version": 2,
  "request_id": "opaque-random-request-id",
  "status": "ok",
  "error_codes": [],
  "no_binding_performed": true
}
```

`status` är en av `ok`, `duplicate`, `rejected` eller `error`.
`no_binding_performed` ska alltid finnas och vara `true`, även vid duplicate,
rejected och error. `error_codes` är en deterministiskt sorterad lista av
innehållsfria koder. Response innehåller ingen sessionsreferens,
korrelationskandidat eller process-/runtimedata. Ett malformed eller oversized
svar behandlas endast som ett best-effort-klientfel.

Efter validerat envelope ekar response samma `request_id`. Vid auth-, size-
eller decodefel före validerat envelope ska fältet utelämnas. Minsta bindande
felkodsmängd är `unsupported_protocol_version`, `unsupported_operation`,
`malformed_request`, `request_too_large`, `unknown_field`,
`invalid_request_id`, `invalid_ingress`, `unauthorized_peer`,
`request_id_conflict`, `replay_cache_full`, `sequencing_capacity_exceeded`,
`revision_overflow`, `concurrency_limit`, `request_in_progress`,
`handler_timeout` och `internal_error`. Koderna får inte bära fria strängar.

## 2. Serverägd sequencing

Den långlivade presence-servern är Paket 6:s sequencingägare. Vid serverstart
skapar den en kryptografiskt slumpad, opak `ProducerEpoch` med minst 128 bitars
entropi. Epochvärdet finns endast i minnet och delas av de hookströmmar som
serverinstansen accepterar.

Om servern inte kan skapa epoken ska den vägra starta ingressreceivern. Den får
inte använda ett konstant, tidsbaserat eller på annat sätt svagt fallbackvärde.

Servern behandlar en ny request i denna ordning:

1. autentisera peeren;
2. läsa hela den bounded requestframen och avkoda exakt ett JSON-värde strikt;
3. validera envelope och ingress;
4. kontrollera replay/conflict samt replay- och sequencingkapacitet;
5. acceptera requesten atomiskt för sequencing.

Vid steg 5, och inte tidigare, skapar servern en in-flight-reservation och
tilldelar atomiskt:

- `ProducerEpoch`: serverinstansens aktuella epoch;
- `Revision`: nästa positiva heltal för `(tool, hook_session_ref)`, med start 1;
- `ObservedAt`: serverklockan läst omedelbart vid acceptpunkten och normaliserad
  med `Round(0).UTC()`.

Obehöriga, malformed, replay-conflict- eller kapacitetsavvisade requests får
varken revision eller `ObservedAt`. Den resulterande interna `HookObservation`
valideras och lämnas därefter till runtimeobservation och observe-only-
korrelation. Klientens klocka avgör inte observationstid eller revisionsordning.

### Samtidighet och atomicitet

En gemensam låst stateövergång ska samordna replaylookup,
kapacitetskontroll, in-flight-reservation, `ObservedAt` och
revisionstilldelning. Låset ska släppas före runtimeobservation och korrelation.
Samtidiga, olika requests för samma hookström får unika, strikt ökande
revisioner i den ordning servern accepterar dem. Ordningen är lokal
acceptordning, inte ett påstående om providerordning.

Den första requesten med ett nytt request-ID reserverar ID:t vid den atomiska
acceptpunkten. En samtidig request med samma request-ID och samma canonical body
väntar inte. Den får omedelbart `status=duplicate` och den innehållsfria
felkoden `request_in_progress`; den får ingen revision och startar ingen ny
runtimeobservation eller korrelation. Samma request-ID med annan body får
`status=rejected` och `request_id_conflict` utan revision.

Reservationen får inte göras synlig förrän den ägande handlerns obligatoriska
slutförandeväg och serverdeadline är etablerade.

Den ägande requesthandlern ska inom en egen ändlig, konfigurerad serverdeadline
ersätta reservationen med ett innehållsfritt slutresultat. Senare identisk
replay inom fönstret får `status=duplicate` tillsammans med det cachade
slutresultatets felkoder och startar ingen ny behandling. Replay-TTL räknas från
slutförandet.

Alla handlerutgångar efter sequencing, inklusive klientfrånkoppling,
serverinternt fel, panic som fångas vid handlergränsen och överskriden
serverdeadline, ska deterministiskt slutföra reservationen under lås. Ett
serverinternt fel cachas som `internal_error`; deadlineöverskridande cachas som
`handler_timeout`. Den tilldelade revisionen är i båda fallen förbrukad och får
varken rullas tillbaka eller återanvändas. Serverhandlern får fortsätta efter
klientfrånkoppling endast inom sin serverdeadline. Deadlinecontexten ska
propageras till runtimeobservation och korrelation; slutförandet av replayposten
ska ske även när detta context har löpt ut.

Kontraktet kräver endast korta stateövergångar under lås, ingen blockerande
replay-synkronisering eller bakgrundsworker. En in-flight-post har exakt en
ägande handler och kan därför inte bli stale medan serverprocessen fortsätter:
handlerns obligatoriska slutförandesteg äger alla utgångar och deadlinevägen.
Om serverprocessen avslutas försvinner hela in-memory-statet och nästa start
använder en ny epoch.

Bodylikhet definieras över en serverproducerad kanonisk kodning av den
validerade trippeln `(tool, hook_session_ref, lifecycle)`, inte över råa
JSON-bytes. Replayposten behöver endast lagra request-ID, ett kryptografiskt
digest av den kanoniska kodningen, tillståndet `in_progress` eller `completed`
samt innehållsfritt status/resultat och expiry för slutförda poster. Den får
inte lagra rå payload.

### Bounds, ended och overflow

- Sequencingtabellen får innehålla högst 1024 hookströmmar.
- Replayfönstret innehåller högst 4096 requests med fem minuters TTL.
- En `ended`-observation får sin nästa revision och lämnar en tombstone med
  senaste revision i sequencingtabellen.
- Tombstones och levande strömmar evictas inte individuellt under samma epoch,
  eftersom återkomst då skulle kunna återanvända en revision.
- Om en ny hookström skulle överskrida 1024 poster avvisas ingressen med en
  innehållsfri `sequencing_capacity_exceeded`; befintliga poster ändras inte.
- Om replaycachen inte kan reservera en post avvisas en ny request innan
  revision tilldelas.
- Revisionen får inte överstiga protokollets säkra heltalsgräns `2^53-1`.
  Nästa event avvisas med `revision_overflow`; värdet får aldrig wrapa.

Paket 6 gör ingen automatisk epochrotation vid kapacitet eller overflow. En
serverrestart raderar sequencing- och replaystate och skapar en ny epoch, varpå
revisioner åter börjar på 1 inom den nya epoken. Epoch från olika serverstarter
får inte ordnas mot varandra.

Sequencingen får inte:

- mutera registry, slots, runtime- eller hookclaims;
- skrivas till fil, databas eller annan persistent state;
- publicera till relay, v2 eller annan presencebackend;
- skapa eller verkställa automatisk bindning.

## 3. Idempotensnivå

Paket 6 garanterar endast transportnära idempotens inom serverns bounded
replayfönster:

- den första accepterade requesten reserverar request-ID och får högst en
  sequencingtilldelning;
- samtidig identisk replay ger omedelbart `duplicate` plus
  `request_in_progress`, utan väntan eller ny behandling;
- identisk replay efter slutförande ger `duplicate` plus det cachade
  slutresultatet;
- identiskt request-ID och annan body ger `rejected` plus
  `request_id_conflict`;
- en request som lämnat replayfönstret behandlas inte som känd retry.

Paket 6 garanterar inte:

- deduplicering av samma provider-event från två separata hookprocesser;
- deduplicering över serverrestart;
- deduplicering utan ett stabilt provider-event-ID;
- global eller persistent exakt-en-gång-leverans.

Den avgränsningen är acceptabel eftersom Paket 6 korrelerar och kasserar
resultatet utan mutation eller publicering. En dubbel observation kan ge dubbel
diagnostik men inte dubbel presenceeffekt. Starkare, cross-process- och
cross-restart-deduplicering är ett krav före eller tillsammans med framtida
state mutation i Paket 8 och får inte retroaktivt påstås finnas i Paket 6.

## 4. Bounded best-effort-klient

### Feature flag

`AURORA_LOCAL_HOOK_ENABLED` är av som default. Efter borttagning av omgivande
whitespace aktiverar endast `1` eller ASCII-case-insensitive `true`. Alla andra
värden betyder av. När flaggan är av får hookkommandot inte läsa socketconfig,
skapa request-ID eller försöka ansluta.

### Socket

`AURORA_LOCAL_HOOK_SOCKET` får ange en explicit absolut och normaliserad
socketpath. Om den saknas får klienten endast använda
`$XDG_RUNTIME_DIR/aurora/presence-hook.sock` när `XDG_RUNTIME_DIR` är absolut
och uppfyller den privata katalogpolicyn. `/tmp`, relativ path, nuvarande
arbetskatalog och repositorypath är aldrig produktdefault. Saknad säker path
gör den lokala leveransen inaktiv för invocationen.

### Deadline och resurser

En invocation gör exakt ett synkront, bounded leveransförsök. När feature flag
är på startar en absolut totaldeadline på 100 ms innan socketconfig läses eller
annan lokal transportförberedelse görs:

| Gräns | Maximalt tak |
| --- | ---: |
| Total | 100 ms |
| Connect | 20 ms |
| Write | 20 ms |
| Read | 60 ms |

Connect-, write- och readgränserna är individuella maximala tak, inte
reserverade tidsandelar. Varje fas begränsas dessutom av den tid som återstår
till totaldeadlinen. Configtolkning, pathsäkerhetskontroll, requestkodning,
socketanslutning, peer-verifiering, write, read, responsdekodning och stängning
ingår alla i samma totalbudget.

Anroparens context kan endast förkorta total- och fasgränserna. Klienten gör
ingen retry, bakgrundskö eller durable delivery och lämnar ingen frikopplad
goroutine efter return. Context cancellation ska stänga en redan öppnad
anslutning; ingen fas eller cleanup får fortsätta utanför den kortaste gällande
deadlinen.

### Fel- och exitpolicy

Alla lokala config-, path-, dial-, auth-, timeout-, broken-pipe-, protocol-,
response- och serverfel är best-effort. De får inte ändra hookkommandots
befintliga exitstatus, v1-state, relaypublicering, transcript-recovery eller
wrapperbeteende. Unsupported event, saknad session och ogiltig neutral ingress
skickas inte.

### Debug

Debug är opt-in och innehållsfri. En debugpost får endast innehålla:

- agenttyp;
- fas, exempelvis `config`, `connect`, `write`, `read` eller `response`;
- grov felklass;
- latency bucket: `lt_10ms`, `lt_50ms`, `lt_100ms` eller `timeout`.

Debug får aldrig innehålla session-ID, request-ID, socketpath, råa felsträngar,
payload, prompt, transcript, CWD, argv, tool input, miljö eller metadata.

## 5. Socket- och trustgräns

Linux-MVP:n använder Unix stream socket.

### Bindande krav

- Socketpathen ska vara absolut och normaliserad; relativ path och `/tmp`-
  default är förbjudna.
- Den säkerhetsrelevanta, användarägda delen av katalogkedjan ska öppnas
  no-follow. Symlänkar får inte följas där den effektiva användaren kan påverka
  komponenterna.
- Den användarägda runtimekatalogen och Aurora-underkatalogen ska ägas av
  förväntad effektiv UID och vara privata med mode `0700`.
- Systemägda prefixkomponenter ska verifieras som icke manipulerbara av den
  effektiva användaren; de behöver inte vara privata användarkataloger.
- Socketfilen ska vara en Unix socket, ägas av förväntad effektiv UID och ha
  mode `0600`.
- Servern ska autentisera klienten med Linux `SO_PEERCRED` före payloaddecode
  och som default endast acceptera samma effektiva UID.
- Klienten ska efter connect verifiera serverpeerens UID mot förväntad effektiv
  UID.

### Defense in depth

Där stödd Linuxmiljö och tillgängliga API:n medger en robust implementation bör
klienten dessutom jämföra socketpathens device/inode före och efter connect och
utföra ytterligare kontroller mot socket replacement. Dessa kontroller
kompletterar peer-UID-verifieringen men är inte ensam trust boundary eller ett
absolut exitkriterium för Paket 6.

Same-effective-UID är en lokal trust boundary, inte processidentitet eller
attestering av payload. Pathbaserad Unix-socket kan minska men inte helt
eliminera TOCTOU, särskilt mot en angripare med samma UID. Den residualrisken
finns kvar även med device/inode-kontroll, ska dokumenteras och får inte döljas
genom att session-ID eller peer-PID kallas hård agentidentitet.

Socketmiljövariabeln är explicit operatörskonfiguration. En manipulerad
hookmiljö kan styra klienten till en annan socket som ägs av samma UID; peer-
och pathkontroller kan inte skilja två processer med samma UID. Paket 6 minskar
konsekvensen genom den minimala ingressen, innehållsfri debug och strikt
observe-only-semantik, men påstår inte att residualrisken är eliminerad.

## 6. Claude- och Codexscope

### Claude

Den befintliga Claude-mappingen får återanvändas efter projektion till den
minimala neutrala ingressen. Faktisk eventleverans och eventföljd ska verifieras
manuellt med en isolerad Claude-konfiguration före soak. `AURORA_CAPTURE_HOOKS`
och raw capture ingår inte i Paket 6 och får inte vara observations- eller
debugkälla.

### Codex

Endast aktuellt officiellt dokumenterade Codex lifecycle-hooks är primär
integration. Faktisk `hooks.json`/`config.toml`, stdinform och eventleverans ska
verifieras manuellt. Codex command hooks är synkrona, vilket gör 100 ms-budgeten
bindande.

Ett providerlevererat `SessionEnd` får inte antas. Den nuvarande wrapperns
genererade `SessionEnd` är en syntetisk legacykälla för v1. Den får inte blandas
med officiella Codexevent i Paket 6 utan ett separat, versionssatt source- och
policybeslut.

Eftersom ingressen inte har ett sourcefält ska Paket 6:s Codexadapter inte
skicka `SessionEnd` lokalt. Wrapperns befintliga v1-hantering fortsätter
oberoende och oförändrad.

## 7. Paket- och beroendegränser

```text
aurora-claude-hook / aurora-codex-hook  (composition roots)
    -> agentspecifikt event
    -> transportneutral, sanerad ingress
    -> generell localhooktransport-klient
    -> Unix socket

aurora-presence-local-server            (composition root)
    -> autentisera ingress
    -> serverägd sequencing
    -> intern HookObservation
    -> observe-only korrelation
    -> no_binding_performed=true
```

- `claudehook` och `codexhook` får inte importera `localhooktransport`.
- `hookadapter` ska förbli transportneutral.
- `localhooktransport` ska förbli agentneutral och får inte importera Claude,
  Codex eller `linuxprocess`.
- Klienten får inte känna till agentspecifika event.
- Cmd-lagret väljer agentadapter och transport och är composition root.
- Ingen importcykel eller agentspecifik default får införas i generiska paket.

Avsedd kodplacering är:

| Paket/kommandon | Paket 6-ansvar |
| --- | --- |
| `internal/hookadapter` | Ny separat `IngressObservation`; ingen socket-, request- eller sequencingkunskap. |
| `internal/claudehook`, `internal/codexhook` | Mappa verifierade agentevent till neutral ingress och kassera övriga fält. |
| `internal/localhooktransport` | Version 2-envelope, bounded agentneutral klient, Linux socketverifiering, receiver och in-memory-sequencer. |
| `cmd/aurora-claude-hook`, `cmd/aurora-codex-hook` | Feature flag, composition och best-effort-send utan ändring av befintligt v1-resultat. |
| `cmd/aurora-presence-local-server` | Komponera auth, ingressreceiver, sequencing, intern observation och observe-only-korrelation. |

`instancepresence`, `instanceregistry`, `publish`, relay-/v2-paket,
runtime-recognition, schemafiler, deployment och systemd lämnas orörda.

## 8. Rekommenderad commitordning

1. `docs: define package 6 ingress and sequencing contract`
2. `harden local observe-only hook client`
3. `add receiver-owned in-memory sequencing`
4. `connect Claude hook behind disabled feature flag`
5. `connect Codex hook behind disabled feature flag`
6. `document manual verification and rollout evidence`

Varje kodcommit ska kunna granskas och testas separat. Ingen senare commit får
krävas för att en tidigare commit ska bevara v1, vara bounded eller förbli
observe-only.

## 9. Manuell verifiering och rollout

1. Integrera kod med feature flag av.
2. Starta presence-servern manuellt i foreground med explicit privat testsocket
   under en verifierad runtimekatalog.
3. Skicka syntetiska, sanerade Claude- och Codexpayloads via stdin med isolerade
   v1-statefiler och utan raw capture.
4. Verifiera server-off, permission denied, timeout, malformed response,
   socketbyte och parallella hookprocesser.
5. Verifiera en faktisk Claude-session med isolerad hookkonfiguration.
6. Verifiera en faktisk Codex-session med isolerad officiell hookkonfiguration;
   anta inte provider-`SessionEnd`.
7. Kör två parallella Claude- och två parallella Codexsessioner och märk
   resultaten utan innehållsdata.
8. Genomför ett längre observe-only soak test med servern fortsatt manuellt
   startad.

Ingen systemdändring, installation eller produktionsaktivering ingår. Rollback
är endast att stänga `AURORA_LOCAL_HOOK_ENABLED`; v1 kräver ingen migration.

## 10. Exitkriterier

Paket 6 är klart först när:

- feature flag är av som default;
- inget lokalt transport- eller serverfel kan få Claude-/Codexhooken att
  misslyckas eller ändra v1;
- den lokala totalbudgeten 100 ms hålls;
- inga känsliga fält lämnar composition root;
- klient och server uppfyller samtliga bindande socket- och UID-krav;
- tillgängliga defense-in-depth-kontroller är prövade och evidensen anger vilka
  som stöds; device/inode-jämförelse är inte ensam ett absolut exitkriterium;
- sequencing är serverägd, bounded, in-memory och kontraktstestad;
- inga dummy-epochs eller dummyrevisioner skickas;
- varje ingressresponse har `no_binding_performed=true`;
- faktisk Claude- och Codexeventleverans är verifierad;
- parallella sessioner, server-off och soak är verifierade;
- ingen registry-, slot-, `Publisher`-, relay- eller v2-koppling finns.

Godkänt Paket 6 godkänner inte bindning eller mutation. Paket 7 och 8 kräver
separata beslut och bevis.

Paket 7 får inte påbörjas som bindingarbete förrän samtliga kriterier ovan är
uppfyllda och evidensen är granskad.

## 11. Öppna verifieringsfrågor

De normativa besluten ovan är låsta. Följande är kvarvarande evidensfrågor, inte
tillstånd att utöka payload eller scope:

- exakt aktuell Claude-hookkonfiguration och verklig eventföljd;
- exakt aktuell officiell Codex-konfiguration, eventuppsättning och stdinform;
- vilka Linux-API:n den stödda miljön medger för klientens peer- och
  före/efter-inodekontroll;
- mätta latencyfördelningar och kapacitetsutfall under parallell soak;
- om 1024 sequencingströmmar och 4096 replayposter räcker i observerad drift.

Om någon bound behöver ändras ska det ske genom en explicit kontraktsändring
före implementation eller rollout, inte genom en lokal deploymentdefault.
