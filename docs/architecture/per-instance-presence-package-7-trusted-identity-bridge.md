# Package 7.0: trusted identity bridge (Linux)

Status: normative design; planned, not implemented, integrated, or active in
production

This document defines how Aurora on Linux may **securely determine which local
Claude or Codex process family** is associated with a hook event **without
trusting** PID, start time, ancestry, runtime identity, or other process hints
**supplied by the hook client**.

It is documentation only. It does not modify Go code, tests, services,
deployment, configuration, or hook installation. It does not weaken the
fail-closed rule. It preserves the working ESP / v1 status path.

Related documents:

- [Package 7 binding policy](per-instance-presence-package-7.md)
- [Package 6 local ingress](per-instance-presence-package-6.md)
- [integration contract](per-instance-presence-integration-contract.md)
- [Linux local transport backend](backends/linux-local-transport.md)
- [Linux process backend](backends/linux-process.md)
- [canonical roadmap](per-instance-presence-roadmap.md)
- [Claude adapter](adapters/claude.md)
- [Codex adapter](adapters/codex.md)

## 0. Placement decision

### 0.1 Prerequisite subpackage, not Package 8 mutation

| Item | Decision |
| --- | --- |
| Package number | **Package 7.0** (trusted identity bridge) |
| Relation to Package 7 | **Prerequisite subpackage** for useful `propose_bind` / atomic `replace` |
| Relation to Package 6 | Runs alongside Package 6 request handling; does **not** put client process claims on the wire; does **not** gate Package 6 observe-only sequencing on attestation success |
| Relation to Package 8 | **Hard boundary**: this bridge never mutates registry, slots, claims, or presence publication |
| Code ownership (future) | Platform backend + composition root; not core correlation scoring |
| Roadmap order | **Before** Package 7 (prerequisite), after Package 6 |

Package 7 policy remains the owner of bind *decisions*. Package 7.0 is the owner
of **server-attested hook-side hard process evidence**. Without 7.0 (or an
equally approved later attestation design), Package 7 must refuse every **new**
association (`missing_trusted_hard_identity`).

Package 7.0 may be implemented and soak-tested while Package 7 policy is still
dry-run / observe-only. Emitting evidence is not authorization to mutate.

### 0.2 Why not “inside Package 7 only”

Package 7 is agent- and OS-neutral policy. Peer credentials, `/proc` generation
reads, and Linux ancestry walks are **platform-backend** work. Folding them into
Package 7 policy would:

- leak Linux fields into generic policy;
- couple binding policy to one transport backend;
- blur the fail-closed identity prerequisite with scoring thresholds.

A named prerequisite subpackage keeps the evidence chain reviewable and portable
in principle, even though Linux is the only MVP implementation.

## 1. Problem statement

### 1.1 What Package 6 provides today

Package 6 `ingest_hook_event` carries only:

- `tool` (`claude` | `codex`);
- `hook_session_ref` (opaque agent session);
- `lifecycle` (`active` | `idle` | `ended`).

After server accept it adds server-owned `ProducerEpoch`, `Revision`, and
`ObservedAt`. It does **not** provide server-attested hook-side PID + start time
or a link to a recognized runtime family.

Transport peer auth uses Linux `SO_PEERCRED` only for **same-effective-UID**
acceptance. That is a local trust boundary for “who may talk on this socket”,
not process-generation identity and not Claude/Codex runtime identity.

The ingress **`tool` is not known** until the request frame has been read,
size-checked, strictly decoded, and validated. Peer credentials and peer
generation capture therefore happen **before** tool is available; tool is used
later only to restrict the runtime candidate namespace (section 3.1).

### 1.2 What Package 7 requires for new associations

Package 7 requires a **hard-identity-positive** pair: trusted hook-side process
evidence matched to a runtime root or member that already carries generation-safe
identity (PID + start time, with host/boot as required). Soft signals alone never
authorize `propose_bind` or `replace`.

### 1.3 Threats this bridge must address

| Threat | Consequence if ignored |
| --- | --- |
| Client-declared PID / start time | Any same-UID process can claim another agent’s identity |
| PID alone without start time | PID reuse binds the wrong generation |
| Trusting peer PID without re-validation | TOCTOU / exit / reuse races |
| Assuming peer **is** the agent runtime | Hook helpers, shells, and wrappers are usually the dialer |
| Using tool as process proof | Client-declared tool is namespace selection only, not hard identity |
| Unbounded ancestry | Ambiguous multi-session trees; performance risk |
| Delayed attestation after peer exit | Empty `/proc` → false “no identity” or, worse, wrong PID reuse |
| Soft co-location (TTY, CWD, process group) | Parallel sessions of the same tool collide |
| Collapsing Package 6 and 7.0 outcomes | Would either reject valid observe-only ingress or smuggle bind authority into sequencing |

## 2. Evaluation of candidate mechanisms

### 2.1 Unix socket peer credentials (`SO_PEERCRED`)

**What it gives (Linux stream sockets):**

- Kernel-reported peer **UID**, **GID**, and **PID** of the process that
  connected (or last performed a credential-relevant action on that connection).
- Independent of client JSON payload.
- Already used by `internal/localhooktransport` for same-UID auth.

**What it does *not* give:**

- Process **start time** / generation.
- Host-ID or boot-ID.
- Proof that the peer is Claude, Codex, or any recognized runtime member.
- Proof against PID reuse after the peer exits.
- Stable identity across `exec` (PID usually preserved; identity of the image
  changes — generation is the same process slot only if start time still
  matches).
- Ancestry or runtime-family membership.

**Verdict:** Necessary first kernel attestation of “which process dialed”,
**insufficient alone** for Package 7 hard identity. Must be composed with
server-side generation lookup and ancestry-to-runtime binding. Must be read
**immediately after accept**, before request decode.

### 2.2 Server-side lookup of peer PID generation and ancestry

**What it adds:**

- Read `/proc/<peer_pid>/stat` (and related allowlisted fields) on the **server**
  to obtain start ticks → normalized `StartedAt` with boot time / USER_HZ.
