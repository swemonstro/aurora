# Linux-processbackend

Status: backendkontrakt efter Paket 5, integrerat i `main` vid `0b0fc65`

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

## Integrerat nuläge

Paket 5 genomförde ansvarsflytten. `internal/linuxprocess` samlar och sanerar
Linuxprocessdata till en intern OS-neutral recognition-input men äger inte
Claude-/Codexklassificering eller familjebildning. Agentreglerna finns i
agentägda recognizers och `internal/runtimerecognition` bygger generiska
runtimekandidater. Kommandolagret komponerar delarna och sammanför diagnostik.

Proc-root, USER_HZ och boot-ID är legitima Linuxbackendvärden när de kommer från
explicit backendconfig. De är inte produktdefaults och får inte läcka in i det
generella kontraktet.

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
identitet. Den reducerade modellen infördes i Paket 5 och är ett internt
recognitioninput, inte ett wireformat.

## Genomförd refaktor i Paket 5

1. ProcReader, stat-parser, boot/starttidsnormalisering och snapshotdiff stannar
   i `internal/linuxprocess`.
2. Atomisk `RuntimeSnapshot` levererar processnapshot och samma samples BootID.
3. Claude- och Codexklassificering ägs av agentpaketens recognizers.
4. `internal/runtimerecognition` producerar generiska och konservativa
   runtimeobservationer.
5. Korrelationsservicen beror på den OS-neutrala runtimekällan.
6. Observe-CLI:t är kompositions- och diagnostikverktyg ovanpå dessa lager.

Paket 6 ändrar inte denna gräns. Verklig hooking skickar inga processhints och
kan inte göra samma UID eller en agentsessionsreferens till hård identitet.
