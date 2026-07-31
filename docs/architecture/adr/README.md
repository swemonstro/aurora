# Aurora Architecture Decision Records

Status: Proposed Index

This directory is reserved for focused Architecture Decision Records. It intentionally starts with an index only. No ADR in this directory is accepted until written and reviewed as a separate document.

## Proposed ADRs

### ADR: Per-User Aurora Agent

Status: Proposed

Decision to make one per-user Aurora Agent the normal product target, while allowing advanced deployments to run separate services.

### ADR: Embedded Local Presentation API

Status: Proposed Product Target

Decision to target an embedded local presentation/API surface in the Agent for normal users. This is not a Milestone 1 migration decision and does not remove `aurora-relay`.

### ADR: Relay For Advanced Deployment

Status: Proposed

Decision to keep `aurora-relay` for server, development, and advanced deployment scenarios.

### ADR: LAN-First DeviceLink

Status: Proposed

Decision to use LAN HTTP with pairing/discovery as the first real DeviceLink path, with USB left as a future option.

### ADR: Built-In Adapters First

Status: Proposed

Decision to ship only built-in verified adapters at first and defer external plugins until a signed capability and sandbox model exists.

### ADR: Manual Or Opt-In Updates First

Status: Proposed

Decision to avoid default automatic update in the first product version.

### ADR: Persistent Slot Mapping

Status: Experiment Required

Decision is not ready. Experiments must determine persistence format, retention, restart behavior, and privacy impact. PID and start time must not become long-lived slot-persistence keys.

### ADR: Windows Process Backend

Status: Experiment Required

Decision is not ready. Experiments must validate normal-user process identity, parent relation, executable identity, command classification, and named-pipe peer authentication.

### ADR: macOS Process Backend

Status: Experiment Required, Not Hardware Verified

Decision is not ready. Experiments must be run on real macOS hardware before support is claimed.

### ADR: USB DeviceLink

Status: Experiment Required

Decision is not ready. Experiments must validate serial permissions, onboarding, firmware behavior, and Windows/macOS/Linux UX.

## ADR Template

Each ADR should include:

- status;
- context;
- alternatives;
- decision;
- motivation;
- drawbacks;
- privacy impact;
- migration impact;
- experiments if not decided;
- rollback;
- links to tests or verification evidence.
