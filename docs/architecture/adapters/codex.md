# Codex-adapter

Status: adapterkontrakt och nulägesinventering för Paket 4.5

Codex-adaptern ska följa det normativa
[integrationskontraktet](../per-instance-presence-integration-contract.md).
Nuvarande Codexintegration innehåller legacyfunktioner för v1 och wrappern;
de är inte målarkitektur för per-instance presence.

## Faktiska fält i nuvarande adapter

`internal/codexhook/hook.go` avkodar följande fält i `codexhook.Event`:

| Fält | Tillåten användning i per-instance observation |
| --- | --- |
| `hook_event_name` | Ja, endast för lifecycle-/statusmapping. |
| `session_id` | Ja, som opak agentsessionsreferens; aldrig runtime- eller instance-ID. |
| `tool_name` | Endast om en dokumenterad lifecyclemapping behöver det; dagens mapping gör inte det. |
| `turn_id` | Nej. Ignoreras av per-instance-transporten. |
| `transcript_path` | Nej. Får inte läsas eller transporteras av Paket 5. |
| `source` | Nej som identitet; nuvarande per-instance-adapter ska ignorera det. |

Endast allowlistade värden får överföras till en generell observation. Rå
payload och godtyckliga metadata-mappar är förbjudna.

## Lifecyclemapping

Nuvarande `codexhook.MapEvent` ger:

| Codex-event | Generell observation |
| --- | --- |
| `SessionStart` | `idle`, ingen hookclaim |
| `UserPromptSubmit` | `active`, hookclaim working |
| `PreToolUse` | `active`, hookclaim working |
| `PermissionRequest` | `active`, hookclaim attention |
| `PostToolUse` | `active`, hookclaim working |
| `Stop` | `idle`, vilket rensar hookclaim |
| `SessionEnd` | `ended` observation; processen avgör fortfarande runtime-exit |

Event som inte finns i tabellen är unsupported och får inte gissas till en
annan lifecycle.

## Förbjudna data och legacygräns

Codex-adaptern får inte läsa, transportera eller logga prompt, transcript,
transcript-path, turninnehåll, CWD, rå argv, kommandorad, terminalinnehåll,
användardokument eller generell miljö.

Nuvarande `SessionStore`, `PermissionWatch` och `ScanTranscript` använder
transcript-path lokalt för v1:s permission-recovery. Det är legacyagentlogik och
får inte återanvändas av Paket 5:s observation eller korrelation. Paket 5 ska
inte läsa den statefilen implicit.

`AURORA_CODEX_WRAPPER_PID` och wrapperns PID är inte generationssäker identitet:
starttid saknas och payload-/miljöpåståendet är inte serverattesterat. Wrappern
är en migrationskälla, inte ett produktkrav.

## Saknad identitet och revisionssemantik

Det observerade hookeventet innehåller ingen verifierad PID plus starttid,
ancestor chain, runtimeidentitet eller host/boot-attestering.

Codex `SessionStore` har ett lokalt `NextRevision`, men den sekvensen hör till
legacyaggregatets statefil. Den saknar beslutad producer epoch och är inte utan
vidare Paket 3:s logiska hookrevision. Att återanvända den skulle koppla det nya
observe-only-flödet till transcript-/legacy-state och göra restartsemantiken
oklar.

En långlivad bridge ska därför äga producer epoch och revision. Paket 4.5
implementerar inte lagringen. Kortlivade hooks får inte använda väggklocka eller
slumpmässig epoch per anrop som ersättning.

## Lageravvikelse och Paket 5

**Nuläge:** `internal/localhooktransport/adapters.go`, `CodexObservation`,
importerar `internal/codexhook` från transportpaketet. Som första steg under
Paket 5 ska mappingen flyttas till Codex-adapterlagret, innan en verklig hook
ansluts.

Observe-only-sändning ska vara explicit avstängd som default och best-effort.
Saknad receiver, authnekande eller timeout får inte ändra Codexhookens v1-
publicering, statefil, transcript-recovery, wrapperbeteende eller exitstatus.
Vanligt `codex` ska förbli användargränssnitt; Paket 5 får inte göra wrappern
obligatorisk.
