# Milestone 1: Aurora Agent Core Contract + Linux Compatibility Shell

Status: Proposed

Milestone 1 creates product architecture contracts and compatibility scaffolding around the current Linux system. It must not replace the working production deployment.

## Goal

Define and validate the Agent core contracts needed for a future user-friendly Aurora product while proving that the current Linux runtime, relay, presentation, slot, and ESP behavior remain untouched.

## Explicit Non-Goals

Milestone 1 must not:

- replace current production services;
- change ESP firmware;
- port to Windows or macOS;
- introduce auto-update;
- introduce external plugins;
- change slot semantics;
- change PresentationBridge semantics;
- change `aurora-relay` production behavior;
- install, deploy, restart, enable, or disable services.

Embedded presentation/API is a proposed product target, not a Milestone 1 migration decision.

Persistent slot mapping is Experiment Required and must not be implemented in Milestone 1.

## Deliverables

### Agent Config Schema

Design a config schema for future Agent composition:

- enabled adapters;
- safe mode;
- privacy mode;
- diagnostics settings;
- DeviceLink configuration;
- local API binding mode;
- compatibility mode for current Linux deployment.

Milestone 1 may document or test the schema, but must not activate a new installed Agent.

### Safe Mode Semantics

Safe mode means:

- only built-in verified adapters;
- no external plugins;
- no raw capture;
- no raw diagnostics;
- no broad environment reads;
- LAN exposure only with explicit configuration or pairing;
- strict schema validation for events and diagnostics.

### Adapter Capability Manifest

Each adapter declares:

- supported agent ID;
- supported runtime recognizer evidence;
- allowed structural command tokens;
- allowed event sources;
- allowed structural status files or logs;
- forbidden fields;
- utility/background command rules.

### Sanitized Diagnostics Contract

Define a diagnostics export that includes:

- schema version;
- component versions;
- active adapters and capabilities;
- opaque instance summaries;
- state/revision summaries;
- reason-code counters;
- DeviceLink health without tokens;
- backend read failure counters.

It must exclude raw argv, environment, raw logs, transcript paths, prompts, answers, tokens, cookies, and project paths.

### Fake DeviceLink / Fake ESP

Define a fake DeviceLink that can consume presentation snapshots without physical ESP hardware. It should verify:

- stable slots;
- active/visible/overflow counts;
- sleeping/offline distinction;
- no unexpected tool-kind dependency in ESP presentation;
- utility bursts do not create pixels.

### Replay And Contract Tests

Define fixtures for:

- Codex utility burst: `codex app-server ...`, `codex --version`;
- interactive Codex and `codex exec`;
- two real separate Codex sessions;
- Claude hook lifecycle;
- Grok fast `attention -> working` or equivalent clear event;
- wrapper and child family collapse;
- hook without safe runtime binding.

### Composition Plan

Document how the future Agent shell can reuse existing packages without changing current production behavior.

## Packages Expected To Be Reused

These packages are expected to remain reusable without semantic change in Milestone 1:

- `internal/runtimerecognition`;
- `internal/runtimepresence`;
- `internal/instanceregistry`;
- `internal/presencebroker`;
- `internal/presencev2`;
- `internal/instancepresence`;
- `internal/instancecorrelation`;
- `internal/codexhook` recognizer semantics including commit `55193b9`;
- `internal/claudehook` event mapping baseline;
- `internal/grokpresence` as a separate adapter surface.

## Packages Not To Change In Milestone 1

- ESP firmware;
- production service files;
- deploy scripts;
- `PresentationBridge` semantics;
- slot allocation semantics;
- `aurora-relay` runtime behavior;
- Go production code unless a later approved implementation plan explicitly scopes it.

## Acceptance Criteria

Milestone 1 is complete when:

- contracts are documented and reviewed;
- fake DeviceLink behavior is specified;
- diagnostics forbidden fields are specified;
- adapter capability manifest semantics are specified;
- replay fixtures are identified;
- existing Linux production services are not changed;
- future implementation can prove `go test ./...` remains green;
- no file outside the approved documentation or later approved test scope is touched.

## Test Plan For Later Implementation

- Unit tests for config schema validation.
- Unit tests for adapter capability manifest validation.
- Contract tests for process backend fixtures.
- Replay tests for sanitized event sequences.
- Fake ESP tests for presentation invariants.
- Forbidden-field tests for diagnostics output.
- Regression tests for Codex utility-process rejection.

## Rollback

Because Milestone 1 is documentation and compatibility scaffolding only, rollback is deletion or revert of the new docs/tests. There is no service state, installed binary, ESP firmware, or production runtime state to roll back.

## Proof Existing Linux Drift Is Unaffected

- No production service is replaced.
- No deploy or systemd file is modified.
- No ESP firmware is changed.
- No PresentationBridge or slot code is changed.
- Current relay remains available for existing deployment.
- Any future shell must run in compatibility or foreground mode until explicitly approved for installation.
