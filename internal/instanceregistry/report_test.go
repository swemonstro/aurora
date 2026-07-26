package instanceregistry

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func newReportTestRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: testTime()}
	registry, err := New(Config{
		Clock: clock, SlotNamespace: "default",
		LeaseDuration: 10 * time.Second, GracePeriod: 5 * time.Second,
		MaximumProducerLeaseDuration: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return registry, clock
}

func testRuntime(pid uint64) instancepresence.RuntimeIdentity {
	return instancepresence.RuntimeIdentity{
		HostID: "broker-host", BootID: "broker-boot",
		RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: testTime()},
	}
}

func report(id string, epoch string, revision uint64, state instancepresence.EffectiveState, pid uint64) ProducerReport {
	now := testTime().Add(time.Duration(revision) * time.Second)
	return ProducerReport{
		InstanceID: instancepresence.InstanceID(id), Tool: instancepresence.ToolClaude,
		Source:         instancepresence.SourceDescriptor{Provider: "producerprotocol", Profile: "claude", CollectorID: "broker-test"},
		Runtime:        testRuntime(pid),
		ProducerEpoch:  instancepresence.ProducerEpoch(epoch),
		Revision:       revision,
		State:          state,
		ObservedAt:     now,
		LeaseExpiresAt: now.Add(time.Minute),
	}
}

func TestApplyProducerReportRegistersNewInstance(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	inst, outcome, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != ReportRegistered {
		t.Fatalf("outcome = %v, want ReportRegistered", outcome)
	}
	if inst.Tool != instancepresence.ToolClaude || inst.State != instancepresence.StateWorking ||
		inst.Revisions.ProducerEpoch != "epoch-1" || inst.Revisions.HookRevision != 1 || inst.Revisions.RuntimeRevision != 1 {
		t.Fatalf("instance = %#v", inst)
	}
	if inst.Lifecycle.LeaseExpiresAt.IsZero() {
		t.Fatal("lease not set")
	}
}

func TestApplyProducerReportRequiresConfiguredMaximumLease(t *testing.T) {
	registry, _ := newTestRegistry(t) // MaximumProducerLeaseDuration left zero.
	_, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1))
	if err == nil {
		t.Fatal("expected error when registry is not configured for producer reports")
	}
}

func TestApplyProducerReportRejectsAlreadyExpiredLease(t *testing.T) {
	registry, clock := newReportTestRegistry(t)
	r := report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1)
	r.LeaseExpiresAt = clock.Now().Add(-time.Second) // still after ObservedAt, but not after "now".
	r.ObservedAt = clock.Now().Add(-2 * time.Second)
	_, _, err := registry.ApplyProducerReport(r)
	if !errors.Is(err, ErrLeaseAlreadyExpired) {
		t.Fatalf("error = %v, want ErrLeaseAlreadyExpired", err)
	}
	if len(registry.ActiveInstances()) != 0 {
		t.Fatal("rejected report created an instance")
	}
}

func TestApplyProducerReportRejectsLeaseExceedingMaximum(t *testing.T) {
	registry, _ := newReportTestRegistry(t) // Maximum: 5 minutes.
	r := report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1)
	r.LeaseExpiresAt = r.ObservedAt.Add(time.Hour)
	_, _, err := registry.ApplyProducerReport(r)
	if !errors.Is(err, ErrLeaseExceedsMaximum) {
		t.Fatalf("error = %v, want ErrLeaseExceedsMaximum", err)
	}
	if len(registry.ActiveInstances()) != 0 {
		t.Fatal("rejected report created an instance")
	}
}

