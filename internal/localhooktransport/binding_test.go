package localhooktransport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
	"github.com/swemonstro/aurora/internal/sessionbinding"
)

type fakeLinker struct {
	mu      sync.Mutex
	results map[string]RuntimeLinkResult // session not available; key by tool+pid
	byCall  []RuntimeLinkResult
	index   int
	delay   time.Duration
}

func (linker *fakeLinker) Link(ctx context.Context, capture IdentityPeerCapture, tool instancepresence.ToolKind) RuntimeLinkResult {
	if linker.delay > 0 {
		select {
		case <-time.After(linker.delay):
		case <-ctx.Done():
			return RuntimeLinkResult{TimedOut: true, Reasons: []string{"attestation_timeout"}}
		}
	}
	linker.mu.Lock()
	defer linker.mu.Unlock()
	if linker.index < len(linker.byCall) {
		result := linker.byCall[linker.index]
		linker.index++
		return result
	}
	if linker.results != nil {
		key := string(tool)
		if result, ok := linker.results[key]; ok {
			return result
		}
	}
	_ = capture
	return RuntimeLinkResult{Reasons: []string{"no_unique_runtime_link"}}
}

type bindingFixture struct {
	t        *testing.T
	clock    *testClock
	registry *instanceregistry.Registry
	sessions *sessionbinding.Registry
	linker   *fakeLinker
	receiver *IngestReceiver
	epoch    instancepresence.ProducerEpoch
}

func newBindingFixture(t *testing.T) *bindingFixture {
	t.Helper()
	clock := &testClock{now: testTime}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default", LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewIngestReceiverWithEpoch(DefaultIngestServerConfig(clock), epoch)
	if err != nil {
		t.Fatal(err)
	}
	linker := &fakeLinker{results: map[string]RuntimeLinkResult{}}
	sessions := sessionbinding.New()
	engine, err := NewBindingEngine(sessions, linker, registry)
	if err != nil {
		t.Fatal(err)
	}
	receiver.SetBinder(engine)
	return &bindingFixture{
		t: t, clock: clock, registry: registry, sessions: sessions,
		linker: linker, receiver: receiver, epoch: epoch,
	}
}

