# Aurora Adapter And Event Contract

Status: Proposed

This document defines the design-level contract for runtime adapters and sanitized events. It is Go-oriented but intentionally not implementation code.

## Adapter Boundary

A RuntimeAdapter owns agent-specific recognition and event mapping. It does not own registry mutation, slot allocation, presentation, device communication, or persistence.

Adapters must operate on sanitized inputs and produce content-free outputs.

## RuntimeAdapter Contract

Required fields and functions at design level:

- `AgentID`: namespaced identifier, for example `anthropic.claude`, `openai.codex`, `xai.grok`.
- `LegacyToolKind`: compatibility projection when needed by current internal APIs.
- `Capabilities`: declared process fields, event sources, and structural files the adapter may use.
- `Recognize(ProcessObservation) -> RuntimeCandidate | Reject`.
- `ClassifyCommand(CommandTokens) -> CommandClassification`.
- `MapEvent(AdapterLocalInput) -> SanitizedEvent | Reject`.
- `ValidateCapabilities() -> error`.

`AdapterLocalInput` is adapter-private parse input. It exists only inside the specific adapter, is never a shared core contract, must never be stored or forwarded, and must be reduced to `SanitizedEvent` before any information leaves the adapter.

Forbidden fields:

- prompt text;
- assistant response text;
- complete argv;
- general environment;
- raw transcript content;
- raw log lines;
- project file contents;
- tokens or cookies.

## CommandClassification

Required fields:

- `Kind`: `interactive`, `exec`, `utility`, `background`, `auth`, `version`, `status`, `update`, `unknown`.
- `Confidence`: `exact`, `strong`, `weak`, `rejected`, `ambiguous`.
- `ReasonCodes`: content-free reason identifiers.
- `StructuralToken`: optional allowlisted command token.

Rules:

- `utility`, `background`, `auth`, `version`, `status`, and `update` must not create a runtime.
- `unknown` must fail closed unless the adapter has separate executable or launch evidence that is explicitly allowed.
- Classification must use only allowlisted structural tokens, never full argv.

## SanitizedEvent

Required fields:

- `AgentID` or compatibility `ToolKind`;
- `HookSessionRef` or adapter-local opaque event stream reference;
- `Lifecycle`: active, idle, ended, or adapter-specific mapped lifecycle;
- `EffectiveState`: idle, working, attention, or error for non-ended events;
- `ProducerEpoch`;
- `Revision` or monotonic sequence;
- `ObservedAt` assigned by the long-lived receiver when possible;
- `SourceKind`: process, hook, structural_log, explicit_lifecycle;
- `ReasonCodes`.

Forbidden fields:

- raw event payload;
- raw transcript path;
- raw log line;
- prompt or response content;
- full command line;
- unredacted filesystem paths.

Event reduction invariant:

- An adapter-approved structural log source must be reduced immediately to `SanitizedEvent`. Raw input must not reach event ingress, runtime recognition, registry, diagnostics, or normal logs.

Error behavior:

- Unsupported event: reject without side effect.
- Missing session reference: reject unless the adapter has a documented safe global lifecycle event.
- Stale revision: no state rollback.
- Ambiguous runtime binding: quarantine/diagnostic only.

## RuntimeCandidate

Required fields:

- `AgentID` or compatibility tool kind;
- root process identity;
- runtime key for active evaluation;
- command classification;
- confidence;
- reason codes;
- candidate family members.

Forbidden fields:

- slot;
- visible state claim from hook;
- raw argv;
- raw environment;
- raw paths.

## RuntimeFamily

Required fields:

- root process;
- members;
- launch process if different from root;
- family shape;
- suspended flag if available;
- confidence and reason codes.

Rules:

- Wrapper and child must not become two visible sessions when ancestry and adapter rules identify one runtime family.
- Multiple roots must produce an uncertain family unless a deterministic tested rule selects one root.

## StateMutation

Required fields:

- target `instance_id`;
- producer epoch;
- revision;
- lifecycle;
- effective state for active/idle updates;
- source kind;
- observed timestamp;
- idempotency key;
- reason codes.

Rules:

- Runtime process observation owns existence and base lifecycle.
- Hook and structural events own activity claims only after safe binding.
- Stale revisions must not restore older state.
- Later valid events may clear `attention`.

## Adapter Invariants

All adapters must satisfy these invariants:

- Utility processes do not create runtimes, slots, or pixels.
- Wrapper and child processes do not become duplicate sessions.
- Two real sessions do not merge into one runtime.
- Hook and process observation do not double-register the same session.
- Hook events without secure runtime binding do not create visible instances.
- Established `instance_id` and slot do not change because of utility bursts.
- Later higher-revision events can move from `attention` to `working`, `idle`, `ended`, or `cancelled` according to adapter mapping.
- Stale revisions and stale epochs cannot overwrite newer state.

## Claude Rules

Status: Proposed

- Claude hooks are primary state signals.
- Claude runtime process observation may create idle runtime presence.
- Claude hook events must bind to exactly one runtime before mutating visible state.
- Unknown or ambiguous hook events must not create a new visible instance.

## Codex Rules

Status: Partly Verified, Partly Proposed

Verified baseline:

- `codex app-server ...` is utility and must not create runtime.
- `codex --version` is utility/version and must not create runtime.
- Interactive Codex remains recognized.
- `codex exec ...` remains recognized.
- Two real separate Codex sessions remain separate.

Proposed extensions:

- Other Codex utility/auth/status/update/server commands must be classified before runtime creation.
- Codex command classification must remain structural and must not retain prompt text or full argv.

## Grok Rules

Status: Proposed

- Grok structural event sources may be read only as adapter-approved structural signals.
- Raw log lines must never be stored, logged, exported, or leave the adapter.
- Later events such as tool execution, streaming, turn ended, or cancelled must be able to clear `attention` if they have a higher valid revision.
- The known fast approval attention-stuck bug must be solved in adapter/state sequencing, not by ESP timeout.

## Future Adapter Rules

Status: Proposed

- Future adapters must declare capabilities before activation.
- Safe mode runs only built-in verified adapters.
- Unknown adapters are disabled by default.
- A plugin model requires a separate signed capability and sandbox decision.

## Conflict Resolution

Priority is not a global state priority. Conflict handling is by owner and revision:

1. Process observation owns runtime existence, lease, missing, and ended lifecycle.
2. Hook events own activity claim only after safe binding.
3. Structural log events own adapter-specific activity claim only after safe binding or explicit adapter-local runtime association.
4. Explicit lifecycle events may close a hook stream but must not kill a live runtime by themselves.
5. Lease expiry can end runtime presence after grace, but cannot be used by ESP to mask stale attention.

An explicit adapter-local runtime association may refer only to an already identified runtime, a verified local association to that runtime instance, and an association that satisfies the adapter's binding rules. It must never create a new visible instance, create a runtime from an unbound hook or log event alone, or bypass process-observation runtime identification. Uncertain or unbound events may only produce quarantine or diagnostic reason codes.

A process renewal must not lower a hook-owned state. A stale hook or structural event must not overwrite a later mutation.
