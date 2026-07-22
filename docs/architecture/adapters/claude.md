# Claude-adapter

Status: adapterkontrakt efter Paket 5 och normativ ingressgräns för Paket 6

Claude-adaptern ska följa det normativa
[integrationskontraktet](../per-instance-presence-integration-contract.md).
Dokumentet beskriver observerade fält i nuvarande kod, inte ett löfte om att
Claude alltid levererar samma payload på alla versioner eller plattformar.

## Faktiska fält i nuvarande adapter

`internal/claudehook/hook.go` avkodar följande fält i `claudehook.Event`:

| Fält | Tillåten användning i per-instance observation |
| --- | --- |
| `hook_event_name` | Ja, endast för lifecycle-/statusmapping. |
| `session_id` | Ja, som opak agentsessionsreferens; aldrig runtime- eller instance-ID. |
| `notification_type` | Ja, kortvarigt för att mappa notification till attention eller idle; ska inte transporteras som fri metadata. |
| `tool_name` | Ja, kortvarigt för `AskUserQuestion`-mapping; ska inte transporteras som fri metadata. |

Nuvarande JSON-parser ignorerar fält som inte finns i `Event`. En framtida
observe-only-adapter får därför aldrig vidarebefordra den råa payloaden eller en
godtycklig metadata-map. Endast ovanstående uttryckligt tillåtna värden får nå
mappingen.

## Lifecyclemapping

Nuvarande `claudehook.MapEvent` ger följande generella betydelse:

| Claude-event | Generell observation |
| --- | --- |
| `UserPromptSubmit` | `active`, hookclaim working |
| `Notification/permission_prompt` | `active`, hookclaim attention |
| `Notification/idle_prompt` | `idle`, vilket rensar hookclaim |
| annan `Notification` | `active`, hookclaim attention |
| `PreToolUse/AskUserQuestion` | `active`, hookclaim attention |
| `PostToolUse/AskUserQuestion` | `active`, hookclaim working |
| `Stop` | `idle`, vilket rensar hookclaim |
| `StopFailure` | `active`, hookclaim error |
| `SessionEnd` | `ended` observation; processen avgör fortfarande runtime-exit |

Paket 6 ska återanvända denna semantik utan att göra observe-only-leveransen
auktoritativ för v1 eller runtime-livscykel.

## Förbjudna data

Claude-adaptern får inte läsa, transportera eller logga prompt, svar,
transcript, CWD, rå argv, kommandorad, terminalinnehåll, användardokument eller
generell miljö. Okända payloadfält ignoreras.

`AURORA_CAPTURE_HOOKS` och `claudehook.Capture` är ett explicit lokalt
diagnostikläge som kan skriva hela råpayloaden. Det ligger utanför per-instance-
transporten, får aldrig aktiveras av Paket 6 och får inte användas som
observationskälla.

## Saknad identitet och revision

Det observerade eventkontraktet innehåller ingen:

- PID plus starttid;
- verifierad ancestor chain;
- runtimeidentitet eller attesterad host/boot-koppling;
- producer epoch;
- monotont hookrevisionsnummer;
- idempotensnyckel med definierad restartsemantik.

`SessionStore` lagrar session-ID, aggregatstatus och tid för v1 men ingen
generationssäker processidentitet eller Paket 3-revision. Session-ID är
verktygsägt och räcker inte som hård identitet.

Presence-servern ska i Paket 6 äga producer epoch, revision och slutlig
observationstid. Den kortlivade hookprocessen får inte skapa eller skicka dessa
värden. Process- och runtimehints ingår inte i ingressen.

## Lagergräns efter Paket 5

Paket 5 flyttade Claude-mappingen till det agentägda paketet. `claudehook`
mappar nu agentens event till den transportneutrala `hookadapter.Observation`;
varken `claudehook` eller `hookadapter` importerar `localhooktransport`.

Under Paket 6 ska kommandolagret projicera den neutrala ingressen till
`ingest_hook_event` och använda den generella lokala klienten. Agentpaketet får
inte känna till requesten, socketen eller transportens wiremodell.

Observe-only-sändning ska vara avstängd som default och best-effort. Saknad
receiver, authfel, timeout eller korrelationsfel får inte ändra befintlig
v1-publicering, statefil eller hookens exitbeteende.

Faktiska Claudeevent och den konfiguration som levererar dem ska verifieras
manuellt innan Paket 6 godkänns. Rå capture ingår inte i verifieringen.
