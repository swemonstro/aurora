# Codex-adapter

Status: adapterkontrakt efter Paket 5 och normativ ingressgräns för Paket 6

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
| `transcript_path` | Nej. Får inte läsas eller transporteras av Paket 6. |
| `source` | Nej som identitet; nuvarande per-instance-adapter ska ignorera det. |

Endast allowlistade värden får överföras till en generell observation. Rå
payload och godtyckliga metadata-mappar är förbjudna.

## Lifecyclemapping

Nuvarande `codexhook.MapEvent` kan mappa:

| Codex-event | Generell observation |
| --- | --- |
| `SessionStart` | `idle`, ingen hookclaim |
| `UserPromptSubmit` | `active`, hookclaim working |
| `PreToolUse` | `active`, hookclaim working |
| `PermissionRequest` | `active`, hookclaim attention |
| `PostToolUse` | `active`, hookclaim working |
| `Stop` | `idle`, vilket rensar hookclaim |
| `SessionEnd` | `ended`; endast syntetisk legacykälla från wrappern, inte ett officiellt provider-event |

Event som inte finns i tabellen är unsupported och får inte gissas till en
annan lifecycle.

Paket 6:s primära källa är endast de lifecycle-hooks som verifieras som
officiellt dokumenterade i den aktuella Codexversionen. `SessionEnd` får inte
antas finnas som provider-hook. Wrappergenererad `SessionEnd` är en separat
syntetisk legacykälla och får inte blandas med officiella event utan ett eget
source- och policybeslut.

Eftersom Paket 6 avsiktligt saknar sourcefält ska dess neutrala ingressadapter
avvisa `SessionEnd` för Codex. Legacy-v1-flödet får fortsatt behandla wrapperns
syntetiska event oberoende; det eventet får bara tas in i Paket 6 efter ett
separat kontraktsbeslut.

## Förbjudna data och legacygräns

Codex-adaptern får inte läsa, transportera eller logga prompt, transcript,
transcript-path, turninnehåll, CWD, rå argv, kommandorad, terminalinnehåll,
användardokument eller generell miljö.

Nuvarande `SessionStore`, `PermissionWatch` och `ScanTranscript` använder
transcript-path lokalt för v1:s permission-recovery. Det är legacyagentlogik och
får inte återanvändas av Paket 6:s observation eller korrelation. Paket 6 ska
inte läsa den statefilen implicit.

`AURORA_CODEX_WRAPPER_PID` och wrapperns PID är inte generationssäker identitet:
starttid saknas och payload-/miljöpåståendet är inte serverattesterat. Wrappern
är en migrationskälla, inte ett produktkrav.

## Legacy notify-brygga

Codex CLI:s legacy `notify` kör ett externt program och skickar notifieringens
JSON som argv-argument. Auroras lokala Codexhook (`aurora-codex-hook-local`)
förväntar sig däremot JSON på stdin. Produktionsbryggan är därför
`aurora-codex-notify`: den väljer endast sista argv-argumentet, skriver det
bytekorrekt till hookens stdin och skriver inte råpayloaden till fil, stdout,
stderr eller logg. Saknad payload är best-effort no-op.

`agent-turn-complete`-notify är för sen för första varvets `working`-signal.
`bin/aurora-codex` injicerar därför också Codex command hooks för
`SessionStart`, `UserPromptSubmit`, `PermissionRequest`, `PreToolUse`,
`PostToolUse` och `Stop`. De command-hookeventen levereras före turn-complete,
har provider-satt `session_id` och går direkt till samma lokala hook via stdin.
Det ger första verkliga uppgiften sekvensen `idle -> working -> idle` utan att
läsa Codex sessionfiler, transcriptinnehåll, prompt eller agentsvar.

Wrappern injicerar även Auroras notify-adapter med `-c notify=...` för nya
sessioner och `resume`/`resume --last`. Användaren behöver inte ändra sin
personliga `~/.codex/config.toml`, flytta tokens eller dela konfiguration mellan
standardprofilen och `CODEX_HOME=~/.codex-business`.