func (f *bindingFixture) registerRuntime(id instancepresence.InstanceID, pid uint64, start time.Time) {
	f.t.Helper()
	_, err := f.registry.Register(instanceregistry.Registration{
		InstanceID: id, Tool: instancepresence.ToolClaude,
		Source: instancepresence.SourceDescriptor{Provider: "claude", Profile: "default", CollectorID: "runtime-agent"},
		Runtime: instancepresence.RuntimeIdentity{
			HostID: "host-a", BootID: "boot-a",
			RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
		},
		ProducerEpoch: f.epoch, RuntimeRevision: 1, ObservedAt: testTime,
		IdempotencyKey: "reg-" + string(id),
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *bindingFixture) handle(session string, lifecycle instancecorrelation.Lifecycle, state instancepresence.EffectiveState, requestID string, capture IdentityPeerCapture, link RuntimeLinkResult) IngestResponse {
	f.t.Helper()
	f.linker.byCall = append(f.linker.byCall[:0], link)
	f.linker.index = 0
	req := IngestRequest{
		ProtocolVersion: IngestProtocolVersion,
		Operation:       OperationIngestHookEvent,
		RequestID:       requestID,
		Payload: IngressPayload{
			Tool: instancepresence.ToolClaude, HookSessionRef: instancepresence.OpaqueIdentity(session),
			Lifecycle: lifecycle, State: state,
		},
	}
	return f.receiver.HandleWithCapture(context.Background(), req, capture)
}

func peerCapture(pid uint64, start time.Time) IdentityPeerCapture {
	return IdentityPeerCapture{
		PeerUID: 1000, PeerPID: int32(pid), GenerationOK: true,
		GenerationPID: pid, GenerationStarted: start,
		Ancestry: []IdentityAncestryHop{{PID: pid, StartedAt: start, IsPeer: true}},
	}
}

func TestBindingTwoConcurrentClaudeSessionsIndependentState(t *testing.T) {
	f := newBindingFixture(t)
	startA := testTime.Add(-time.Hour)
	startB := testTime.Add(-2 * time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	idB := instancepresence.InstanceID("runtime-b")
	f.registerRuntime(idA, 100, startA)
	f.registerRuntime(idB, 200, startB)

	capA := peerCapture(100, startA)
	capB := peerCapture(200, startB)
	linkA := RuntimeLinkResult{RuntimeRef: idA, Unique: true, Rule: "L1_root", Reasons: []string{"unique_runtime_link"}}
	linkB := RuntimeLinkResult{RuntimeRef: idB, Unique: true, Rule: "L1_root", Reasons: []string{"unique_runtime_link"}}

	// Session A working
	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-a1", capA, linkA)
	if resp.Status != StatusOK || resp.NoBindingPerformed {
		t.Fatalf("A working = %#v", resp)
	}
	// Session B attention
	resp = f.handle("session-b", instancecorrelation.LifecycleActive, instancepresence.StateAttention, "req-b1", capB, linkB)
	if resp.Status != StatusOK || resp.NoBindingPerformed {
		t.Fatalf("B attention = %#v", resp)
	}

	instA, err := f.registry.Get(idA)
	if err != nil || instA.State != instancepresence.StateWorking {
		t.Fatalf("A state = %#v err=%v", instA, err)
	}
	instB, err := f.registry.Get(idB)
	if err != nil || instB.State != instancepresence.StateAttention {
		t.Fatalf("B state = %#v err=%v", instB, err)
	}

	// A idle leaves B unchanged
	resp = f.handle("session-a", instancecorrelation.LifecycleIdle, instancepresence.StateIdle, "req-a2", capA, linkA)
	if resp.Status != StatusOK || resp.NoBindingPerformed {
		t.Fatalf("A idle = %#v", resp)
	}
	instA, _ = f.registry.Get(idA)
	instB, _ = f.registry.Get(idB)
	if instA.State != instancepresence.StateIdle {
		t.Fatalf("A after idle = %q", instA.State)
	}
	if instB.State != instancepresence.StateAttention {
		t.Fatalf("B changed after A idle: %q", instB.State)
	}

	// A ended removes only A's binding
	resp = f.handle("session-a", instancecorrelation.LifecycleEnded, "", "req-a3", capA, linkA)
	if resp.Status != StatusOK || resp.NoBindingPerformed {
		t.Fatalf("A ended = %#v", resp)
	}
	if _, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-a"); ok {
		t.Fatal("A session binding still present")
	}
	if ref, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-b"); !ok || ref != idB {
		t.Fatalf("B binding = %q ok=%t", ref, ok)
	}
	// Runtime A still registered (not ended by session end)
	if inst, err := f.registry.Get(idA); err != nil || !inst.Status.Active() {
		t.Fatalf("runtime A should remain active: %#v err=%v", inst, err)
	}
}

func TestBindingRejectsZeroAndAmbiguousLinks(t *testing.T) {
	f := newBindingFixture(t)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, testTime)
	cap := peerCapture(100, testTime)

	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-0", cap,
		RuntimeLinkResult{Reasons: []string{"no_unique_runtime_link"}})
	if resp.Status != StatusRejected || !hasIngestCode(resp, CodeNoUniqueRuntimeLink) || !resp.NoBindingPerformed {
		t.Fatalf("zero links = %#v", resp)
	}

	resp = f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-2", cap,
		RuntimeLinkResult{Reasons: []string{"ambiguous_runtime_link"}})
	if resp.Status != StatusRejected || !hasIngestCode(resp, CodeAmbiguousRuntimeLink) || !resp.NoBindingPerformed {
		t.Fatalf("ambiguous = %#v", resp)
	}
}

