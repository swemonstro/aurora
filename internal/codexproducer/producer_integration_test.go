//go:build linux

package codexproducer

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencebroker"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// testBroker is a minimal, real, Codex-only broker (registry + ingestor +
// producerprotocol listener), built from the same generic packages
// cmd/aurora-presence-broker composes — never a mock of the wire protocol —
// so these tests prove this producer's messages are actually accepted by
// the real broker stack, not just internally self-consistent.
type testBroker struct {
	registry   *instanceregistry.Registry
	socketPath string
}

// mutableClock is a manually-advanced clock shared between a testBroker's
// registry and a Producer, so tests can deterministically prove behavior
// that depends on real elapsed time (lease expiry, timestamp freshness)
// without sleeping or racing a real wall clock. It is safe for concurrent
// use: the broker's own background goroutine (RunProducerListener, and
// transitively the registry) calls Now() concurrently with the test
// goroutine advancing it.
type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *mutableClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *mutableClock) Advance(delta time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(delta)
}

func (clock *mutableClock) Set(value time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = value
}

func newTestBroker(t *testing.T) *testBroker {
	t.Helper()
	return newTestBrokerWithClock(t, systemClockForTest{})
}

func newTestBrokerWithClock(t *testing.T, clock producerprotocol.Clock) *testBroker {
	t.Helper()
	// producerprotocol's secure-socket validation rejects world-writable or
	// symlinked ancestor directories, which /tmp sometimes is; a directory
	// under the user's own home mirrors cmd/aurora-presence-broker's own
	// test helper (secureTempDir) for the same reason.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-codexproducer-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default",
		LeaseDuration:                time.Minute,
		GracePeriod:                  10 * time.Second,
		MaximumProducerLeaseDuration: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ingestor, err := presencebroker.NewIngestor(registry, "host-fixture", "codexproducer-test")
	if err != nil {
		t.Fatal(err)
	}

	socketPath := filepath.Join(directory, "codex.sock")
	config := producerprotocol.DefaultConfig(clock)
	config.SocketPath = socketPath
	config.BoundTool = producerprotocol.ToolCodex
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}

	authenticator := producerprotocol.SameUIDAuthenticator{ServerUID: uint32(os.Geteuid())}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		presencebroker.RunProducerListener(ctx, listener, authenticator, ingestor, io.Discard)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = listener.Close()
	})

	return &testBroker{registry: registry, socketPath: socketPath}
}

func waitForRegistryState(t *testing.T, registry *instanceregistry.Registry, id producerprotocol.InstanceID, wantState instancepresence.EffectiveState, wantRevision uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := registry.Get(instancepresence.InstanceID(id))
		if err == nil && inst.State == wantState && uint64(inst.Revisions.RuntimeRevision) == wantRevision {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("instance %q did not converge to state=%s revision=%d before deadline", id, wantState, wantRevision)
}

func newTestProducer(t *testing.T, broker *testBroker, epoch producerprotocol.ProducerEpoch) *Producer {
	t.Helper()
	return newTestProducerWithClock(t, broker, epoch, systemClockForTest{})
}

func newTestProducerWithClock(t *testing.T, broker *testBroker, epoch producerprotocol.ProducerEpoch, clock Clock) *Producer {
	t.Helper()
	sources := testSources(t, "", SourceEntry{Label: "business", Path: testCodexHomeDir(t, "business")})
	dialConfig := producerprotocol.DefaultConfig(clock)
	dialConfig.SocketPath = broker.socketPath
	dialConfig.BoundTool = producerprotocol.ToolCodex

	producer, err := NewProducer(Config{
		DialConfig:        dialConfig,
		Sources:           sources,
		PollInterval:      time.Second,
		LeaseDuration:     5 * time.Second,
		ReconnectMinDelay: 5 * time.Millisecond,
		ReconnectMaxDelay: 20 * time.Millisecond,
		PendingHookTTL:    time.Second,
		ProcRoot:          "/proc",
		Clock:             clock,
		Stderr:            io.Discard,
	}, epoch)
	if err != nil {
		t.Fatal(err)
	}
	return producer
}

func TestProducer_EndToEndReportsToRealBroker(t *testing.T) {
	broker := newTestBroker(t)
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	producer := newTestProducer(t, broker, epoch)

	ctx := context.Background()
	producer.tryConnect(ctx)
	if producer.conn == nil {
		t.Fatal("expected producer to connect to the broker")
	}

	instanceID := DeriveInstanceID("business", 999, time.Now().UTC())
	state, revision, _ := producer.machine.Discover(instanceID, "business")
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateIdle, 1)

	state, revision, changed := producer.machine.ApplyHookEvent(instanceID, "business", producerprotocol.StateWorking)
	if !changed {
		t.Fatal("expected a real transition to working")
	}
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateWorking, 2)

	state, revision, changed = producer.machine.ApplyHookEvent(instanceID, "business", producerprotocol.StateAttention)
	if !changed {
		t.Fatal("expected a real transition to attention")
	}
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateAttention, 3)
}