- Build generation-safe `ProcessIdentity{PID, StartedAt}` for the peer.
- Walk **verified** parent links (PID + start time, re-checked) toward a
  recognized Claude/Codex runtime family already produced by Packages 2/5
  recognition.
- Reuse the same generation-safety patterns as `linuxprocess.verifiedParent`
  and `runtimerecognition` family construction (double-read races, fail closed).

**What it still requires:**

- A live or still-readable peer process during the generation-capture phase of
  the attestation transaction (section 4).
- After the request is validated, a unique same-tool runtime candidate reachable
  by **acceptable ancestry rules** (section 6).
- Host-ID and boot-ID from the process backend, not from the client.

**Verdict:** Required second half of the Linux MVP evidence chain. Without it,
`SO_PEERCRED` only re-proves same-UID dialer presence. Peer generation capture
must follow peer auth **immediately** and **before** request decode, so the
dialer is still expected to be alive; ancestry/runtime join follows only after
validated `tool` is known.

### 2.3 One short-lived connection per hook invocation

Package 6 client model: exactly one synchronous dial / request / response per
delivery attempt; connection closed after the attempt; total client budget
100 ms.

**Benefits for identity:**

- Peer credentials attach to **this** invocation’s dialer, not a long-lived
  multiplexed client that might later fork/exec into something else.
- The attestation transaction can capture peer generation immediately after
  accept, while the dialer is still expected to be alive (hook process waiting
  on its write/read path).
- No cross-invocation peer identity cache is required for correctness (caches
  would reintroduce reuse and exit races).

**Limits:**

- Does not by itself identify the agent runtime.
- Extremely short-lived peers that exit before generation capture still fail
  closed for identity (Package 6 may still sequence if the request is valid).
- Connection reuse (if ever introduced) would be a **contract regression** for
  this bridge unless a full new attestation transaction runs every request with
  fresh generation checks.

**Verdict:** **Required composition rule** for the Linux MVP. The current
Package 6 invocation model is **compatible** and is the preferred carrier. It
is **not sufficient** without server-side generation + ancestry.

### 2.4 Wrappers, shells, exec, fork, PID reuse, delayed handling

| Phenomenon | Risk | Bridge rule |
| --- | --- | --- |
| Shell wrapper (`sh -c`, `bash`, `env`) | Peer is shell, not agent | Ancestry walk through allowlisted launcher roles only; never treat shell as runtime alone |
| `npx` / package launcher | Peer may be node launcher or wrapper | Match recognized launch identities / family members from Package 5 recognizers |
| `exec` into hook binary | Same PID, new image, **same** start time | Generation identity still valid; classification uses current comm/executable/launch marks during the transaction |
| `fork` without exec | Child dials; parent is agent | Prefer exact peer-as-member; else verified parent chain |
| PID reuse | New process, same PID, new start time | Always require `StartedAt`; re-read stat; generation conflict → no hard identity |
| Delayed handling | Peer gone; PID reused by stranger | Capture generation immediately after `SO_PEERCRED`; double-read; if unreadable or unstable → fail closed for identity |
| Hook helper binary (`aurora-claude-hook` / `aurora-codex-hook`) | Peer is **not** the long-lived agent | Expected; identity is “peer process generation + path to unique runtime family”, not “peer == root” |

### 2.5 Is the peer the agent, a wrapper, or a helper?

For command-style hooks (Claude / Codex as used by Aurora today), the dialer is
**expected** to be a short-lived **helper** process started by the agent
runtime (or by a thin shell/npx wrapper around the helper), **not** the
long-lived Claude/Codex root process itself.

Therefore:

1. **Direct equality** of peer identity with runtime root is **sufficient** when
   it occurs, but **must not be required**.
2. **Member match**: peer generation equals a validated member of exactly one
   same-tool runtime family → strong path (Package 7 `strong` class subject to
   human authorization).
3. **Ancestry match**: peer is not a recognized member, but a bounded verified
   ancestor chain reaches exactly one same-tool runtime root/member set →
   acceptable only under section 6 rules.
4. **Unrecognized helper with no unique chain** → no trusted hard identity.

Unresolved product measurement: on Blue1, what are the actual trees for live
Claude and Codex hook firings (section 16).

### 2.6 Binding the attested peer to an already recognized runtime family

The bridge does **not** invent agent classification. It:

1. Captures trusted peer generation identity (before tool is known).
2. After the Package 6 request is strictly validated, uses the validated
   `tool` only to restrict the candidate namespace.
3. Obtains a **current** process/runtime snapshot from the existing
   ProcessBackend + AgentRuntimeRecognizer pipeline (Packages 2/5).
4. Attempts a **unique** join between peer evidence and one runtime candidate
   for that tool.
5. Publishes one immutable internal result: trusted evidence **or**
   `trusted_hard_identity_present=false` with reason codes (section 4).

Classification remains owned by agent adapters; family construction remains
owned by `runtimerecognition`. The bridge only **attests linkage**.

### 2.7 Race conditions (auth → inspect → exit)

Ordered risks:

1. **Auth without generation:** `SO_PEERCRED` PID accepted for UID, then process
   exits and PID is reused before `/proc` read → **must** capture generation
   immediately after auth, validate start time, and double-read; on any
   mismatch, fail closed for identity.
2. **Generation before tool:** peer generation is valid, but request is not yet
   validated → do not attempt tool-scoped runtime join until after strict
   decode/validation; do not treat unvalidated tool claims as namespace.
3. **Generation without ancestry:** peer identity valid but parents already
   exited → cannot link to runtime → fail closed for identity (no soft
   fallback).
4. **Ancestry without unique runtime:** chain reaches multiple same-tool
   families or an ambiguous family → fail closed for identity.
5. **Snapshot skew:** runtime snapshot taken long before/after peer generation
   capture may miss the peer or show a different generation → the attestation
   transaction and the snapshot used for join must be **time-bounded**
   (section 5).
6. **Handler after client disconnect:** Package 6 may finish sequencing after
   client disconnect within server deadline. Peer generation must already have
   been captured in the early phases of the attestation transaction; later
   join steps must not invent identity if the peer is gone. Package 6
   sequencing outcome remains independent (section 4.3).

Process inspection steps are **not** literally atomic with each other; the
contract uses a **bounded attestation transaction** (section 4).