func TestBindingTimeoutDuringIdentityMeasure(t *testing.T) {
	f := newBindingFixture(t)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, testTime)
	f.linker.delay = 50 * time.Millisecond
	config := DefaultIngestServerConfig(f.clock)
	config.MaximumHandlingTime = 5 * time.Millisecond
	receiver, err := NewIngestReceiverWithEpoch(config, f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewBindingEngine(f.sessions, f.linker, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	receiver.SetBinder(engine)
	f.receiver = receiver

	f.linker.byCall = []RuntimeLinkResult{{RuntimeRef: idA, Unique: true}}
	f.linker.index = 0
	req := testIngestRequest("timeout-bind", instancepresence.ToolClaude, "session-a", instancecorrelation.LifecycleActive)
	resp := receiver.HandleWithCapture(context.Background(), req, peerCapture(100, testTime))
	if resp.Status != StatusError || !hasIngestCode(resp, CodeHandlerTimeout) || !resp.NoBindingPerformed {
		t.Fatalf("timeout = %#v", resp)
	}
}

func TestBindingRuntimeNotRegistered(t *testing.T) {
	f := newBindingFixture(t)
	cap := peerCapture(100, testTime)
	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-nr", cap,
		RuntimeLinkResult{RuntimeRef: "missing-runtime", Unique: true, Rule: "L1_root"})
	if resp.Status != StatusRejected || !hasIngestCode(resp, CodeRuntimeNotRegistered) || !resp.NoBindingPerformed {
		t.Fatalf("not registered = %#v", resp)
	}
	if f.sessions.Len() != 0 || f.sessions.ReservedLen() != 0 {
		t.Fatalf("runtime_not_registered left binding: committed=%d reserved=%d", f.sessions.Len(), f.sessions.ReservedLen())
	}
}

func TestBindingSessionConflictOnDifferentRuntime(t *testing.T) {
	f := newBindingFixture(t)
	startA := testTime.Add(-time.Hour)
	startB := testTime.Add(-2 * time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	idB := instancepresence.InstanceID("runtime-b")
	f.registerRuntime(idA, 100, startA)
	f.registerRuntime(idB, 200, startB)
	capA := peerCapture(100, startA)
	linkA := RuntimeLinkResult{RuntimeRef: idA, Unique: true, Rule: "L1_root"}
	linkB := RuntimeLinkResult{RuntimeRef: idB, Unique: true, Rule: "L1_root"}

	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-1", capA, linkA); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	// Same session later links to different runtime.
	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-2", peerCapture(200, startB), linkB)
	if resp.Status != StatusRejected || !hasIngestCode(resp, CodeSessionBindingConflict) || !resp.NoBindingPerformed {
		t.Fatalf("conflict = %#v", resp)
	}
}

func TestBindingTwoSessionsCannotShareRuntime(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	cap := peerCapture(100, start)
	link := RuntimeLinkResult{RuntimeRef: idA, Unique: true, Rule: "L1_root"}
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-1", cap, link); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	resp := f.handle("session-b", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-2", cap, link)
	if resp.Status != StatusRejected || !hasIngestCode(resp, CodeSessionBindingConflict) {
		t.Fatalf("shared runtime = %#v", resp)
	}
}

func TestBindingPIDReuseDifferentStartTime(t *testing.T) {
	f := newBindingFixture(t)
	startOld := testTime.Add(-time.Hour)
	startNew := testTime.Add(-time.Minute)
	idOld := instancepresence.InstanceID("runtime-old")
	idNew := instancepresence.InstanceID("runtime-new")
	// Same PID, different start → different runtime identities.
	f.registerRuntime(idOld, 100, startOld)
	f.registerRuntime(idNew, 100, startNew)

	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-1",
		peerCapture(100, startOld), RuntimeLinkResult{RuntimeRef: idOld, Unique: true}); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	// End old session, then bind new generation with same PID.
	if resp := f.handle("session-a", instancecorrelation.LifecycleEnded, "", "req-2",
		peerCapture(100, startOld), RuntimeLinkResult{RuntimeRef: idOld, Unique: true}); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	resp := f.handle("session-b", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "req-3",
		peerCapture(100, startNew), RuntimeLinkResult{RuntimeRef: idNew, Unique: true})
	if resp.Status != StatusOK || resp.NoBindingPerformed {
		t.Fatalf("pid reuse bind = %#v", resp)
	}
	if ref, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-b"); !ok || ref != idNew {
		t.Fatalf("bound ref = %q ok=%t", ref, ok)
	}
}