func TestApplyProducerReportRejectsObservedAtTooFarInFuture(t *testing.T) {
	clock := &fakeClock{now: testTime()}
	registry, err := New(Config{
		Clock: clock, SlotNamespace: "default",
		LeaseDuration: 10 * time.Second, GracePeriod: 5 * time.Second,
		MaximumProducerLeaseDuration: 5 * time.Minute,
		MaximumClockSkew:             30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	r := report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1)
	r.ObservedAt = clock.Now().Add(time.Minute) // beyond the 30s skew budget.
	r.LeaseExpiresAt = r.ObservedAt.Add(time.Minute)
	_, _, err = registry.ApplyProducerReport(r)
	if !errors.Is(err, ErrClockSkewTooLarge) {
		t.Fatalf("error = %v, want ErrClockSkewTooLarge", err)
	}
	if len(registry.ActiveInstances()) != 0 {
		t.Fatal("rejected report created an instance")
	}
	// Just inside the budget must be accepted.
	within := report("inst-b", "epoch-1", 1, instancepresence.StateWorking, 2)
	within.ObservedAt = clock.Now().Add(20 * time.Second)
	within.LeaseExpiresAt = within.ObservedAt.Add(time.Minute)
	if _, _, err := registry.ApplyProducerReport(within); err != nil {
		t.Fatalf("report within clock skew budget rejected: %v", err)
	}
}

func TestApplyProducerReportRejectsObservedAtTooFarInPast(t *testing.T) {
	clock := &fakeClock{now: testTime()}
	registry, err := New(Config{
		Clock: clock, SlotNamespace: "default",
		LeaseDuration: 10 * time.Second, GracePeriod: 5 * time.Second,
		MaximumProducerLeaseDuration: 5 * time.Minute,
		MaximumReportAge:             30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	r := report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1)
	r.ObservedAt = clock.Now().Add(-time.Minute) // beyond the 30s age budget.
	r.LeaseExpiresAt = clock.Now().Add(time.Minute)
	_, _, err = registry.ApplyProducerReport(r)
	if !errors.Is(err, ErrReportTooOld) {
		t.Fatalf("error = %v, want ErrReportTooOld", err)
	}
	if len(registry.ActiveInstances()) != 0 {
		t.Fatal("rejected report created an instance")
	}
	// Just inside the budget must be accepted.
	within := report("inst-b", "epoch-1", 1, instancepresence.StateWorking, 2)
	within.ObservedAt = clock.Now().Add(-20 * time.Second)
	within.LeaseExpiresAt = clock.Now().Add(time.Minute)
	if _, _, err := registry.ApplyProducerReport(within); err != nil {
		t.Fatalf("report within report age budget rejected: %v", err)
	}
}

// TestApplyProducerReportMaximumLeaseAnchoredToRegistryNowNotObservedAt is
// the direct regression test for the observed_at-anchoring bypass: a
// producer that lies about ObservedAt (claiming it is far in the future)
// must not be able to make an actually-far-future LeaseExpiresAt look like
// a short span by measuring it against its own claimed observation time
// instead of the registry's real clock. This registry deliberately leaves
// MaximumClockSkew unconfigured so the skew check above cannot be the one
// catching this — only the lease check's own anchor matters here.
func TestApplyProducerReportMaximumLeaseAnchoredToRegistryNowNotObservedAt(t *testing.T) {
	registry, clock := newReportTestRegistry(t) // MaximumProducerLeaseDuration: 5 minutes, no skew/age configured.
	r := report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1)
	r.ObservedAt = clock.Now().Add(time.Hour)        // far-future claimed observation time.
	r.LeaseExpiresAt = r.ObservedAt.Add(time.Minute) // small span from ITS OWN observed_at...
	// ...but LeaseExpiresAt is actually clock.Now()+1h1m, far beyond the
	// configured 5-minute maximum measured from real broker time.
	_, _, err := registry.ApplyProducerReport(r)
	if !errors.Is(err, ErrLeaseExceedsMaximum) {
		t.Fatalf("error = %v, want ErrLeaseExceedsMaximum (observed_at-anchoring bypass not closed)", err)
	}
	if len(registry.ActiveInstances()) != 0 {
		t.Fatal("bypass report created an instance")
	}
}

func TestApplyProducerReportRevisionMustStrictlyIncrease(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	if _, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 5, instancepresence.StateWorking, 1)); err != nil {
		t.Fatal(err)
	}
	_, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 4, instancepresence.StateIdle, 1))
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("error = %v, want ErrStaleRevision", err)
	}
	inst, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.State != instancepresence.StateWorking || inst.Revisions.HookRevision != 5 {
		t.Fatalf("stale revision changed state: %#v", inst)
	}
}

