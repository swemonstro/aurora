# Package 7: safe binding policy and correlation lifecycle

Status: normative design; planned, not implemented, integrated, or active in
production

Package 7 defines when Aurora may treat a sanitized hook observation as
**safely bindable** to exactly one local runtime instance, and how that binding
decision evolves over the correlation lifecycle. It produces binding decisions
and proposals only. It does **not** mutate the registry, slots, hook claims,
runtime claims, or any visible presence state. Package 8 is the first package
allowed to apply approved decisions as mutation.

This document is documentation only. It does not redesign Packages 0–6 and does
not change the working overall ESP / v1 status path.

Related documents:

- [canonical roadmap](per-instance-presence-roadmap.md)
- [integration contract](per-instance-presence-integration-contract.md)
- [architecture decisions](per-instance-presence-decisions.md)
- [Package 3 correlation](per-instance-presence-package-3.md)
- [Package 6 local ingress](per-instance-presence-package-6.md)
- [Package 7.0 trusted identity bridge](per-instance-presence-package-7-trusted-identity-bridge.md)
- [main design](per-instance-presence.md)

## 1. Purpose and product boundary

### 1.1 What Package 7 answers

Given:

- one sanitized hook stream identified by `(tool, hook_session_ref)`;
- zero or more observed local runtimes for the same tool;
- optional prior Package 7 decision state for that hook stream;
- optional **trusted hard process identity** for the hook side (section 2.0);

decide deterministically whether Aurora may:

| Outcome | Meaning |
| --- | --- |
| `propose_bind` | Evidence is strong enough that Package 8 *may* create or refresh a binding later |
| `keep` | An existing accepted decision remains valid; no new bind proposal |
| `suspend` | Prior decision is no longer trusted for activity claims; retain history only |
| `replace` | **One atomic** policy decision: prior association ends and a new unique hard match is authorized (Package 8 applies mutation safely later) |
| `remove` | Prior decision is terminated with no simultaneous new association |
| `refuse` | No safe bind; fail closed; no mutation proposal |

### 1.2 What Package 7 must not do

Package 7 must not:

- mutate `instanceregistry`, slots, hook claims, or runtime claims;
- publish to relay, v2, or any presence backend;
- change v1 source aggregation or ESP presentation;
- invent hard process identity from soft signals alone;
- treat payload-declared PID/start-time/runtime fields as trusted identity;
- emit `propose_bind` for a **new** association when no trusted hard identity
  bridge is available (section 2.0);
- bind on `weak`, `ambiguous`, or `rejected` evidence;
- treat Package 3’s `would_bind_under_current_threshold` as automatic bind
  authorization;
- persist decision state outside a bounded in-memory window owned by the long-
  lived local presence process;
- install, daemonize, or enable production features by default;
- treat unapproved measurement defaults as final product safety thresholds
  (section 5 and section 16).

### 1.3 Boundary with Package 8

```text
Package 3  -> scored proposals / ambiguity / rejection  (observe-only)
Package 6  -> sequenced ingress (tool, session, lifecycle only today)
Package 7  -> BindingDecision (policy; no mutation)
Package 8  -> apply BindingDecision to registry/slots (separate activation)
```

Package 8 may consume only decisions whose `decision` is `propose_bind`,
`keep`, `suspend`, `replace`, or `remove`, and only when its own feature flag
and safety gates are on. Package 7 never calls the registry.

For `replace`, Package 7 emits **one** atomic policy decision carrying old and
new runtime refs. Package 8 alone decides the safe mutation sequence (for
example clear then bind, or transactional update). Package 7 does **not**
require a separate earlier `remove` decision before a later `propose_bind` when
the replace preconditions hold.

### 1.4 Entry criteria from Package 6

Package 7 implementation work must not start until Package 6 exit criteria are
met and reviewed, including:

- real Claude and Codex event delivery verified behind a default-off flag;
- server-owned sequencing without dummy epochs/revisions;
- every Package 6 response reports `no_binding_performed=true`;
- no registry/slot/relay/v2 coupling from Package 6.

Package 7’s **docs-first contract** may exist earlier so policy is locked before
code. Useful Package 7 implementation that can emit `propose_bind` for new
associations is additionally blocked until a **trusted hard identity bridge**
exists (section 2.0). That bridge is specified as prerequisite subpackage
**Package 7.0** in
[per-instance-presence-package-7-trusted-identity-bridge.md](per-instance-presence-package-7-trusted-identity-bridge.md).
It is not provided by Package 6 as implemented today and is not an implicit
capability of Package 7 policy itself.

## 2. Inputs and trust model

### 2.0 Package 6 today: no server-attested hard process identity

