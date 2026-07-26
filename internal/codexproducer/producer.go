package codexproducer

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// Clock is the sole time source used by this package's orchestration, so
// tests never depend on wall-clock time.
type Clock interface {
	Now() time.Time
}

// Config bounds one Producer's behavior. See cmd/aurora-codex-presence for
// the CLI flags that populate this.
type Config struct {
	// DialConfig configures the broker connection. DialConfig.SocketPath is
	// the broker's Codex socket (e.g. /run/aurora/broker-codex.sock in
	// production, or a private temporary socket in every test). Callers
	// must set DialConfig.BoundTool = producerprotocol.ToolCodex is applied
	// automatically by Producer; DialConfig.Clock must be set.
	DialConfig producerprotocol.Config

	Sources *SourceSet

	// PollInterval is how often /proc is rescanned and every tracked
	// instance's lease is renewed (a fresh, higher-revision report is sent
	// for each tracked instance every tick — see Machine.Renew).
	PollInterval time.Duration

	// LeaseDuration is how far into the future each report's
	// lease_expires_at is set from its observed_at. It must stay
	// comfortably under the broker's own -maximum-lease (2 minutes by
	// default) and comfortably above PollInterval so a short reconnect or a
	// missed tick or two does not let the lease lapse.
	LeaseDuration time.Duration

	// ReconnectMinDelay and ReconnectMaxDelay bound exponential backoff
	// between broker reconnect attempts.
	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration

	// PendingHookTTL bounds how long an unresolved hook delivery (see
	// Correlator) waits for a matching recognized process before being
	// dropped.
	PendingHookTTL time.Duration

	ProcRoot string
	Clock    Clock
	Stderr   io.Writer
}

func (config Config) validate() error {
	if config.Sources == nil {
		return fmt.Errorf("codex producer: sources must be configured")
	}
	if config.PollInterval <= 0 {
		return fmt.Errorf("codex producer: poll interval must be positive")
	}
	if config.LeaseDuration <= config.PollInterval {
		return fmt.Errorf("codex producer: lease duration must exceed poll interval")
	}
	if config.Clock == nil {
		return fmt.Errorf("codex producer: clock must be configured")
	}
	if config.ProcRoot == "" {
		return fmt.Errorf("codex producer: proc root must be configured")
	}
	return nil
}

// Producer is the standalone Codex presence shadow producer: it polls /proc
// for recognized Codex processes (Recognizer), derives idle/working/
// attention purely from observed hook events (Machine, fed via optional hook
// ingress deliveries resolved by Correlator), and reports normalized
// producerprotocol.Message values to the broker's Codex socket, treating the
// broker entirely as an external system (see doc.go).
type Producer struct {
	config     Config
	recognizer *Recognizer
	machine    *Machine
	correlator *Correlator
	epoch      producerprotocol.ProducerEpoch

	hookDeliveries chan HookDelivery
	stderr         io.Writer

	conn                 *producerprotocol.Conn
	nextReconnectAttempt time.Time
	reconnectDelay       time.Duration
}

// NewProducer builds a Producer. epoch must be freshly generated once per
// producer process (see NewProducerEpoch) — never reused across restarts.
func NewProducer(config Config, epoch producerprotocol.ProducerEpoch) (*Producer, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if err := epoch.Validate(); err != nil {
		return nil, fmt.Errorf("codex producer: %w", err)
	}
	recognizer, err := NewRecognizer(config.ProcRoot, config.Clock, config.Sources)
	if err != nil {
		return nil, err
	}
	stderr := config.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	return &Producer{
		config:         config,
		recognizer:     recognizer,
		machine:        NewMachine(),
		correlator:     NewCorrelator(config.PendingHookTTL),
		epoch:          epoch,
		hookDeliveries: make(chan HookDelivery, 256),
		stderr:         stderr,
		reconnectDelay: config.ReconnectMinDelay,
	}, nil
}

// DeliverHook is the callback IngressListener.Serve should call for every
// authenticated, decoded hook delivery. It never blocks indefinitely: if the
// internal channel is full (the main loop is unable to keep up), the
// delivery is dropped rather than stalling the ingress connection — shadow
// mode tolerates lost updates far better than it tolerates the shadow path
// ever affecting anything else.
func (producer *Producer) DeliverHook(delivery HookDelivery) {
	select {
	case producer.hookDeliveries <- delivery:
	default:
		fmt.Fprintln(producer.stderr, "codex producer: hook delivery channel full, dropping one delivery")
	}
}

