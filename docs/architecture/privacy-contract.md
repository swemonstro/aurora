# Aurora Privacy Contract

Status: Proposed

This document defines Aurora's privacy boundary for product architecture, adapters, diagnostics, and device communication. It is stricter than implementation convenience. If a future adapter cannot meet it, that adapter must be explicitly reviewed before product activation.

## Core Rule

Aurora observes local metadata needed to display AI-agent status. Aurora does not collect, persist, export, or transmit user content, credentials, prompts, answers, terminal output, project contents, raw transcripts, or broad environment data.

## Data Classes

| Data | May Read? | May Exist In Memory? | May Persist Locally? | May Enter Diagnostics? | May Go To ESP? | Retention | Reason |
| --- | --- | --- | --- | --- | --- | --- | --- |
| PID | Yes | Yes | Active runtime identity and short tombstone only | Opaque or redacted form only | No | Active runtime plus short tombstone | Needed for generation-safe identity with start time. |
| Process start time | Yes | Yes | Active runtime identity and short tombstone only | Opaque or redacted form only | No | Active runtime plus short tombstone | Prevents PID reuse confusion. |
| Parent PID | Yes | Yes | No, except transient runtime evidence | Counters or reason codes only | No | Snapshot/evaluation window | Needed for family/root selection. |
| Executable basename | Yes | Yes | Adapter reason evidence if needed | Yes, if non-sensitive basename only | No | Short diagnostic window | Needed for recognition. |
| Executable full path | Limited | Transient only | No by default | No | No | None after reduction | Paths can reveal user/project/tool layout. |
| Complete argv | No | No | No | No | No | None | May contain prompts, file paths, or secrets. |
| Structural argv tokens | Yes, allowlisted | Yes | No by default | Classification only | No | Evaluation window | Needed to reject utility commands such as `codex app-server`. |
| Working directory | Avoid; adapter-local only if required | Transient only | No | No | No | None after local decision | Reveals project names and filesystem structure. |
| General environment | No | No | No | No | No | None | Often contains credentials and tokens. |
| `CODEX_HOME` or equivalent profile path | Allowlisted only | Transient only | No raw path | No raw path | No | None after local decision | May be needed for local profile/trust lookup. |
| Adapter-approved structural status files | Yes, minimum fields only | Yes, within adapter | Derived state only | Reason codes only | No | Evaluation window | Some tools expose state only through local structured metadata. |
| Raw log lines | Only from explicitly adapter-approved structural log sources | Immediate adapter parse-scope only | Never | Never | Never | Discard immediately after parsing | Raw log lines may contain content and must never leave the adapter. |
| Raw event payloads | No normal operation | Adapter-local parse buffer only | Never | Never | Never | Discard after mapping | Hooks may include paths or content. |
| Raw transcripts | No | No | No | No | No | None | Contains prompts and answers. |
| Opaque `instance_id` | Yes | Yes | Yes | Yes | Yes | Active plus tombstone/presentation history | Content-free runtime identity. |
| Tool kind | Yes | Yes | Yes | Yes | Not by default | Active plus diagnostics | Needed internally; ESP normally needs only state/slot. |
| State | Yes | Yes | Yes | Yes | Yes | Active plus short history | Core product output. |
| Revision | Yes | Yes | Yes | Yes | No by default | Active plus tombstone | Prevents stale state rollback. |
| Reason codes | Yes | Yes | Aggregated or recent bounded | Yes | No | Bounded | Content-free diagnostics. |
| Slot | Yes | Yes | Experiment Required for persistence | Yes | Yes | Active; persistence undecided | Stable physical presentation. |
| IP address of paired device | Yes | Yes | Yes, if paired | Redacted or local subnet only | No | Until unpair/reset | Needed for LAN DeviceLink. |
| Pairing token | Yes | Yes | Yes, private local storage | Never | Used by device, not displayed | Until unpair/rotate | Device authorization. |
| API token/OAuth token/session cookie | No | No | No | No | No | None | Explicitly forbidden secret material. |

## Diagnostics Policy

Diagnostics export must be explicit user action. It must be content-blind and must pass a forbidden-field scan before writing the export bundle.

Allowed diagnostic examples:

- component versions;
- enabled adapters and capabilities;
- active counts and presentation shape;
- opaque instance IDs;
- state and revision summaries;
- reason-code counters;
- process read failure counters;
- DeviceLink health without tokens.

Forbidden diagnostic examples:

- raw argv;
- raw environment;
- raw hook payloads;
- raw log lines;
- raw transcript paths or contents;
- prompt or response text;
- project paths;
- pairing tokens or credentials.

## Adapter Log Rule

Adapter-approved structural log sources may be read only to derive allowed state or reason codes. Reading must be limited to the minimum necessary structural fields. Raw log lines may exist only inside the adapter's immediate parse-scope, must be discarded directly after parsing, and must never be saved, logged, exported, sent to registry, or leave the adapter boundary.

## Device Privacy Rule

ESP communication uses presentation data only. By default this is limited to schema/API version, pixel, opaque instance ID, state, and capacity/overflow fields. Tool kind is not sent to ESP by default.

## Persistence Rule

PID and process start time are runtime identity evidence for active instances and short tombstones. They must not become long-lived slot-persistence keys. Any durable slot mapping design is Experiment Required and must avoid retaining raw process identity longer than needed.