func TestProducer_LeaseRenewalAdvancesLeaseExpiry(t *testing.T) {
	broker := newTestBroker(t)
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	producer := newTestProducer(t, broker, epoch)
	ctx := context.Background()
	producer.tryConnect(ctx)

	instanceID := DeriveInstanceID("business", 1000, time.Now().UTC())
	state, revision, _ := producer.machine.Discover(instanceID, "business")
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateIdle, 1)

	first, err := broker.registry.Get(instancepresence.InstanceID(instanceID))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(20 * time.Millisecond)
	state, revision, ok := producer.machine.Renew(instanceID)
	if !ok {
		t.Fatal("expected Renew to succeed for a tracked instance")
	}
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateIdle, 2)

	second, err := broker.registry.Get(instancepresence.InstanceID(instanceID))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Lifecycle.LeaseExpiresAt.After(first.Lifecycle.LeaseExpiresAt) {
		t.Fatalf("expected lease renewal to advance lease_expires_at: first=%v second=%v", first.Lifecycle.LeaseExpiresAt, second.Lifecycle.LeaseExpiresAt)
	}
}

func TestProducer_ReconnectAfterConnectionDrop(t *testing.T) {
	broker := newTestBroker(t)
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	producer := newTestProducer(t, broker, epoch)
	ctx := context.Background()
	producer.tryConnect(ctx)
	if producer.conn == nil {
		t.Fatal("expected initial connect to succeed")
	}

	instanceID := DeriveInstanceID("business", 1001, time.Now().UTC())
	state, revision, _ := producer.machine.Discover(instanceID, "business")
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateIdle, 1)

	// Simulate a dropped connection (network blip): close the underlying
	// conn out from under the producer without going through send's own
	// error path.
	_ = producer.conn.Close()
	producer.conn = nil

	deadline := time.Now().Add(2 * time.Second)
	for producer.conn == nil && time.Now().Before(deadline) {
		producer.tryConnect(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	if producer.conn == nil {
		t.Fatal("expected producer to reconnect after a dropped connection")
	}

	// Full-state reconciliation after reconnect: renew must still work and
	// reach the broker on the new connection.
	state, revision, ok := producer.machine.Renew(instanceID)
	if !ok {
		t.Fatal("expected Renew to succeed")
	}
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateIdle, 2)
}

func TestProducer_RestartUsesNewEpochAndTakesOverAfterOldConnectionCloses(t *testing.T) {
	broker := newTestBroker(t)
	firstEpoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	first := newTestProducer(t, broker, firstEpoch)
	ctx := context.Background()
	first.tryConnect(ctx)

	instanceID := DeriveInstanceID("business", 1002, time.Now().UTC())
	state, revision, _ := first.machine.Discover(instanceID, "business")
	first.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateIdle, 1)

	// "Crash" the first producer: close its connection so the broker's
	// per-connection session releases its live generation claim.
	_ = first.conn.Close()

	secondEpoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if secondEpoch == firstEpoch {
		t.Fatal("producer restart must generate a new epoch")
	}
	second := newTestProducer(t, broker, secondEpoch)
	second.tryConnect(ctx)
	if second.conn == nil {
		t.Fatal("expected second producer to connect")
	}

	// The same underlying instance id (derived purely from source+PID+start
	// time — see DeriveInstanceID) is re-established under the new epoch.
	// The broker may reject the very first attempt if it has not yet
	// noticed the first connection's close; retry briefly, exactly like
	// cmd/aurora-presence-broker's own retryUntilInstanceState tests do.
	deadline := time.Now().Add(2 * time.Second)
	msg := producerprotocol.Message{
		ProtocolVersion: producerprotocol.CurrentProtocolVersion,
		Tool:            producerprotocol.ToolCodex,
		InstanceID:      instanceID,
		ProducerEpoch:   secondEpoch,
		State:           producerprotocol.StateIdle,
		Revision:        1,
		ObservedAt:      time.Now().UTC(),
		LeaseExpiresAt:  time.Now().UTC().Add(5 * time.Second),
	}
	for time.Now().Before(deadline) {
		if err := second.conn.WriteMessage(msg); err != nil {
			t.Fatal(err)
		}
		inst, getErr := broker.registry.Get(instancepresence.InstanceID(instanceID))
		if getErr == nil && inst.Revisions.ProducerEpoch == instancepresence.ProducerEpoch(secondEpoch) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("second producer never took over the instance under its new epoch")
}

