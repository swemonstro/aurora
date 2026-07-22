# Paket 4: säker lokal observe-only hooktransport

Paket 4 finns i `internal/localhooktransport`. Det tar emot exakt en sanerad
hookobservation per lokal Unix-anslutning, autentiserar peer före decode, tar en
read-only Paket 2-snapshot och kör Paket 3-korrelation. Svaret är endast ett
förslag och har alltid `no_binding_performed=true`. Ingen registry-, slot-,
relay-, heartbeat- eller v1-kod anropas.

## Trust boundaries och socket

Linuxservern lyssnar endast på en explicit Unix-socket. Om `-socket` saknas
används `$XDG_RUNTIME_DIR/aurora/presence-hook.sock`; om variabeln saknas eller
inte är absolut krävs en explicit sökväg. Ingen TCP-fallback finns.

Hela katalogkedjan öppnas komponentvis med `O_DIRECTORY|O_NOFOLLOW` och varje
öppnad deskriptor verifieras med `fstat`. Alla komponenter utom `/` avvisas om
de är skrivbara för grupp eller övriga; läs-/execute-rättigheter är tillåtna.
Sticky bit gör inte en group/world-writable ancestor betrodd, så en explicit
socket under `/tmp` accepteras inte enbart på grund av `/tmp`-modellen.
Rekommenderad plats är användarens privata `XDG_RUNTIME_DIR` eller en annan
motsvarande privat, icke-skrivbar kedja.

Den slutliga runtimekatalogen ska dessutom ägas av serverns effektiva UID och
vara en katalog med `0700`. Servern binder genom den verifierade katalog-
deskriptorn (`/proc/self/fd/N/name`), vilket stänger kontroll–bind-fönstret för
utbytta sökvägskomponenter. Slutnamnet får inte finnas; en symlink, vanlig fil
eller tidigare socket skrivs aldrig över. Socketen sätts till `0600`. Cleanup
sker först efter kontroll av sockettyp, rättighet, UID, device och inode; en
ersatt eller främmande socket lämnas kvar med ett säkerhetsfel i stället för
att tas bort.

På Linux kommer peeridentiteten från `SO_PEERCRED`. Standardauth accepterar
endast serverns effektiva UID; en explicit UID-allowlist finns i intern
konfiguration men används inte av CLI:t. Payloaden kan inte deklarera UID och
auth sker innan frame eller JSON läses. Detta kräver ingen root. UID-semantik
över containers och user namespaces måste verifieras i målmiljön. Andra OS har
ingen transportimplementation i Paket 4.

## Protokoll och resursgränser

Protokollversion 1 är en fyrbytes big-endian längd följd av ett strict JSON-
dokument. En anslutning bär högst en `correlate_observation` och ett svar.
Okända fält och trailing JSON avvisas. Standardgränserna är:

- request och response: högst 64 KiB vardera
- samtidiga autentiserade requests: 8 (konfigurerbart 1–128)
- read/write deadline: 2 sekunder; handläggning: 5 sekunder
- en hook och högst 12 runtimes per request
- request-ID 64 tecken och övriga opaka ID:n 128 tecken som standard
- observation högst två minuter gammal, med två sekunders framtidstolerans
- replaycache 256 poster, fem minuters TTL, endast i minnet

Requesten innehåller version, opakt request-ID, operation och Paket 3:s
allowlistade hookfält. Den har ingen generell metadata-map. Prompt, transcript,
CWD, argv/command line, miljö, UID, nycklar, terminalinnehåll och användardata
kan därför inte uttryckas.

Svaret innehåller version, request-ID, status, innehållsfria felkoder,
korrelationssummary, opaka hook-/runtimereferenser, score/confidence/reason codes
och unmatched/ambiguous/rejected. Det serialiserar aldrig runtimefamiljen,
processidentitet, PID/starttid, host/boot, processgrupp, OS-session, terminal,
provider/profile/source eller rå hookpayload. Interna fel mappas till stabila
koder som `malformed_request`, `unauthorized_peer`, `read_timeout`,
`concurrency_limit`, `invalid_hook_observation`, `stale_observation`,
`correlation_failed`, `ambiguous_result`, `insufficient_evidence` och
`internal_error`; paths och råa Go-fel skickas inte.

## Flöde, adapters och replay