**Normative fact about Packages 0–6 as implemented today:**

Package 6 local ingress carries only:

- `tool`
- `hook_session_ref`
- `lifecycle`

plus server-owned `ProducerEpoch`, `Revision`, and `ObservedAt` after accept.

It does **not** provide:

- server-attested PID + process start time for the hook sender;
- server-attested runtime identity linking the hook stream to a process family;
- any other hard process generation evidence on the hook side.

Therefore, with Packages 0–6 **exactly as implemented today**, Package 7:

1. **must fail closed** for any evaluation that would create a **new**
   association;
2. **must not emit `propose_bind`** (and must not emit `replace` that creates a
   new association) unless hard identity is supplied by an **explicitly
   defined, trusted server-side source** outside today’s Package 6 payload;
3. may still emit `refuse`, and may evaluate lifecycle of a prior decision only
   if that prior decision was created under a later, approved identity bridge
   (or under test fixtures that inject trusted identity).

This missing trusted identity bridge is an **explicit prerequisite** for useful
Package 7 implementation. It is **not** assumed to exist inside Package 7
policy itself. The normative Linux design is **Package 7.0** (trusted identity
bridge): kernel peer credentials, server-side generation lookup, and bounded
ancestry join to an already recognized runtime family — never client-declared
process fields. See
[Package 7.0](per-instance-presence-package-7-trusted-identity-bridge.md).

#### Allowed future source boundary (not implemented here)

Hard identity for bind eligibility must be **derived or attested locally by
Aurora**, never trusted from agent payload alone. Allowed future sources are
limited to:

| Allowed class | Examples (future, versioned) | Status |
| --- | --- | --- |
| Trusted local process/runtime observation | OS-neutral runtime snapshot members already attested by the process backend (PID + start time, host/boot as backend provides) used only as the **runtime** side of a pair | Exists for runtimes (Packages 2/5); not a hook-side identity |
| Server-side attestation of the hook peer | Package 7.0: ordered bounded attestation transaction — immediate `SO_PEERCRED` + peer generation after accept, then Package 6 request validation, then tool-namespaced ancestry join; one immutable checkpoint result (evidence or explicit absence). Not operationally atomic process inspection | Designed in Package 7.0 docs; not implemented; Blue1 evidence still required |
| Later **versioned** ingress contract | A new protocol version that carries only fields Aurora can treat as server-attested or that are filled server-side after attestation — never raw client claims as hard identity | Not Package 6 v2 as shipped; not required if Package 7.0 server-side attestation succeeds |

**Forbidden as hard identity:**

- payload-declared PID, start time, or runtime identity from the hook client;
- soft signals (process group, OS session, terminal fingerprint, start-time
  proximity, tool match alone);
- session-ID, source, provider, profile, CWD, argv, transcript path;
- `SO_PEERCRED` PID alone without generation and unique runtime link;
- Package 3 `would_bind_under_current_threshold` without trusted hard identity.

Payload-declared process/runtime hints, if they appear on any path, remain
**untrusted claims**. They may be used only as soft/diagnostic input under
Package 3 rules and must never alone authorize `propose_bind` or `replace`.

### 2.1 Inputs

| Input | Provenance | Role in Package 7 |
| --- | --- | --- |
| Sequenced hook observation | Package 6 server: epoch, revision, `ObservedAt`, `tool`, `hook_session_ref`, `lifecycle` | Subject of the decision |
| Trusted hook-side hard identity (optional) | Package 7.0 checkpoint result (or later approved equivalent) — **not** Package 6 payload; absence is explicit (`trusted_hard_identity_present=false`) | Required for new `propose_bind` / `replace` |
| Runtime observations | OS-neutral snapshot + agent recognizers | Candidate set (runtime-side hard identity) |
| Package 3 correlation result | Deterministic scoring of the same inputs | Evidence classification |
| Prior Package 7 decision (if any) | In-memory decision table for this epoch | Lifecycle continuity |
| Evaluation time | Server clock at decision, `Round(0).UTC()` | Staleness and ordering |

### 2.2 Hard identity (required for bind eligibility)

A pair is **hard-identity positive** only when **both** hold:

1. The **runtime** side has a validated generation-safe identity (PID + start
   time at root or member, with host/boot as required by the runtime model).
2. The **hook** side has **trusted** hard process evidence (section 2.0) that
   matches that runtime root or member (or full runtime identity), free of hard
   conflicts.

Matching forms (once trusted hook-side evidence exists):

1. **Exact runtime identity**: same `host_id` + `boot_id` + root
   `ProcessIdentity` (PID + start time).