### 2.8 Required host, boot, PID, and process-start-time evidence

| Evidence | Source | Role |
| --- | --- | --- |
| Peer UID (auth only) | `SO_PEERCRED` | Same-UID gate; **not** hard process identity |
| Peer PID | `SO_PEERCRED` | Kernel dialer PID; untrusted until generation confirmed |
| Peer start time | Server `/proc` + boot time / USER_HZ | Completes generation-safe process identity |
| Host-ID | Installation / process backend config | Runtime join key |
| Boot-ID | Process backend (or explicit config) | Separates boots; PID namespaces of past boots |
| Runtime root/member identities | Packages 2/5 snapshot | Targets for hard match |
| Validated ingress `tool` | Package 6 request after strict validation | Restricts candidate **namespace** only; **not** process proof |

Client JSON must never supply PID, start time, host-ID, boot-ID, or runtime
identity as authoritative hard evidence. The client-declared `tool` may select
the candidate namespace after validation but is **not** hard identity evidence.

### 2.9 Linux implementation boundaries and future portability

| Concern | Linux MVP | Portable rule |
| --- | --- | --- |
| Peer kernel identity | `SO_PEERCRED` on Unix stream socket | Backend-specific peer principal API |
| Generation | `/proc/<pid>/stat` startticks + btime | Backend must provide generation-safe process identity |
| Ancestry | Verified PPID chain with re-read | Backend must provide verified parent or equivalent |
| Classification | Existing recognizers | Unchanged; OS-neutral candidates |
| Generic core | Must not import `/proc` or `SO_PEERCRED` | Consume attestation result only |

Windows named-pipe or macOS local transport would need their own peer +
generation designs. Until those exist, absence of a backend means no trusted
hook identity on that platform — fail closed, not a simulated Linux field.

### 2.10 Failure when identity cannot be proven

| Failure | Package 7.0 outcome | Package 6 sequencing |
| --- | --- | --- |
| Peer auth fails | No attestation result for this connection; transport rejects as today (`unauthorized_peer`) | No accept (existing Package 6 rule) |
| Peer PID unreadable / disappeared | `trusted_hard_identity_present=false`, `peer_process_unreadable` | Valid ingress may still sequence observe-only |
| Start time unstable / PID reuse during read | `false`, `peer_generation_unstable` / `pid_reused` | Valid ingress may still sequence |
| Request invalid / not Package 6 ingest | No Package 7.0 join; no trusted evidence for that request | Existing Package 6 reject path |
| No unique same-tool runtime join | `false`, `no_unique_runtime_link` / `ambiguous_runtime_link` | Valid ingress may still sequence |
| Ancestry depth exceeded / disallowed intermediate | `false`, `ancestry_unresolved` | Valid ingress may still sequence |
| Snapshot / attestation skew | `false`, `attestation_snapshot_skew` | Valid ingress may still sequence |
| Any internal error at bridge boundary | `false`, `attestation_internal_error` | Valid ingress may still sequence |

When peer auth succeeds and the Package 6 request is valid, **Package 6
sequencing and Package 7.0 attestation are separate outcomes** (section 4.3).
Package 7 must **refuse** new associations whenever trusted evidence is absent.
Never invent identity from soft signals to “be helpful”.

## 3. Normative trusted evidence chain

### 3.1 Required server-side order

The ingress `tool` is **not** known until the request frame has been read and
strictly validated. The required order is:

```text
[1] Accept Unix stream connection
        |
        v
[2] Immediately read SO_PEERCRED  -->  peer UID/GID/PID
        |
        v
[3] Authenticate peer (same-effective-UID / allowlist)
        |-- fail --> unauthorized_peer; stop (Package 6 + 7.0)
        v
[4] Immediately capture and stabilize peer process generation
        |  /proc double-read; ProcessIdentity {PID, StartedAt}
        |  host_id + boot_id from process backend context
        |-- unstable/unreadable --> record identity failure factors;
        |                           continue only for Package 6 path
        v
[5] Read full bounded request frame; size-check
        |
        v
[6] Strictly decode exactly one JSON value; unknown-fields rejected
        |
        v
[7] Validate Package 6 envelope + ingress
        |  (protocol_version, operation, request_id, tool,
        |   hook_session_ref, lifecycle)
        |-- fail --> Package 6 reject; no trusted evidence publish
        v
[8] Use validated tool ONLY to restrict runtime candidate set
        |  (namespace selection; not hard identity evidence)
        v
[9] Perform verified ancestry + unique same-tool runtime-family join
        |  under skew / depth / uniqueness bounds
        v
[10] Attestation checkpoint (section 4): publish ONE immutable internal result
        |     either TrustedHookProcessEvidence
        |     or trusted_hard_identity_present=false + reason codes
        |
        +--> [11a] Package 6 sequencing of validated ingress
        |         (observe-only; independent of [10] success/failure
        |          when the request itself is valid)
        |
        +--> [11b] Package 7 policy consumes [10] only
        |         (refuse new bind if evidence absent)
        |
        v
[12] Package 8 mutation  -- NEVER from this package --
```

Steps [2]–[4] must not wait for payload decode. Steps [8]–[9] must not run
before [7] succeeds. Partial intermediate process facts must not become visible
as trusted evidence (section 4).

### 3.2 What is authoritative

| Artifact | Authoritative? |
| --- | --- |
| Kernel `SO_PEERCRED` UID/GID/PID at read time | Yes, as **kernel peer credentials** |
| Server-read peer `ProcessIdentity` (PID + StartedAt) after stable double-read | Yes, as **peer generation** |
| Server process backend host-ID / boot-ID | Yes, for join keys |
| Server-built runtime candidates (Packages 2/5) | Yes, as **runtime side** of the pair |
| Bridge-produced unique link peer→runtime after checkpoint | Yes, only when section 6 rules pass and the checkpoint publishes success |
| Validated Package 6 `tool` | Yes for **candidate namespace** and stream key with session/lifecycle; **not** process proof |
| Package 6 `hook_session_ref`, `lifecycle` | Yes for stream identity / lifecycle; **not** process proof |
| Package 6 server epoch / revision / ObservedAt | Yes for **sequencing** only |
| Any client-declared PID, start time, PPID, runtime identity, host/boot | **Never** authoritative hard identity |
| Soft signals (pgrp, session, TTY, start proximity) | Diagnostic / Package 3 soft score only |

