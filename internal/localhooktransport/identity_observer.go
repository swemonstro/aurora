package localhooktransport

import (
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

// IdentityAncestryHop is one connection-local process generation on the peer
// parent chain (PID + start time only). Captured before request frame read.
type IdentityAncestryHop struct {
	PID       uint64
	StartedAt time.Time
	Depth     int
	IsPeer    bool
}

// IdentityPeerCapture is a connection-local peer generation and verified parent
// chain produced immediately after SO_PEERCRED authentication and before
// request frame read. It is not a wire type and must never be echoed to the
// hook client.
type IdentityPeerCapture struct {
	PeerUID           uint32
	PeerGID           uint32
	PeerPID           int32
	GenerationOK      bool
	GenerationPID     uint64
	GenerationStarted time.Time
	// Ancestry is captured with the peer generation (hop 0 = peer) before the
	// request is read, while the dialer is still expected to be alive.
	Ancestry        []IdentityAncestryHop
	ReasonCodes     []string
	CaptureDuration time.Duration
	CapturedAt      time.Time
}

// IngestIdentityObserver is an optional Package 7.0 diagnostic hook at the
// local server composition boundary. Implementations must be read-only, must
// not mutate registry/slots/presence, and must not influence Package 6
// responses. Failures inside the observer must be swallowed by the caller.
type IngestIdentityObserver interface {
	// CapturePeer runs immediately after peer auth succeeds and before the
	// request frame is read. It must capture peer generation and a bounded
	// verified parent chain while the dialer is still expected to be alive.
	// The result is connection-local (return value), not a process-global cache.
	CapturePeer(peer PeerIdentity) IdentityPeerCapture

	// CompleteIngest runs after Package 6 ingest handling returns. validated is
	// true only when the request was a valid Package 6 ingress (status ok or
	// duplicate). tool/lifecycle are set only when validated. Implementations
	// may join the pre-captured ancestry against current runtime candidates
	// using validated tool as namespace only; they must not re-require the peer
	// process to still be alive, and must never alter the Package 6 response.
	CompleteIngest(capture IdentityPeerCapture, tool instancepresence.ToolKind, lifecycle instancecorrelation.Lifecycle, validated bool)
}
