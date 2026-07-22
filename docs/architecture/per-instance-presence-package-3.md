# Paket 3: observe-only korrelation

Paket 3 finns i `internal/instancecorrelation`. Det jämför sanerade
hookobservationer med Paket 0:s `RuntimeCandidate` och producerar förslag,
confidence, reason codes, ambiguity och innehållsfria riskmått. Det muterar
aldrig registry, runtime, hookclaim eller slot. `cmd/aurora-presence-correlate`
är ett ändligt lokalt analysverktyg, inte hooktransport, collector eller daemon.

## Input och integritetsgräns

`RuntimeObservation` omsluter en validerad processfamilj med observationstid,
livscykel och valfria opaka processgrupps-, OS-sessions- och terminalfingerprint.
`HookObservation` innehåller verktyg, opak sessionsreferens, producer epoch,
revision, observationstid och livscykel. Process-/runtimehint, PID-only-hint,
host, boot och opaka exekveringskontexter är uttryckligen valfria; tomt eller
utelämnat betyder ”inte tillgängligt”, aldrig konflikt.

Source/provider/profile valideras som metadata men ger ingen poäng och är aldrig
identitet. Paketet accepterar inte prompt, rå argv, CWD, transcript-path,
terminalinnehåll, miljö, UID, nycklar eller användardokument. CLI:t använder
strict JSON decoding, en 1 MiB-gräns och avvisar okända fält.

Dagens Claude- och Codex-hookpayloads har session-ID och lifecycle-event men
ingen generationssäker processidentitet. Paket 3 ändrar därför inte hookkoden
och läser inte hookarnas state-/transcriptfiler. Codex- och Claudefall mäts med
explicita sanerade fixtures tills en separat, beslutad lokal transport finns.

## Signaler och scoring

Hårda positiva signaler är exakt `RuntimeIdentity`, exakt root
`ProcessIdentity` och exakt medlems-`ProcessIdentity`. Root/runtime ger
`exact`; medlem ger minst `strong`. PID utan starttid är bara
`missing_process_start_time` + `pid_only_hint`.

Hårda konflikter blockerar alltid paret: tool-, host- eller bootmotsägelse,
samma PID med annan starttid, explicit process/runtimehint till annan kandidat,
stale observation, omöjlig tidsordning samt ended/live-motsägelse. Flera mjuka
signaler kan inte neutralisera en konflikt.

Standardvikterna är heltal och ligger i en validerad `Config`, inte global state:

| Signal | Poäng |
| --- | ---: |
| exakt runtime | 1200 |
| exakt rootprocess | 1000 |
| exakt familjemedlem | 700 |
| processgrupp / OS-session | 45 vardera |
| starttidsnärhet | 25 |
| host / boot / tool | 20 vardera |
| terminal / observationsnärhet | 15 vardera |
| PID-only | 5 |

Varje bidrag finns i resultatet som `{code, points}`. Standardgränserna är
`strong >= 500`, `weak >= 70` och ambiguity-delta 10. Valideringen kräver att
tool, TTY, PID-only och vardera tidsnärhetssignalen ligger under weak var för
sig. Flera stödjande signaler får tillsammans nå weak men kräver review. Summan
av alla möjliga mjuka signaler måste ligga strikt under strong; strong/exact
kräver därför exakt runtime-, root- eller medlemsidentitet med PID och starttid.
Source ger fortsatt ingen poäng. `would_bind_under_current_threshold` är endast
ett observe-only mätfält för exact/strong; det är inte ett godkänt automatiskt
bindningsbeslut eller en aktiverad policy.

## Global matchning och ambiguity

Alla hook/runtime-par poängsätts först till en kandidatmatris. En dynamisk
programmering över runtime-bitmasker maximerar sedan total score med högst en
runtime per hook och högst en hookidentitet per runtime. Input sorteras på opaka
referenser och lika lösningar har en stabil lexikografisk tie-break, så
inputordning påverkar inte resultatet.

Varje vald kant prövas därefter genom att lösningen räknas om utan kanten. Om en
alternativ global lösning ligger inom ambiguity-delta rapporteras alla berörda
hooks/runtimes som `ambiguous`; ingen av dem blir förslag. Detta fångar både två
runtimes för en hook och två hooks som konkurrerar om samma runtime.

