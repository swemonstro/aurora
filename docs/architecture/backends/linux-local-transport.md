# Linuxbackend för lokal hooktransport

Status: backendkontrakt efter Paket 5 och säkerhetsgräns för Paket 6

Det normativa kontraktet finns i
[integrationskontraktet](../per-instance-presence-integration-contract.md).
Paket 4:s säkerhetsmodell dokumenteras i
[Paket 4](../per-instance-presence-package-4.md).

## Backendansvar

Linuxbackenden ansvarar för:

- Unix-socketens pathvalidering, privata katalogkedja och no-follow-semantik;
- bindning utan att skriva över befintlig fil eller främmande socket;
- socketrättighet och ägarskap samt säker cleanup;
- Linux `SO_PEERCRED` före payloaddecode;
- read/write deadlines och anslutningens livscykel;
- mappning av backendfel till innehållsfria transportfel.

Unix-socket, filesystemmode, effektiv UID och `SO_PEERCRED` är
plattformsspecifika detaljer. De får inte krävas av `HookObservation`, request,
response, korrelator eller agentadapter.

## Transportneutral autentisering

Målgränsen är:

```text
lokal transportbackend
    -> verifiera OS-peer
    -> AuthenticatedPrincipal { opaque_ref, assurance }
    -> generell receiver
```

Den opaka principalen är endast ett resultat av backendens kontroll. Payloaden
får inte deklarera eller påverka den. Numerisk UID/GID/PID ska stanna i
Linuxbackenden och får inte vara generell användaridentitet.

En autentiserad samma-UID-peer är fortfarande en lokal avsändare, inte en
attestering av payloadens PID/starttid. Hård processidentitet måste verifieras
separat server-side. En Linuximplementation kan pröva peerprocessens
kernelrapporterade PID mot PID + starttid och verifierad ancestry i samma
snapshot. Om peeren har försvunnit eller relationen är tvetydig finns ingen hård
identitet; backenden får inte falla tillbaka till payloadens PID.

## Konfigurationsgräns

Backendconfig omfattar:

- absolut socketpath eller säker Linuxdefault;
- filesystem- och ägarpolicy;
- Linux peerpolicy;
- dial/listen-detaljer.

Receiver-/protokollconfig omfattar:

- request- och responsstorlek;
- concurrency och handläggningstid;
- observationsålder och identifierarlimiter;
- replaydiagnostik;
- runtime- och hookantal.

Produktconfig väljer om observe-only-funktionen är aktiverad. Paket 6 använder
`AURORA_LOCAL_HOOK_ENABLED`, av som default, och
`AURORA_LOCAL_HOOK_SOCKET`, som måste vara en absolut path i en privat
runtimekatalog. Kommandolagret väljer agentadapter; transportpaketet känner inte
till Claude eller Codex.

## Nuläge

| Fil och symbol/ansvar | Avvikelse | När den hanteras |
| --- | --- | --- |
| `internal/localhooktransport/types.go`, `PeerIdentity` och `Authenticator` | Det generella gränssnittet uttrycks direkt som UID/GID/PID. | Kan skjutas upp för Linux-MVP; måste ersättas före annan transportbackend eller stabilt auth-API. |
| `internal/localhooktransport/config.go`, `Config` | Socketpath och Linuxkomposition blandas med generella receiver-/protokollimiter. | Kan skjutas upp; ska delas före produktinstallation eller named-pipe-backend. |
| `internal/localhooktransport/server_linux.go`, `Server` | Servern kombinerar Linuxlistener, auth och receiver. Det är acceptabel komposition så länge wire- och receiverkontrakt inte kräver Linuxfält. | Behåll internt. |
| Det befintliga interna `HookObservation`-flödet | Typen innehåller serverintern korrelationsmetadata och får därför inte återanvändas som hookingress med dummyvärden. | Paket 6 inför den separata operationen `ingest_hook_event`; servern skapar intern `HookObservation` efter accepterad ingress. |

`PeerIdentity` och den sammanslagna `Config` är övergångsformer och får inte
publiceras som slutliga produktkontrakt.

## Paket 6:s socket- och trustgräns

Den bindande klient-, ingress- och sequencingdefinitionen finns i
[Paket 6](../per-instance-presence-package-6.md). Linuxtransporten använder Unix
stream socket vid en absolut, normaliserad path. Den användarägda
runtimekatalogen och Aurora-underkatalogen ska vara no-follow-kontrollerade,
ägda av förväntad effektiv UID och privata med mode `0700`. Systemägda
prefixkomponenter ska vara icke manipulerbara av den effektiva användaren.
Socketfilen ska vara en Unix socket med förväntad ägare och mode `0600`.
Servern verifierar `SO_PEERCRED` och same-effective-UID; klienten verifierar
serverpeerens UID efter connect.

Jämförelse av device/inode före och efter connect samt ytterligare socket-
replacement-kontroller är defense in depth där stödd Linuxmiljö och API medger
en robust implementation. De är inte ensamma ett absolut exitkriterium.

Samma UID är inte processidentitet. Ingressen innehåller därför inga process-
eller runtimehints, och transportautentisering kan inte ensam skapa bindning.
Pathbaserad Unix socket har kvar en reducerad TOCTOU-risk även efter sådana
kontroller; den risken ska redovisas och får inte döljas som hård identitet.

## Framtida named-pipe-backend

Paket 6 implementerar och lovar inte Windowsstöd. En framtida named-pipe-
backend ska dock kunna:

1. återanvända det versionssatta generiska request-/responsekontraktet;
2. verifiera peer med Windows egna säkerhetsmekanismer;
3. producera samma transportneutrala principalresultat;
4. använda egna path-, ACL-, deadline- och cleanupregler;
5. komponeras med samma receiver, runtimekälla och korrelator.

Den får inte införa syntetiska UID:n eller Unix-pathsemantik i kärnan. Om detta
inte går utan domänändring är gränsen fel och ska rättas genom ett separat
versionssatt arkitekturbeslut.
