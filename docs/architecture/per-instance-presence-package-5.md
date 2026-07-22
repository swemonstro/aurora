# Paket 5: arkitekturrefaktorer A–C

Status: implementerat och integrerat i `main` vid `0b0fc65`; inte aktivt i
produktion

Paket 5 förberedde den verkliga hookanslutningen genom att flytta ansvar till
rätt lager utan att ansluta någon hook. Det normativa lagerkontraktet finns i
[integrationskontraktet](per-instance-presence-integration-contract.md).

## Levererade refaktorer

### A. OS-neutral runtimekälla

Korrelationsservicen konsumerar generiska runtimeobservationer och importerar
inte Linuxprocessbackenden. Linuxkompositionen tar en atomisk snapshot med
bootidentitet och projicerar den via det OS-neutrala recognitionlagret.

### B. Agentadapters utanför transportpaketet

Claude- och Codexevent mappas i agentägda paket till en transportneutral
observation. `internal/localhooktransport` importerar inte agentpaketen och
transporterar endast generiska kontrakt.

### C. Agentrecognizers utanför Linuxbackenden

Linuxbackenden samlar processdata men äger inte Claude-/Codexklassificering.
Agentägda recognizers tolkar sanerade generiska processobservationer och det
OS-neutrala recognitionlagret bildar runtimefamiljer konservativt.

## Oförändrad produktgräns

Paket 5:

- anslöt ingen verklig Claude- eller Codexhook;
- skapade ingen feature flag eller automatisk bindning;
- muterade inte registry, slots eller hookclaims;
- införde ingen persistence, relay-/v2-publicering eller v1-ändring;
- installerade eller aktiverade ingen daemon eller systemd-enhet;
- ändrade inte wireformat, schema, `ToolKind` eller AgentID.

Verklig lokal hook-ingress är kanoniskt
[Paket 6](per-instance-presence-package-6.md). Äldre formuleringar som kallade
hela refaktor- och hookanslutningsarbetet Paket 5 är supersedade av
[roadmapen](per-instance-presence-roadmap.md).