func TestApplyProducerReportSameRevisionIdempotentOrRejected(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	r := report("inst-a", "epoch-1", 3, instancepresence.StateWorking, 1)
	if _, _, err := registry.ApplyProducerReport(r); err != nil {
		t.Fatal(err)
	}
	// Exact retry: idempotent no-op.
	inst, outcome, err := registry.ApplyProducerReport(r)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != ReportDuplicate {
		t.Fatalf("outcome = %v, want ReportDuplicate", outcome)
	}
	if inst.State != instancepresence.StateWorking {
		t.Fatalf("instance = %#v", inst)
	}
	// Same revision, different content: rejected, not silently accepted.
	conflicting := report("inst-a", "epoch-1", 3, instancepresence.StateAttention, 1)
	_, _, err = registry.ApplyProducerReport(conflicting)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("error = %v, want ErrRevisionConflict", err)
	}
	after, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.State != instancepresence.StateWorking {
		t.Fatalf("conflicting same-revision payload changed state: %#v", after)
	}
}

func TestApplyProducerReportRejectsCrossToolTakeover(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	if _, _, err := registry.ApplyProducerReport(report("shared-id", "epoch-1", 1, instancepresence.StateIdle, 1)); err != nil {
		t.Fatal(err)
	}
	codexReport := report("shared-id", "epoch-2", 1, instancepresence.StateWorking, 2)
	codexReport.Tool = instancepresence.ToolCodex
	_, _, err := registry.ApplyProducerReport(codexReport)
	if !errors.Is(err, ErrToolMismatch) {
		t.Fatalf("error = %v, want ErrToolMismatch", err)
	}
	inst, getErr := registry.Get("shared-id")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.Tool != instancepresence.ToolClaude || inst.State != instancepresence.StateIdle || inst.Revisions.ProducerEpoch != "epoch-1" {
		t.Fatalf("cross-tool attempt mutated the instance: %#v", inst)
	}
}

func TestApplyProducerReportTakeoverResetsRevisionAndPreservesSlot(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	first, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 40, instancepresence.StateWorking, 1))
	if err != nil {
		t.Fatal(err)
	}
	inst, outcome, err := registry.ApplyProducerReport(report("inst-a", "epoch-2", 1, instancepresence.StateIdle, 2))
	if err != nil {
		t.Fatalf("takeover rejected: %v", err)
	}
	if outcome != ReportApplied {
		t.Fatalf("outcome = %v, want ReportApplied", outcome)
	}
	if inst.Revisions.ProducerEpoch != "epoch-2" || inst.Revisions.HookRevision != 1 || inst.Revisions.RuntimeRevision != 1 {
		t.Fatalf("takeover did not reset revisions: %#v", inst.Revisions)
	}
	if inst.State != instancepresence.StateIdle {
		t.Fatalf("takeover did not apply new state: %#v", inst)
	}
	if inst.Slot != first.Slot {
		t.Fatalf("takeover changed slot: before=%#v after=%#v", first.Slot, inst.Slot)
	}
	if inst.Status != instancepresence.RuntimeAlive {
		t.Fatalf("status = %v, want alive", inst.Status)
	}
}

// TestApplyProducerReportTreatsAnyEpochChangeAsATakeoverMechanism pins the
// documented, intentional scope of this primitive: the registry has no
// notion of connections, so it cannot itself tell "a genuinely new
// generation" apart from "a third epoch replaying after a takeover already
// happened" — both are just "the stored epoch differs from the report's".
// It therefore always accepts a differing epoch as a fresh takeover; it is
// presencebroker.Ingestor's connection-aware claim tracking (not this
// package) that must decide whether a given epoch change is legitimate
// before ever calling this method (see ingest.go and its own tests for
// "old epoch never reactivates after a real takeover" — that guarantee
// lives one layer up, deliberately, and is exercised there).
func TestApplyProducerReportTreatsAnyEpochChangeAsATakeoverMechanism(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	if _, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 9, instancepresence.StateWorking, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-2", 1, instancepresence.StateIdle, 2)); err != nil {
		t.Fatal(err)
	}
	inst, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-3", 1, instancepresence.StateError, 3))
	if err != nil {
		t.Fatalf("registry primitive rejected a third differing epoch: %v", err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-3" || inst.State != instancepresence.StateError {
		t.Fatalf("instance = %#v", inst)
	}
}