2. **Exact root process identity**: trusted hook process evidence matches the
   runtime root with PID + start time.
3. **Exact member process identity**: trusted hook process evidence matches a
   validated family member with PID + start time.

Without trusted hook-side hard identity, the pair is **not** hard-identity
positive, regardless of soft score or Package 3 confidence labels derived from
incomplete inputs.

Soft signals alone (process group, OS session, terminal fingerprint, start-time
proximity, tool match, host/boot metadata without process generation) may
support diagnosis and weak proposals, but **never** authorize `propose_bind` or
`replace`.

### 2.3 Hard conflicts (always fail closed)

Any hard conflict on the candidate pair forces `refuse` (or `suspend` /
`remove` for an existing decision when re-evaluated), including:

- tool mismatch;
- host or boot mismatch when both sides assert values;
- PID reuse with different start time;
- trusted process/runtime evidence pointing at another candidate;
- stale hook or runtime observation beyond the configured age window
  (section 5 — values are proposed defaults until approved);
- impossible time order (process started materially after the hook observation);
- ended/live lifecycle contradiction on the evaluated pair;
- producer-epoch conflict for the same hook stream when comparing revisions
  across epochs (epochs are not ordered against each other);
- missing trusted hard identity when evaluating a **new** association
  (`missing_trusted_hard_identity`).

Soft signal agreement cannot override a hard conflict.

## 3. Confidence and eligibility rules

Package 7 reuses Package 3 confidence labels. It adds a separate **bind
eligibility** gate so that scoring thresholds are not silently treated as
authorization.

| Confidence | Package 3 meaning | Package 7 bind eligibility |
| --- | --- | --- |
| `exact` | Hard runtime or root identity match **using trusted evidence** | Eligible **only if** unique, non-ambiguous, hard-identity positive, and exact/strong policy is human-approved for consumption (section 16) |
| `strong` | Hard member identity (or hard-positive + score) **using trusted evidence** | Same gates; **whether strong is authorized at all** is a proposed default requiring human approval (section 16) |
| `weak` | Soft signals only | **Never** eligible |
| `rejected` | Conflict or insufficient score | **Never** eligible |
| `ambiguous` | Competing global assignment | **Never** eligible |

Until exact-vs-strong authorization is human-approved for mutating consumers,
implementations must not present either label as a final product safety
threshold. Policy code may still classify them for dry-run diagnostics.

### 3.1 Mandatory prerequisites for `propose_bind` (new association)

All of the following must hold at the evaluation instant:

1. **Package 6 sequencing accepted** the hook observation (valid epoch, positive
   revision, server `ObservedAt`).
2. **Hook validation** succeeds (tool, non-empty session ref, lifecycle).
3. **Trusted hard identity** is present for the hook side from an allowed
   source (section 2.0). With Package 6 as implemented today, this fails.
4. **Exactly one** runtime candidate is selected for this hook after global
   matching.
5. That pair’s confidence is `exact` or `strong` **and** that class is approved
   for authorization under section 16 (until approval: dry-run only).
6. That pair is **hard-identity positive** (section 2.2).
7. The pair has **zero** hard conflicts.
8. The pair is **not** part of an ambiguity group (section 4).
9. Hook and runtime observations are **fresh** under the active age window
   (section 5).
10. Runtime lifecycle is compatible with the intended action (section 6).
11. No prior decision forbids the action (section 7).
12. Decision-table and correlation candidate limits are not exceeded.

If any prerequisite fails → **fail closed** (`refuse`, or a lifecycle action
that does not create a new association).

With Packages 0–6 only, prerequisite 3 fails for every new association →
**`refuse`**, never `propose_bind`.

### 3.2 Relationship to `would_bind_under_current_threshold`

Package 3 may set `would_bind_under_current_threshold=true` for exact/strong
scored edges. Package 7 treats that flag as **diagnostic only**. Authorization
requires section 3.1 in full. Implementations must not alias the two.

## 4. Zero, one, or several matching runtimes

Let `H` be the hook stream and `R` the set of same-tool live runtime candidates
in the current snapshot (bounded; Package 3 uses a maximum of 12 as a
**proposed** evaluation bound — section 5).

| Situation | Decision |
| --- | --- |
| No trusted hook-side hard identity | `refuse` for new association (`missing_trusted_hard_identity`) |
| `\|R\| = 0` | `refuse` — unmatched hook; no bind |
| Exactly one hard-identity-positive exact/strong unique pair | `propose_bind` only if section 3.1 holds fully |
| Multiple runtimes, one unique winner after global match, not ambiguous | `propose_bind` only if section 3.1 holds fully |
| Two or more candidates within ambiguity delta of the best assignment | `refuse` as ambiguous — no bind for any involved hook/runtime |
| Best score is weak only | `refuse` |
| Best pair hard-conflicts | `refuse` |
| Candidate set exceeds configured maximum | `refuse` for the batch (`candidate_limit_exceeded`) — no partial greedy bind |

