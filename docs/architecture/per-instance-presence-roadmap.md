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

`Implemented` eller `integrated` innebär inte installerad eller aktiverad.
Observe-only-kod kan vara integrerad utan att ingå i produktionsflödet.

## Aktuell paketstatus

Statusen avser `main` vid `0b0fc65`. Paket 0–5 och Paket 4.5 är integrerade.
Paket 2–6 är eller ska vara strikt observe-only och är inte aktiva i
produktion.

| Paket | Leverans | Planned | Implemented | Integrated | Active in production |
| --- | --- | --- | --- | --- | --- |
| 0 | Domänkontrakt, fixtures och v2-schema | ja | ja | ja | nej |
| 1 | In-memory-register och v2-domänsemantik | ja | ja | ja | nej |
| 2 | Observe-only Linux-processadapter | ja | ja | ja | nej |
| 3 | Observe-only korrelationsmotor | ja | ja | ja | nej |
| 4 | Säker lokal observe-only hooktransport | ja | ja | ja | nej |
| 4.5 | Produktgräns, lagerkontrakt och kanonisk roadmap | ja | ja | ja | nej |
| 5 | Refaktorer A–C: runtimekälla, recognition, agentadapters och transportgränser | ja | ja | ja | nej |
| 6 | Verklig lokal hook-ingress med receiverägd sequencing och bounded best-effort-klient | ja | nej | nej | nej |
| 7 | Säker binding policy och korrelationslivscykel (docs-first kontrakt) | ja | delvis (docs) | nej | nej |
| 8 | Registry- och slotmutation bakom separat aktivering | ja | nej | nej | nej |

Paket 1:s register är inte kopplat till relayns v1-runtime eller till Paket
2–6. Ingen status ovan påstår att v2, automatisk bindning eller registrymutation
är aktiv.

## Integrerade paket

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
startad Linuxserver och en diagnostisk klient över privat Unix-socket. Peer
autentiseras, protokollet är begränsat och svaret är sanerat. Befintliga
produktionshookar skickar inte till transporten.

### Paket 4.5 — produkt- och integrationskontrakt

Paket 4.5 låste produktlagren, den långsiktiga agentidentifieraren,
metadataägarskapet, Linux-MVP:n och Paket 5:s obligatoriska refaktorer. Det
normativa kontraktet finns i
[integrationskontraktet](per-instance-presence-integration-contract.md).

### Paket 5 — arkitekturrefaktorer A–C

[Paket 5](per-instance-presence-package-5.md) är integrerat i `main` vid
`0b0fc65`. Paketet:

- gjorde korrelationsservicens runtimekälla OS-neutral;
- flyttade runtime recognition och familjebildning ur Linuxbackenden;
- gav Claude- och Codexadaptrarna ägarskap över recognition och eventmapping;
- gjorde hookadaptern transportneutral och lokaltransporten agentneutral.

Paket 5 anslöt ingen verklig hook. Det införde ingen mutation, persistence,
publicering, installation eller produktionsaktivering.

## Planerade paket

### Paket 6 — verklig lokal observe-only hook-ingress

[Paket 6](per-instance-presence-package-6.md) ska ansluta faktiska Claude- och
Codexhookprocesser till den manuellt startade lokala mottagaren. Paketet inför
en separat sanerad ingressoperation, receiverägd in-memory sequencing och en
bounded best-effort-klient bakom `AURORA_LOCAL_HOOK_ENABLED`, vars default är
av.

Paket 6 utför ingen automatisk bindning och får inte mutera registry eller
slots, persistera presence-state, publicera till relay/v2, ändra v1 eller
installera eller aktivera någon tjänst.

### Paket 7 — säker binding policy och korrelationslivscykel

[Paket 7](per-instance-presence-package-7.md) definierar det normativa
kontraktet för när en sanerad hookobservation får betraktas som säkert
bindningsbar till exakt en lokal runtimeinstans. Kontraktet omfattar:

- hård identitet och fail-closed-regler;
- confidence- och ambiguitetsgränser;
- noll/en/flera runtimes och samtidiga sessioner;
- stale-fönster och lifecycle (`active` / `idle` / `ended`);
- keep / suspend / replace / remove för befintliga beslut;
- innehållsfri audit och bounded in-memory decision state;
- strikt gräns mot Paket 8: beslut och förslag utan registry- eller slotmutation.

Normativ dokumentation finns; implementation, integration och aktivering saknas.
`would_bind_under_current_threshold` från Paket 3 förblir diagnostik och är inte
bindningsauktorisation. Paket 6 som den är implementerad ger ingen
serverattesterad hård processidentitet i ingressen; med endast Paket 0–6 ska
Paket 7 fail-closed och inte avge `propose_bind` för nya associationer förrän en
uttrycklig trusted identity-brygga finns. `replace` är ett atomärt
policybeslut; Paket 8 styr mutationsapplikation. Åldersfönster, grace, TTL,
kapaciteter och exact-vs-strong är föreslagna mätdefaults som kräver mänskligt
godkännande före muterande konsumtion. Paket 7 får inte mutera registry, slots,
hookclaims, runtimeclaims eller publicera till relay/v2. Implementeringsarbete
förutsätter att Paket 6:s exitkriterier är uppfyllda och granskade.

### Paket 8 — registry- och slotmutation

Paket 8 får först efter separat säkerhets- och rolloutbeslut koppla godkänd
Paket 7-bindningspolicy till registry- och slotmutation. Mutationen ska ha egen
feature flag, replay-/revisionspolicy, rollback och v1-isolering. Paket 8 är den
första punkten där ett `propose_bind`-beslut får bli synlig presence-state.

## Migrationsnotering för Paket 5 och 6

Roadmapen och integrationskontraktet kallade tidigare både de obligatoriska
refaktorerna A–C och den efterföljande verkliga hookanslutningen för Paket 5.
Efter integrationen av A–C vid `0b0fc65` gäller följande kanoniska uppdelning:

| Tidigare formulering | Kanonisk benämning |
| --- | --- |
| “Paket 5:s första refaktorer A–C” | Paket 5 |
| “Paket 5: verklig observe-only hookanslutning” | Paket 6 |
| Framtida säker bindning | Paket 7 |
| Framtida registry-/slotmutation | Paket 8 |

Äldre dokumentation får citeras som historik, men får inte användas för att
namnge nya leveranser.

## Äldre supersedad plan

Avsnitt 4 i `per-instance-presence-decisions.md` är en historisk plan och inte
paketindex. Durable relay, ESP och ytterligare plattformsbackends har fortfarande
inga nya aktuella paketnummer. När de planeras ska roadmapen först uppdateras
med unika nummer; de får inte återanvända Paket 5–8 ovan.
