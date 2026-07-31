# Aurora Product Architecture

Status: Proposed

This document defines the proposed product-level architecture for Aurora as a local-first, content-blind status system for parallel AI-agent sessions. It is a target architecture and does not change the current Linux production deployment.

## Scope

Aurora should let a normal user install, understand, trust, and remove the product without learning systemd, process graphs, cron, or internal agent logs.

The product must remain:

- local first;
- content blind;
- token free where possible;
- least privileged;
- transparent about observed metadata;
- stable for multiple concurrent sessions;
- robust against short-lived utility, auth, status, server, update, and helper processes.

## Verified Baseline

The current Linux development deployment has these verified properties:

- ESP firmware mirrors server-provided presentation and does not create sessions.
- `internal/runtimerecognition` builds runtime families from sanitized process observations.
- `internal/runtimepresence` syncs runtime families into the instance registry.
- `internal/instanceregistry` owns active instances, lifecycle, revisions, and slots.
- `internal/presencebroker.PresentationBridge` publishes distinct presentation transitions and intentionally does not coalesce transient states.
- Commit `55193b9` fixes the verified Codex utility-process ghost by ignoring `codex app-server ...` and `codex --version` as runtimes while preserving interactive Codex and `codex exec`.

## Target Component Model

The proposed product target is one per-user Aurora Agent per computer. The Agent is a composition boundary, not a monolith that erases internal module boundaries.

```text
AI tool processes and hooks
        |
        v
Platform ProcessBackend + local event ingress
        |
        v
Built-in RuntimeAdapters
        |
        v
Runtime recognition and safe correlation
        |
        v
Instance registry and stable slot projection
        |
        v
Presentation API and DeviceLink
        |
        v
ESP or fake presentation client
```

## Proposed Components

### Aurora Agent

Status: Proposed

The normal product should run one long-lived process per OS user. It composes process observation, adapters, sanitized event ingress, registry, presentation, diagnostics, and device communication.

This is a product target, not a Milestone 1 migration decision. Milestone 1 must not replace the current working Linux services.

### Embedded Presentation API

Status: Proposed Product Target

The normal product should eventually expose local presentation and diagnostics from the Agent instead of requiring users to manage a separate relay process.

This is not a decision to remove or replace `aurora-relay` during Milestone 1. The current relay remains valid for development, server, and advanced deployment.

### Advanced Relay Deployment

Status: Proposed

`aurora-relay` should remain available for advanced or server-style deployments where a separate local or network coordinator is useful.

### Platform Backends

Status: Proposed

Platform-specific code should translate OS process and local IPC information into OS-neutral contracts. It must not classify agent tools, build runtime families, mutate registry state, or publish presentation.

### Built-In Adapters

Status: Proposed

Claude, Codex, Grok, and future first-party adapters should be built in for the first product versions. External plugins are explicitly out of scope until a signed capability and sandbox model exists.

### DeviceLink

Status: Proposed

Device communication should be represented by a DeviceLink abstraction so LAN, USB, and fake CI clients can share presentation semantics.

LAN HTTP is the recommended first real device path because it matches current ESP behavior. USB remains a future option.

## Product Invariants

- One visible instance represents one real agent runtime generation.
- Utility processes must not create runtimes, instance IDs, slots, or pixels.
- Established slots must not move because of utility bursts or helper processes.
- Wrapper and child processes must not become two visible sessions.
- Two real independent sessions must not be merged.
- Hooks and structural event sources may update only safely bound runtimes.
- A hook event without a safe runtime binding must not create a visible instance.
- Later valid events with higher revision may clear `attention`.
- PresentationBridge semantics must not be changed to hide invalid intermediate registry states.

## Milestone Boundary

Milestone 1 is limited to Agent Core Contract + Linux Compatibility Shell. It defines contracts and compatibility tests around the existing architecture. It does not replace current production services, change ESP firmware, change slot semantics, change PresentationBridge, or introduce auto-update or plugins.

## Open Product Decisions

- Persistent slot mapping: Experiment Required.
- Embedded local presentation API activation path: Proposed target, migration undecided.
- USB DeviceLink: Experiment Required.
- Windows and macOS process backends: Experiment Required.
- Auto-update: deferred; first product version should be manual or opt-in.
