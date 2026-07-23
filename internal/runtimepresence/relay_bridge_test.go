package runtimepresence

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/relay"
	"github.com/swemonstro/aurora/internal/status"
)

type memorySource struct {
	mu        sync.Mutex
	instances []instancepresence.Instance
}

func (s *memorySource) ActiveInstances() []instancepresence.Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]instancepresence.Instance, len(s.instances))
	copy(out, s.instances)
	return out
}

func (s *memorySource) set(instances ...instancepresence.Instance) {
	s.mu.Lock()
	s.instances = append([]instancepresence.Instance{}, instances...)
	s.mu.Unlock()
}

type bridgePublisher struct {
	mu         sync.Mutex
	publishes  []presence.Snapshot
	removes    []string
	publishErr error
	removeErr  error
	// failNextNPublish fails the next N publish calls then succeeds.
	failNextNPublish int
	failNextNRemove  int
}

func (p *bridgePublisher) Publish(_ context.Context, snapshot presence.Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNextNPublish > 0 {
		p.failNextNPublish--
		if p.publishErr != nil {
			return p.publishErr
		}
		return errors.New("publish failed")
	}
	if p.publishErr != nil {
		return p.publishErr
	}
	p.publishes = append(p.publishes, snapshot)
	return nil
}

func (p *bridgePublisher) Remove(_ context.Context, source string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNextNRemove > 0 {
		p.failNextNRemove--
		if p.removeErr != nil {
			return p.removeErr
		}
		return errors.New("remove failed")
	}
	if p.removeErr != nil {
		return p.removeErr
	}
	p.removes = append(p.removes, source)
	return nil
}

func (p *bridgePublisher) snapshot() (publishes []presence.Snapshot, removes []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]presence.Snapshot{}, p.publishes...), append([]string{}, p.removes...)
}

func fixedClock(t time.Time) Clock {
	return func() time.Time { return t }
}

func testInstance(id string, tool instancepresence.ToolKind, state instancepresence.EffectiveState) instancepresence.Instance {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	return instancepresence.Instance{
		ID: instancepresence.InstanceID(id), Tool: tool,
		Source: instancepresence.SourceDescriptor{Provider: "linux-runtime", Profile: "default", CollectorID: "test"},
		Runtime: instancepresence.RuntimeIdentity{
			HostID: "host-a", BootID: "boot-a",
			RootProcess: instancepresence.ProcessIdentity{PID: 1, StartedAt: now.Add(-time.Hour)},
		},
		Status: instancepresence.RuntimeAlive, State: state,
		Slot: instancepresence.Slot{Namespace: "default", Index: 0, AssignedAt: now},
		Lifecycle: instancepresence.LifecycleTimestamps{
			DiscoveredAt: now, LastSeenAt: now, LeaseExpiresAt: now.Add(time.Minute), StateChangedAt: now,
		},
		Revisions: instancepresence.Revisions{ProducerEpoch: "epoch-a", RuntimeRevision: 1},
	}
}