### 3.3 What remains untrusted

Even after successful peer auth:

- payload process/runtime hints (forbidden on Package 6 v2 wire; if present on
  any other path, still untrusted);
- client-declared `tool` as proof of which process family dialed (namespace
  only);
- peer **comm**/executable alone as proof of being Claude/Codex without
  recognizer + family join;
- “same UID ⇒ same agent session”;
- “same TTY / CWD ⇒ same instance”;
- Package 3 `would_bind_under_current_threshold` without this bridge;
- any intermediate `/proc` observation that has not passed the attestation
  checkpoint (section 4).

## 4. Bounded attestation transaction (attestation checkpoint)

### 4.1 Why not “operationally atomic process inspection”

`SO_PEERCRED` reads, `/proc` generation reads, ancestry checks, and runtime
snapshots **cannot be literally atomic** with respect to the kernel process
table. Concurrent exit, fork, exec, and PID reuse can interleave with each
server read.

This contract therefore does **not** claim a single kernel-atomic inspection.
It defines a **bounded attestation transaction** that ends at an **attestation
checkpoint**.

### 4.2 Definition

A **bounded attestation transaction** is the ordered server-side procedure of
section 3.1 steps [2]–[10] for one accepted connection and one request attempt,
subject to the wall budget and bounds in section 5.

The **attestation checkpoint** is the single publish point at the end of that
transaction.

The **only** atomic property of the checkpoint is **publication atomicity**:

1. **No partial trusted evidence becomes visible** to Package 7, audit
   consumers, or any other internal consumer while the transaction is in
   progress.
2. After all ordered checks complete, the transaction publishes **exactly one**
   immutable internal result:
   - **success:** `TrustedHookProcessEvidence` with
     `trusted_hard_identity_present=true` and link fields; or
   - **failure:** `trusted_hard_identity_present=false` with sorted
     content-free reason codes (and empty link fields).
3. There is no third published state such as “tentative”, “partial”, or
   “soft-trusted”.

Retained safety techniques inside the transaction (not claimed as full
atomicity):

- double-read of peer generation fields;
- verified parent re-reads (child + parent generations);
- generation conflict / PID reuse detection;
- snapshot skew limits;
- depth, intermediate, and uniqueness bounds;
- fail-closed on timeout, unreadable peer, ambiguity, or internal error.

### 4.3 Separation of Package 6 sequencing and Package 7.0 attestation

| Outcome | Owner | Effect |
| --- | --- | --- |
| Package 6 sequencing | Package 6 | On valid ingress: may assign epoch/revision/`ObservedAt` and continue observe-only correlation diagnostics under Package 6 rules |
| Package 7.0 attestation | Package 7.0 | Publishes trusted evidence **or** explicit absence with reason codes |
| Package 6 wire response | Package 6 | Unchanged shape: status, error codes, `no_binding_performed=true`; **must not** echo process identity or attestation success |
| Package 7 bind decision | Package 7 | New association requires trusted evidence; otherwise `refuse` / `missing_trusted_hard_identity` |

**Normative rules:**

1. A **valid** Package 6 ingress **may still be sequenced observe-only** when
   identity attestation fails or times out.
2. Attestation failure **must not** invent identity and **must not** authorize
   bind.
3. Attestation failure **must not** be required to change Package 6 status to
   `rejected` solely because hard identity is missing (missing identity is not
   a Package 6 ingress-validity error).
4. Peer **auth** failure remains a Package 6/transport rejection
   (`unauthorized_peer`) as today — that is not an attestation-vs-sequencing
   split; the peer was never accepted.
5. The internal observation path after a valid sequenced ingress **must** carry
   the attestation result explicitly:
   - trusted evidence present, or
   - `trusted_hard_identity_present=false` with reason codes.
6. Package 7 consumes that field only; it does not re-derive hard identity from
   the Package 6 payload.

### 4.4 Timing notes

- Capture peer generation (steps [2]–[4]) **immediately** after accept/auth so
  the dialer is still expected to be inspectable.
- Do **not** perform tool-scoped runtime join before step [7] validation.
- Prefer completing the attestation transaction within the Package 6 handler
  deadline and the section 5 attestation budget; on timeout, publish failure
  (`attestation_timeout`), not partial success.
- After the checkpoint publishes, later client disconnect must not rewrite the
  immutable attestation result.

### 4.5 Anti-TOCTOU rules inside the transaction

- Do not cache peer generation across connections.
- Do not trust a PID read earlier in a different request.
- Double-read peer stat (or equivalent) around multi-field inspection.
- If peer start ticks change between reads → `peer_generation_unstable`.
- If peer vanishes during generation capture → `peer_process_unreadable`.
- Ancestry verification must re-check child and parent generations (same spirit
  as `linuxprocess.verifiedParent`).
- The runtime snapshot used for join must be covered by section 5 skew bounds
  relative to the peer generation capture time (or an equivalent documented
  transaction clock).
- Never promote intermediate generation or ancestry notes to trusted evidence
  outside the checkpoint publish.

## 5. Security limits, timing limits, and bounded state

Numeric values below are **proposed measurement defaults** for dry-run and
Blue1 calibration. They are not final product safety thresholds and require
human approval before mutating consumers treat them as authorizing (same
discipline as Package 7 §5).

| Parameter | Proposed default | Purpose |
| --- | ---: | --- |
| Attestation wall budget (server) | 20 ms | Keep within Package 6 handler budget; checkpoint failure on timeout |
| Maximum ancestry depth (verified parents) | 6 | Bound walks through shells/helpers |
| Maximum unrecognized intermediates in chain | 3 | Align with family-bridge spirit in recognition |
| Maximum runtime candidates considered for join | 12 | Match Package 3 candidate bound spirit |
| Snapshot skew relative to peer generation capture | 2 s | Reject stale joins |
| Peer process max age at generation capture | 2 minutes | Bound absurd long-lived mis-dials; still require unique runtime link |
| Concurrent attestation transactions | Share server concurrency semaphore | No unbounded fan-out |
| In-memory attestation audit entries | 4096 ring | Content-free; oldest dropped |
| Cross-connection peer identity cache | **Forbidden** | Correctness |

