# Blue1: lokal Aurora-deployment

Status: lokal deploymentdokumentation, inte normativ produktarkitektur

Det här dokumentet samlar repositoryts Blue1-specifika exempel så att de inte
misstas för produktdefault. Det flyttar eller ändrar inte befintlig README,
wrapper, systemd-enhet eller installation. Operativ verklighet har inte
verifierats operativt av Paket 6; “nuvarande” nedan betyder repositoryinnehåll,
inte att en tjänst är installerad eller aktiv.

Normativa lager- och configregler finns i
[integrationskontraktet](../architecture/per-instance-presence-integration-contract.md).

## Repositorydokumenterad konfiguration

| Lokal uppgift | Repositorykälla | Klassificering |
| --- | --- | --- |
| Utvecklingscheckout `/srv/dev/aurora` | `README.md` och wrapperns hookdefault | Lokal utvecklingssökväg; inte installationsdefault. |
| Codexbinär `/home/carl/.npm-global/bin/codex` | `bin/aurora-codex` | Personlig legacydefault; måste konfigureras för annan användare. |
| Codexhook `/srv/dev/aurora/bin/aurora-codex-hook` | `bin/aurora-codex` | Lokal legacydefault; inte generell paketering. |
| Wrapper `bin/aurora-codex` | wrapper och README | Befintlig migrations-/v1-mekanism; inte målkrav för per-instance presence. |
| systemd-enhet `aurora-relay.service` | `deploy/systemd/aurora-relay.service` | Linuxdeployment för relayn; inte del av Paket 2–6:s kärna. |
| användare/grupp `carl` | samma systemd-enhet | Blue1-specifikt och inte produktdefault. |
| relaylisten `0.0.0.0:8080` | systemd-enheten och README | Lokal LAN-exponering; kräver separat säkerhetsbedömning och är inte domänkontrakt. |
| exempeladress `192.168.0.247` | README | Historiskt/lokalt nätverksexempel; inte default eller discoverymekanism. |
| installation till `/usr/local/bin` och `/etc/systemd/system` | `scripts/install-aurora-relay.sh` | Linuxdistribution för v1-relay; inte lokal hooktransportinstallation. |

Ingen av paths, användare, grupper, IP-adresser, portar eller enhetsnamn ovan får
kopieras till generell Paket 6-kod eller configdefault.

## Historisk lokal evidens

[Paket 2](../architecture/per-instance-presence-package-2.md) redovisar en
read-only `/proc`-sampling på Blue1 den 2026-07-22. Den visar vilka
processformer som råkade finnas vid mättillfället och är inte en generell
Claude-/Codextopologi.

[Paket 3](../architecture/per-instance-presence-package-3.md) och
[Paket 4](../architecture/per-instance-presence-package-4.md) skiljer faktisk
live processobservation från syntetisk hookmetadata. De resultaten påstår inte
verklig bindningsprecision.

## Nuvarande kontra målbild

Nuvarande repository har:

- v1-relay och hookpublicering över HTTP;
- personlig Codexwrapper med konfigurerbara men lokalt hårdkodade fallbackvärden;
- en systemd-enhet för v1-relayn;
- manuella observe-only-kommandon för Paket 2–4.

Målbilden för Paket 6 är däremot endast en avstängd, best-effort observe-only-
sändning till en manuellt startad lokal receiver. Paket 6 ska inte installera en
tjänst, använda produktionssocket, ändra wrappern till ett krav eller aktivera
någon relay-/registrymutation.

## Lokal konfiguration före eventuell användning

En Blue1-operatör måste uttryckligen välja och verifiera:

- faktisk binär- och hookinstallation i stället för checkoutpaths;
- användare, grupp och filrättigheter;
- privat runtimekatalog och lokal socketpath;
- relayns bindadress och nätverksexponering;
- feature flag och rollback för observe-only-hookanslutning;
- att befintligt v1-flöde fungerar oberoende.

Detta är deploymentarbete. Värdena får inte bli del av `Instance`,
`HookObservation`, `RuntimeCandidate`, korrelatorn eller transportprotokollet.
