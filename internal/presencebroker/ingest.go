package presencebroker

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// ErrInstanceToolMismatch is returned when a message's InstanceID is
// already registered under a different Tool. A producer must never mutate
// another tool's instance, regardless of which socket the message arrived
// on, and this is never a legitimate takeover — cross-tool takeover is
// always rejected, independent of producer_epoch.
var ErrInstanceToolMismatch = errors.New("instance id is already registered under a different tool")

// ErrGenerationStillActive is returned when a message reports a
// producer_epoch that differs from the instance's current epoch while that
// current epoch's connection is still live (has not disconnected and the
// instance has not independently been retired by lease expiry). A new
// generation may only take over after the previous one has gone away — see
// ProducerSession and Ingestor.
var ErrGenerationStillActive = errors.New("a live generation already owns this instance under a different producer epoch")

// ErrProducerEpochRetired is returned when a message reports a
// producer_epoch that this instance id has already moved on from — i.e. a
// later generation has since taken over, whether or not that later
// generation's connection is itself still live. A retired epoch can never
// reactivate: an epoch's authority ends permanently the moment a later
// generation takes over, so a superseded producer that reconnects (or a
// belated/duplicated connection) can never resurrect a stale generation
// even after the epoch that superseded it has itself disconnected. See
// Ingestor.generations.
var ErrProducerEpochRetired = errors.New("producer epoch has already been superseded and can never reactivate")

// ErrConnectionEpochChanged is returned by ProducerSession.Apply when a
// message on an already-bound connection reports a different tool or
// producer_epoch than the first message this connection ever sent. A
// single connection speaks for exactly one (tool, producer_epoch) pair for
// its lifetime — it may carry many different instance_ids under that pair,
// but never more than one pair — and a producer that needs to report a new
// generation must open a new connection.
var ErrConnectionEpochChanged = errors.New("connection attempted to change its bound tool or producer epoch")

// generationRecord tracks, for one instance id, the full generation
// history needed to decide whether an incoming report may proceed:
//
//   - tool is fixed for the lifetime of this instance id (cross-tool
//     takeover is always rejected, regardless of epoch).
//   - currentEpoch is the producer_epoch currently allowed to mutate this
//     instance id (via an ordinary continuing report) or to have been the
//     most recent generation to hold it (if live is false).
//   - live is true exactly while some connection is known to still own
//     currentEpoch — i.e. it has sent at least one accepted report and has
//     not yet closed. A different epoch may only take over while live is
//     false (or the registry independently already considers the instance
//     ended — see claimGeneration's staleClaim check, for the case where a
//     connection never formally closes).
//   - owner is the opaque token (ProducerSession.owner) of the connection
//     currently entitled to currentEpoch while live is true. Two live
//     connections can legitimately present the exact same (tool, epoch) —
//     e.g. a producer that reconnects without the broker having noticed
//     the old socket died yet — and epoch equality alone cannot tell them
//     apart: only owner can. owner is meaningless while live is false (the
//     most recent owner's identity does not grant it any special right to
//     reclaim the id later — see claimGeneration's resume path, which
//     accepts any connection presenting currentEpoch once live is false).
//     owner is purely internal bookkeeping: it is never derived from, or
//     exposed to, the wire protocol, and never appears in a log line (see
//     ClassifyIngestError, which never formats owner).
//   - retiredEpochs is every epoch this instance id has ever moved on from.
//     Membership is permanent: once an epoch is retired it can never
//     reactivate, even after the epoch that superseded it has itself gone
//     away. Without this, A -> disconnect -> B -> disconnect -> A would
//     wrongly let A reactivate, since a live/not-live flag alone cannot
//     distinguish "A's original generation" from "A returning after being
//     superseded".
//
// retiredEpochs only grows on a genuine takeover (a new producer_epoch
// actually appearing for this id), so in practice it stays tiny — bounded
// by how many times this specific instance id's underlying process has
// restarted, not by message volume. It is never evicted: evicting an
// entry would silently reopen exactly the rollback hole this field exists
// to close. This mirrors instanceregistry.Registry's own instances map,
// which likewise never garbage-collects an instance id once seen — see the
// G.3 report's note on that structural asymmetry.
type generationRecord struct {
	tool          instancepresence.ToolKind
	currentEpoch  instancepresence.ProducerEpoch
	live          bool
	owner         uint64
	retiredEpochs map[instancepresence.ProducerEpoch]struct{}
}