// Run drives the producer until ctx is done: connecting to the broker,
// polling /proc, and applying hook deliveries, until cancellation. It always
// attempts one poll tick immediately on start (so a shadow run reports
// something without waiting a full PollInterval) and returns only after
// every in-flight operation has settled.
func (producer *Producer) Run(ctx context.Context) error {
	producer.tryConnect(ctx)
	producer.pollTick(ctx)

	ticker := time.NewTicker(producer.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if producer.conn != nil {
				_ = producer.conn.Close()
			}
			return nil
		case delivery := <-producer.hookDeliveries:
			producer.handleHookDelivery(delivery)
		case <-ticker.C:
			producer.pollTick(ctx)
		}
	}
}

func (producer *Producer) handleHookDelivery(delivery HookDelivery) {
	mapped, err := MapEffectiveState(delivery.Observation.EffectiveState)
	if err != nil {
		return
	}
	source, matched := producer.config.Sources.Match(delivery.EnvCodexHome)
	if !matched {
		// Unconfigured CODEX_HOME: ignore, per "processer från en
		// okonfigurerad CODEX_HOME ska ignoreras" — never guessed.
		return
	}
	session := SessionID(delivery.Observation.HookSessionRef)
	now := producer.config.Clock.Now()
	if id, resolved := producer.correlator.Deliver(session, source, mapped, delivery, now); resolved {
		producer.applyAndSend(id, source, mapped)
	}
	// Not yet resolved: queued inside Correlator, replayed once Reconcile
	// (the next pollTick) finds a matching recognized process.
}

func (producer *Producer) applyAndSend(id producerprotocol.InstanceID, source SourceLabel, mapped producerprotocol.State) {
	state, revision, changed := producer.machine.ApplyHookEvent(id, source, mapped)
	if changed {
		producer.send(id, state, revision)
	}
}

func (producer *Producer) pollTick(ctx context.Context) {
	producer.tryConnect(ctx)
	now := producer.config.Clock.Now()
	recognized, err := producer.recognizer.Observe(ctx)
	if err != nil {
		fmt.Fprintln(producer.stderr, "codex producer: observe:", classifyObserveError(err))
		return
	}

	seen := make(map[producerprotocol.InstanceID]struct{}, len(recognized))
	justSent := make(map[producerprotocol.InstanceID]struct{}, len(recognized))
	for _, instance := range recognized {
		seen[instance.InstanceID] = struct{}{}
		state, revision, isNew := producer.machine.Discover(instance.InstanceID, instance.Source)
		if isNew {
			producer.send(instance.InstanceID, state, revision)
			justSent[instance.InstanceID] = struct{}{}
		}
	}

	for _, resolvedSession := range producer.correlator.Reconcile(recognized, now) {
		seen[resolvedSession.InstanceID] = struct{}{}
		for _, mapped := range resolvedSession.States {
			state, revision, changed := producer.machine.ApplyHookEvent(resolvedSession.InstanceID, resolvedSession.Source, mapped)
			if changed {
				producer.send(resolvedSession.InstanceID, state, revision)
				justSent[resolvedSession.InstanceID] = struct{}{}
			}
		}
	}

	for _, tracked := range producer.machine.Snapshot() {
		if _, stillPresent := seen[tracked.InstanceID]; !stillPresent {
			producer.machine.Forget(tracked.InstanceID)
			producer.correlator.ForgetInstance(tracked.InstanceID)
		}
	}

	// Periodic full-state reconciliation / lease renewal: every instance
	// still tracked (and not already sent this tick) gets a fresh,
	// higher-revision report so a short reconnect or a dropped message
	// cannot leave it to expire early — see Machine.Renew's doc comment for
	// why this must bump revision even when State is unchanged.
	for _, tracked := range producer.machine.Snapshot() {
		if _, alreadySent := justSent[tracked.InstanceID]; alreadySent {
			continue
		}
		state, revision, ok := producer.machine.Renew(tracked.InstanceID)
		if ok {
			producer.send(tracked.InstanceID, state, revision)
		}
	}
}