Komplexiteten är `O(H * R * 2^R)` och minnet `O(2^R)`. Konfigurationen tillåter
högst 12 logiska hooks respektive 12 runtimes. Större input ger
`candidate_limit_exceeded`, endast unmatched-resultat och ingen partiell eller
greedy fallback.

## Tid, revision och idempotens

Tider kopieras utan monotonic metadata och normaliseras till UTC. Standardvärden
är 30 sekunders start-/observationsnärhet, två minuters maxålder och två
sekunders tolerans när hooken observeras före process-startsignalen. Väggklockan
avgör inte revisionsordning.

Observationer grupperas på tool + opak hooksessionsreferens. Identiska retries
med samma revision dedupliceras även med ny idempotensnyckel. Samma revision med
olika payload rapporteras som `same_revision_conflict`; återanvänd
idempotensnyckel med ändrad observation som `idempotency_conflict`. Lägre
revisioner markeras `out_of_order_revision` och `superseded_revision`. Olika
producer epochs ordnas aldrig med revision eller tid utan ger
`producer_epoch_conflict`.

Valfria `expected_matches` använder endast opaka referenser och ger lokala
TP/TN/FP/FN-räknare. De är test-/mät-ground-truth, inte bindningsstate.

## Reason codes och CLI

Resultatet använder de innehållsfria koderna för exact runtime/process/root/
member, host/boot/tool, processgrupp/session/terminal, tidsnärhet, PID-only,
alla identitets-/lifecycle-/stale-konflikter, revision/idempotens,
insufficient evidence, ambiguity/competition och kandidatgräns.

En tom standardkörning är ändlig:

```text
go run ./cmd/aurora-presence-correlate
```

Sanerad fil eller explicit stdin:

```text
go run ./cmd/aurora-presence-correlate -input fixture.json -pretty
go run ./cmd/aurora-presence-correlate -input - < fixture.json
```

CLI:t skriver bara JSON till stdout och gör inga nätverksanrop eller writes.

## Observe-only mätning 2026-07-22

Mätningen hade två uttryckligen separata delar:

- **Faktisk live processobservation:** en enda read-only Paket 2-sampling av
  `/proc` såg 231 processer, 1 Claude-familj, 2 Codex-familjer, 226 unknown och
  0 ambiguous. Former var direct Claude, wrapper + Node-launcher + direct Codex
  samt direct Codex. Endast `argv_prefix_truncated=1` rapporterades utöver
  klassificeringsräknarna; inga råa argv eller sökvägar sparades.
- **Syntetisk sanerad korrelation:** `testdata/mixed.json` innehöll 4 runtimes
  och 3 hooks. Resultatet var 1 exact, 0 strong, 0 weak, 1 ambiguous grupp och
  1 rejected hook. Exact avgjordes av processgeneration/root; ambiguity av två
  lika processgrupps-/sessionskandidater. Två labels gav TP=1, TN=1, FP=0,
  FN=0.
- **Faktisk hookmetadata:** ingen lästes eller korrelerades. De nuvarande
  produktionspayloads som granskades saknar process-starttid och säker lokal
  processkontext; att gissa från session-ID eller source vore kontraktsbrott.

Livefamiljerna kombinerades avsiktligt inte med den syntetiska hookfixturen, så
mätningen säger inget om verklig bindningsprecision. Fler märkta parallell-,
terminaldelnings-, reparenting-, stale- och processracefall behövs.

## Ej implementerat och kriterier före automatisk bindning

Paket 3 implementerar ingen hook-IPC, peer-verifiering, automatisk bindning,
registry-/slotmutation, heartbeat, relay/endpoints, persistence, daemon,
installation, feature flag eller ändring av v1/Paket 1/Paket 2-runtimebeteende.

Automatisk bindning kräver senare ett uttryckligt arkitekturbeslut baserat på
representativa märkta Claude-/Codexmätningar, dokumenterad och accepterad
false-positive-gräns, stabila resultat för parallella/delade terminalkontexter,
beslutad threshold/ambiguity-policy, säker autentiserad lokal hooktransport och
end-to-end-bevis att ambiguous/rejected aldrig muterar en instans. Paket 3:s
standardvikter är mätparametrar tills dessa kriterier är uppfyllda.
