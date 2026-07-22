# Paket 1: in-memory-kärna för per-instance presence

Paket 1 implementeras i `internal/instanceregistry`. Paketet äger det
trådsäkra, transport- och OS-neutrala registret för kanoniska instanser. Det
återanvänder Paket 0:s domäntyper i `internal/instancepresence` och projicerar
rena snapshots till wiretyperna i `internal/presencev2`.

## Ansvar och övergångar

Registrering kräver instance-ID, tool, source-metadata, full runtimeidentitet,
producer epoch, positiv runtime-revision, observationstid och idempotensnyckel.
En ny instans startar som `alive/idle`, med hook-revision 0 och lägsta lediga
logiska slot i det konfigurerade namespacet. Source är endast metadata; unikhet
upprätthålls med instance-ID och aktiv runtimeidentitet.

Runtime-livscykeln är:

```text
alive -- lease expiry --> suspect_missing -- grace expiry --> ended tombstone
   ^                              |
   +------ ny giltig heartbeat ---+
```

`alive` och `suspect_missing` ingår i active snapshot. `ended` gör det inte,
kan inte återupplivas av en vanlig mutation och behålls endast som en in-memory
tombstone för identitets- och retrykontroll. Hookclaim rensas och slotten släpps
när övergången till `ended` sker. Övriga slots kompakteras aldrig. En ny instans
får därefter lägsta lediga index.

## Revisioner och idempotens

Runtime och hook har separata monotona revisioner inom samma producer epoch.
En annan epoch kan inte ordnas och avvisas; byte av epoch kräver framtida,
explicit re-registration. Lägre revision avvisas som stale. Samma revision och
samma ägarpayload är en idempotent retry, även om retry-anropet använder en ny
idempotensnyckel. Samma revision med annan payload är revisionskonflikt. En
idempotensnyckel som återanvänds för annan payload är idempotenskonflikt.
`observed_at` lagras i den accepterade ägarpayloaden för retryjämförelse men
avgör aldrig ordningen.

Processlagret äger runtime-status, serverns last-seen/lease, runtime-revision och
epoch. Hooklagret äger claim och hook-revision. Hook `idle` rensar claim;
`working`, `attention` och `error` sätter den. Effektivt state härleds alltid
från aktiv runtime plus hookclaim. En runtime-heartbeat kan därför aldrig sänka
ett hookägt state. `state_changed_at` flyttas endast när en hookmutation faktiskt
ändrar det aktiva effektiva tillståndet.

## Lease och presentation

Både lease duration och grace period är obligatorisk `Registry`-konfiguration;
Paket 1 inför ingen produktionsdefault. En injicerad `Clock` och explicita
`ExpireLeases`-anrop gör testerna deterministiska. Expirygränser är inklusiva.
Om ett anrop sker långt efter båda gränserna gör första scanningen ändå den
konservativa övergången till `suspect_missing`; nästa scanning får avsluta
instansen. Det garanterar att gracetillståndet är observerbart.

`CanonicalSnapshot` innehåller alla aktiva instanser i slotordning och blir
`presence=sleeping` med en icke-nil tom lista när registret saknar aktiva
instanser. `Presentation(pixel_capacity)` ändrar aldrig slots: en instans vars
logiska slot ryms visas på samma pixelindex, annars hamnar den deterministiskt i
overflowlistan. Kapacitet 0 är giltig.

## Medvetet utanför Paket 1

Paketet registrerar inga HTTP-endpoints och är inte kopplat till relayns v1-
runtime. Det innehåller ingen persistence, feature flag, auth, processadapter,
processpollning, collector-daemon, hook-IPC, korrelation eller bindningströskel.
Det installerar, deployar eller startar inte om något. Paket 2 är fortsatt
observe-only-arbete för OS-adaptern och får inte skriva produktionsstatus.
