# Aurora Process Backend Contract

Status: Proposed

This document defines the design-level contract for platform-specific process observation. It does not implement Windows or macOS support and does not change the current Linux backend.

## Purpose

A ProcessBackend translates operating-system process metadata into sanitized, OS-neutral observations for runtime recognition. It is not an agent adapter and must not decide that a process is Claude, Codex, Grok, or any future agent.

## ProcessBackend Contract

Required behavior:

- `Snapshot(context) -> ProcessSnapshot` from one bounded observation pass.
- `BootIdentity(context) -> opaque boot identity` where the platform supports it.
- capability reporting for fields that may be unavailable on a platform.
- content-free diagnostics counters.

Required snapshot fields:

- observed timestamp;
- boot identity when available;
- process observations;
- backend diagnostics.

Forbidden behavior:

- tool classification;
- runtime family construction;
- registry mutation;
- slot allocation;
- presentation publishing;
- logging raw command lines, environment, working directories, transcripts, or prompts.

## ProcessObservation Contract

Required fields:

- process identity: PID or platform process identifier plus generation/start time when available;
- parent relation: verified parent identity or conservative parent hint;
- executable identity: sanitized basename or opaque executable identity;
- owner identity as opaque value if available;
- process group, job, session, terminal, or console identities as opaque values when available;
- optional sanitized launch identities derived from allowlisted rules;
- optional adapter-approved structural command tokens.

Forbidden fields:

- complete argv;
- raw command line;
- broad environment;
- raw working directory;
- prompt text;
- terminal contents;
- raw transcript path or content;
- source code or project file names.

## Structural Command Tokens

Backends may expose structural command tokens only when all conditions hold:

- the token is allowlisted by adapter or composition rules;
- option values and free-form arguments are discarded;
- paths are reduced to safe basename or launch identity;
- tokens are used only for recognition and not public diagnostics.

Example allowed Codex tokens include `codex`, `exec`, `app-server`, and `version`. The existence of such tokens does not permit storing full argv.

## Error Behavior

- If a process disappears during observation, mark uncertain or omit it.
- If parent generation cannot be verified, provide only a hint or no parent.
- If access is denied, increment diagnostics and fail closed.
- If start time cannot be obtained, the backend must lower confidence or omit the process from runtime identity creation.
- If command token extraction fails, recognition must continue with executable evidence where previous behavior allows it.

## Privacy Rules

- Raw OS data must be reduced inside the backend before crossing the backend boundary.
- PID and start time are active runtime identity evidence and short tombstone evidence only. They are not long-lived slot-persistence data.
- Full executable paths should not cross the boundary unless converted to an opaque identity needed for recognition.
- Environment reads must be avoided by default. Any allowlisted variable such as `CODEX_HOME` must be reduced locally and must not be logged or exported as a raw path.

## Platform Variations

### Linux

Status: Partly Implemented

The existing Linux backend uses `/proc` to derive PID, parent, start time, process group, session, terminal, sanitized executable identities, limited structural argv tokens, and selected allowlisted local metadata.

Linux-specific details must remain backend internals and must not become general contract fields.

### Windows

Status: Experiment Required

A Windows backend must determine which data can be read as a normal user without broad privileges. It should prefer per-user observation and named-pipe ingress. Administrator-only process access must not be required for normal product operation.

Experiments required:

- generation-safe process identity using creation time;
- parent relation reliability;
- executable identity access without `SeDebugPrivilege`;
- command classification without retaining command lines;
- named-pipe peer authentication.

### macOS

Status: Experiment Required, Not Hardware Verified

A macOS backend must be validated on real macOS hardware before support is claimed. It should start with user-level process APIs and LaunchAgent deployment.

Experiments required:

- process identity and start time availability;
- parent relation reliability;
- executable identity access;
- structural command token extraction without content leakage;
- whether any higher-privilege event framework is necessary or avoidable.

Endpoint-style privileged monitoring is not part of the default product contract.

## Acceptance Invariants

- Process names alone are insufficient for final product recognition.
- Utility commands must be rejectable before runtime creation.
- Wrapper and child processes must be representable without duplicate visible sessions.
- Two independent process generations must remain distinct.
- Backend uncertainty must not create visible instances by itself.
