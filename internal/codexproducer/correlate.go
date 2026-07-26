package codexproducer

import (
	"sort"
	"sync"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/producerprotocol"
	"github.com/swemonstro/aurora/internal/sessionbinding"
)

// SessionID is Codex's own opaque hook session_id, scoped to the Codex tool
// namespace only (see internal/sessionbinding.Key). It is never treated as a
// runtime or instance identity.
type SessionID = instancepresence.OpaqueIdentity

// queuedHookEvent is one hook delivery received for a session that is not
// yet bound to a recognized instance. Queuing (rather than keeping only the
// latest) preserves the full observed state sequence once binding succeeds,
// so a PermissionRequest that arrives moments before correlation resolves is
// never silently dropped in favor of a later event.
type queuedHookEvent struct {
	state      producerprotocol.State
	receivedAt time.Time
}

type pendingSession struct {
	rawSession       SessionID
	source           SourceLabel
	queue            []queuedHookEvent
	firstReceivedAt  time.Time
	lastProcessGroup string
	lastOSSession    string
}

// Correlator binds Codex hook session identifiers to instance ids exactly
// once each, reusing internal/sessionbinding's generic (tool, session) ->
// instance_id registry (the same package internal/localhooktransport's
// binding engine uses for every tool) rather than re-deriving exclusive
// session/runtime binding from scratch.
//
// Its matching rule is deliberately narrower than the monolith's
// internal/instancecorrelation.Engine (a weighted, ambiguity-aware scoring
// engine over many evidence kinds): Codex's own hook payload carries no PID
// at all (docs/architecture/adapters/codex.md's "saknad identitet" section),
// so the only evidence available here is the configured source (from the
// delivering hook process's own CODEX_HOME) and that same process's own
// captured OS process-group/session (see ingress_linux.go). Resolve binds a
// pending session only when exactly one same-source unbound candidate
// exists, or process-group/session evidence uniquely picks one out of
// several. Anything left ambiguous stays pending rather than guessed, and
// expires (dropped, never bound) after pendingTTL — a documented G.4 gap,
// not a heuristic.
//
// Every internal key (pending map, sessionbinding.Registry's own Session
// value) is a composite of (source, session_id), never session_id alone:
// internal/sessionbinding.Key only carries (Tool, Session), and Tool is
// fixed to Codex for this entire package, so two different configured
// sources whose underlying session identifiers happen to be identical
// strings would otherwise collide into a single binding — see
// "samma underliggande sessionsidentifierare i två olika källor får inte
// kollidera" — exactly what compositeSession exists to prevent.
type Correlator struct {
	mu         sync.Mutex
	bindings   *sessionbinding.Registry
	sessionOf  map[producerprotocol.InstanceID]instancepresence.OpaqueIdentity
	pending    map[instancepresence.OpaqueIdentity]*pendingSession
	pendingTTL time.Duration
}

// maxPendingSessions and maxQueuedEventsPerSession bound the memory a
// misbehaving or unbounded hook stream can consume before Reconcile's
// pendingTTL expiry ever runs (e.g. many distinct session ids arriving
// faster than one poll interval, or many hook events for a single session
// that never resolves): a same-UID-authenticated hook is a low-risk
// caller, but this is a shadow-mode process that should not depend on
// trusting its own peers to be well-behaved. maxQueuedEventsPerSession
// keeps the most recent events (dropping the oldest) so the eventually
// resolved replay still reflects current reality rather than a stale
// truncated prefix.
const (
	maxPendingSessions        = 512
	maxQueuedEventsPerSession = 64
)

// NewCorrelator constructs a Correlator. pendingTTL bounds how long a hook
// delivery may wait for a matching recognized process before being dropped.
func NewCorrelator(pendingTTL time.Duration) *Correlator {
	return &Correlator{
		bindings:   sessionbinding.New(),
		sessionOf:  make(map[producerprotocol.InstanceID]instancepresence.OpaqueIdentity),
		pending:    make(map[instancepresence.OpaqueIdentity]*pendingSession),
		pendingTTL: pendingTTL,
	}
}

// compositeSession scopes a Codex hook session_id by its configured source,
// so identical session_id strings from different sources are never treated
// as the same session (see Correlator's doc comment). It is opaque and
// content-free beyond the operator-chosen source label.
func compositeSession(source SourceLabel, session SessionID) instancepresence.OpaqueIdentity {
	return instancepresence.OpaqueIdentity(string(source) + "\x00" + string(session))
}

// Deliver records one hook delivery. If (source, session) is already bound,
// the mapped state is returned for immediate application; otherwise it is
// queued until Reconcile can resolve it against currently recognized
// instances.
func (correlator *Correlator) Deliver(session SessionID, source SourceLabel, mapped producerprotocol.State, hint HookDelivery, now time.Time) (id producerprotocol.InstanceID, resolved bool) {
	correlator.mu.Lock()
	defer correlator.mu.Unlock()
	composite := compositeSession(source, session)
	for instance, boundComposite := range correlator.sessionOf {
		if boundComposite == composite {
			return instance, true
		}
	}
	entry, exists := correlator.pending[composite]
	if !exists {
		if len(correlator.pending) >= maxPendingSessions {
			// At capacity: drop rather than grow unbounded. A legitimate
			// session that never gets a chance to be tracked here simply
			// never resolves and is never reported — a safe degradation in
			// shadow mode, never a wrong bind or a crash.
			return "", false
		}
		entry = &pendingSession{rawSession: session, source: source, firstReceivedAt: now}
		correlator.pending[composite] = entry
	}
	entry.queue = append(entry.queue, queuedHookEvent{state: mapped, receivedAt: now})
	if len(entry.queue) > maxQueuedEventsPerSession {
		// Keep the most recent window so an eventual replay reflects
		// current reality rather than a stale, arbitrarily-truncated
		// prefix of a session that received unusually many events while
		// still unresolved.
		entry.queue = entry.queue[len(entry.queue)-maxQueuedEventsPerSession:]
	}
	if hint.ProcessGroupOrJob != "" {
		entry.lastProcessGroup = hint.ProcessGroupOrJob
	}
	if hint.OSSession != "" {
		entry.lastOSSession = hint.OSSession
	}
	return "", false
}

