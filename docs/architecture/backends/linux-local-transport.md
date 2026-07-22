# Linuxbackend för lokal hooktransport

Status: målgräns och nulägesinventering för Paket 4.5

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

Produktconfig väljer om observe-only-funktionen är aktiverad och vilka adapters
som används. Lokal deploymentconfig anger den faktiska socketpathen och
operatörens startkommando.

## Nuläge

| Fil och symbol/ansvar | Avvikelse | När den hanteras |
| --- | --- | --- |
| `internal/localhooktransport/types.go`, `PeerIdentity` och `Authenticator` | Det generella gränssnittet uttrycks direkt som UID/GID/PID. | Kan skjutas upp för Linux-MVP; måste ersättas före annan transportbackend eller stabilt auth-API. |
| `internal/localhooktransport/config.go`, `Config` | Socketpath och Linuxkomposition blandas med generella receiver-/protokollimiter. | Kan skjutas upp; ska delas före produktinstallation eller named-pipe-backend. |
| `internal/localhooktransport/server_linux.go`, `Server` | Servern kombinerar Linuxlistener, auth och receiver. Det är acceptabel komposition så länge wire- och receiverkontrakt inte kräver Linuxfält. | Behåll internt. |
| `internal/localhooktransport/types.go`, transportens `HookObservation` | Payloaden kan bära process/runtimehint, men saknar proveniensnivå. | Proveniens måste säkras under Paket 5 innan en verklig hook ansluts eller observationen korreleras. |

`PeerIdentity` och den sammanslagna `Config` är övergångsformer och får inte
publiceras som slutliga produktkontrakt.

## Framtida named-pipe-backend

Paket 4.5 implementerar och lovar inte Windowsstöd. En framtida named-pipe-
backend ska dock kunna:

1. återanvända det versionssatta generiska request-/responsekontraktet;
2. verifiera peer med Windows egna säkerhetsmekanismer;
3. producera samma transportneutrala principalresultat;
4. använda egna path-, ACL-, deadline- och cleanupregler;
5. komponeras med samma receiver, runtimekälla och korrelator.

Den får inte införa syntetiska UID:n eller Unix-pathsemantik i kärnan. Om detta
inte går utan domänändring är gränsen fel och ska rättas genom ett separat
versionssatt arkitekturbeslut.