func TestApplyProducerReportTakeoverReassignsSlotWhenOldOneReclaimed(t *testing.T) {
	registry, clock := newReportTestRegistry(t)
	shortLease := report("inst-a", "epoch-1", 1, instancepresence.StateWorking, 1)
	shortLease.LeaseExpiresAt = shortLease.ObservedAt.Add(10 * time.Second) // short enough to expire below.
	first, _, err := registry.ApplyProducerReport(shortLease)
	if err != nil {
		t.Fatal(err)
	}
	// Let inst-a's generation fully end (lease + grace period elapse).
	clock.Advance(20 * time.Second)
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	ended, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if ended.Status != instancepresence.RuntimeEnded {
		t.Fatalf("test setup: inst-a status = %v, want ended", ended.Status)
	}
	// A different instance now takes the freed slot. Only Index matters for
	// collision purposes; AssignedAt legitimately differs by wall-clock time.
	other, _, err := registry.ApplyProducerReport(report("inst-b", "epoch-x", 1, instancepresence.StateWorking, 9))
	if err != nil {
		t.Fatal(err)
	}
	if other.Slot.Index != first.Slot.Index {
		t.Fatalf("test setup: inst-b did not reuse the freed slot index: %#v vs %#v", other.Slot, first.Slot)
	}
	// inst-a's new generation takes over: its old slot is occupied now, so
	// it must get a different one rather than colliding with inst-b.
	revived, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-2", 1, instancepresence.StateIdle, 2))
	if err != nil {
		t.Fatal(err)
	}
	if revived.Slot.Index == other.Slot.Index {
		t.Fatalf("revived instance collided with inst-b's slot index: %#v", revived.Slot)
	}
	snapshot, err := registry.CanonicalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot invalid after slot reassignment: %v", err)
	}
}

func TestApplyProducerReportRevisionsIndependentAcrossInstances(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	if _, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 1, instancepresence.StateIdle, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ApplyProducerReport(report("inst-b", "epoch-1", 1, instancepresence.StateIdle, 2)); err != nil {
		t.Fatal(err)
	}
	for revision := uint64(2); revision <= 5; revision++ {
		r := report("inst-a", "epoch-1", revision, instancepresence.StateWorking, 1)
		if _, _, err := registry.ApplyProducerReport(r); err != nil {
			t.Fatalf("inst-a revision %d: %v", revision, err)
		}
	}
	b, err := registry.Get("inst-b")
	if err != nil {
		t.Fatal(err)
	}
	if b.Revisions.HookRevision != 1 || b.State != instancepresence.StateIdle {
		t.Fatalf("inst-b affected by inst-a's advances: %#v", b)
	}
}