Rules:

- No disk/SQLite persistence of attestation state in Package 7.0.
- Server restart drops all bridge state with the process.
- Attestation must not open network connections or follow untrusted paths.
- `/proc` reads remain read-only, root-bounded, no-follow as in Package 2.
- Bridge must not log argv, CWD, prompt, session secrets, or raw paths.

## 6. Acceptable ancestry rules (Claude and Codex)

### 6.1 Join outcomes (normative)

Let `P` be the captured peer generation from the transaction. Let `R` be the
set of live same-`tool` runtime candidates from the current recognition
snapshot, where `tool` is the **validated** Package 6 ingress tool (namespace
restriction only).

A **successful hard link** exists only if exactly one of the following holds
for exactly one candidate `r ∈ R`:

| Rank | Rule | Maps toward Package 7 confidence |
| --- | --- | --- |
| L1 | `P` equals `r` root process identity (PID + start time) | `exact` (root/runtime) |
| L2 | `P` equals a validated **member** of `r` (PID + start time) | `strong` (member) unless policy later collapses member→exact |
| L3 | `P` is not in `r`, but a **verified** parent chain from `P` of length ≤ depth bound reaches a process identity that equals the root or a member of `r`, and every intermediate satisfies section 6.2 | `strong` (ancestry-linked helper); **not** automatic `exact` |

If zero or more than one `r` satisfies any rule → **no hard link** (checkpoint
publishes failure).

Package 7 still applies uniqueness, ambiguity, freshness, and human approval
gates. This bridge only supplies `trusted_hard_identity_present` and the
linked process/runtime refs when the checkpoint succeeds.

### 6.2 Allowed intermediates on L3 chains

An intermediate process on the path from peer toward the runtime may be:

1. A process already classified by the same-tool recognizer (wrapper, node,
   native, direct); or
2. An **unrecognized** process whose opaque executable/comm identity is in a
   **small allowlist of launcher basenames** used by existing launch rules
   (for example `sh`, `bash`, `zsh`, `fish`, `env`, `npx`, `node`, `nodejs`)
   **and** whose execution context is compatible with the runtime candidate
   (same process-group / OS-session compatibility checks already used by
   family building — soft context as **constraint**, not as identity); or
3. Counted toward the “unrecognized intermediate” budget when not classified.

Disallowed:

- Jumping through a process classified as a **different** tool;
- Jumping through a process that is a member of a **different** same-tool
  runtime candidate (would create cross-session ambiguity);
- Using PPID numbers without verified start times;
- Using soft-only co-location without a verified parent edge;
- Using unvalidated or client-only tool claims to widen the candidate set.

### 6.3 Claude-specific notes

Recognized forms today include direct/native executables (`claude`,
`claude-code`, `claude-native-*`, `aurora-claude`), Node launch of
`@anthropic-ai/claude-code`, and aurora wrapper launch identities.

Expected hook path (hypothesis pending Blue1 measurement):

```text
claude/node runtime family
    -> (optional shell/npx)
    -> aurora-claude-hook  (peer dialer)
```

L2 applies if the hook binary is grouped into the family as a member.
L3 applies if the hook is a short-lived child not retained as a family member
in the snapshot.

### 6.4 Codex-specific notes

Recognized forms today include `codex`, `aurora-codex`, `codex-*` /
`codex-linux-*`, Node launch of `@openai/codex`, and aurora wrapper launches.

Synthetic wrapper `SessionEnd` remains **out of Package 6 ingress**; the bridge
must not invent ended-runtime linkage from v1-only synthetic events.

Expected hook path (hypothesis pending Blue1 measurement): same helper-child
pattern as Claude.

### 6.5 What “unique” means under parallel sessions

Two Claude (or two Codex) sessions on one host:

- Soft signals alone → no hard link (Package 7 already refuses).
- Bridge succeeds only if peer generation + ancestry selects **one** runtime
  family uniquely under the validated tool namespace.
- If both families share launcher intermediates in a way that leaves two legal
  L3 targets → `ambiguous_runtime_link`.

## 7. PID reuse prevention

Normative requirements:

1. **Never** treat numeric PID as identity.
2. Generation key is always `(host_id, boot_id, pid, started_at)` for joins
   that cross process and runtime evidence; process-local keys are
   `(pid, started_at)` within one boot snapshot.
3. Peer generation double-read during the attestation transaction.
4. Parent edges require verified parent generation (re-read child + parent).
5. If a runtime member disappears and a new process reuses the PID with a new
   start time, the old generation is invalid; Package 7 `replace` / `remove`
   rules apply later — the bridge must not continue the old generation.
6. No cross-boot PID comparison without boot-ID equality.
7. No durable “PID lease” store in Package 7.0.

## 8. Current hook invocation model: is it enough?

### 8.1 Sufficient for dialer attestation?

**Yes, with conditions.** One short-lived connection per Package 6 invocation
plus immediate `SO_PEERCRED` plus immediate server-side generation capture is a
sound way to attest **which process dialed for this event**, provided the peer
is still readable within the early phases of the attestation transaction.

### 8.2 Sufficient for agent runtime identity?

**Only if** Blue1 (and soak) measurements show that from that dialer, L1–L3
rules uniquely reach the correct runtime family for real Claude and Codex hook
firings, including parallel sessions and common wrappers, after the validated
tool restricts the namespace.

Until those measurements pass, Package 7 must continue to refuse new
associations even if 7.0 code exists behind a default-off flag.

### 8.3 Required changes to client/server composition

**Client (hook commands):**

- No new process fields on the wire.
- Keep one connection per invocation.
- Keep best-effort / fail-open for v1 exit status.
- Do not claim local PID in payload “to help” the server.

**Server (presence local server composition):**

- Follow section 3.1 order exactly: peer creds and generation **before**
  request decode; tool-scoped join **after** validation.
- End each transaction at the attestation checkpoint with one immutable
  internal result (success or explicit failure).
