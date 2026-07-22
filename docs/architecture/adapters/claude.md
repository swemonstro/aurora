# Claude-adapter

Status: adapterkontrakt och nulägesinventering för Paket 4.5

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

Paket 5 ska återanvända denna semantik utan att göra observe-only-leveransen
auktoritativ för v1 eller runtime-livscykel.

## Förbjudna data

Claude-adaptern får inte läsa, transportera eller logga prompt, svar,
transcript, CWD, rå argv, kommandorad, terminalinnehåll, användardokument eller
generell miljö. Okända payloadfält ignoreras.

`AURORA_CAPTURE_HOOKS` och `claudehook.Capture` är ett explicit lokalt
diagnostikläge som kan skriva hela råpayloaden. Det ligger utanför per-instance-
transporten, får aldrig aktiveras av Paket 5 och får inte användas som
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

En framtida långlivad bridge ska äga epoch och revision. Den kortlivade
hookprocessen ska inte skapa en ny epoch per anrop. Processhint från payload är
overifierad tills mottagaren attesterat den server-side.

## Lageravvikelse och Paket 5

**Nuläge:** `internal/localhooktransport/adapters.go`, `ClaudeObservation`,
importerar `internal/claudehook` från transportpaketet. Det vänder beroendet åt
fel håll.

Som första steg under Paket 5 ska mappingen flyttas till Claude-adapterlagret,
innan en verklig hook ansluts. Adaptern får bero på en generell
`HookObservation`- och klientgräns; transporten får inte importera Claudepaketet.

Observe-only-sändning ska vara avstängd som default och best-effort. Saknad
receiver, authfel, timeout eller korrelationsfel får inte ändra befintlig
v1-publicering, statefil eller hookens exitbeteende.