// Transport retry contract (chosen explicitly; do not mix with any
// byte-for-byte replay model): this producer never retransmits a specific,
// previously-attempted producerprotocol.Message. Every call to send builds
// a brand-new message from Machine's current, authoritative
// (state, revision) pair and the clock's current instant — never a cached
// copy of an earlier attempt. There is no "last sent message" stored
// anywhere in this package.
//
// Why this is safe without knowing what a prior send actually did:
// WriteMessage's return value — success or error — is NOT reliable evidence
// of whether the broker's ingest layer actually decoded and applied that
// specific message. This is deliberately treated as an ambiguous outcome,
// not resolved by reasoning about framing internals: a stream write can
// itself have an ambiguous result (the peer received only part of a frame;
// or the peer received the complete frame and the connection broke before
// this producer's own call returned a clear success or failure), so an
// error here must never be read as proof the report was not applied, and a
// success must never be read as proof it was. See G.3's documented ack gap
// (handleProducerConnection's own doc comment in
// internal/presencebroker/listener.go) for the same point from the
// broker's side.
//
// Because the outcome is unknown either way, this producer does not try to
// resolve it — it just keeps making forward progress. The next natural
// trigger (a new hook-driven state change, or the next poll tick's
// unconditional per-instance renewal — see Machine.Renew) always
// constructs a fresh message with a strictly higher revision and the
// clock's then-current observed_at/lease_expires_at. That single property —
// revision strictly increasing, paired with a fresh payload every time — is
// what makes convergence certain regardless of the previous attempt's real
// outcome:
//
//   - if the previous, uncertain report was never applied, the broker
//     simply accepts the new, higher revision as the first update past
//     whatever it last held;
//   - if the previous, uncertain report was in fact applied, the broker
//     still accepts the new, higher revision as newer than it (internal/
//     instanceregistry only ever rejects a revision that is lower, or equal
//     with a different payload — never one that is strictly higher).
//
// Either way, this producer never needs to learn which of the two actually
// happened.
//
// A corollary this package deliberately relies on: a report's own lease
// never needs to be "renewed by resending the same lease" after a slow
// reconnect. If reconnecting takes long enough that the original
// lease_expires_at has already passed, the next send still just builds a
// fresh message off the current clock — it is structurally impossible for
// this producer to loop retrying an already-expired lease, because there is
// no retry loop at all, only ordinary forward progress.
func (producer *Producer) send(id producerprotocol.InstanceID, state producerprotocol.State, revision uint64) {
	now := producer.config.Clock.Now()
	msg := producerprotocol.Message{
		ProtocolVersion: producerprotocol.CurrentProtocolVersion,
		Tool:            producerprotocol.ToolCodex,
		InstanceID:      id,
		ProducerEpoch:   producer.epoch,
		State:           state,
		Revision:        producerprotocol.Revision(revision),
		ObservedAt:      now,
		LeaseExpiresAt:  now.Add(producer.config.LeaseDuration),
	}
	if producer.conn == nil {
		return // reconnect is attempted at the top of every tick; nothing to send to right now.
	}
	if err := producer.conn.WriteMessage(msg); err != nil {
		fmt.Fprintln(producer.stderr, "codex producer: send:", producerprotocol.ErrorCodeOf(err))
		_ = producer.conn.Close()
		producer.conn = nil
	}
}

func (producer *Producer) tryConnect(ctx context.Context) {
	if producer.conn != nil {
		return
	}
	now := producer.config.Clock.Now()
	if now.Before(producer.nextReconnectAttempt) {
		return
	}
	conn, err := producerprotocol.Dial(ctx, producer.config.DialConfig)
	if err != nil {
		producer.backoff(now)
		return
	}
	if err := conn.Bind(producerprotocol.ToolCodex); err != nil {
		_ = conn.Close()
		producer.backoff(now)
		return
	}
	producer.conn = conn
	producer.reconnectDelay = producer.config.ReconnectMinDelay
}

func (producer *Producer) backoff(now time.Time) {
	producer.nextReconnectAttempt = now.Add(producer.reconnectDelay)
	producer.reconnectDelay *= 2
	if producer.reconnectDelay > producer.config.ReconnectMaxDelay {
		producer.reconnectDelay = producer.config.ReconnectMaxDelay
	}
}

// Diagnostics reports content-free counts for optional operator visibility
// (never instance content, session identifiers, or CODEX_HOME paths): how
// many instances are currently tracked, and how many hook deliveries are
// still awaiting correlation.
func (producer *Producer) Diagnostics() (tracked int, pending int) {
	return len(producer.machine.Snapshot()), producer.correlator.PendingCount()
}

func classifyObserveError(err error) string {
	if err == nil {
		return ""
	}
	return "process observation failed"
}