func TestAggregatePriority(t *testing.T) {
	tests := []struct {
		name string
		in   []instancepresence.EffectiveState
		want status.State
	}{
		{"idle", []instancepresence.EffectiveState{instancepresence.StateIdle}, status.Idle},
		{"idle+working", []instancepresence.EffectiveState{instancepresence.StateIdle, instancepresence.StateWorking}, status.Working},
		{"working+attention+idle", []instancepresence.EffectiveState{
			instancepresence.StateWorking, instancepresence.StateAttention, instancepresence.StateIdle,
		}, status.Attention},
		{"error beats attention", []instancepresence.EffectiveState{
			instancepresence.StateAttention, instancepresence.StateError,
		}, status.Error},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var instances []instancepresence.Instance
			for i, state := range test.in {
				instances = append(instances, testInstance("i"+string(rune('a'+i)), instancepresence.ToolClaude, state))
			}
			got := aggregateByTool(instances)[instancepresence.ToolClaude]
			if got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}

func TestBridgeIdleClaudePublishes(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{}
	now := time.Date(2026, 7, 23, 15, 0, 0, 0, time.UTC)
	bridge, err := NewRelayBridge(src, pub, fixedClock(now), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(testInstance("a", instancepresence.ToolClaude, instancepresence.StateIdle))
	bridge.Reconcile(context.Background())
	publishes, removes := pub.snapshot()
	// Absent tools are removed each reconcile (safe for empty relay).
	for _, source := range removes {
		if source == SourceClaudeRuntime {
			t.Fatalf("claude removed while active: %#v", removes)
		}
	}
	if len(publishes) != 1 {
		t.Fatalf("publishes = %#v", publishes)
	}
	if publishes[0].Source != SourceClaudeRuntime || publishes[0].State != status.Idle {
		t.Fatalf("snapshot = %#v", publishes[0])
	}
	if publishes[0].Version != presence.ProtocolVersion {
		t.Fatalf("version = %d", publishes[0].Version)
	}
	if !publishes[0].Timestamp.Equal(now.UTC()) {
		t.Fatalf("timestamp = %s", publishes[0].Timestamp)
	}
}

func TestBridgeAggregationAndDemotion(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(
		testInstance("a", instancepresence.ToolClaude, instancepresence.StateIdle),
		testInstance("b", instancepresence.ToolClaude, instancepresence.StateWorking),
	)
	bridge.Reconcile(context.Background())
	publishes, _ := pub.snapshot()
	if publishes[len(publishes)-1].State != status.Working {
		t.Fatalf("want working, got %#v", publishes[len(publishes)-1])
	}

	src.set(
		testInstance("a", instancepresence.ToolClaude, instancepresence.StateWorking),
		testInstance("b", instancepresence.ToolClaude, instancepresence.StateAttention),
		testInstance("c", instancepresence.ToolClaude, instancepresence.StateIdle),
	)
	bridge.Reconcile(context.Background())
	publishes, _ = pub.snapshot()
	if publishes[len(publishes)-1].State != status.Attention {
		t.Fatalf("want attention, got %#v", publishes[len(publishes)-1])
	}

	// Highest priority disappears → demote to working.
	src.set(
		testInstance("a", instancepresence.ToolClaude, instancepresence.StateWorking),
		testInstance("c", instancepresence.ToolClaude, instancepresence.StateIdle),
	)
	bridge.Reconcile(context.Background())
	publishes, _ = pub.snapshot()
	if publishes[len(publishes)-1].State != status.Working {
		t.Fatalf("want demoted working, got %#v", publishes[len(publishes)-1])
	}
}

func TestBridgeLastInstanceRemovesSource(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(testInstance("a", instancepresence.ToolClaude, instancepresence.StateWorking))
	bridge.Reconcile(context.Background())
	src.set()
	bridge.Reconcile(context.Background())
	_, removes := pub.snapshot()
	foundClaudeRemove := false
	for _, source := range removes {
		if source == SourceClaudeRuntime {
			foundClaudeRemove = true
		}
	}
	if !foundClaudeRemove {
		t.Fatalf("removes = %#v, want claude-runtime", removes)
	}
	if _, present := bridge.LastPublished(SourceClaudeRuntime); present {
		t.Fatal("claude still marked published")
	}
}

func TestBridgeClaudeAndCodexSeparateSources(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(
		testInstance("c", instancepresence.ToolClaude, instancepresence.StateWorking),
		testInstance("x", instancepresence.ToolCodex, instancepresence.StateAttention),
	)
	bridge.Reconcile(context.Background())
	publishes, _ := pub.snapshot()
	bySource := map[string]status.State{}
	for _, snap := range publishes {
		bySource[snap.Source] = snap.State
	}
	if bySource[SourceClaudeRuntime] != status.Working || bySource[SourceCodexRuntime] != status.Attention {
		t.Fatalf("bySource = %#v", bySource)
	}
}

func TestBridgeFeedsRelayStoreAggregate(t *testing.T) {
	src := &memorySource{}
	// Use a publisher that also writes into relay.Store.
	store := &relay.Store{}
	pub := &storePublisher{store: store}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(
		testInstance("c", instancepresence.ToolClaude, instancepresence.StateIdle),
		testInstance("x", instancepresence.ToolCodex, instancepresence.StateWorking),
	)
	bridge.Reconcile(context.Background())
	latest, ok := store.Latest()
	if !ok {
		t.Fatal("no aggregate")
	}
	// Two sources: idle + working → aggregate working (same priority table as bridge).
	if latest.State != status.Working {
		t.Fatalf("aggregate state = %q, want working", latest.State)
	}
	if latest.Source != "aurora-aggregate" && latest.Source != SourceCodexRuntime {
		// With two sources Latest returns aggregate; with equal counts still aggregate.
		t.Fatalf("aggregate source = %q", latest.Source)
	}
}

type storePublisher struct {
	store *relay.Store
}

func (p *storePublisher) Publish(_ context.Context, snapshot presence.Snapshot) error {
	p.store.Set(snapshot)
	return nil
}

func (p *storePublisher) Remove(_ context.Context, source string) error {
	p.store.Remove(source)
	return nil
}

func TestBridgePublishFailureRetried(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{failNextNPublish: 1}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(testInstance("a", instancepresence.ToolClaude, instancepresence.StateIdle))
	bridge.Reconcile(context.Background())
	if _, present := bridge.LastPublished(SourceClaudeRuntime); present {
		t.Fatal("failed publish must not mark published")
	}
	publishes, _ := pub.snapshot()
	if len(publishes) != 0 {
		t.Fatalf("publishes after fail = %#v", publishes)
	}
	bridge.Reconcile(context.Background())
	if state, present := bridge.LastPublished(SourceClaudeRuntime); !present || state != status.Idle {
		t.Fatalf("retry state present=%v state=%q", present, state)
	}
	publishes, _ = pub.snapshot()
	if len(publishes) != 1 {
		t.Fatalf("publishes = %#v", publishes)
	}
}

func TestBridgeRemoveFailureRetried(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(testInstance("a", instancepresence.ToolClaude, instancepresence.StateIdle))
	bridge.Reconcile(context.Background())
	src.set()
	pub.failNextNRemove = 1
	bridge.Reconcile(context.Background())
	if _, present := bridge.LastPublished(SourceClaudeRuntime); !present {
		t.Fatal("failed remove must keep previous published mark for retry semantics")
	}
	// Actually: on remove failure we should NOT mark absent. lastOK still present=true.
	// Next reconcile retries remove.
	pub.failNextNRemove = 0
	bridge.Reconcile(context.Background())
	if _, present := bridge.LastPublished(SourceClaudeRuntime); present {
		t.Fatal("still published after successful remove")
	}
	_, removes := pub.snapshot()
	if len(removes) < 2 {
		// one failed attempt may still append on success only - our recording only appends on success
		// first remove failed so only one successful remove
		if len(removes) != 1 {
			t.Fatalf("removes = %#v", removes)
		}
	}
}

func TestBridgePeriodicRepublishRestoresRelay(t *testing.T) {
	src := &memorySource{}
	store := &relay.Store{}
	pub := &storePublisher{store: store}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(testInstance("a", instancepresence.ToolClaude, instancepresence.StateWorking))
	bridge.Reconcile(context.Background())
	// Simulate relay restart: empty in-memory store.
	store.Remove(SourceClaudeRuntime)
	if _, ok := store.Latest(); ok {
		t.Fatal("store should be empty after simulated restart")
	}
	// Unchanged active state still republished on reconcile.
	bridge.Reconcile(context.Background())
	latest, ok := store.Latest()
	if !ok || latest.Source != SourceClaudeRuntime || latest.State != status.Working {
		t.Fatalf("restored = %#v ok=%t", latest, ok)
	}
}

func TestBridgePayloadContentFree(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.set(testInstance("session-secret", instancepresence.ToolClaude, instancepresence.StateWorking))
	bridge.Reconcile(context.Background())
	publishes, _ := pub.snapshot()
	if len(publishes) != 1 {
		t.Fatal(publishes)
	}
	// Snapshot only has version/source/state/timestamp — no content fields.
	snap := publishes[0]
	if snap.Source == "" || snap.State == "" {
		t.Fatalf("incomplete %#v", snap)
	}
	// Instance id used in test must not leak into source name.
	if strings.Contains(snap.Source, "session") {
		t.Fatalf("source leaked content: %q", snap.Source)
	}
}

func TestBridgeErrorsDoNotPanicRegistryReads(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{publishErr: errors.New("relay down")}
	var stderr strings.Builder
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Hour, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	src.set(testInstance("a", instancepresence.ToolClaude, instancepresence.StateError))
	bridge.Reconcile(context.Background())
	// Source still readable after failed publish.
	if len(src.ActiveInstances()) != 1 {
		t.Fatal("source corrupted")
	}
	if !strings.Contains(stderr.String(), "relay bridge publish") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBridgeRaceConcurrentMutationsAndReconcile(t *testing.T) {
	src := &memorySource{}
	pub := &bridgePublisher{}
	bridge, err := NewRelayBridge(src, pub, time.Now, time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		bridge.Run(ctx)
	}()
	go func() {
		defer wait.Done()
		for i := 0; i < 50; i++ {
			src.set(
				testInstance("a", instancepresence.ToolClaude, instancepresence.StateWorking),
				testInstance("b", instancepresence.ToolClaude, instancepresence.StateAttention),
			)
			src.set(testInstance("a", instancepresence.ToolClaude, instancepresence.StateIdle))
			src.set()
			src.set(testInstance("x", instancepresence.ToolCodex, instancepresence.StateError))
		}
		cancel()
	}()
	wait.Wait()
	// Final reconcile after cancel is not required; ensure no panic and publisher usable.
	bridge.Reconcile(context.Background())
}

func TestNewRelayBridgeValidates(t *testing.T) {
	if _, err := NewRelayBridge(nil, &bridgePublisher{}, time.Now, 0, nil); err == nil {
		t.Fatal("nil source")
	}
	if _, err := NewRelayBridge(&memorySource{}, nil, time.Now, 0, nil); err == nil {
		t.Fatal("nil publisher")
	}
	if _, err := NewRelayBridge(&memorySource{}, &bridgePublisher{}, nil, 0, nil); err == nil {
		t.Fatal("nil clock")
	}
}
