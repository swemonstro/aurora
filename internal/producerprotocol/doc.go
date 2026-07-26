// Package producerprotocol defines the versioned wire protocol that presence
// producers (one per tool: Claude, Codex, Grok, or any future tool) use to
// report state to a presence broker, together with its local Unix domain
// socket transport.
//
// This package is deliberately isolated:
//
//   - it imports no Claude-, Codex-, or Grok-specific package, and no other
//     Aurora package at all;
//   - it never mutates registry, snapshot, or relay state;
//   - it never decides which tool a peer or socket belongs to. It only
//     exposes primitives (PeerIdentity capture, Conn.Bind, IdentityBinder)
//     that let a future broker enforce that binding without tool-specific
//     logic leaking into the transport.
//
// The wire message is intentionally minimal: protocol_version, tool,
// instance_id, producer_epoch, state, revision, observed_at,
// lease_expires_at. Unknown fields are rejected so that adding a field is
// always a protocol_version change, never a silent extension — as it was
// for producer_epoch itself, added in version 2 (see CurrentProtocolVersion)
// so a broker can tell "producer restarted, revision legitimately reset"
// apart from "stale or replayed data for the same generation," a
// distinction Revision alone cannot make.
//
// socket_linux.go's secure-socket-directory logic and framing.go's
// length-prefixed frame codec are close cousins of the equivalent code in
// internal/localhooktransport (symlink/race-safe bind, same-owner checks,
// 4-byte big-endian frames). The duplication is intentional for now, not
// missed: extracting a shared primitive would mean either this package
// depending on localhooktransport or both depending on a new package,
// either of which is a change to localhooktransport's package boundary and
// therefore out of scope for this package's own foundation. It is a
// reasonable follow-up once the broker step's requirements make the right
// shared shape clear, but isolation until then keeps producerprotocol
// reviewable and changeable without touching an unrelated package.
package producerprotocol
