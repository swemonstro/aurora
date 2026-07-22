# Paket 2: observe-only Linux-processadapter

Paket 2 finns i `internal/linuxprocess`. Det implementerar Paket 0:s
`ProcessAdapter` för Linux och producerar read-only `ProcessSnapshot`,
`RuntimeCandidate` och `ProcessExit`. Paketet skriver aldrig till
`instanceregistry`, relay, hooks eller disk. `cmd/aurora-presence-observe` är ett
ändligt lokalt mätverktyg, inte en collector eller daemon.

## Lästa `/proc`-fält

Adaptern öppnar en explicit konfigurerad proc-root genom en rotbegränsad läsare.
Symlink-rötter och symlink-filer avvisas. Följande filer/fält läses:

- `/proc/stat`: endast `btime` för normaliserad process-starttid;
- `/proc/sys/kernel/random/boot_id`: endast när boot-ID inte ges explicit;
- `/proc/<pid>/stat`: PID, `comm`, state, PPID, processgrupp, session, TTY och
  startticks;
- `/proc/<pid>/comm`: begränsad executable/comm-signal;
- `/proc/<pid>/cmdline`: högst 1024 byte och fyra argv-prefixposter;
- `/proc/<pid>/status`: endast första UID-värdet används som opak ägarsignal.

`/proc/<pid>/stat` parsas utan att dela det parentesslutna `comm`-fältet, så
mellanslag och parenteser i processnamn bevaras korrekt. Startidentiteten är
boot time plus startticks/USER_HZ. USER_HZ är konfigurerbart; CLI-defaulten 100
ska verifieras per stödd Linuxarkitektur.

Rå argv lagras eller skrivs aldrig ut. Det begränsade prefixet reduceras direkt
till executable-basename och allowlistade Claude-/Codex-paketmarkörer; okända
argument, promptar, tokens och andra värden kasseras. Adaptern läser inte `environ`,
`cwd`, transcript, terminalinnehåll eller användardokument. Host-ID måste anges
explicit. Observe-kandidatreferenser är deterministiska hashreferenser för en
sampling och är inte kanoniska registrerade instance-ID:n.

## Klassificering och processfamiljer

Klassificeringen använder executable-basename, `comm` och reducerade markörer
för kända Claude-/Codex-binärer, deras Node-paket och explicita wrappers. En
okänd eller motsägelsefull signal förblir `unknown`.

Kända processer kopplas endast längs verifierad parent-chain och kompatibel
processgrupp/session. Wrapper, Node-launcher, native child och upp till tre
observerade okända mellanprocesser kan ingå i samma familj. Medlemmar sorteras
på PID/starttid och en levande root förekommer exakt en gång. Source, provider
och profile finns inte i grupperingsindatan och kan därför inte slå ihop
parallella starter.

Om en tidigare root saknas men en enda barnkedja lever används den högsta
observerade processen som observe-only-root och orsaken
`root_missing_child_alive` anges. Flera möjliga roots, eller en försvunnen
mellanprocess som lämnar flera möjliga komponenter i samma exekveringskontext,
ger en osäker familj med `ambiguous_root` och `multiple_possible_roots`; ingen
godtycklig kandidat väljs.

## Race-, exit- och diagnostiksemantik

Varje process `stat` läses före och efter övriga begränsade fält. Om processen
försvinner, blir oläsbar, återanvänder PID eller får ogiltig stat under läsningen
utelämnas den konservativt och PID:n markeras osäker. Rotfel som gör hela
proc-snapshoten omöjlig returneras som adapterfel; ett enskilt processrace gör
inte det.

`DiffSnapshots` jämför PID plus starttid och ger deterministiskt sorterade exits.
`DiffSamples` undertrycker en exit när samma PID var osäker i aktuell sampling.
En återanvänd PID ger gammal generation som exit och ny generation som process,
samt reason code `pid_reused`.

Lokala innehållsfria reason codes är:

- `identified_claude_family`, `identified_codex_family`, `unknown_process`;
- `ambiguous_root`, `multiple_possible_roots`, `root_missing_child_alive`;
- `process_disappeared_during_read`, `permission_denied`, `invalid_proc_data`;
- `pid_reused`, `argv_prefix_truncated`.

## Observe-only CLI

En säker standardsampling är ändlig och kräver ett explicit opakt host-ID:

```text
go run ./cmd/aurora-presence-observe -host-id observe-local
```

Val finns för `-proc-root`, `-boot-id`, `-clock-ticks`, `-samples`, `-interval`
och `-format json|text`. Flera samplingar är fortfarande begränsade till högst
100. Standardoutput innehåller endast räknare, opaka kandidatreferenser,
PID/starttid, medlemsantal, familjeform, exits och reason codes. Verktyget har
ingen nätverks-, registry-, relay-, hook- eller persistencekod.

## Live read-only-resultat på blue1

En enda lokal sampling kördes 2026-07-22 07:57 CEST utan sudo, installation,
bakgrundsprocess eller filutdata:

- 219 processer observerades;
- 1 Claude-familj och 2 Codex-familjer identifierades;
- 214 processer klassades `unknown` och 0 familjer var ambiguous;
- formerna var en direkt Claude, en Codex wrapper + Node-launcher + direkt
  verktygsprocess med tre medlemmar samt en direkt Codex;
- en cmdline var längre än argv-prefixgränsen och gav endast
  `argv_prefix_truncated=1`;
- inga parserfel, permissionsfel eller processrace rapporterades;
- en sampling gav avsiktligt inget exit-latensunderlag.

Ingen rå kommandorad, miljö, sökväg eller annan känslig processdata skrevs ut
eller sparades.

## Fortsatt öppet efter Paket 2 och medvetet ej implementerat

Observe-only-underlaget behöver fler märkta parallell-, wrapper-, reparenting-,
Ctrl+C-, krasch-, SSH- och terminaldödsfall för false-positive/negative-tal och
exit-latens. Verifierade executable-signaturer, USER_HZ per stödd arkitektur och
exakta Linux runtime-root-regler behöver kalibreras. Automatisk
hookbindningströskel beslutas först från representativt underlag och före ett
framtida muterande bindningspaket. Paket 3:s senare korrelation och Paket 4:s
lokala transport förblir observe-only och utgör inte ett sådant beslut.

Paket 2 implementerar ingen hookkorrelation eller bindning, collector, polling i
bakgrunden, registry-/slotmutation, relay-/v2-endpoint, persistence, feature flag,
installation, systemd, processpåverkan, firmware eller ändring av v1-beteende.