func TestBindingReplayDoesNotMutateTwice(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	cap := peerCapture(100, start)
	link := RuntimeLinkResult{RuntimeRef: idA, Unique: true}
	req := IngestRequest{
		ProtocolVersion: IngestProtocolVersion, Operation: OperationIngestHookEvent,
		RequestID: "replay-1",
		Payload: IngressPayload{
			Tool: instancepresence.ToolClaude, HookSessionRef: "session-a",
			Lifecycle: instancecorrelation.LifecycleActive, State: instancepresence.StateWorking,
		},
	}
	f.linker.byCall = []RuntimeLinkResult{link}
	f.linker.index = 0
	first := f.receiver.HandleWithCapture(context.Background(), req, cap)
	if first.Status != StatusOK || first.NoBindingPerformed {
		t.Fatalf("first = %#v", first)
	}
	inst, _ := f.registry.Get(idA)
	rev := inst.Revisions.HookRevision
	// Replay identical request — binder must not run again.
	f.linker.byCall = []RuntimeLinkResult{{Reasons: []string{"should-not-run"}}}
	f.linker.index = 0
	replay := f.receiver.HandleWithCapture(context.Background(), req, cap)
	if replay.Status != StatusDuplicate || replay.NoBindingPerformed {
		t.Fatalf("replay = %#v", replay)
	}
	inst, _ = f.registry.Get(idA)
	if inst.Revisions.HookRevision != rev {
		t.Fatalf("hook revision advanced on replay: %d -> %d", rev, inst.Revisions.HookRevision)
	}
}

func TestBindingRequestIDConflict(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	cap := peerCapture(100, start)
	link := RuntimeLinkResult{RuntimeRef: idA, Unique: true}
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "conflict-1", cap, link); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	// Same request ID, different body.
	f.linker.byCall = []RuntimeLinkResult{link}
	f.linker.index = 0
	conflict := IngestRequest{
		ProtocolVersion: IngestProtocolVersion, Operation: OperationIngestHookEvent,
		RequestID: "conflict-1",
		Payload: IngressPayload{
			Tool: instancepresence.ToolClaude, HookSessionRef: "session-a",
			Lifecycle: instancecorrelation.LifecycleIdle, State: instancepresence.StateIdle,
		},
	}
	resp := f.receiver.HandleWithCapture(context.Background(), conflict, cap)
	if resp.Status != StatusRejected || !hasIngestCode(resp, CodeRequestIDConflict) || !resp.NoBindingPerformed {
		t.Fatalf("conflict = %#v", resp)
	}
}

type failingMutator struct {
	HookMutator
	fail error
}

func (m failingMutator) ApplyNextHookMutation(
	id instancepresence.InstanceID,
	producerEpoch instancepresence.ProducerEpoch,
	state instancepresence.EffectiveState,
	observedAt time.Time,
	idempotencyKey string,
) (instancepresence.Instance, error) {
	if m.fail != nil {
		return instancepresence.Instance{}, m.fail
	}
	return m.HookMutator.ApplyNextHookMutation(id, producerEpoch, state, observedAt, idempotencyKey)
}

func TestBindingRegistryMutationFailure(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	engine, err := NewBindingEngine(f.sessions, f.linker, failingMutator{HookMutator: f.registry, fail: errors.New("boom")})
	if err != nil {
		t.Fatal(err)
	}
	f.receiver.SetBinder(engine)
	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "mut-fail",
		peerCapture(100, start), RuntimeLinkResult{RuntimeRef: idA, Unique: true})
	if resp.Status != StatusError || !hasIngestCode(resp, CodeRegistryMutationFailed) || !resp.NoBindingPerformed {
		t.Fatalf("mutation fail = %#v", resp)
	}
	if f.sessions.Len() != 0 || f.sessions.ReservedLen() != 0 {
		t.Fatalf("registry_mutation_failed left binding: committed=%d reserved=%d", f.sessions.Len(), f.sessions.ReservedLen())
	}
}

func TestBindingFailedEndedMutationKeepsBinding(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	cap := peerCapture(100, start)
	link := RuntimeLinkResult{RuntimeRef: idA, Unique: true}
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "end-1", cap, link); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	// First ended mutation fails — binding must remain for retry.
	engine, err := NewBindingEngine(f.sessions, f.linker, failingMutator{HookMutator: f.registry, fail: errors.New("boom")})
	if err != nil {
		t.Fatal(err)
	}
	f.receiver.SetBinder(engine)
	resp := f.handle("session-a", instancecorrelation.LifecycleEnded, "", "end-2", cap, link)
	if resp.Status != StatusError || !hasIngestCode(resp, CodeRegistryMutationFailed) {
		t.Fatalf("ended fail = %#v", resp)
	}
	if ref, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-a"); !ok || ref != idA {
		t.Fatalf("binding dropped after failed end: %q ok=%t", ref, ok)
	}
	// Successful end after recovery.
	engineOK, err := NewBindingEngine(f.sessions, f.linker, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	f.receiver.SetBinder(engineOK)
	resp = f.handle("session-a", instancecorrelation.LifecycleEnded, "", "end-3", cap, link)
	if resp.Status != StatusOK {
		t.Fatalf("retry end = %#v", resp)
	}
	if _, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-a"); ok {
		t.Fatal("binding still present after successful end")
	}
}