Aurora-wrappern läser inte, skriver inte och mergar inte användarens
`config.toml`. För Aurora-startade sessioner är wrapperns `-c notify=...` det
medvetna invokationslagret ovanför personlig config, så en befintlig
user-level `notify` ersätts under just den wrapperstarten utan att filen ändras.

Om användaren uttryckligen skickar en CLI-config för `notify`, `hooks` eller
`features.hooks` vidarebefordrar wrappern det valet oförändrat och lägger inte
till motsvarande Aurora-default. Det undviker dubbla notify-definitioner och
låter ett medvetet användarval vara högsta invokationslager.

Om `CODEX_HOME/hooks.json` redan innehåller Auroras lokala hookkommando för alla
sex lifecycle-eventen ovan lägger wrappern inte till ett andra likadant
lifecycle-lager. Det bevarar befintliga Blue1-profiler utan dubbel watcher eller
dubbla revisioner från samma provider-event.

Normal start sker via Blue1-kommandot:

```sh
b1 <workstream> codex
```

eller direkt från checkouten:

```sh
/srv/dev/aurora/bin/aurora-codex
CODEX_HOME="$HOME/.codex-business" /srv/dev/aurora/bin/aurora-codex resume --last
```

Efter framtida deploy kan en read-only Blue1-driftverifiering göras genom att
läsa snapshotsfilen före, under och efter en ny Codex-turn och jämföra
Codexinstansens `state`, `revisions` och `state_changed_at`:

```sh
jq '
  .instances[]
  | select(.tool == "codex")
  | {
      instance_id,
      state,
      revisions,
      state_changed_at
    }
' /run/aurora/snapshot.json
```

Förväntad signal är att samma Codexinstans går från `idle` till `working` under
första verkliga uppgiften och tillbaka till `idle` när varvet avslutas, med
ökande hookrevisioner. Testet får inte aktivera rå capture eller skriva
notify-/hookpayloads till disk.

## Saknad identitet och revisionssemantik

Det observerade hookeventet innehåller ingen verifierad PID plus starttid,
ancestor chain, runtimeidentitet eller host/boot-attestering.

Codex `SessionStore` har ett lokalt `NextRevision`, men den sekvensen hör till
legacyaggregatets statefil. Den saknar beslutad producer epoch och är inte utan
vidare Paket 3:s logiska hookrevision. Att återanvända den skulle koppla det nya
observe-only-flödet till transcript-/legacy-state och göra restartsemantiken
oklar.

Presence-servern ska därför äga producer epoch, revision och slutlig
observationstid. Paket 6 håller denna sequencing bounded i minnet och
persisterar den inte. Kortlivade hooks får varken skapa eller skicka dessa
värden.

## Lagergräns efter Paket 5

Paket 5 flyttade Codex-mappingen till det agentägda paketet. `codexhook` mappar
nu agentens event till den transportneutrala `hookadapter.Observation`; varken
`codexhook` eller `hookadapter` importerar `localhooktransport`.

Under Paket 6 ska kommandolagret projicera den neutrala ingressen till
`ingest_hook_event` och använda den generella lokala klienten. Agentpaketet får
inte känna till requesten, socketen eller transportens wiremodell.

Observe-only-sändning ska vara explicit avstängd som default och best-effort.
Saknad receiver, authnekande eller timeout får inte ändra Codexhookens v1-
publicering, statefil, transcript-recovery, wrapperbeteende eller exitstatus.
Vanligt `codex` ska förbli användargränssnitt; Paket 6 får inte göra wrappern
obligatorisk.

Aktuell officiell hookkonfiguration, faktiska eventnamn och synkron
kommandoleverans ska verifieras manuellt. Den bindande lokala totalbudgeten är
100 ms och rå payload får inte fångas för att genomföra verifieringen.