- Keep Package 6 sequencing outcome independent for valid ingress.
- Keep Package 6 response shape (`no_binding_performed=true`, no process echo).
- Do not import Linux bridge types into `instancecorrelation` scoring core;
  policy consumes a neutral attestation result.
- Feature flag for bridge work must default off in production composition until
  exit criteria (section 14) are met.

**Transport package boundary:**

- `localhooktransport` may expose peer credentials to a backend attestor in the
  composition root / Linux backend, but should not grow Claude/Codex-specific
  ancestry rules (those stay with process + recognition composition).

**No change required to ESP/v1 path.**

## 9. Machine-readable internal evidence shape (non-wire)

Illustrative server-internal result published at the attestation checkpoint.
Not a public API; not sent to the hook client; not written to registry.

**Success:**

```json
{
  "attested_at": "2026-07-22T12:00:00Z",
  "assurance": "linux_peercred_proc_ancestry_v1",
  "trusted_hard_identity_present": true,
  "tool": "claude",
  "peer_process": { "pid": 4242, "started_at": "2026-07-22T11:59:59Z" },
  "linked_runtime_ref": "runtime-opaque-01",
  "linked_process": { "pid": 3900, "started_at": "2026-07-22T11:50:00Z" },
  "link_rule": "L3_ancestry_to_member",
  "host_id": "opaque-host",
  "boot_id": "opaque-boot",
  "reason_codes": ["peer_generation_ok", "unique_runtime_link", "ancestry_verified"],
  "package6_sequencing_independent": true,
  "no_mutation_performed": true
}
```

**Failure (still attachable to a successfully sequenced Package 6 observation):**

```json
{
  "attested_at": "2026-07-22T12:00:00Z",
  "assurance": "linux_peercred_proc_ancestry_v1",
  "trusted_hard_identity_present": false,
  "tool": "claude",
  "peer_process": null,
  "linked_runtime_ref": "",
  "linked_process": null,
  "link_rule": "none",
  "reason_codes": ["peer_process_unreadable", "trusted_hard_identity_absent", "fail_closed"],
  "package6_sequencing_independent": true,
  "no_mutation_performed": true
}
```

The internal path after Package 6 accept **must** carry one of these two shapes
(or an equivalent schema with the same semantics). Package 7 refuses new
binding when `trusted_hard_identity_present` is false. Package 6 behavior and
response remain unchanged for valid ingress.

## 10. Audit fields and reason codes

### 10.1 Audit fields (content-free)

| Field | Content |
| --- | --- |
| `attested_at` | UTC timestamp of checkpoint publish |
| `assurance` | Fixed token for the evidence class, e.g. `linux_peercred_proc_ancestry_v1` |
| `trusted_hard_identity_present` | bool |
| `tool` | Validated `claude` / `codex` if request reached validation; else empty |
| `link_rule` | `none` / `L1_root` / `L2_member` / `L3_ancestry` |
| `peer_pid` | optional numeric for local diagnostics only; never from client payload |
| `peer_started_at` | generation time if known |
| `runtime_ref` | opaque if linked |
| `reason_codes` | sorted content-free codes |
| `duration_bucket` | e.g. `lt_5ms`, `lt_20ms`, `timeout` |
| `package6_sequencing_independent` | always `true` for this bridge design |
| `no_mutation_performed` | always `true` |

Must not include session IDs as secrets beyond existing opaque stream refs,
argv, paths, prompts, or raw OS error strings.

### 10.2 Minimum reason codes

`peer_auth_ok`, `peer_auth_failed`, `peer_credentials_unreadable`,
`peer_process_unreadable`, `peer_generation_ok`, `peer_generation_unstable`,
`pid_reused`, `request_validated`, `tool_namespace_selected`,
`ancestry_verified`, `ancestry_unresolved`,
`ancestry_depth_exceeded`, `disallowed_intermediate`,
`unique_runtime_link`, `no_unique_runtime_link`, `ambiguous_runtime_link`,
`tool_mismatch_on_link`, `attestation_snapshot_skew`,
`attestation_timeout`, `attestation_internal_error`,
`trusted_hard_identity_present`, `trusted_hard_identity_absent`,
`package6_sequenced_without_identity`, `fail_closed`,
`no_mutation_performed`.

Package 7 continues to emit `missing_trusted_hard_identity` when this bridge
yields absence.

## 11. Explicit boundary against Package 8 mutation

Package 7.0 **must not**:

- call `instanceregistry` or slot APIs;
- publish to relay, v2, or any presence backend;
- alter v1 source aggregation or ESP presentation;
- treat successful attestation as automatic bind;
- persist bindings;
- install hooks or enable production defaults;
- reject valid Package 6 observe-only sequencing solely for missing hard
  identity.

Successful attestation only raises `trusted_hard_identity_present` for Package 7
policy. Package 8 alone may apply approved decisions under its own flags.

## 12. Tests and evidence required before Package 7 may emit `propose_bind`

Before any configuration allows Package 7 to emit `propose_bind` or association-
creating `replace` for real (non-fixture) data:

### 12.1 Contract / unit tests (implementation phase)

- Peer generation is captured before request decode in the prescribed order.
- Tool-scoped join does not run until after strict validation.
- Peer generation double-read detects start-tick change → checkpoint failure.
- PID reuse fixtures never link old generation to new process.
- L1 / L2 / L3 happy paths with synthetic proc trees.
- Ambiguous two-runtime trees → checkpoint failure.
- Different-tool intermediate → checkpoint failure.
- Peer unreadable → checkpoint failure **and** valid Package 6 ingress still
  sequences observe-only with `trusted_hard_identity_present=false`.
- Partial intermediate state is never visible as trusted evidence.
- Checkpoint publishes exactly one immutable success or failure result.
- Client payload process fields ignored even if maliciously present on a
  non-Package-6 path under test.
- Client-declared tool alone never creates hard identity.
- Attestation timeout → checkpoint failure, not partial success.
- No registry mock interactions from bridge package tests.
- Package 6 response shape unchanged on attestation failure.

### 12.2 Blue1 / live measurements (blocking)