func TestBindingExistingSessionSurvivesTimeoutAndZeroLink(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	cap := peerCapture(100, start)
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "ex-1", cap,
		RuntimeLinkResult{RuntimeRef: idA, Unique: true}); resp.Status != StatusOK {
		t.Fatal(resp)
	}

	// Timeout on re-measure must not drop binding; state still updates.
	f.linker.delay = 0
	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateAttention, "ex-2", cap,
		RuntimeLinkResult{TimedOut: true, Reasons: []string{"attestation_timeout"}})
	if resp.Status != StatusOK || resp.NoBindingPerformed {
		t.Fatalf("timeout rebind = %#v", resp)
	}
	if inst, err := f.registry.Get(idA); err != nil || inst.State != instancepresence.StateAttention {
		t.Fatalf("state after timeout link = %#v err=%v", inst, err)
	}

	// Zero link on re-measure keeps binding.
	resp = f.handle("session-a", instancecorrelation.LifecycleIdle, instancepresence.StateIdle, "ex-3", cap,
		RuntimeLinkResult{Reasons: []string{"no_unique_runtime_link"}})
	if resp.Status != StatusOK || resp.NoBindingPerformed {
		t.Fatalf("zero rebind = %#v", resp)
	}
	if inst, err := f.registry.Get(idA); err != nil || inst.State != instancepresence.StateIdle {
		t.Fatalf("state after zero link = %#v err=%v", inst, err)
	}
	if ref, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-a"); !ok || ref != idA {
		t.Fatalf("binding lost: %q ok=%t", ref, ok)
	}
}

func TestBindingExistingSessionConflictOnDifferentUniqueLink(t *testing.T) {
	f := newBindingFixture(t)
	startA := testTime.Add(-time.Hour)
	startB := testTime.Add(-2 * time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	idB := instancepresence.InstanceID("runtime-b")
	f.registerRuntime(idA, 100, startA)
	f.registerRuntime(idB, 200, startB)
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "cf-1",
		peerCapture(100, startA), RuntimeLinkResult{RuntimeRef: idA, Unique: true}); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "cf-2",
		peerCapture(200, startB), RuntimeLinkResult{RuntimeRef: idB, Unique: true})
	if resp.Status != StatusRejected || !hasIngestCode(resp, CodeSessionBindingConflict) {
		t.Fatalf("conflict = %#v", resp)
	}
	if ref, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-a"); !ok || ref != idA {
		t.Fatalf("binding changed on conflict: %q ok=%t", ref, ok)
	}
}