// TestProducer_ReconnectAfterInterruptionUsesFreshRevisionAndTimestamps is
// the direct regression test for the G.4 ultrareview requirement to prove,
// with an injected (not wall-clock) time source, that a transport
// interruption followed by reconnect never resends a stale revision with
// fresh timestamps and never loses a state change that happened while
// disconnected. It also proves the reverse hazard does not occur: the
// resumed report never reuses the original (by-then long-expired) lease
// window — see Machine.Renew's doc comment on why a lease renewal must
// always bump revision.
func TestProducer_ReconnectAfterInterruptionUsesFreshRevisionAndTimestamps(t *testing.T) {
	// Seeded from real wall-clock time, not an arbitrary fixed date: the
	// underlying producerprotocol.Conn still uses this same clock to
	// compute *real* OS-level read/write socket deadlines (SetWriteDeadline
	// etc.), so its starting point must track actual wall time or those
	// deadlines could already be in the past the moment they are set. Only
	// the deliberate advance below (simulating slow-reconnect elapsed time)
	// needs to be fake; the baseline must not be.
	clock := &mutableClock{now: time.Now().UTC()}
	broker := newTestBrokerWithClock(t, clock)
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	producer := newTestProducerWithClock(t, broker, epoch, clock)
	ctx := context.Background()
	producer.tryConnect(ctx)
	if producer.conn == nil {
		t.Fatal("expected initial connect to succeed")
	}

	instanceID := DeriveInstanceID("business", 2000, clock.Now())
	state, revision, _ := producer.machine.Discover(instanceID, "business")
	producer.send(instanceID, state, revision)
	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateIdle, 1)

	first, err := broker.registry.Get(instancepresence.InstanceID(instanceID))
	if err != nil {
		t.Fatal(err)
	}
	originalLeaseExpiry := first.Lifecycle.LeaseExpiresAt

	// Simulate a transport interruption: the connection drops.
	_ = producer.conn.Close()
	producer.conn = nil

	// While disconnected, a real state change happens (e.g. a hook event
	// resolved) — Machine records it immediately (revision bumped to 2),
	// but send() is a deliberate no-op while conn is nil: the report is not
	// lost, it is simply not deliverable yet.
	state, revision, changed := producer.machine.ApplyHookEvent(instanceID, "business", producerprotocol.StateWorking)
	if !changed || revision != 2 {
		t.Fatalf("expected the disconnected state change to reach revision 2, got state=%v revision=%d changed=%v", state, revision, changed)
	}
	producer.send(instanceID, state, revision) // no-op: producer.conn is nil.

	// Advance the clock well past the ORIGINAL lease window, simulating a
	// slow reconnect. If the producer ever resent the stale revision-1
	// report with new timestamps, or resent revision 2 with the ORIGINAL
	// (by-now-expired) observed_at/lease_expires_at, the broker's own
	// ObservedAt/LeaseExpiresAt bookkeeping would betray it below.
	clock.Advance(30 * time.Second)
	if !clock.Now().After(originalLeaseExpiry) {
		t.Fatal("test setup error: clock did not actually advance past the original lease")
	}

	// Reconnect.
	deadline := time.Now().Add(2 * time.Second)
	for producer.conn == nil && time.Now().Before(deadline) {
		producer.tryConnect(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	if producer.conn == nil {
		t.Fatal("expected producer to reconnect")
	}
	// Trigger the same renewal step a real poll tick performs for every
	// still-tracked instance (see Producer.pollTick's final loop) — not a
	// manual replay of the earlier failed send, which correctly bumps
	// revision again (3): a lease renewal always advances revision even
	// when State is unchanged (see Machine.Renew's doc comment), and here
	// it also carries forward the working state set while disconnected.
	// (Producer.pollTick itself is not used directly here because it would
	// also re-run real /proc recognition, which does not recognize this
	// hand-constructed instance and would incorrectly conclude it has
	// disappeared — a test-fixture limitation, not a production concern.)
	state, revision, ok := producer.machine.Renew(instanceID)
	if !ok {
		t.Fatal("expected Renew to succeed for a tracked instance")
	}
	producer.send(instanceID, state, revision)

	waitForRegistryState(t, broker.registry, instanceID, instancepresence.StateWorking, 3)
	second, err := broker.registry.Get(instancepresence.InstanceID(instanceID))
	if err != nil {
		t.Fatal(err)
	}

	if second.Revisions.RuntimeRevision <= first.Revisions.RuntimeRevision {
		t.Fatalf("revision must strictly increase across reconnect: first=%d second=%d", first.Revisions.RuntimeRevision, second.Revisions.RuntimeRevision)
	}
	if !second.Lifecycle.LastSeenAt.After(originalLeaseExpiry) {
		t.Fatalf("resumed report must carry a fresh observed time from the advanced clock, not a stale one: LastSeenAt=%v, original lease expiry=%v", second.Lifecycle.LastSeenAt, originalLeaseExpiry)
	}
	if !second.Lifecycle.LeaseExpiresAt.After(originalLeaseExpiry) {
		t.Fatalf("resumed report's lease must extend from the advanced clock, not reuse the original (already expired) lease window: new=%v, original=%v", second.Lifecycle.LeaseExpiresAt, originalLeaseExpiry)
	}
	if second.State != instancepresence.StateWorking {
		t.Fatalf("the state change made while disconnected must not be lost: got %v, want working", second.State)
	}
}