// Ingestor translates already-validated producerprotocol.Message values
// into instanceregistry.Registry operations via a single atomic call per
// message (Registry.ApplyProducerReport). It performs no process
// observation or tool-specific interpretation: Tool and State are carried
// through as opaque data.
//
// Lock ordering: the only two locks Ingestor's own code ever touches are
// generationsMu (below) and, transitively, registry.mu inside
// instanceregistry.Registry. Every call path acquires generationsMu first
// and always releases it (via a plain Unlock or by returning from the
// locked section) before ever calling into the registry — except
// claimGeneration's staleClaim check, which calls registry.Get while still
// holding generationsMu; this is safe only because instanceregistry never
// calls back into Ingestor (it has no reference to it and no notion of
// generations), so the acquisition order generationsMu -> registry.mu is
// the sole direction anywhere in this package, and a cycle is therefore
// impossible. Do not add any call from instanceregistry back into
// Ingestor, and do not acquire registry.mu before generationsMu anywhere
// in this file, or this guarantee breaks.
type Ingestor struct {
	registry     *instanceregistry.Registry
	hostID       string
	collectorID  string
	bootID       instancepresence.BootIdentity
	syntheticPID atomic.Uint64
	sessionSeq   atomic.Uint64

	generationsMu sync.Mutex
	generations   map[instancepresence.InstanceID]*generationRecord
}

// NewIngestor constructs an Ingestor. hostID identifies this broker's host
// (an opaque configured value, not read from any process); collectorID
// identifies this broker instance as the source of the registrations it
// creates (e.g. "aurora-presence-broker").
func NewIngestor(registry *instanceregistry.Registry, hostID, collectorID string) (*Ingestor, error) {
	if registry == nil {
		return nil, fmt.Errorf("registry must not be nil")
	}
	if strings.TrimSpace(hostID) == "" {
		return nil, fmt.Errorf("host ID must not be empty")
	}
	if strings.TrimSpace(collectorID) == "" {
		return nil, fmt.Errorf("collector ID must not be empty")
	}
	return &Ingestor{
		registry:    registry,
		hostID:      hostID,
		collectorID: collectorID,
		// bootID discriminates one broker process's registrations from
		// another's; it is never read from the OS and carries no process
		// identity. It only needs to be unique enough, alongside the
		// per-instance synthetic PID below, that instanceregistry's
		// internal active-runtime de-duplication key never collides
		// between two genuinely different instances.
		bootID:      instancepresence.BootIdentity(fmt.Sprintf("producerprotocol-%d", time.Now().UnixNano())),
		generations: make(map[instancepresence.InstanceID]*generationRecord),
	}, nil
}

// NewSession starts tracking one connection's producer identity. Each
// producerprotocol connection must use its own session, and only that
// session: a session enforces that its connection speaks for exactly one
// (tool, producer_epoch) pair for its lifetime (ErrConnectionEpochChanged
// otherwise), while allowing that one connection to carry many different
// instance_ids under that pair — each with its own independent revision,
// state, and lease. Close releases every instance_id this session ever
// successfully claimed, so a new connection may legitimately take over any
// of them.
//
// Each session is assigned a process-local, opaque owner token distinct
// from every other session this Ingestor has ever created (sessionSeq is
// monotonic and never reused), so two live sessions presenting the exact
// same (tool, producer_epoch) are still distinguishable — see
// generationRecord.owner.
func (ingestor *Ingestor) NewSession() *ProducerSession {
	return &ProducerSession{ingestor: ingestor, owner: ingestor.sessionSeq.Add(1)}
}

// ProducerSession is the per-connection handle for Ingestor. See NewSession.
type ProducerSession struct {
	ingestor *Ingestor
	// owner is this session's opaque generation-ownership token (see
	// generationRecord.owner). Fixed for the session's lifetime, assigned
	// once in NewSession.
	owner uint64

	mu      sync.Mutex
	bound   bool
	tool    instancepresence.ToolKind
	epoch   instancepresence.ProducerEpoch
	claimed map[instancepresence.InstanceID]struct{}
}