// ResolvedSession is one pending session Reconcile successfully bound to an
// instance, together with the full ordered sequence of states observed while
// it was pending — callers must apply every entry, in order, to Machine.
type ResolvedSession struct {
	Session    SessionID
	InstanceID producerprotocol.InstanceID
	Source     SourceLabel
	States     []producerprotocol.State
}

// Reconcile attempts to resolve every pending session against candidates
// (the current poll's recognized instances). Candidates already bound to a
// session are automatically excluded. Expired, still-unresolved pending
// sessions (older than pendingTTL) are dropped silently (see Correlator's
// doc comment); resolved sessions are returned for the caller to replay into
// a Machine and are removed from the pending set.
func (correlator *Correlator) Reconcile(candidates []RecognizedInstance, now time.Time) []ResolvedSession {
	correlator.mu.Lock()
	defer correlator.mu.Unlock()

	claimed := make(map[producerprotocol.InstanceID]struct{}, len(correlator.sessionOf))
	for instance := range correlator.sessionOf {
		claimed[instance] = struct{}{}
	}

	composites := make([]instancepresence.OpaqueIdentity, 0, len(correlator.pending))
	for composite := range correlator.pending {
		composites = append(composites, composite)
	}
	sort.Slice(composites, func(first, second int) bool {
		return correlator.pending[composites[first]].firstReceivedAt.Before(correlator.pending[composites[second]].firstReceivedAt)
	})

	var resolved []ResolvedSession
	for _, composite := range composites {
		entry := correlator.pending[composite]
		var sameSource []RecognizedInstance
		for _, candidate := range candidates {
			if candidate.Source != entry.source {
				continue
			}
			if _, already := claimed[candidate.InstanceID]; already {
				continue
			}
			sameSource = append(sameSource, candidate)
		}

		match, ok := selectUniqueCandidate(sameSource, entry)
		if !ok {
			if now.Sub(entry.firstReceivedAt) > correlator.pendingTTL {
				delete(correlator.pending, composite)
			}
			continue
		}

		if err := correlator.bindings.Reserve(instancepresence.ToolCodex, composite, instancepresence.InstanceID(match.InstanceID)); err != nil {
			continue
		}
		if err := correlator.bindings.Commit(instancepresence.ToolCodex, composite); err != nil {
			_ = correlator.bindings.Rollback(instancepresence.ToolCodex, composite)
			continue
		}
		correlator.sessionOf[match.InstanceID] = composite
		claimed[match.InstanceID] = struct{}{}

		states := make([]producerprotocol.State, len(entry.queue))
		for index, event := range entry.queue {
			states[index] = event.state
		}
		resolved = append(resolved, ResolvedSession{Session: entry.rawSession, InstanceID: match.InstanceID, Source: entry.source, States: states})
		delete(correlator.pending, composite)
	}
	return resolved
}

// selectUniqueCandidate applies the process-group/session disambiguation
// rule described on Correlator.
func selectUniqueCandidate(candidates []RecognizedInstance, entry *pendingSession) (RecognizedInstance, bool) {
	if len(candidates) == 1 {
		return candidates[0], true
	}
	if len(candidates) == 0 {
		return RecognizedInstance{}, false
	}
	if entry.lastProcessGroup != "" {
		if match, unique := uniqueByField(candidates, func(candidate RecognizedInstance) string {
			return string(candidate.ProcessGroupOrJob)
		}, entry.lastProcessGroup); unique {
			return match, true
		}
	}
	if entry.lastOSSession != "" {
		if match, unique := uniqueByField(candidates, func(candidate RecognizedInstance) string {
			return string(candidate.OSSession)
		}, entry.lastOSSession); unique {
			return match, true
		}
	}
	return RecognizedInstance{}, false
}

func uniqueByField(candidates []RecognizedInstance, field func(RecognizedInstance) string, want string) (RecognizedInstance, bool) {
	var match RecognizedInstance
	count := 0
	for _, candidate := range candidates {
		if field(candidate) == want {
			match = candidate
			count++
		}
	}
	if count == 1 {
		return match, true
	}
	return RecognizedInstance{}, false
}

// ForgetInstance releases a resolved instance's session binding, e.g. because
// its process disappeared. Safe to call for an instance with no binding.
func (correlator *Correlator) ForgetInstance(id producerprotocol.InstanceID) {
	correlator.mu.Lock()
	defer correlator.mu.Unlock()
	session, ok := correlator.sessionOf[id]
	if !ok {
		return
	}
	_, _ = correlator.bindings.Unbind(instancepresence.ToolCodex, session)
	delete(correlator.sessionOf, id)
}

// PendingCount reports how many sessions are currently awaiting resolution
// (diagnostic use only, e.g. tests).
func (correlator *Correlator) PendingCount() int {
	correlator.mu.Lock()
	defer correlator.mu.Unlock()
	return len(correlator.pending)
}