### 4.1 Simultaneous sessions and runtimes

- At most **one runtime per hook stream** may be proposed.
- At most **one hook stream per runtime** may hold an active Package 7 decision
  of type `propose_bind` / `keep` / `replace`-result Bound at a time (one-to-one).
- If two hook streams compete for the same runtime under global matching,
  Package 3 marks ambiguity or selects one edge; Package 7 never binds both.
- Two Claude (or two Codex) sessions in the same CWD/TTY must remain unbound
  unless trusted hard identity separates them. Soft co-location is insufficient.

### 4.2 Determinism and ties

Package 7 decisions must be a pure function of:

- normalized correlation input;
- presence or absence of trusted hard identity;
- Package 3 result (itself deterministic under fixed config);
- prior Package 7 state for the hook stream;
- evaluation time and configured windows.

Tie handling:

1. Prefer higher confidence (`exact` > `strong` > others — others never bind).
2. Prefer higher score among equal confidence.
3. Prefer lexicographically smaller opaque runtime ref, then hook ref (same as
   Package 3 global matcher stability).
4. Never use wall-clock arrival order, map iteration order, or PID magnitude as
   a silent tie-break without documenting it in the decision audit record.

If a tie remains after (1)–(3) → treat as **ambiguous** and `refuse`.

## 5. Time windows, capacities, and measurement defaults

**Important:** numeric age windows, grace periods, TTLs, capacities, ambiguity
deltas, and exact-vs-strong authorization below are **proposed measurement
defaults** for implementation dry-runs and soak calibration. They are **not**
final product safety thresholds. They require **human approval** before any
mutating consumer (Package 8 or otherwise) may treat them as authorizing
production binds.

Implementations must label these values as `proposed_default` in config and
must not silently promote them to “safe for mutation” without recorded
approval (section 16).

| Parameter | Proposed default | Effect if exceeded / role | Approval status |
| --- | ---: | --- | --- |
| Maximum observation age (hook or runtime) | 2 minutes | Treat as stale → fail closed | Proposed; needs approval |
| Start-time / observation proximity (soft score only) | 30 seconds | Soft points only; never alone authorizes bind | Proposed; needs approval |
| Allowed hook lead before process start signal | 2 seconds | Lifecycle conflict if violated | Proposed; needs approval |
| Ambiguity score delta | 10 (Package 3 default) | Competing assignments → no bind | Proposed; needs approval |
| Decision retention TTL (in-memory) | 5 minutes after last evaluation | Prior decision forgotten; next evaluation cold | Proposed; needs approval |
| Maximum decisions retained | 1024 hook streams | Refuse new streams; no ad hoc eviction of Bound within epoch | Proposed; needs approval |
| Audit ring buffer | 4096 | Oldest dropped | Proposed; needs approval |
| Correlation candidate size | 12 hooks / 12 runtimes | Over limit → refuse batch | Proposed; needs approval |
| Suspend grace (time or miss count) | *unset product value* | Until approved, prefer conservative `suspend` then human-defined remove | Explicitly open |
| Exact-vs-strong authorization | *unset product value* | Until approved, no mutating consumption of either | Explicitly open |

Rules independent of specific numbers:

- Client clock never orders revisions; Package 6 server time is authoritative for
  hook `ObservedAt`.
- Stale evidence is fail-closed: better to show process `idle` than bind wrong.
- Freshness is re-checked on every evaluation, including `keep`.
- Loosening any window relative to an approved value requires a new explicit
  contract change; tightening for soak may be proposed but still needs approval
  before mutation.

## 6. Lifecycle transitions: active, idle, ended

Hook lifecycle comes from agent adapters (Package 6 ingress). Runtime lifecycle
comes from process observation.

### 6.1 Hook lifecycle effects on decisions

| Hook lifecycle | No prior decision | Prior Bound still valid | Prior decision invalid |
| --- | --- | --- | --- |
| `active` | Evaluate; `propose_bind` only if section 3.1 | `keep` if still unique trusted hard match; else re-evaluate | `suspend`, `remove`, or atomic `replace` (section 7) |
| `idle` | Same as active for identity; often `refuse` today without identity bridge | `keep` binding identity; claim clearance is Package 8 | Same identity rules as active |
| `ended` | `refuse` new bind; `remove` if prior existed | `remove` | `remove` / no-op |

Notes:

- `idle` on the hook means “clear activity claim”, not “unbind runtime”
  (ADR-05). Package 7 may `keep` the session→runtime association while
  Package 8 later clears claims.
- `ended` ends the association. A later new session ref is a new stream.
- Wrapper-synthetic Codex `SessionEnd` is not a Package 6 ingress source;
  Package 7 must not invent ended transitions from unverified synthetic events
  unless a later versioned source policy explicitly allows them.

### 6.2 Runtime lifecycle effects

| Runtime lifecycle | Effect on existing decision for that runtime |
| --- | --- |
| `active` / alive | May `keep` if trusted hard match still holds |
| Missing from snapshot once | Do not immediately remove; **suspend** path |
| Missing beyond approved grace / process ended | `remove`, or atomic `replace` if section 7.4 holds |
| New generation (same PID, new start time) | Old generation invalid; `replace` if new unique trusted hard match exists, else `remove` |
| Tool no longer recognized for process | `suspend` / `remove`; never rebind on name alone |

Re-correlation after runtime restart or disappearance:

1. If the bound runtime identity still appears with the same generation →
   re-validate; `keep` or `suspend`/`remove` on conflict.
2. If the runtime disappears → **suspend** (do not immediately associate another
   runtime).
3. If the prior generation is **proven invalid** and exactly one new unique
   trusted hard match exists → **one atomic `replace`** (section 7.4). Package 7
   does **not** require a separate `remove` decision first.
4. If the prior generation is proven invalid and no new match exists →
   **`remove`**.
5. PID reuse without matching start time never continues the old decision.

## 7. Keep, suspend, replace, remove

### 7.1 Decision state machine (in-memory)

`replace` is a **single atomic Package 7 transition** from Bound/Suspended to a
new Bound target when preconditions hold. It is not modeled as mandatory
`remove` followed by a later `propose_bind`.

```text
(none) --propose_bind--> Bound
Bound --keep--> Bound
Bound --suspend--> Suspended
Bound --replace--> Bound'     (atomic; old generation invalid + new unique hard match)
Bound --remove--> (none)
Suspended --keep--> Bound    (same generation revalidated)
Suspended --replace--> Bound' (atomic; same preconditions as from Bound)
Suspended --remove--> (none)
Suspended --propose_bind--> Bound  (only same-generation refresh if modeled as new propose; prefer keep)
Any --refuse--> (state unchanged)
```

Package 8 may implement `replace` mutation as multi-step internal updates; that
is a Package 8 concern. Package 7 still emits one decision: `replace`.

### 7.2 When to keep

`keep` when all hold:

- prior decision is `Bound` (or Suspended returning to Bound on revalidation)
  for the same `(tool, hook_session_ref)`;
- same runtime generation (full runtime identity including start time);
- current evaluation still hard-identity positive with **trusted** evidence,
  unique, non-ambiguous;
- observations fresh under the active window;
- no hard conflict.

`keep` does not re-score soft signals into a new target.

### 7.3 When to suspend

`suspend` when a previously bound association is no longer safe to use for
activity claims, but terminal removal is not yet proven:

- runtime missing from one or more snapshots inside the (proposed/approved)
  grace window;
- temporary ambiguity appears while the prior runtime is still the historically
  unique hard match;
- evidence freshness fails but the generation is not yet proven replaced.

While suspended, Package 7 must not emit `propose_bind` to a **different**
runtime. It may emit atomic `replace` only when section 7.4 is fully satisfied.
Package 8 must not apply new hook claims for a suspended decision.

### 7.4 When to replace (atomic)

`replace` is one Package 7 decision. It is allowed only when **all** hold:

1. Prior decision is `Bound` or `Suspended`.
2. Prior runtime generation is **proven ended** or **hard-conflicts** with
   continued association (generation conflict). Mere absence inside grace is
   not enough → use `suspend` instead.
3. Exactly one new unique hard-identity-positive exact/strong candidate exists,
   using **trusted** hard identity (section 2.0–2.2).
4. The new candidate is not ambiguous with any other.
5. All section 3.1 prerequisites that apply to authorizing a new association
   hold for the new target (including trusted identity).
6. Audit records both old and new runtime refs and reason
   `prior_generation_invalid`.

**Not replace:**

- prior generation merely missing → `suspend`;
- no trusted hard identity for the new pair → `remove` or stay suspended, never
  soft replace;
- two-step “always `remove` then later `propose_bind`” is **not** required by
  Package 7 when the above holds; that sequencing is optional only as a
  Package 8 mutation strategy for applying a single `replace` decision.

### 7.5 When to remove

`remove` when:

- hook lifecycle is `ended`;
- prior runtime generation is confirmed ended **and** no replace candidate
  satisfies section 7.4;
- suspend grace (once approved) expires without reappearance of the same
  generation and replace does not apply;
- hard conflict proves the association was wrong (prefer remove over silent
  keep);
- producer epoch of the local server changes (all decisions drop with memory).

### 7.6 Decision table (summary)

| Prior | Current evidence | Decision |
| --- | --- | --- |
| none | no trusted hard identity | `refuse` |
| none | unique trusted exact/strong hard match + section 3.1 | `propose_bind` |
| none | zero / weak / ambiguous / conflict | `refuse` |
| Bound, same generation still unique trusted hard match | fresh | `keep` |
| Bound, same generation missing briefly | within grace | `suspend` |
| Bound, generation proven invalid, unique new trusted hard match | section 7.4 | **`replace` (atomic)** |
| Bound, generation proven invalid, no new match | proven end | `remove` |
| Bound, hook `ended` | any | `remove` |
| Bound, ambiguous now | competing candidates | `suspend`; no new bind |
| Suspended, same generation returns unique trusted hard | revalidated | `keep` |
| Suspended, generation proven invalid, unique new trusted hard match | section 7.4 | **`replace` (atomic)** |
| Suspended, different runtime only soft match | soft only | stay suspended / later `remove` — never soft replace |

## 8. Conflicting evidence

| Conflict class | Rule |
| --- | --- |
| Hard identity conflict | Always fail closed; never bind |
| Missing trusted hard identity for new association | `refuse` (`missing_trusted_hard_identity`) |
| Soft vs soft disagreement | Does not create bind; may leave unmatched |
| Prior bind vs new soft better score | Soft never overrides prior hard bind; only hard invalidation does |
| Same request-id replay (Package 6) | Not a Package 7 concern; no second sequencing |
| Same revision different payload (Package 3) | `refuse` / diagnostic conflict; no bind |
| Cross-epoch revision compare | Never order; new epoch is cold start for decisions |

## 9. Audit information for each decision

Every decision must produce a content-free audit record suitable for local
diagnostics and later Package 8 dry-run. The record must **not** contain prompt,
argv, CWD, transcript, paths, raw environment, request IDs used as secrets, or
free-form error strings from the OS.

### 9.1 Required audit fields

| Field | Content |
| --- | --- |
| `evaluated_at` | UTC timestamp of decision |
| `tool` | `claude` / `codex` (legacy ToolKind until AgentID migration) |
| `hook_stream_ref` | Opaque `tool:hook_session_ref` (or equivalent opaque ref) |
| `decision` | `propose_bind` / `keep` / `suspend` / `replace` / `remove` / `refuse` |
| `reason_codes` | Sorted content-free codes (see below) |
| `confidence` | `exact` / `strong` / `weak` / `rejected` / `ambiguous` / `none` |
| `hook_lifecycle` | `active` / `idle` / `ended` |
| `runtime_ref` | Opaque runtime candidate id if any, else empty |
| `prior_runtime_ref` | Opaque prior runtime if any (required for `replace`) |
| `prior_decision` | Previous decision enum if any |
| `producer_epoch` | Server epoch of the hook observation |
| `hook_revision` | Server-assigned revision |
| `hard_identity` | `true` / `false` |
| `trusted_hard_identity_present` | `true` / `false` |
| `unique_match` | `true` / `false` |
| `ambiguous` | `true` / `false` |
| `stale` | `true` / `false` |
| `score` | Integer score if a pair was scored, else 0 |
| `would_bind_diagnostic` | Package 3 flag copied for measurement only |
| `no_mutation_performed` | Always `true` in Package 7 |

### 9.2 Minimum reason codes

Include at least these content-free codes when applicable:

`eligible_exact`, `eligible_strong`, `insufficient_evidence`, `weak_only`,
`ambiguous_match`, `hard_conflict`, `stale_observation`, `no_runtime`,
`multiple_runtimes_unresolved`, `missing_trusted_hard_identity`,
`prior_kept`, `prior_suspended`, `prior_replaced`, `prior_removed`,
`lifecycle_ended`, `generation_mismatch`, `unverified_process_hint`,
`candidate_limit_exceeded`, `decision_capacity_exceeded`, `fail_closed`,
`parameters_unapproved_for_mutation`.

## 10. Bounded in-memory state

Package 7 state lives only in the long-lived local presence process memory.

Bounds in section 5 are **proposed defaults** until approved. Semantic rules:

- do not write decision or audit state to disk, SQLite, or relay in Package 7;
- server restart drops all decisions;
- new Package 6 producer epoch implies cold decision state;
- never randomly evict another stream’s Bound decision to free capacity; refuse
  new streams instead when at capacity.