// Apply maps one already-validated message onto the registry through this
// session. msg must already have passed producerprotocol.ValidateMessage
// (e.g. as returned by a successful Conn.ReadMessage call). A rejection of
// one instance_id on this connection (any error return) never affects any
// other instance_id already claimed on the same session — the caller
// should log and keep reading, exactly as it would for any other instance
// id's rejection.
func (session *ProducerSession) Apply(msg producerprotocol.Message) (instancepresence.Instance, error) {
	id := instancepresence.InstanceID(msg.InstanceID)
	tool := instancepresence.ToolKind(msg.Tool)
	epoch := instancepresence.ProducerEpoch(msg.ProducerEpoch)

	session.mu.Lock()
	if session.bound && (session.tool != tool || session.epoch != epoch) {
		session.mu.Unlock()
		return instancepresence.Instance{}, ErrConnectionEpochChanged
	}
	session.mu.Unlock()

	instance, err := session.ingestor.apply(msg, id, tool, epoch, session.owner)
	if err != nil {
		return instancepresence.Instance{}, err
	}

	session.mu.Lock()
	session.tool, session.epoch, session.bound = tool, epoch, true
	if session.claimed == nil {
		session.claimed = make(map[instancepresence.InstanceID]struct{})
	}
	session.claimed[id] = struct{}{}
	session.mu.Unlock()
	return instance, nil
}

// Close releases every instance_id this session ever successfully claimed,
// so a new connection may take over any of them. It is safe to call even if
// the session never sent a valid message, and safe to call more than once.
func (session *ProducerSession) Close() {
	session.mu.Lock()
	bound, epoch, claimed := session.bound, session.epoch, session.claimed
	session.bound = false
	session.claimed = nil
	session.mu.Unlock()
	if !bound {
		return
	}
	for id := range claimed {
		session.ingestor.release(id, epoch, session.owner)
	}
}

// apply performs the connection-independent part of ingest: tentatively
// deciding (using the generations map) whether this report may proceed,
// then issuing the single atomic registry call, and rolling the tentative
// generation change back if the registry rejects the report. This is the
// transactional boundary between claims and registry: a generation change
// is never left in place unless ApplyProducerReport actually accepted the
// report, for every rejection reason (invalid observed_at, invalid lease,
// stale revision, revision conflict, tool mismatch, a rejected takeover, or
// any other validation failure of a brand-new instance id's first report).
func (ingestor *Ingestor) apply(
	msg producerprotocol.Message,
	id instancepresence.InstanceID,
	tool instancepresence.ToolKind,
	epoch instancepresence.ProducerEpoch,
	owner uint64,
) (instancepresence.Instance, error) {
	rollback, err := ingestor.claimGeneration(id, tool, epoch, owner)
	if err != nil {
		return instancepresence.Instance{}, err
	}

	report := instanceregistry.ProducerReport{
		InstanceID: id, Tool: tool,
		Source: instancepresence.SourceDescriptor{
			Provider: "producerprotocol", Profile: string(tool), CollectorID: ingestor.collectorID,
		},
		Runtime:        ingestor.syntheticRuntimeIdentity(msg),
		ProducerEpoch:  epoch,
		Revision:       uint64(msg.Revision),
		State:          instancepresence.EffectiveState(msg.State),
		ObservedAt:     msg.ObservedAt,
		LeaseExpiresAt: msg.LeaseExpiresAt,
	}
	instance, _, applyErr := ingestor.registry.ApplyProducerReport(report)
	if applyErr != nil {
		if rollback != nil {
			rollback()
		}
		return instancepresence.Instance{}, applyErr
	}
	return instance, nil
}