func TestBindingParallelNewSessionsCannotShareRuntime(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-shared")
	f.registerRuntime(idA, 100, start)

	sharedLinker := &mapLinker{links: map[uint64]RuntimeLinkResult{
		100: {RuntimeRef: idA, Unique: true, Rule: "L1_root"},
		101: {RuntimeRef: idA, Unique: true, Rule: "L1_root"},
	}}
	engine, err := NewBindingEngine(f.sessions, sharedLinker, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	f.receiver.SetBinder(engine)

	var wait sync.WaitGroup
	results := make(chan IngestResponse, 2)
	wait.Add(2)
	for i, session := range []string{"session-a", "session-b"} {
		session := session
		reqID := "par-" + session
		pid := uint64(100 + i) // different peers, same linked runtime
		go func() {
			defer wait.Done()
			req := IngestRequest{
				ProtocolVersion: IngestProtocolVersion, Operation: OperationIngestHookEvent,
				RequestID: reqID,
				Payload: IngressPayload{
					Tool: instancepresence.ToolClaude, HookSessionRef: instancepresence.OpaqueIdentity(session),
					Lifecycle: instancecorrelation.LifecycleActive, State: instancepresence.StateWorking,
				},
			}
			results <- f.receiver.HandleWithCapture(context.Background(), req, peerCapture(pid, start))
		}()
	}
	wait.Wait()
	close(results)
	var okCount, rejectCount int
	for resp := range results {
		switch {
		case resp.Status == StatusOK && !resp.NoBindingPerformed:
			okCount++
		case resp.Status == StatusRejected && hasIngestCode(resp, CodeSessionBindingConflict):
			rejectCount++
		default:
			t.Fatalf("unexpected response %#v", resp)
		}
	}
	if okCount != 1 || rejectCount != 1 {
		t.Fatalf("ok=%d reject=%d", okCount, rejectCount)
	}
	if f.sessions.Len() != 1 {
		t.Fatalf("committed bindings = %d", f.sessions.Len())
	}
	if f.sessions.ReservedLen() != 0 {
		t.Fatalf("leaked reservations = %d", f.sessions.ReservedLen())
	}
}

func TestBindingSessionBAfterSessionAEndAdvancesRuntimeHookRevision(t *testing.T) {
	// HookRevision is per runtime, not per ingress session stream.
	// Session A may end at stream rev N while runtime HookRevision is higher;
	// session B must still bind and mutate the same living runtime.
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	cap := peerCapture(100, start)
	link := RuntimeLinkResult{RuntimeRef: idA, Unique: true, Rule: "L1_root"}

	// session A: working → idle → working (3 stream revisions, 3 runtime hook revs)
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "sa-1", cap, link); resp.Status != StatusOK {
		t.Fatalf("A working: %#v", resp)
	}
	if resp := f.handle("session-a", instancecorrelation.LifecycleIdle, instancepresence.StateIdle, "sa-2", cap, link); resp.Status != StatusOK {
		t.Fatalf("A idle: %#v", resp)
	}
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "sa-3", cap, link); resp.Status != StatusOK {
		t.Fatalf("A working again: %#v", resp)
	}
	afterA, err := f.registry.Get(idA)
	if err != nil {
		t.Fatal(err)
	}
	revAfterA := afterA.Revisions.HookRevision
	if revAfterA < 3 {
		t.Fatalf("HookRevision after session A = %d, want >= 3", revAfterA)
	}

	if resp := f.handle("session-a", instancecorrelation.LifecycleEnded, "", "sa-end", cap, link); resp.Status != StatusOK {
		t.Fatalf("A end: %#v", resp)
	}
	if _, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-a"); ok {
		t.Fatal("session A still bound")
	}
	alive, err := f.registry.Get(idA)
	if err != nil || !alive.Status.Active() {
		t.Fatalf("runtime A should remain alive: %#v err=%v", alive, err)
	}
	revAfterEnd := alive.Revisions.HookRevision
	if revAfterEnd <= revAfterA {
		t.Fatalf("SessionEnd should advance HookRevision: afterA=%d afterEnd=%d", revAfterA, revAfterEnd)
	}

	// session B starts its own stream at revision 1 but runtime continues.
	resp := f.handle("session-b", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "sb-1", cap, link)
	if resp.Status != StatusOK {
		t.Fatalf("session B first working rejected: %#v (want no stale_revision)", resp)
	}
	if hasIngestCode(resp, CodeRegistryMutationFailed) {
		t.Fatalf("session B mutation failed: %#v", resp)
	}
	afterB, err := f.registry.Get(idA)
	if err != nil {
		t.Fatal(err)
	}
	if afterB.State != instancepresence.StateWorking {
		t.Fatalf("state after B = %q, want working", afterB.State)
	}
	if afterB.Revisions.HookRevision <= revAfterEnd {
		t.Fatalf("HookRevision after B = %d, want > %d (after session A end)", afterB.Revisions.HookRevision, revAfterEnd)
	}
}