See section 16. At minimum, labeled parallel Claude and Codex sessions must
show unique correct links for real hook firings without false links across
sessions.

### 12.3 Soak / false-positive gate

Human-approved maximum false-positive rate for automatic bind remains a Package
7 §16 open product decision. Bridge soak evidence feeds that decision; it does
not replace it.

### 12.4 Package 6 exit criteria

Still required before Package 7 **implementation** work that depends on real
ingress (Package 7 docs already state this).

## 13. Comparison summary

| Mechanism | Alone enough for `propose_bind`? | Role in MVP |
| --- | --- | --- |
| `SO_PEERCRED` | No | Kernel dialer PID + UID auth (immediate after accept) |
| Server `/proc` generation | No | Peer hard process identity (immediate after auth) |
| Validated ingress `tool` | No | Candidate namespace only |
| Ancestry + recognition join | No by itself (needs peer start + validated tool) | Link helper → runtime family |
| One connection per invocation | No | Sound carrier; required composition |
| Soft signals | Never | Diagnostics only |
| Client-declared PID/start | Never | Forbidden as hard identity |
| Full chain §3 + checkpoint §4 | **Candidate** yes, after Blue1 evidence + Package 7 gates | Normative Linux MVP |

## 14. Exit criteria for Package 7.0

Package 7.0 is **implemented** only when:

- this contract is testable with fixtures for sections 3–7 and 10;
- section 3.1 ordering is contract-tested (generation before decode; join after
  validated tool);
- publication atomicity at the attestation checkpoint is contract-tested (no
  partial trusted evidence);
- Package 6 sequencing remains independent of attestation failure for valid
  ingress;
- fail-closed paths never emit trusted evidence on uncertainty;
- no client process fields are treated as authoritative;
- no Package 8 / registry mutation paths exist;
- composition keeps ESP/v1 unchanged;
- feature defaults remain off;
- Blue1 measurements in section 16 are recorded and reviewed.

Package 7.0 **enables Package 7 `propose_bind` eligibility** only when, in
addition:

- L1–L3 rules are validated on real Claude and Codex hook trees;
- parallel-session uniqueness is evidenced;
- attestation latency fits proposed budgets or budgets are re-approved;
- Package 7 human approvals for thresholds / exact-vs-strong remain satisfied.

Docs-first Package 7.0 does not activate anything.

## 15. Consistency with other packages

- Package 6 wire payload stays minimal; no process identity smuggling.
- Package 6 observe-only sequencing of valid ingress is not gated on Package
  7.0 success.
- Package 7 §2.0 prerequisite is satisfied only by an approved bridge of this
  class (or a later versioned equivalent).
- Integration contract: process-hint provenance remains untrusted from clients;
  hard identity is server-side attestation via a bounded transaction and
  checkpoint.
- Linux local transport: same-UID auth remains necessary but not sufficient;
  peer generation capture follows immediately after auth.
- Process backend and recognition remain the only agent classification path.

## 16. Manual Blue1 measurement diagnostic (read-only)

A default-off, manually invoked diagnostic is available on the local presence
server for collecting Package 7.0 evidence on Blue1. It is **not** production
attestation authorization and does **not** emit `propose_bind`.

### 16.1 Enablement

Off unless an absolute JSONL path is provided via:

- flag: `-identity-measure-file /absolute/path/to/identity-measure.jsonl`
- or env: `AURORA_IDENTITY_MEASURE_FILE` (same absolute path semantics)

Package 6 ingest still requires `AURORA_LOCAL_HOOK_ENABLED=true` on both the
server process environment (for composition) and the hook client environment.

### 16.2 First Claude measurement command

In a dedicated shell (foreground server; no systemd install):

```bash
# Terminal A — local server with Package 6 ingest + Package 7.0 measure
export AURORA_LOCAL_HOOK_ENABLED=true
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
MEASURE_FILE="${XDG_RUNTIME_DIR}/aurora/package70-identity-measure.jsonl"
mkdir -p "${XDG_RUNTIME_DIR}/aurora"
chmod 700 "${XDG_RUNTIME_DIR}/aurora"
: > "$MEASURE_FILE"
chmod 600 "$MEASURE_FILE"

go run ./cmd/aurora-presence-local-server \
  -host-id blue1-manual \
  -socket "${XDG_RUNTIME_DIR}/aurora/presence-hook.sock" \
  -identity-measure-file "$MEASURE_FILE"
```

In another shell, run a real Claude session whose hooks deliver Package 6
ingress to that socket (feature flag on, same socket path). Then inspect:

```bash
# Terminal B — after Claude hook events fire
wc -l "$MEASURE_FILE"
tail -n 5 "$MEASURE_FILE" | python3 -m json.tool
```

### 16.3 Stop without changing installed services

The diagnostic runs only in the foreground process that was started with the
flag/env. Stop it with Ctrl-C or `SIGTERM` on that process. Do **not** change
systemd units, installed hooks, or production defaults. Leaving
`-identity-measure-file` unset (and unsetting `AURORA_IDENTITY_MEASURE_FILE`)
keeps the observer off on the next start.

### 16.4 Capture timing (measurement)

On each authenticated connection when the diagnostic is enabled:

1. **Immediately after peer auth, before reading the request frame:** capture
   peer generation (`PID` + start time) and a bounded verified parent chain.
   This must not wait until after the Package 6 response — the hook helper may
   exit then.
2. **After strict Package 6 request validation:** use validated `tool` only as
   a candidate **namespace** to join the **pre-captured** chain against current
   recognized runtime candidates (long-lived agent processes).
3. Append one JSONL record. Measurement failure or success **must not** change
   the Package 6 wire response.

This diagnostic is **not** approved production attestation. A single candidate
link must not be labeled as production `trusted_hard_identity_present`.

### 16.5 JSON Lines output schema

Each Package 6 handling path may append one JSON object (schema
`aurora.package70.identity_measure.v1`) with content-free fields including:

| Field | Meaning |
| --- | --- |
| `schema` | `aurora.package70.identity_measure.v1` |
| `measured_at` | UTC timestamp |
| `tool` / `lifecycle` | Validated Package 6 namespace only when `validated_ingress` |
| `validated_ingress` | Request was a valid Package 6 body |
| `peer_uid` / `peer_pid` | From `SO_PEERCRED` |
| `peer_generation_ok` / `peer_started_at` | Server-side generation capture (pre-request) |
| `ancestry` | Pre-request verified PID+start hops; root/member match flags after join |
| `possible_links` | Candidate L1/L2/L3 links (diagnostic, not bind) |
| `matching_runtime_count` | Zero / one / multiple |
| `link_rules` | Sorted rule names present |
| `reason_codes` | Sorted content-free codes |
| `capture_duration_micros` / `measure_duration_micros` | Timing |
| `duration_bucket` | Coarse latency bucket |
| `diagnostic_unique_link` | True only when exactly one candidate link was observed; **not** production hard identity |
| `package6_sequencing_independent` | Always `true` |
| `no_mutation_performed` | Always `true` |

Related diagnostic reason codes include `diagnostic_unique_link`,
`diagnostic_no_unique_link`, and `diagnostic_ambiguous_link`. Do not treat these
as Package 7 authorization.

Implementation: `internal/linuxidentitymeasure` observer attached from
`cmd/aurora-presence-local-server` only when the path is set. Peer generation and
ancestry use `linuxprocess.CaptureAncestryChain` immediately after auth.
Measurement failure never rejects an otherwise valid Package 6 observe-only
ingress.

## 17. Unresolved questions requiring Blue1 measurements

These are **evidence gaps**, not permission to weaken fail-closed behavior or to
trust client fields.

1. **Actual parent trees** for `aurora-claude-hook` and `aurora-codex-hook` when
   fired from live Claude Code and Codex sessions (including npx/npm wrappers).
2. **Whether the hook process appears as a family member** in Package 2/5
   snapshots during the connection, or only as a transient child missing from
   the snapshot.
3. **Parallel sessions:** two Claude and two Codex simultaneously — does L2/L3
   uniquely select the correct runtime every time under validated tool
   namespaces?
4. **Peer lifetime:** distribution of dialer lifetime vs accept → generation
   capture latency; rate of `peer_process_unreadable`.
5. **Attestation cost:** p50/p95/p99 time for the full transaction (generation +
   ancestry + join) under realistic process counts (~200+ as previously
   observed).
6. **USER_HZ / start-time normalization** consistency between peer reads and
   runtime snapshot members (no systematic off-by-tick mismatches).
7. **Exec patterns:** does any supported install path `exec` the hook over a
   shell in a way that changes classification mid-transaction?
8. **Permission boundaries:** same-UID `/proc` readability for peer and parents
   without elevated privileges.
9. **Whether L3 is required in practice** or L2 membership always holds when
   hooks fire.
10. **False-link scenarios** under shared TTY/process group with distinct
    families — confirm soft context constraints do not over-merge.

Use section 16’s diagnostic to collect labeled JSONL for (1)–(5) and (9)–(10).
Until those are answered with evidence, Package 7 must not treat the bridge as
production-authorizing even if code lands behind flags.

## 18. Worked examples

### Example A — Packages 0–6 only

Hook ingress accepted and sequenced; no bridge.

→ Internal path has no trusted evidence.  
→ Package 7: `refuse` / `missing_trusted_hard_identity`.  
→ Package 6 response unchanged observe-only.

### Example B — peer is helper child of unique Claude runtime (L3)

- Accept → `SO_PEERCRED` PID 5000 → auth OK → `/proc` start T_h stable.
- Request validated: `tool=claude`.
- Verified parent chain: 5000 → shell → claude root PID 4000 start T_r.
- One Claude runtime candidate with that root.

→ Checkpoint: trusted evidence, `link_rule=L3_ancestry`.  
→ Package 6: sequences as usual.  
→ Package 7 may consider `propose_bind` only if all other Package 7 §3.1 gates
hold.

### Example C — two Claude runtimes, soft-only separation

- Peer chain reaches a shell shared by soft session signals only; two legal L3
  targets or none unique after validated `tool=claude`.

→ Checkpoint: `ambiguous_runtime_link` / `no_unique_runtime_link`,
`trusted_hard_identity_present=false`.  
→ Package 6: still sequences valid ingress observe-only.  
→ Package 7: `refuse`.

### Example D — PID reuse during generation capture

- First stat start ticks S1; second read S2 ≠ S1.

→ Checkpoint: `peer_generation_unstable` / `pid_reused`.  
→ No trusted evidence.  
→ Valid Package 6 ingress may still sequence.

### Example E — client sends process fields (forbidden path)

- Even if a buggy client includes PID claims, bridge ignores them.
- Client `tool` only restricts namespace after validation.

→ Authoritative path remains §3 only.

### Example F — attestation fails; Package 6 still succeeds

- Peer generation unreadable after auth; request is otherwise valid Package 6
  ingress.

→ Checkpoint: `trusted_hard_identity_present=false`,
`package6_sequenced_without_identity`.  
→ Package 6: `status=ok` (or other sequencing outcome),
`no_binding_performed=true`, no process fields in response.  
→ Package 7: `refuse` / `missing_trusted_hard_identity`.

### Example G — successful attestation must not mutate

- Checkpoint returns trusted evidence; Package 7 dry-run emits `propose_bind`.

→ Registry unchanged; `no_mutation_performed=true` end-to-end until Package 8.

## 19. Implementation sketch (non-normative placement)

| Layer | Responsibility |
| --- | --- |
| `localhooktransport` (Linux) | Accept, peer creds, optional `IngestIdentityObserver`; no agent rules |
| `linuxprocess.CaptureGeneration` | Generation double-read for peer PID |
| `linuxidentitymeasure` | Read-only Blue1 diagnostic: ancestry + L1/L2/L3 JSONL |
| Composition (`cmd/aurora-presence-local-server`) | Attach observer only when `-identity-measure-file` / env set |
| Package 6 sequencer | Independent observe-only sequencing of valid ingress |
| Future `bindingpolicy` (Package 7) | Consume trusted evidence; never produce hard identity from soft scores |
| Registry / publish | Untouched until Package 8 |

No import cycle from policy to registry; no agent import into generic transport.
The measurement diagnostic must remain default-off and never authorize bind.
