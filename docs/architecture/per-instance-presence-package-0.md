# Paket 0: kontrakt och mätplan

Detta dokument är en kort kodkarta för Paket 0. Normativa beslut och migrering
finns fortsatt i `per-instance-presence.md` och
`per-instance-presence-decisions.md`.

## Ansvar

- `internal/instancepresence` äger plattformsneutral instansidentitet,
  runtime-/hookägarskap, revisioner, processadapter-, korrelations- och
  slotcoordinatorgränssnitt samt små rena invariantfunktioner.
- `internal/instancepresence/contracttest` äger återanvändbara, syntetiska
  process- och korrelationsfixtures. De är inte en executable-klassificerare
  eller automatisk bindningsalgoritm.
- `internal/presencev2` äger v2:s separata wiretyper och rena validering. Det
  registrerar inga endpoints.
- `api/v2/presence.schema.json`, `api/v2/openapi.json` och `api/v2/examples`
  är det maskinläsbara v2-kontraktet. V1 ligger oförändrat i
  `internal/presence` och `internal/relay`.

## Observe-only-signaler och datagräns

En kommande lokal adapter får mäta opaka executable-identiteter, PID tillsammans
med process-starttid, verifierade parent/ancestor-relationer,
processgrupp/job-identitet, OS-session, ägaridentitet samt opaka lokala
fingerprints för terminal och CWD. Transcript får endast bidra som ett opakt,
lokalt jämförelsevärde. Dessa signaler används för att utvärdera entydighet och
får inte publiceras som rått innehåll.

Följande data är förbjudna i central state, nättrafik, fixtures, mätvärden och
loggar: prompt, terminaloutput, argv/full kommandorad, rå CWD, transcript-path
och generell miljödata. Hemligheter och innehåll från användarens arbete får
inte samlas in.

## Innehållsfri mätplan

Paket 2 ska per verktyg och adaptervariant räkna `matched`, `ambiguous` och
`unmatched`, samt klassificera syntetiska/manuellt märkta false-positive- och
false-negative-testfall. Exit-latens mäts som tidsdifferensen mellan verifierad
process-exit och observe-only-detektering, sammanfattad i histogram. Endast
antal, latensintervall, opak adapterversion och orsakskod får lämna maskinen.

Mätningen ska omfatta parallella starter, delad terminal/CWD-fingerprint,
Node/native-familjer, PID-återanvändning, Ctrl+C, krasch, SSH-bortfall och
terminaldöd. Osäker korrelation muterar fortfarande ingen kandidat.

Den slutliga automatiska bindningströskeln är avsiktligt inte definierad i
Paket 0. Paket 3 och Paket 4 är fortsatt observe-only och har inte låst eller
aktiverat automatisk bindning. Tröskeln får beslutas först efter representativa
mätresultat och måste vara låst före ett framtida muterande bindningspaket.