## 11. Fail-closed behavior

Default when unsure: **do not bind**.

| Condition | Behavior |
| --- | --- |
| Packages 0–6 only (no identity bridge) | `refuse` all new associations |
| Insufficient evidence | `refuse`; process instances may remain process-owned `idle` |
| Ambiguity | `refuse` new bind; `suspend` existing if threatened |
| Error / panic at policy boundary | `refuse`; never emit `propose_bind` or `replace` |
| Over capacity | `refuse` new streams; do not randomly evict another stream’s Bound state |
| Unverified process hints only | `refuse` |
| Unapproved measurement defaults used for mutation | Forbidden; dry-run only (`parameters_unapproved_for_mutation`) |
| Package 6 observation missing sequencing | Not a Package 7 input |

Failure mode (ADR-04): missing status and content-free diagnostics beat wrong
status on another instance.

## 12. Worked examples

### Example A — Packages 0–6 only (current production-shaped stack)

- Package 6 ingress: `tool=claude`, `hook_session_ref=hs_1`, `lifecycle=active`.
- One or many Claude runtimes visible in the process snapshot.
- No server-attested hook-side PID/start time.

**Decision:** `refuse` with `missing_trusted_hard_identity`.  
**Not permitted:** `propose_bind`, even if soft signals or Package 3 diagnostic
`would_bind` look attractive.

### Example B — Package 7.0 trusted hard identity + safe exact match

- Same as A, **plus** Package 7.0 checkpoint success after the prescribed order:
  accept → `SO_PEERCRED` → peer generation (PID 18420 start T0) → validated
  Package 6 request (`tool=claude`) → L1/L2/L3 link to the unique Claude
  runtime root (see Package 7.0).
- Correlation: exact root match, unique, not ambiguous.
- Prior: none.
- Parameters approved for the evaluation class (section 16).

**Decision:** `propose_bind` to that runtime.  
**Audit:** `eligible_exact`, `hard_identity=true`,
`trusted_hard_identity_present=true`, `unique_match=true`.

### Example C — two sessions, shared soft signals only

- Two Codex runtimes; two hooks; shared process group soft signal only.
- Package 6 ingress has no hard identity (today).

**Decision:** `refuse` for both (`missing_trusted_hard_identity` and/or
`weak_only` / `ambiguous_match`).

### Example D — existing bind, hook goes idle

- Prior Bound under a trusted identity bridge; still unique hard match.
- Hook lifecycle `idle`.

**Decision:** `keep` association.  
**Package 8 (later):** may clear hook claim to reveal process base `idle`.  
**Package 7:** does not unbind solely because of idle.

### Example E — runtime generation replaced (atomic replace)

- Prior Bound to PID 100 start T0 with trusted identity.
- Snapshot proves T0 ended; PID 100 start T1 appears as the unique trusted hard
  match for the same hook stream.

**Decision:** **one atomic `replace`** to the T1 generation (section 7.4).  
**Not required:** a prior Package 7 `remove` decision followed by a later
`propose_bind`.  
**Package 8:** may apply the replace as multi-step mutation internally.  
**Never:** continue the T0 decision onto T1 without replace preconditions.

### Example F — runtime disappears without proven end

- Prior Bound; runtime missing inside grace; no proven end; no new unique hard
  match.

**Decision:** `suspend`. Not `replace`. Not soft rebind.

### Example G — would_bind diagnostic without eligibility

- Package 3 marks `would_bind_under_current_threshold=true`, but either hard
  identity is missing or a second runtime is within ambiguity delta.

**Decision:** `refuse`. Diagnostic flag is ignored for authorization.

### Example H — hook ended

- Prior Bound; hook lifecycle `ended`.

**Decision:** `remove`. No new bind on that stream.

## 13. Proposed machine-readable decision shape (non-wire)

Illustrative internal shape for a future Package 7 implementation. Not a public
API and not a registry write:

```json
{
  "decision": "replace",
  "tool": "claude",
  "hook_session_ref": "hs_5ba8f1d4",
  "runtime_ref": "runtime-opaque-02",
  "prior_runtime_ref": "runtime-opaque-01",
  "confidence": "exact",
  "hard_identity": true,
  "trusted_hard_identity_present": true,
  "reason_codes": ["prior_replaced", "prior_generation_invalid", "eligible_exact"],
  "producer_epoch": "epoch-opaque",
  "hook_revision": 3,
  "evaluated_at": "2026-07-22T12:00:00Z",
  "no_mutation_performed": true
}
```

## 14. Package placement (when implemented)

Intended future code ownership (not implemented by this document):