// claimGeneration decides whether (id, tool, epoch, owner) may proceed to
// the registry, and if so, tentatively updates the generation history to
// reflect it.
//
// On success it returns a rollback closure the caller must invoke if the
// subsequent registry call fails, so a failed attempt never permanently
// blocks a later legitimate one, and never leaves owner/live/retiredEpochs
// pointing at a generation the registry itself never actually accepted;
// rollback is nil only when nothing about the generation history changed
// (the connection that already owns this exact generation sending another,
// possibly-rejected, report for it), since there is then nothing to undo.
func (ingestor *Ingestor) claimGeneration(
	id instancepresence.InstanceID,
	tool instancepresence.ToolKind,
	epoch instancepresence.ProducerEpoch,
	owner uint64,
) (rollback func(), err error) {
	ingestor.generationsMu.Lock()
	defer ingestor.generationsMu.Unlock()

	rec, has := ingestor.generations[id]
	if !has {
		ingestor.generations[id] = &generationRecord{
			tool: tool, currentEpoch: epoch, live: true, owner: owner,
			retiredEpochs: make(map[instancepresence.ProducerEpoch]struct{}),
		}
		return func() {
			ingestor.generationsMu.Lock()
			defer ingestor.generationsMu.Unlock()
			// Only undo if nothing else has since taken this id further:
			// otherwise we'd erase a newer, independently-established
			// generation. tool/epoch/owner all matching is a stronger
			// guarantee than epoch alone, though under generationsMu's
			// strict serialization nothing else could actually have run
			// between establishing this tentative record and this rollback
			// firing — this guard is defense in depth, not load-bearing.
			if current, ok := ingestor.generations[id]; ok &&
				current.tool == tool && current.currentEpoch == epoch && current.owner == owner {
				delete(ingestor.generations, id)
			}
		}, nil
	}

	if rec.tool != tool {
		return nil, ErrInstanceToolMismatch
	}

	if epoch == rec.currentEpoch {
		if rec.live && rec.owner != owner {
			// A DIFFERENT connection currently owns this exact (tool,
			// epoch) generation. Two live connections can present the
			// identical epoch (e.g. a producer that reconnected before the
			// broker noticed its old socket died); only one may ever be
			// the recognized owner at a time.
			return nil, ErrGenerationStillActive
		}
		if rec.live && rec.owner == owner {
			// This exact connection already owns it: no generation-history
			// change is being made, so there is nothing to roll back if
			// the registry now rejects this particular report (e.g. a
			// stale or conflicting revision) — this connection remains the
			// rightful owner regardless, and may simply retry.
			return nil, nil
		}
		// Not currently live: a legitimate resume, whether the same
		// connection retrying after its own earlier report failed, or a
		// new connection reconnecting with the same epoch after the real
		// owner disconnected.
		previousOwner := rec.owner
		rec.live = true
		rec.owner = owner
		return func() {
			ingestor.generationsMu.Lock()
			defer ingestor.generationsMu.Unlock()
			if current, ok := ingestor.generations[id]; ok &&
				current.currentEpoch == epoch && current.owner == owner {
				current.live = false
				current.owner = previousOwner
			}
		}, nil
	}

	if _, retired := rec.retiredEpochs[epoch]; retired {
		return nil, ErrProducerEpochRetired
	}
	if rec.live {
		// A live claim exists for a DIFFERENT epoch. This can only proceed
		// as a takeover if the registry independently already considers
		// the current generation retired (lease fully expired) — a live
		// connection's claim is not overridable just because a message
		// with a different epoch showed up.
		existing, getErr := ingestor.registry.Get(id)
		staleClaim := getErr == nil && existing.Status == instancepresence.RuntimeEnded
		if !staleClaim {
			return nil, ErrGenerationStillActive
		}
	}

	previousEpoch, previousOwner, previousLive := rec.currentEpoch, rec.owner, rec.live
	rec.retiredEpochs[previousEpoch] = struct{}{}
	rec.currentEpoch = epoch
	rec.live = true
	rec.owner = owner
	return func() {
		ingestor.generationsMu.Lock()
		defer ingestor.generationsMu.Unlock()
		if current, ok := ingestor.generations[id]; ok && current.currentEpoch == epoch && current.owner == owner {
			// A rejected takeover must never leave the attempted epoch
			// retired, or the previous (still rightfully current) epoch
			// blocked from continuing to operate or from being retried by
			// a genuinely new epoch afterwards.
			delete(current.retiredEpochs, previousEpoch)
			current.currentEpoch = previousEpoch
			current.live = previousLive
			current.owner = previousOwner
		}
	}, nil
}