`CorrelationService` har injicerbara snapshot-, correlator- och clockgränser.
Varje godkänd request tar en ny Paket 2-sampling, omsätter familjerna till Paket
3-runtimeobservationer, korrelerar den enda hooken och kasserar allt efter
svaret. Varken förslag eller hookhistorik överförs till nästa request.

Rena Claude- och Codex-adapters mappar befintligt session-ID och lifecycle-event
till en observation. Epoch, revision, idempotensnyckel och tid måste tillföras
explicit av ett framtida säkert anrop. Dagens payloads ger ingen
generationssäker processidentitet, så adaptrarna gissar inte PID/root och de
befintliga produktionshookarna skickar ingenting till Paket 4. Codex
`transcript_path`, source och turn-ID ignoreras uttryckligen.

Replaycachen används bara för transportdiagnostik. Identisk request-ID/payload
returnerar ett markerat duplicate-svar; samma ID med annan payload avvisas.
Cachen är storleksbegränsad, TTL-styrd med injicerad clock och försvinner vid
restart. Overflow avvisas konservativt. Detta är inte den replay- eller
revisionspolicy som krävs för framtida mutation.

Standardloggtypen kan endast bära accept/reject, version, status, duration-
bucket, runtimeantal, confidence och reason codes. Den kan inte bära request-,
session-, process- eller sourceidentitet. Server-CLI:t använder ingen
requestloggning som standard.

## Manuella kommandon

Starta mottagaren explicit i foreground:

```text
go run ./cmd/aurora-presence-local-server \
  -host-id local-fixture -socket "$XDG_RUNTIME_DIR/aurora/presence-hook.sock"
```

Skicka exakt en sanerad fixture från fil eller stdin:

```text
go run ./cmd/aurora-presence-local-client \
  -socket "$XDG_RUNTIME_DIR/aurora/presence-hook.sock" \
  -input internal/localhooktransport/testdata/claude-observation.json
```

Servern daemoniserar inte och städar sin verifierade socket vid normal signal.
Klienten är ändlig och läser aldrig implicit hookstate.

## Tester och lokal mätning 2026-07-22

Det deterministiska fixtureunderlaget gav exact root, strong member,
weak/review, ambiguous och rejected conflict utan bindning. Malformed och
förbjudna fält avvisades; injicerad annan peer avvisades före decode.
Filesystemtesterna verifierade no-follow-kedja, symlinkar, befintlig fil,
`0700`/`0600`, främmande socket och ägar-/inodebaserad cleanup. En verklig lokal
Unix-anslutning verifierade `SO_PEERCRED` mot processens faktiska UID och PID.
Kontrollerad server/klient-körning verifierade ett sanerat exact-förslag och
cleanup; den använde syntetisk process-/hookidentitet.

Den separata livekörningen tog en faktisk read-only Paket 2-snapshot med tre
runtimefamiljer och kombinerade den med en uttryckligen syntetisk Claude-hook.
Resultatet var 0 exact/strong/weak/ambiguous, en rejected hook och
`insufficient_evidence`: två familjer hade tool conflict och den återstående
saknade hård identitet. Samma-UID-peer accepterades via faktisk `SO_PEERCRED`,
servern stängdes med signal och socketcleanup verifierades. Ingen faktisk
hookmetadata lästes och ingen verklig Claude-/Codex-bindningsprecision påstås.
Unauthorized-peer-fallet testades via injicerad auth, eftersom en annan verklig
UID inte skapades. Inga mätdata skrevs permanent.

## Ej implementerat och spärrar före mutation

Paket 4 installerar eller aktiverar ingen hookavsändare, daemon, systemd-enhet,
TCP-/HTTP-endpoint, registry-/slotmutation, heartbeat, relay, persistence,
bakgrundskö, retry eller automatisk bindning. Det ändrar inte befintliga Claude-
eller Codex-hookars output/state och inte v1-beteendet.

Bindningsmutation får inte införas förrän peer-auth och socketägarskap/cleanup
är verifierade i målmiljön; produktionshookar bär en accepterad
generationssäker identitet; märkta parallella Claude-/Codexfall är mätta och en
false-positive-gräns beslutad; muterande replay-/revisionspolicy är beslutad;
weak, ambiguous och rejected bevisligen inte kan mutera registry; feature flag
och rollback finns; samt end-to-end-tester visar oförändrad v1.
