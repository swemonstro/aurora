# Kanonisk roadmap för per-instance presence

Status: normativ roadmap

Detta dokument är den enda kanoniska numreringen av arbetspaketen för Auroras
per-instance presence. Arkitekturprinciperna i
[huvuddesignen](per-instance-presence.md) och fattade ADR-beslut i
[beslutsdokumentet](per-instance-presence-decisions.md) gäller fortsatt, men
äldre paketnumrering och äldre tidsplaner är supersedade av denna roadmap.

## Statusbegrepp

Fyra statusar ska hållas åtskilda:

- **planned:** omfattning och inträdesvillkor är definierade;
- **implemented:** kod eller normativ dokumentation finns och är verifierbar;
- **integrated:** implementationen finns på `main`;
- **active in production:** funktionen är installerad och uttryckligen aktiverad
  i en produktionsmiljö.

`Implemented` eller `integrated` innebär alltså inte installerad eller aktiverad.
Ett observe-only-verktyg kan vara integrerat utan att ingå i produktionsflödet.

## Aktuell paketstatus

Statusen nedan avser baslinjen `main` vid `0e19843` samt Paket 4.5-
dokumentationen i den här ändringen. Paket 4.5 är inte `integrated` förrän
ändringen faktiskt finns på `main`.

| Paket | Leverans | Planned | Implemented | Integrated | Active in production |
| --- | --- | --- | --- | --- | --- |
| 0 | Domänkontrakt, fixtures och v2-schema | ja | ja | ja | nej |
| 1 | In-memory-register och v2-domänsemantik | ja | ja | ja | nej |
| 2 | Observe-only Linux-processadapter | ja | ja | ja | nej |
| 3 | Observe-only korrelationsmotor | ja | ja | ja | nej |
| 4 | Säker lokal observe-only hooktransport | ja | ja | ja | nej |
| 4.5 | Produktgräns, lagerkontrakt och kanonisk roadmap | ja | ja | nej | nej |
| 5 | Verklig observe-only hookanslutning bakom avstängd feature flag | ja | nej | nej | nej |

Paket 0–4 är integrerade i `main`. Paket 2–4 är uttryckligen observe-only,
kräver manuell körning och är inte aktiva i produktion. Paket 1 är inte kopplat
till relayns v1-runtime och registrerar inga endpoints. Ingen av statusarna ovan
påstår att v2, collector, automatisk bindning eller registrymutation är aktiv.

## Implementerade paket

### Paket 0 — kontrakt och mätplan

[Paket 0](per-instance-presence-package-0.md) definierar den
plattformsneutrala domänen, processkontrakt, fixtures, v2-wiretyper och
regressionsskydd för v1. Paketet aktiverar ingen runtimefunktion.

### Paket 1 — in-memory-kärna

[Paket 1](per-instance-presence-package-1.md) implementerar ett trådsäkert
in-memory-register med lifecycle, revisioner, idempotens, slots och
presentation. Registret är inte inkopplat i produktionsrelayn.

### Paket 2 — observe-only Linux-processadapter

[Paket 2](per-instance-presence-package-2.md) läser Linux `/proc` read-only och
producerar snapshots, exits och observerade processfamiljer. CLI:t är ett
ändligt diagnostikverktyg, inte en collector eller tjänst.

### Paket 3 — observe-only korrelation

[Paket 3](per-instance-presence-package-3.md) poängsätter sanerade hooksignaler
mot runtimekandidater och rapporterar exact, strong, weak, ambiguous och
rejected. `would_bind_under_current_threshold` är diagnostik och utför ingen
bindning.

### Paket 4 — säker lokal observe-only hooktransport

[Paket 4](per-instance-presence-package-4.md) tillhandahåller en manuellt
startad Linuxserver och klient över privat Unix-socket. Peer autentiseras,
protokollet är begränsat och svaret är sanerat. Befintliga produktionshookar
skickar inte till transporten.

## Paket 4.5 — produkt- och integrationskontrakt

Paket 4.5 består av dokumentation och gör ingen runtimeändring. Det:

- låser de fem produktlagren och deras tillåtna beroenden;
- beslutar en namespacad, utbyggbar agentidentifierare som långsiktig modell;
- definierar ägarskap för host-ID, producer epoch och revision;
- klassificerar nuvarande lageravvikelser och när de ska hanteras;
- avgränsar första MVP till Linux utan att göra Linux till domänkontrakt;
- definierar Paket 5:s inträdesvillkor, acceptanskriterier och rollback.

Det normativa kontraktet finns i
[integrationskontraktet](per-instance-presence-integration-contract.md).
Linux- och agentdetaljer finns i:

- [Linux-processbackend](backends/linux-process.md)
- [Linux lokal transport](backends/linux-local-transport.md)
- [Claude-adapter](adapters/claude.md)
- [Codex-adapter](adapters/codex.md)
- [lokal Blue1-deployment](../deployment/blue1.md), som inte är normativ

## Paket 5 — verklig observe-only hookanslutning

Paket 5 ska ansluta verkliga Claude- och Codexhookar till den lokala receivern
bakom en explicit feature flag vars default är av. Sändningen ska vara
best-effort, agentägd och begränsad. Receivern startas fortsatt manuellt.

Paket 5 får inte införa:

- registry- eller slotmutation;
- persistence eller bakgrundskö;
- relay- eller v2-publicering;
- automatisk bindning;
- beroende från v1-flödet till observe-only-transporten;
- installations-, daemon- eller systemdaktivering.

Paket 5 ska börja med de små, sammanhållna refaktorer som skiljer
processinsamling, agentigenkänning, runtimekälla och hookadapters enligt
[integrationskontraktets](per-instance-presence-integration-contract.md)
lagerregler. Ingen verklig hook får anslutas innan dessa refaktorer är klara.
Paket 5:s exakta acceptanskriterier finns i samma dokument.

## Supersedad numrering

Avsnitt 4 i `per-instance-presence-decisions.md` var en tidig plan och är inte
längre paketindex. Följande namn får inte återanvändas som aktuella paket:

| Äldre plan | Faktiskt utfall och ny benämning |
| --- | --- |
| Äldre Paket 2: Linuxadapter och collector | Paket 2 levererade endast observe-only-adaptern. Collector är inte implementerad. |
| Äldre Paket 3: lokal hook-IPC och säker bindning | Delades i Paket 3 korrelation, Paket 4 lokal observe-only-transport och planerat Paket 5 hookanslutning. Ingen säker bindningsmutation är implementerad. |
| Äldre Paket 4: durable relay och shadow traffic | Inte implementerat och har inget nytt paketnummer ännu. |
| Äldre Paket 5: fler-pixel-ESP | Inte implementerat och har inget nytt paketnummer ännu. |
| Äldre Paket 6–8 | Framtida idéer utan aktuella paketnummer. |

När durable relay, ESP eller plattformsbackends planeras ska roadmapen först
uppdateras med nya unika nummer. Äldre ADR-beslut om dessa ämnen är fortfarande
giltiga där de uttryckligen är markerade som beslut; endast paketnumreringen och
tidsordningen är supersedade.