// release marks (id)'s claim as no longer live if epoch and owner both
// still match, but only if nothing newer has already taken over in the
// meantime (a belated or duplicate Close from a superseded, or merely
// no-longer-current, connection must never clobber a later owner's live
// claim). Matching on owner in addition to epoch is required: two live
// connections can share the same epoch (see claimGeneration), so epoch
// alone cannot tell a stale Close from this connection's own legitimate
// one. release deliberately never deletes the generation record itself —
// see generationRecord's retiredEpochs field — so the id's generation
// history survives across any number of disconnect/reconnect cycles.
func (ingestor *Ingestor) release(id instancepresence.InstanceID, epoch instancepresence.ProducerEpoch, owner uint64) {
	ingestor.generationsMu.Lock()
	defer ingestor.generationsMu.Unlock()
	if rec, ok := ingestor.generations[id]; ok && rec.currentEpoch == epoch && rec.owner == owner {
		rec.live = false
	}
}

// syntheticRuntimeIdentity builds the RuntimeIdentity instanceregistry.Registry
// requires to create a record. This broker performs no process observation:
// RootProcess.PID here is a broker-internal monotonic counter, never a real
// OS PID, and RootProcess.StartedAt is the time this instance was first
// registered, not a process start time. Both only need to be unique among
// this Ingestor's concurrently active instances, which the counter
// guarantees regardless of timestamp collisions.
func (ingestor *Ingestor) syntheticRuntimeIdentity(msg producerprotocol.Message) instancepresence.RuntimeIdentity {
	return instancepresence.RuntimeIdentity{
		HostID: ingestor.hostID,
		BootID: ingestor.bootID,
		RootProcess: instancepresence.ProcessIdentity{
			PID:       ingestor.syntheticPID.Add(1),
			StartedAt: msg.ObservedAt,
		},
	}
}

// ClassifyIngestError maps an Apply error into a stable, content-free
// string safe to log: it is built entirely from errors.Is checks and never
// formats the error itself, so it can never echo an InstanceID or other
// message content (instanceregistry.DomainError.Error() does include the
// InstanceID — callers must use this classification for logs, never
// err.Error() directly, for any error Apply/Session.Apply can return).
func ClassifyIngestError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrConnectionEpochChanged):
		return "connection_epoch_changed"
	case errors.Is(err, ErrInstanceToolMismatch):
		return "tool_mismatch"
	case errors.Is(err, ErrGenerationStillActive):
		return "generation_still_active"
	case errors.Is(err, ErrProducerEpochRetired):
		return "producer_epoch_retired"
	case errors.Is(err, instanceregistry.ErrToolMismatch):
		return "tool_mismatch"
	case errors.Is(err, instanceregistry.ErrLeaseAlreadyExpired):
		return "lease_already_expired"
	case errors.Is(err, instanceregistry.ErrLeaseExceedsMaximum):
		return "lease_exceeds_maximum"
	case errors.Is(err, instanceregistry.ErrClockSkewTooLarge):
		return "clock_skew_too_large"
	case errors.Is(err, instanceregistry.ErrReportTooOld):
		return "report_too_old"
	case errors.Is(err, instanceregistry.ErrStaleRevision):
		return "stale_revision"
	case errors.Is(err, instanceregistry.ErrRevisionConflict):
		return "revision_conflict"
	case errors.Is(err, instanceregistry.ErrIdempotencyConflict):
		return "idempotency_conflict"
	case errors.Is(err, instanceregistry.ErrIdentityConflict):
		return "identity_conflict"
	case errors.Is(err, instanceregistry.ErrEpochConflict):
		return "epoch_conflict"
	case errors.Is(err, instanceregistry.ErrRuntimeEnded):
		return "runtime_ended"
	case errors.Is(err, instanceregistry.ErrNotFound):
		return "not_found"
	default:
		return "internal_error"
	}
}
