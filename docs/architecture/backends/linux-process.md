# Linux-processbackend

Status: målgräns och nulägesinventering för Paket 4.5

Det normativa lagerkontraktet finns i
[integrationskontraktet](../per-instance-presence-integration-contract.md).
Det här dokumentet beskriver Linuximplementationen och utgör inte ett löfte om
Windows- eller macOS-stöd.

## Ansvar

Linux-processbackenden får läsa Linux `/proc` read-only och översätta följande
till OS-neutrala processobservationer:

- PID och parentrelation;
- process-starttid normaliserad från boot time och startticks;
- bootidentitet från explicit config eller avgränsad boot-ID-källa;
- opak executable-/ägaridentitet;
- processgrupp, OS-session och terminalfingerprint;
- konservativ osäkerhet när en process försvinner eller inte kan läsas;
- generationssäkra exits mellan snapshots.

Backenden ansvarar även för rotbegränsad no-follow-läsning, read limits,
korrekt parsning av `/proc/<pid>/stat`, deterministisk sortering och att rå
plattformdata inte läcker vidare.

Den får inte:

- bestämma att en process är Claude, Codex, Hermes eller annan agent;
- äga agenters executable-, wrapper-, Node- eller native-regler;
- bygga agentfamiljer eller tilldela agent-ID;
- läsa prompt, transcript, CWD, terminalinnehåll eller generell miljö;
- exponera rå argv eller full kommandorad;
- registrera instanser, mutera registry eller publicera till relay.

## Nuläge

`internal/linuxprocess` implementerar en säker proc-läsare men omfattar också
agentigenkänning och familjebygge. Följande är övergångsformer:

| Fil och symbol/ansvar | Avvikelse | När den hanteras |
| --- | --- | --- |
| `internal/linuxprocess/adapter.go`, `Adapter.Observe` | Klassificerar varje `rawProcess`, bygger familjer och räknar Claude-/Codexfamiljer efter att snapshoten skapats. | Under Paket 5, före hookanslutning. |
| `internal/linuxprocess/adapter.go`, `argvSignals` | Reducerar lokalt argv-prefix med Claude-/Codexspecifika paketmarkörer. Dataminimeringen är bra, men agentkunskapen ligger i backend. | Under Paket 5, utan att rå argv flyttas ut. |
| `internal/linuxprocess/classify.go`, `classify`, `toolFromArgv` | Äger Claude-/Codexbinärer, paketmarkörer och wrapperroller. | Under Paket 5, före hookanslutning. |
| `internal/linuxprocess/families.go`, `buildFamilies` | Bygger agentfamiljer och skapar tool-bundna kandidat-ID:n. | Under Paket 5, före hookanslutning. |
| `internal/linuxprocess/types.go`, `Family`, `UncertainFamily`, `Summary` och agent-reason codes | Paketets publika sampleform blandar procdiagnostik med agentresultat. | Under Paket 5, före hookanslutning. |
| `cmd/aurora-presence-observe`, rapporttyper | Visar Claude-/Codexfamiljer och är därmed ett kompositions-/diagnostikverktyg ovanpå backend och recognizers. | Kan behållas internt efter att dess beroenden komponerats om. |

Detta är arkitekturella avvikelser, inte bevis för Blue1-låsning. Proc-root,
USER_HZ och boot-ID är legitima Linuxbackendvärden när de kommer från explicit
backendconfig.

## Målflöde

```text
Linux /proc
    |
    v
Linux ProcessBackend
    |  ProcessSnapshot + plattformsdiagnostik
    v
installerade AgentRuntimeRecognizer
    |  RuntimeCandidate / uncertain
    v
RuntimeObservationSource
    |
    v
OS-neutral korrelationsservice
```

Recognizers ska tolka validerade opaka executable-/launchidentiteter. Rå argv
får inte bli recognizerinput. Om Linux behöver reducera ett begränsat argv-
prefix ska reduceringen drivas av en explicit allowlist från kompositions-/
adapterlagret och omedelbart kassera allt som inte blir en opak tillåten
identitet. Den exakta typen ska kontraktstestas innan den införs; Paket 4.5
lägger inte till en halv sådan modell.

## Minimal framtida refaktor

1. Behåll procReader, stat-parser, boot/starttidsnormalisering och snapshotdiff i
   `internal/linuxprocess`.
2. Låt `Adapter.Observe` eller `Snapshot` returnera processnapshot och separat
   Linuxdiagnostik, utan agentfamiljer.
3. Flytta Claude- och Codexklassificering till agentägda recognizers med samma
   fixtures och dataminimering.
4. Lägg en kompositionskomponent som kör alla installerade recognizers,
   deduplicerar/markerar konflikter och producerar generiska
   `RuntimeObservation`-värden.
5. Låt Paket 4:s korrelationsservice bero på denna runtimekälla.
6. Behåll observe-CLI:t som ett internt verktyg ovanpå hela kompositionen.

Refaktorn ska göras sammanhållet som första steg under Paket 5 och före verklig
hookanslutning. Paket 4.5 flyttar ingen kod och ändrar inget observerbeteende.