| Package | Responsibility |
| --- | --- |
| `internal/instancecorrelation` | Unchanged scoring engine (Package 3) |
| New policy package (e.g. `internal/bindingpolicy`) | Pure Package 7 decision function |
| Package 7.0 attestation composition (Linux peer + proc + recognition join) | Trusted hard identity bridge (prerequisite); see Package 7.0 doc |
| `internal/instanceregistry` | Unchanged until Package 8 |
| `cmd/aurora-presence-local-server` | Optional composition behind default-off flags later |
| Claude/Codex hooks, relay, v1 publish | Unchanged |

No import cycle that lets policy mutate registry.

## 15. Exit criteria for Package 7 (implementation phase)

Package 7 is **implemented** only when:

- this contract is testable with fixtures covering sections 2, 4–8, and 12;
- with Packages 0–6 only, fixtures prove new associations always `refuse` with
  `missing_trusted_hard_identity` (or equivalent);
- every path proves `no_mutation_performed` / no registry interaction;
- `weak` / `ambiguous` / `rejected` never yield `propose_bind` or `replace`;
- `replace` is contract-tested as one atomic decision (not mandatory remove +
  later propose_bind);
- keep/suspend/remove tables are contract-tested;
- audit records are content-free and bounded;
- config values from section 5 are marked proposed and gated for mutation;
- default configuration keeps the feature off in production composition;
- Package 6 entry criteria remain satisfied;
- human-approved values from section 16 are recorded before any mutating
  consumer is enabled.

Package 7 **active in production** requires a separate activation decision after
Package 8 readiness is understood; docs-first Package 7 does not activate
anything.

## 16. Unresolved decisions requiring human approval

The following are **not** settled as final product safety thresholds and must
not be filled by implementation convenience:

1. **Trusted hard identity bridge live evidence** — Package 7.0 design is
   documented; Blue1 measurements and soak (Package 7.0 §16) remain open before
   the bridge may authorize Package 7 `propose_bind` / `replace`.
2. **Accepted false-positive rate** for automatic `propose_bind` / `replace` in
   production (from labeled soak data).
3. **Whether `strong` (member) is authorized**, or only `exact` (root/runtime),
   for mutating consumption (includes Package 7.0 L2/L3 link classes).
4. **Suspend grace** duration and/or snapshot miss count before `remove`.
5. **Numeric approval** of age windows, proximity windows, TTLs, capacities, and
   ambiguity delta in section 5 (proposed defaults only until then).
6. **Cross-process / cross-restart deduplication** strength required before
   Package 8 mutation.
7. **Production activation** coupling to Package 8 feature flags and rollout.

Until (1)–(3) and (5) are approved with evidence, an implementation must only
emit decisions in dry-run / observe-only mode and must not authorize mutating
consumers. With Packages 0–6 alone, new associations remain `refuse` regardless.

**Settled by this revision (not open):**

- Package 6 as implemented today does **not** supply server-attested hard
  process identity.
- The trusted identity bridge is **Package 7.0**, a **prerequisite subpackage**
  of Package 7 policy — not Package 8 mutation and not an implicit Package 6
  capability. Normative design:
  [Package 7.0](per-instance-presence-package-7-trusted-identity-bridge.md).
- `SO_PEERCRED` alone does **not** prove the final Claude/Codex runtime; peer
  generation and unique ancestry/runtime join are required.
- Client-declared process fields are never hard identity. Validated ingress
  `tool` is candidate **namespace** only, not process proof.
- Package 7.0 uses a **bounded attestation transaction** and **checkpoint**
  (publication atomicity only); process inspection steps are not claimed to be
  kernel-atomic.
- Valid Package 6 observe-only sequencing is **independent** of Package 7.0
  attestation success; the internal path must carry trusted evidence **or**
  explicit `trusted_hard_identity_present=false` with reason codes.
- `replace` is an **atomic Package 7 decision**; Package 8 owns mutation
  application safety.
- Section 5 numbers are **proposed measurement defaults**, not final safety
  thresholds.

## 17. Consistency notes

- ADR-04 remains supreme: uncertain correlation never updates a candidate.
- ADR-05 remains supreme: process owns idle base; hook owns activity claim.
- Package 3 weights and thresholds remain measurement defaults until re-approved
  for mutation.
- Package 6 remains observe-only ingress with a minimal payload; Package 7 does
  not smuggle process identity into that payload without a versioned contract
  change. Package 7.0 attests identity **server-side**, keeps it off the hook
  response wire, and does not gate valid Package 6 sequencing on attestation
  success.
- The overall ESP status path (v1 `/presence` aggregation) remains untouched.