func TestBindingEndedWhenRuntimeAlreadyGone(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	cap := peerCapture(100, start)
	link := RuntimeLinkResult{RuntimeRef: idA, Unique: true}
	if resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "gone-1", cap, link); resp.Status != StatusOK {
		t.Fatal(resp)
	}
	// End runtime out of band.
	if _, err := f.registry.EndRuntime(idA, presencev2.RuntimeMutation{
		ProducerEpoch: f.epoch, RuntimeRevision: 2, Status: instancepresence.RuntimeEnded,
		ObservedAt: testTime.Add(time.Second), IdempotencyKey: "end-runtime",
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.handle("session-a", instancecorrelation.LifecycleEnded, "", "gone-2", cap, link)
	if resp.Status != StatusOK {
		t.Fatalf("ended after runtime gone = %#v", resp)
	}
	if _, ok := f.sessions.Lookup(instancepresence.ToolClaude, "session-a"); ok {
		t.Fatal("binding remained after idempotent end")
	}
}

func TestBindingContentFreeResponse(t *testing.T) {
	f := newBindingFixture(t)
	start := testTime.Add(-time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	f.registerRuntime(idA, 100, start)
	resp := f.handle("session-a", instancecorrelation.LifecycleActive, instancepresence.StateWorking, "cf-1",
		peerCapture(100, start), RuntimeLinkResult{RuntimeRef: idA, Unique: true})
	data, err := EncodeIngestResponseJSON(resp, DefaultIngestMaximumResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"hook_session_ref", "producer_epoch", "runtime_ref", "pid", "cwd", "proposals",
	} {
		if containsJSONKey(string(data), forbidden) {
			t.Fatalf("leaked %q in %s", forbidden, data)
		}
	}
	if resp.NoBindingPerformed {
		t.Fatal("successful bind must set no_binding_performed=false")
	}
}

func containsJSONKey(data, key string) bool {
	return len(data) > 0 && (stringIndex(data, `"`+key+`"`) >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestBindingParallelABRace(t *testing.T) {
	f := newBindingFixture(t)
	startA := testTime.Add(-time.Hour)
	startB := testTime.Add(-2 * time.Hour)
	idA := instancepresence.InstanceID("runtime-a")
	idB := instancepresence.InstanceID("runtime-b")
	f.registerRuntime(idA, 100, startA)
	f.registerRuntime(idB, 200, startB)

	raceLinker := &mapLinker{links: map[uint64]RuntimeLinkResult{
		100: {RuntimeRef: idA, Unique: true, Rule: "L1_root"},
		200: {RuntimeRef: idB, Unique: true, Rule: "L1_root"},
	}}
	engine, err := NewBindingEngine(f.sessions, raceLinker, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	f.receiver.SetBinder(engine)

	var wait sync.WaitGroup
	errs := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		req := IngestRequest{
			ProtocolVersion: IngestProtocolVersion, Operation: OperationIngestHookEvent,
			RequestID: "race-a",
			Payload: IngressPayload{
				Tool: instancepresence.ToolClaude, HookSessionRef: "session-a",
				Lifecycle: instancecorrelation.LifecycleActive, State: instancepresence.StateWorking,
			},
		}
		resp := f.receiver.HandleWithCapture(context.Background(), req, peerCapture(100, startA))
		if resp.Status != StatusOK || resp.NoBindingPerformed {
			errs <- errors.New("A failed")
		}
	}()
	go func() {
		defer wait.Done()
		req := IngestRequest{
			ProtocolVersion: IngestProtocolVersion, Operation: OperationIngestHookEvent,
			RequestID: "race-b",
			Payload: IngressPayload{
				Tool: instancepresence.ToolClaude, HookSessionRef: "session-b",
				Lifecycle: instancecorrelation.LifecycleActive, State: instancepresence.StateAttention,
			},
		}
		resp := f.receiver.HandleWithCapture(context.Background(), req, peerCapture(200, startB))
		if resp.Status != StatusOK || resp.NoBindingPerformed {
			errs <- errors.New("B failed")
		}
	}()
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	instA, _ := f.registry.Get(idA)
	instB, _ := f.registry.Get(idB)
	if instA.State != instancepresence.StateWorking || instB.State != instancepresence.StateAttention {
		t.Fatalf("A=%q B=%q", instA.State, instB.State)
	}
}

type mapLinker struct {
	links map[uint64]RuntimeLinkResult
}

func (m *mapLinker) Link(_ context.Context, capture IdentityPeerCapture, _ instancepresence.ToolKind) RuntimeLinkResult {
	if result, ok := m.links[capture.GenerationPID]; ok {
		return result
	}
	return RuntimeLinkResult{Reasons: []string{"no_unique_runtime_link"}}
}