// TestApplyProducerReportNeverExposesAnIntermediateState is the direct
// regression test for atomic ingest: the old two-step design (a runtime
// mutation that defaulted a new instance to idle, followed by a separate
// hook mutation that applied the real state) meant a concurrent reader
// could observe the instance as idle between the two calls, even though no
// producer ever reported idle. ApplyProducerReport folds both steps into
// one locked operation specifically so that window cannot exist: this test
// hammers CanonicalSnapshot and Presentation on separate goroutines while a
// brand-new instance is registered directly into StateWorking, and fails
// if idle is ever observed for it.
func TestApplyProducerReportNeverExposesAnIntermediateState(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	const instanceID = "inst-atomic"

	stop := make(chan struct{})
	violation := make(chan string, 1)
	var readers sync.WaitGroup

	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snapshot, err := registry.CanonicalSnapshot()
			if err != nil {
				continue
			}
			for _, inst := range snapshot.Instances {
				if string(inst.InstanceID) == instanceID && inst.State == instancepresence.StateIdle {
					select {
					case violation <- "canonical snapshot observed idle":
					default:
					}
				}
			}
		}
	}()

	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			presentation, err := registry.Presentation(5)
			if err != nil {
				continue
			}
			for _, pixel := range presentation.Pixels {
				if string(pixel.InstanceID) == instanceID && pixel.State == instancepresence.StateIdle {
					select {
					case violation <- "presentation observed idle":
					default:
					}
				}
			}
		}
	}()

	for run := 0; run < 2000; run++ {
		select {
		case v := <-violation:
			close(stop)
			readers.Wait()
			t.Fatal(v)
		default:
		}
		if run == 0 {
			if _, _, err := registry.ApplyProducerReport(report(instanceID, "epoch-1", 1, instancepresence.StateWorking, 1)); err != nil {
				close(stop)
				readers.Wait()
				t.Fatal(err)
			}
			continue
		}
		// Keep the readers under real contention for the whole run via
		// repeated no-op-adjacent revisions on an unrelated instance.
		other := report("inst-filler", "epoch-1", uint64(run), instancepresence.StateWorking, 2)
		_, _, _ = registry.ApplyProducerReport(other)
	}
	close(stop)
	readers.Wait()
	select {
	case v := <-violation:
		t.Fatal(v)
	default:
	}
}

// TestApplyProducerReportRejectionLeavesEverythingByteForByteUnchanged is
// the explicit, direct check for the blocking-point-1 requirement: on a
// rejected report, State, Revisions (including ProducerEpoch), Slot, Tool,
// and Lifecycle (including LeaseExpiresAt — a rejected report must not
// renew the lease either) are all byte-for-byte identical to before the
// attempt. Each rejected variant below deliberately carries a materially
// different lease deadline and state than what is already stored, so a
// bug that let any field leak through would be caught, not masked by the
// rejected report happening to look like the stored one.
func TestApplyProducerReportRejectionLeavesEverythingByteForByteUnchanged(t *testing.T) {
	registry, _ := newReportTestRegistry(t)
	before, _, err := registry.ApplyProducerReport(report("inst-a", "epoch-1", 5, instancepresence.StateWorking, 1))
	if err != nil {
		t.Fatal(err)
	}

	attempts := []ProducerReport{
		// Stale revision.
		func() ProducerReport {
			r := report("inst-a", "epoch-1", 4, instancepresence.StateError, 1)
			r.LeaseExpiresAt = r.ObservedAt.Add(4 * time.Minute)
			return r
		}(),
		// Same revision, conflicting content.
		func() ProducerReport {
			r := report("inst-a", "epoch-1", 5, instancepresence.StateAttention, 1)
			r.LeaseExpiresAt = r.ObservedAt.Add(3 * time.Minute)
			return r
		}(),
		// Cross-tool.
		func() ProducerReport {
			r := report("inst-a", "epoch-9", 6, instancepresence.StateIdle, 1)
			r.Tool = instancepresence.ToolCodex
			r.LeaseExpiresAt = r.ObservedAt.Add(2 * time.Minute)
			return r
		}(),
	}

	for _, attempt := range attempts {
		if _, _, err := registry.ApplyProducerReport(attempt); err == nil {
			t.Fatalf("attempt unexpectedly accepted: %#v", attempt)
		}
		after, getErr := registry.Get("inst-a")
		if getErr != nil {
			t.Fatal(getErr)
		}
		if after.State != before.State {
			t.Fatalf("state changed: before=%v after=%v", before.State, after.State)
		}
		if after.Revisions != before.Revisions {
			t.Fatalf("revisions changed: before=%#v after=%#v", before.Revisions, after.Revisions)
		}
		if after.Lifecycle != before.Lifecycle {
			t.Fatalf("lifecycle (including lease) changed: before=%#v after=%#v", before.Lifecycle, after.Lifecycle)
		}
		if after.Slot != before.Slot {
			t.Fatalf("slot changed: before=%#v after=%#v", before.Slot, after.Slot)
		}
		if after.Tool != before.Tool {
			t.Fatalf("tool changed: before=%v after=%v", before.Tool, after.Tool)
		}
	}
}
