package presencebroker

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// generationOf is a test-only peek at an Ingestor's internal generation
// bookkeeping, used to assert directly on owner/live/retiredEpochs instead
// of only inferring them indirectly through Apply's accept/reject outcome.
func generationOf(t *testing.T, ingestor *Ingestor, id producerprotocol.InstanceID) (generationRecord, bool) {
	t.Helper()
	ingestor.generationsMu.Lock()
	defer ingestor.generationsMu.Unlock()
	rec, ok := ingestor.generations[instancepresence.InstanceID(id)]
	if !ok {
		return generationRecord{}, false
	}
	retired := make(map[instancepresence.ProducerEpoch]struct{}, len(rec.retiredEpochs))
	for epoch := range rec.retiredEpochs {
		retired[epoch] = struct{}{}
	}
	return generationRecord{
		tool: rec.tool, currentEpoch: rec.currentEpoch, live: rec.live, owner: rec.owner,
		retiredEpochs: retired,
	}, true
}

func newIngestTestRegistry(t *testing.T) (*instanceregistry.Registry, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default",
		LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
		MaximumProducerLeaseDuration: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, clock
}

func newTestIngestor(t *testing.T, registry *instanceregistry.Registry) *Ingestor {
	t.Helper()
	ingestor, err := NewIngestor(registry, "host-fixture", "aurora-presence-broker-test")
	if err != nil {
		t.Fatal(err)
	}
	return ingestor
}

func ingestMessage(tool producerprotocol.Tool, instanceID producerprotocol.InstanceID, epoch producerprotocol.ProducerEpoch, revision producerprotocol.Revision, state producerprotocol.State, observedAt time.Time) producerprotocol.Message {
	return producerprotocol.Message{
		ProtocolVersion: producerprotocol.CurrentProtocolVersion,
		Tool:            tool,
		InstanceID:      instanceID,
		ProducerEpoch:   epoch,
		State:           state,
		Revision:        revision,
		ObservedAt:      observedAt,
		LeaseExpiresAt:  observedAt.Add(time.Minute),
	}
}

func TestIngestorRegistersNewInstancePerTool(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	for _, tool := range []producerprotocol.Tool{producerprotocol.ToolClaude, producerprotocol.ToolCodex, producerprotocol.ToolGrok} {
		id := producerprotocol.InstanceID(string(tool) + "-1")
		session := ingestor.NewSession()
		inst, err := session.Apply(ingestMessage(tool, id, "epoch-1", 1, producerprotocol.StateWorking, now))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if inst.Tool != instancepresence.ToolKind(tool) || inst.State != instancepresence.StateWorking {
			t.Fatalf("%s: instance = %#v", tool, inst)
		}
	}
	if len(registry.ActiveInstances()) != 3 {
		t.Fatalf("active instances = %d, want 3", len(registry.ActiveInstances()))
	}
}

func TestIngestorRevisionMustStrictlyIncrease(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 2, producerprotocol.StateWorking, now)); err != nil {
		t.Fatalf("revision 1->2 rejected: %v", err)
	}
	inst, err := registry.Get("inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != instancepresence.StateWorking {
		t.Fatalf("state = %v, want working", inst.State)
	}

	// Revision 2 -> 1 must be rejected and must not touch stored state.
	_, err = session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateIdle, now))
	if !errors.Is(err, instanceregistry.ErrStaleRevision) {
		t.Fatalf("revision 2->1 error = %v, want ErrStaleRevision", err)
	}
	after, err := registry.Get("inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != instancepresence.StateWorking || after.Revisions.HookRevision != 2 {
		t.Fatalf("older revision overwrote state: %#v", after)
	}
}

func TestIngestorSameRevisionIsIdempotentOrDeterministicallyRejected(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	msg := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)
	if _, err := session.Apply(msg); err != nil {
		t.Fatal(err)
	}
	// Exact retry of the same revision with identical content: idempotent.
	if _, err := session.Apply(msg); err != nil {
		t.Fatalf("exact retry rejected: %v", err)
	}
	// Same revision number, different content: deterministically rejected,
	// never silently accepted as if it were the retry above.
	conflicting := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateAttention, now)
	conflicting.ObservedAt = now.Add(time.Second)
	if _, err := session.Apply(conflicting); err == nil {
		t.Fatal("conflicting same-revision payload was accepted")
	}
	inst, err := registry.Get("inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != instancepresence.StateWorking {
		t.Fatalf("conflicting payload changed state: %#v", inst)
	}
}

func TestIngestorRevisionsAreIndependentAcrossInstances(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	sessionA, sessionB := ingestor.NewSession(), ingestor.NewSession()

	if _, err := sessionA.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionB.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-b", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	for revision := producerprotocol.Revision(2); revision <= 5; revision++ {
		if _, err := sessionA.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", revision, producerprotocol.StateWorking, now)); err != nil {
			t.Fatalf("inst-a revision %d: %v", revision, err)
		}
	}
	if _, err := sessionB.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-b", "epoch-1", 2, producerprotocol.StateWorking, now)); err != nil {
		t.Fatalf("inst-b revision 2 rejected after inst-a advanced independently: %v", err)
	}
	a, err := registry.Get("inst-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := registry.Get("inst-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Revisions.HookRevision != 5 || b.Revisions.HookRevision != 2 {
		t.Fatalf("revisions not independent: a=%d b=%d", a.Revisions.HookRevision, b.Revisions.HookRevision)
	}
}

func TestIngestorRevisionsAreIndependentAcrossTools(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	claude, codex, grok := ingestor.NewSession(), ingestor.NewSession(), ingestor.NewSession()

	if _, err := claude.Apply(ingestMessage(producerprotocol.ToolClaude, "claude-1", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := codex.Apply(ingestMessage(producerprotocol.ToolCodex, "codex-1", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := grok.Apply(ingestMessage(producerprotocol.ToolGrok, "grok-1", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	for revision := producerprotocol.Revision(2); revision <= 4; revision++ {
		if _, err := codex.Apply(ingestMessage(producerprotocol.ToolCodex, "codex-1", "epoch-1", revision, producerprotocol.StateWorking, now)); err != nil {
			t.Fatalf("codex revision %d: %v", revision, err)
		}
	}
	claudeInst, _ := registry.Get("claude-1")
	codexInst, _ := registry.Get("codex-1")
	grokInst, _ := registry.Get("grok-1")
	if claudeInst.Revisions.HookRevision != 1 || grokInst.Revisions.HookRevision != 1 {
		t.Fatalf("unrelated tools advanced: claude=%d grok=%d", claudeInst.Revisions.HookRevision, grokInst.Revisions.HookRevision)
	}
	if codexInst.Revisions.HookRevision != 4 || codexInst.State != instancepresence.StateWorking {
		t.Fatalf("codex = %#v", codexInst)
	}
}

func TestIngestorRejectsCrossToolMutation(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	claude, codex := ingestor.NewSession(), ingestor.NewSession()

	if _, err := claude.Apply(ingestMessage(producerprotocol.ToolClaude, "shared-id", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	_, err := codex.Apply(ingestMessage(producerprotocol.ToolCodex, "shared-id", "epoch-2", 1, producerprotocol.StateWorking, now))
	if !errors.Is(err, ErrInstanceToolMismatch) {
		t.Fatalf("cross-tool mutation error = %v, want ErrInstanceToolMismatch", err)
	}
	inst, getErr := registry.Get("shared-id")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.Tool != instancepresence.ToolClaude || inst.State != instancepresence.StateIdle {
		t.Fatalf("cross-tool attempt mutated the instance: %#v", inst)
	}
}

func TestIngestorRejectsEpochChangeOnLiveConnection(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	// Same connection, different epoch, no Close() in between: this must
	// never be accepted, regardless of what the registry would otherwise
	// allow as a takeover.
	_, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-2", 1, producerprotocol.StateIdle, now))
	if !errors.Is(err, ErrConnectionEpochChanged) {
		t.Fatalf("epoch change on live connection error = %v, want ErrConnectionEpochChanged", err)
	}
	inst, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.Revisions.ProducerEpoch != "epoch-1" || inst.State != instancepresence.StateWorking {
		t.Fatalf("epoch-changing message on a live connection mutated the instance: %#v", inst)
	}
}

func TestIngestorRejectsNewEpochWhileOldGenerationStillConnected(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	oldSession := ingestor.NewSession()

	if _, err := oldSession.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 9, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	// A different (not-yet-closed) connection tries to take over with a
	// new epoch while the old one is still live: must be rejected.
	newSession := ingestor.NewSession()
	_, err := newSession.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-2", 1, producerprotocol.StateIdle, now))
	if !errors.Is(err, ErrGenerationStillActive) {
		t.Fatalf("takeover while old generation live error = %v, want ErrGenerationStillActive", err)
	}
	inst, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.Revisions.ProducerEpoch != "epoch-1" || inst.State != instancepresence.StateWorking {
		t.Fatalf("rejected takeover attempt mutated the instance: %#v", inst)
	}

	// Now the old generation's connection disconnects...
	oldSession.Close()
	// ...and the new generation may take over, resuming the same
	// instance id with its revision reset to 1.
	inst, err = newSession.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-2", 1, producerprotocol.StateIdle, now))
	if err != nil {
		t.Fatalf("takeover after disconnect rejected: %v", err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-2" || inst.Revisions.HookRevision != 1 || inst.State != instancepresence.StateIdle {
		t.Fatalf("takeover did not apply: %#v", inst)
	}

	// The old epoch must never reactivate or overwrite the new generation.
	_, err = oldSession.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 10, producerprotocol.StateError, now))
	if err == nil {
		t.Fatal("old generation's connection was allowed to write after being superseded")
	}
	final, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if final.Revisions.ProducerEpoch != "epoch-2" || final.State != instancepresence.StateIdle {
		t.Fatalf("old epoch corrupted the new generation: %#v", final)
	}
}

func TestIngestorTakeoverAfterLeaseExpiryWithoutFormalDisconnect(t *testing.T) {
	// The old generation's connection never formally closes (e.g. a
	// network partition the OS hasn't yet reported), but the registry has
	// independently retired the instance via lease expiry: a new epoch
	// must still be allowed to take over ("explicit pensionerats").
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	oldSession := ingestor.NewSession()

	shortLease := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)
	shortLease.LeaseExpiresAt = now.Add(10 * time.Second)
	if _, err := oldSession.Apply(shortLease); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Second) // past lease + grace period.
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	ended, err := registry.Get("inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != instancepresence.RuntimeEnded {
		t.Fatalf("test setup: status = %v, want ended", ended.Status)
	}

	newSession := ingestor.NewSession()
	inst, err := newSession.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-2", 1, producerprotocol.StateIdle, clock.Now()))
	if err != nil {
		t.Fatalf("takeover after lease-driven retirement rejected: %v", err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-2" || inst.Status != instancepresence.RuntimeAlive {
		t.Fatalf("instance = %#v", inst)
	}
}

func TestIngestorRetiredEpochNeverReactivatesAfterSecondTakeover(t *testing.T) {
	// A -> disconnect -> B -> disconnect -> A must still be rejected: it is
	// not enough to check "is the current owner's connection live", since
	// after B disconnects, A's original epoch is not live either. Only a
	// permanent retired-epoch history distinguishes "A's own original
	// generation" from "A trying to resurrect itself after being
	// superseded".
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	sessionA := ingestor.NewSession()
	if _, err := sessionA.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-A", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	sessionA.Close()

	sessionB := ingestor.NewSession()
	if _, err := sessionB.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-B", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatalf("B takeover after A disconnect rejected: %v", err)
	}
	sessionB.Close()

	// A retries with its ORIGINAL epoch. Even though B has also since
	// disconnected (nothing "live" blocks it), A's epoch was permanently
	// retired the moment B took over.
	sessionA2 := ingestor.NewSession()
	_, err := sessionA2.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-A", 2, producerprotocol.StateWorking, now))
	if !errors.Is(err, ErrProducerEpochRetired) {
		t.Fatalf("A reactivation after B's takeover error = %v, want ErrProducerEpochRetired", err)
	}
	inst, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.Revisions.ProducerEpoch != "epoch-B" || inst.State != instancepresence.StateIdle {
		t.Fatalf("rejected A reactivation mutated the instance: %#v", inst)
	}

	// A brand new epoch C, never seen before, must still be able to take
	// over — retirement blocks only epochs this id has actually moved past,
	// not future generations in general.
	sessionC := ingestor.NewSession()
	instC, err := sessionC.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-C", 1, producerprotocol.StateWorking, now))
	if err != nil {
		t.Fatalf("brand-new epoch C takeover rejected: %v", err)
	}
	if instC.Revisions.ProducerEpoch != "epoch-C" || instC.State != instancepresence.StateWorking {
		t.Fatalf("instance = %#v", instC)
	}

	// The retired epoch must not even be able to renew a lease or change
	// state now that C owns the id.
	_, err = sessionA2.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-A", 3, producerprotocol.StateAttention, now))
	if !errors.Is(err, ErrProducerEpochRetired) {
		t.Fatalf("retired A error after C's takeover = %v, want ErrProducerEpochRetired", err)
	}
	final, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if final.Revisions.ProducerEpoch != "epoch-C" || final.State != instancepresence.StateWorking {
		t.Fatalf("retired epoch corrupted C's generation: %#v", final)
	}
}

func TestIngestorOtherInstancesUnaffectedByRetiredEpochHistory(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	// inst-a goes through a full takeover cycle...
	sessionA := ingestor.NewSession()
	if _, err := sessionA.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-A", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	sessionA.Close()
	sessionB := ingestor.NewSession()
	if _, err := sessionB.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-B", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}

	// ...while inst-other, an entirely unrelated instance id, is created and
	// advanced independently. None of inst-a's generation churn may affect it.
	other := ingestor.NewSession()
	if _, err := other.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-other", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-other", "epoch-1", 2, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	inst, err := registry.Get("inst-other")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-1" || inst.Revisions.HookRevision != 2 || inst.State != instancepresence.StateWorking {
		t.Fatalf("unrelated instance affected by inst-a's generation history: %#v", inst)
	}
}

func TestIngestorSameEpochReconnectAfterDisconnectResumes(t *testing.T) {
	// A producer that disconnects and reconnects with the SAME epoch (a
	// legitimate resume, not a takeover) must be allowed to continue.
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	first := ingestor.NewSession()
	if _, err := first.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second := ingestor.NewSession()
	inst, err := second.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 2, producerprotocol.StateIdle, now))
	if err != nil {
		t.Fatalf("same-epoch reconnect rejected: %v", err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-1" || inst.Revisions.HookRevision != 2 || inst.State != instancepresence.StateIdle {
		t.Fatalf("instance = %#v", inst)
	}
}

func TestIngestorOneConnectionCarriesMultipleInstanceIDsIndependently(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	const count = 5
	ids := make([]producerprotocol.InstanceID, count)
	for i := 0; i < count; i++ {
		ids[i] = producerprotocol.InstanceID(fmt.Sprintf("claude-session-%d", i))
		if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, ids[i], "epoch-shared", 1, producerprotocol.StateIdle, now)); err != nil {
			t.Fatalf("instance %d initial apply: %v", i, err)
		}
	}
	// Advance each instance a different number of times so their revisions
	// (and thus their leases, which are recomputed per report) diverge.
	for i, id := range ids {
		for revision := producerprotocol.Revision(2); revision <= producerprotocol.Revision(2+i); revision++ {
			msg := ingestMessage(producerprotocol.ToolClaude, id, "epoch-shared", revision, producerprotocol.StateWorking, now)
			msg.LeaseExpiresAt = now.Add(time.Duration(i+1) * 10 * time.Second)
			if _, err := session.Apply(msg); err != nil {
				t.Fatalf("instance %d revision %d: %v", i, revision, err)
			}
		}
	}
	for i, id := range ids {
		inst, err := registry.Get(instancepresence.InstanceID(id))
		if err != nil {
			t.Fatal(err)
		}
		wantRevision := instancepresence.HookRevision(2 + i)
		if inst.Revisions.HookRevision != wantRevision {
			t.Fatalf("instance %d revision = %d, want %d", i, inst.Revisions.HookRevision, wantRevision)
		}
		// Every instance received at least one working-state advance (i=0
		// runs the inner loop exactly once too), independently of the others.
		wantState := instancepresence.StateWorking
		if inst.State != wantState {
			t.Fatalf("instance %d state = %v, want %v", i, inst.State, wantState)
		}
	}

	// Closing the connection must release every instance id it claimed, not
	// just the first: a fresh session must be able to take over any of them
	// with a new epoch.
	session.Close()
	for i, id := range ids {
		takeover := ingestor.NewSession()
		inst, err := takeover.Apply(ingestMessage(producerprotocol.ToolClaude, id, "epoch-next", 1, producerprotocol.StateIdle, now))
		if err != nil {
			t.Fatalf("instance %d takeover after connection close rejected: %v", i, err)
		}
		if inst.Revisions.ProducerEpoch != "epoch-next" {
			t.Fatalf("instance %d = %#v", i, inst)
		}
	}
}

func TestIngestorConnectionCannotChangeToolMidStream(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	// Same connection, same epoch, but a DIFFERENT tool: must never be
	// accepted, even for a brand new instance id this connection has never
	// spoken about before.
	_, err := session.Apply(ingestMessage(producerprotocol.ToolCodex, "inst-b", "epoch-1", 1, producerprotocol.StateIdle, now))
	if !errors.Is(err, ErrConnectionEpochChanged) {
		t.Fatalf("tool change on live connection error = %v, want ErrConnectionEpochChanged", err)
	}
	if _, getErr := registry.Get("inst-b"); getErr == nil {
		t.Fatal("tool-changing message created an instance it should never have reached")
	}
}

func TestIngestorErrorOnOneInstanceIsolatedFromOthersOnSameConnection(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-b", "epoch-1", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	// A stale (out-of-order) revision for inst-a must be rejected without
	// affecting inst-b, which shares the same connection/session.
	_, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 0, producerprotocol.StateError, now))
	if err == nil {
		t.Fatal("revision 0 was accepted")
	}
	// inst-b must still be reachable and advanceable on the same session.
	instB, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-b", "epoch-1", 2, producerprotocol.StateWorking, now))
	if err != nil {
		t.Fatalf("inst-b affected by inst-a's rejected report: %v", err)
	}
	if instB.Revisions.HookRevision != 2 || instB.State != instancepresence.StateWorking {
		t.Fatalf("inst-b = %#v", instB)
	}
	instA, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if instA.Revisions.HookRevision != 1 || instA.State != instancepresence.StateWorking {
		t.Fatalf("inst-a corrupted by inst-b's independent progress: %#v", instA)
	}
}

func TestIngestorTwoConcurrentGenerationsCannotBothWin(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	oldSession := ingestor.NewSession()
	if _, err := oldSession.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	oldSession.Close()

	// Each goroutine represents a genuinely DIFFERENT candidate next
	// generation (distinct epoch) racing to take over — not the same
	// generation's message replayed, which claimGeneration correctly
	// treats as one continuing generation rather than a race.
	const attempts = 8
	var wait sync.WaitGroup
	results := make([]error, attempts)
	epochs := make([]producerprotocol.ProducerEpoch, attempts)
	wait.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func(index int) {
			defer wait.Done()
			session := ingestor.NewSession()
			epoch := producerprotocol.ProducerEpoch(fmt.Sprintf("epoch-candidate-%d", index))
			epochs[index] = epoch
			msg := ingestMessage(producerprotocol.ToolClaude, "inst-a", epoch, 1, producerprotocol.StateIdle, now)
			_, results[index] = session.Apply(msg)
		}(index)
	}
	wait.Wait()

	wins := 0
	var winningEpoch producerprotocol.ProducerEpoch
	for index, err := range results {
		if err == nil {
			wins++
			winningEpoch = epochs[index]
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (results=%v)", wins, results)
	}
	final, err := registry.Get("inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if final.Revisions.ProducerEpoch != instancepresence.ProducerEpoch(winningEpoch) || final.State != instancepresence.StateIdle {
		t.Fatalf("final = %#v, want epoch %q", final, winningEpoch)
	}
	// The generation map's own view of who won must agree with the
	// registry's: claims and registry must never describe different
	// accepted epochs.
	rec, ok := generationOf(t, ingestor, "inst-a")
	if !ok {
		t.Fatal("generation record missing after concurrent takeover race")
	}
	if rec.currentEpoch != instancepresence.ProducerEpoch(winningEpoch) || !rec.live {
		t.Fatalf("generation record = %#v, want live current epoch %q", rec, winningEpoch)
	}
}

// TestIngestorSecondSessionSameEpochRejectedWhileFirstIsLive is the direct
// regression test for risk 1: two concurrent ProducerSessions presenting
// the exact same (tool, producer_epoch) must never both be able to claim
// the same instance id. Before the owner token was introduced,
// claimGeneration's "epoch == rec.currentEpoch" branch treated ANY session
// presenting the current epoch as "the same generation continuing" and
// unconditionally set live=true, regardless of which connection was
// asking — so a second, entirely different connection racing in with the
// identical epoch would also succeed.
func TestIngestorSecondSessionSameEpochRejectedWhileFirstIsLive(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	sessionA := ingestor.NewSession()
	if _, err := sessionA.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-E", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatalf("A's initial claim rejected: %v", err)
	}

	// B, a genuinely different connection, presents the exact same
	// (tool, epoch) while A is still live.
	sessionB := ingestor.NewSession()
	_, err := sessionB.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-E", 1, producerprotocol.StateIdle, now))
	if !errors.Is(err, ErrGenerationStillActive) {
		t.Fatalf("B's concurrent same-epoch claim error = %v, want ErrGenerationStillActive", err)
	}
	rec, ok := generationOf(t, ingestor, "inst-a")
	if !ok || !rec.live || rec.owner != sessionA.owner {
		t.Fatalf("B's rejected claim disturbed A's ownership: rec=%#v ok=%v", rec, ok)
	}
	inst, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.State != instancepresence.StateWorking {
		t.Fatalf("B's rejected claim mutated the instance: %#v", inst)
	}

	// A closes: B may now legitimately resume with the same epoch — this
	// is the same epoch reconnecting after the real owner disconnected,
	// not a takeover.
	sessionA.Close()
	instB, err := sessionB.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-E", 2, producerprotocol.StateIdle, now))
	if err != nil {
		t.Fatalf("B's resume after A's close rejected: %v", err)
	}
	if instB.State != instancepresence.StateIdle || instB.Revisions.HookRevision != 2 {
		t.Fatalf("instance = %#v", instB)
	}
	recAfter, ok := generationOf(t, ingestor, "inst-a")
	if !ok || !recAfter.live || recAfter.owner != sessionB.owner {
		t.Fatalf("B did not become the recognized owner: rec=%#v", recAfter)
	}

	// A stale, duplicate Close (A already closed once) must never affect
	// B, the new rightful owner.
	sessionA.Close()
	sessionA.Close()
	recStillB, ok := generationOf(t, ingestor, "inst-a")
	if !ok || !recStillB.live || recStillB.owner != sessionB.owner {
		t.Fatalf("A's stale/duplicate close disturbed B's ownership: rec=%#v", recStillB)
	}
	finalInst, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if finalInst.State != instancepresence.StateIdle {
		t.Fatalf("instance disturbed by A's stale close: %#v", finalInst)
	}
}

// TestIngestorTakeoverAfterExpiryThenStaleSessionACloseDoesNotAffectB
// covers the remaining risk-1 scenario: A's connection never formally
// closes (an unnoticed network partition); its lease simply expires,
// letting B take over with a NEW epoch via the staleClaim override. A's
// session eventually "notices" and calls Close belatedly — this must never
// touch B's new generation, even though A's session was, at the time it
// was granted, the recognized owner of the epoch that has since been
// retired.
func TestIngestorTakeoverAfterExpiryThenStaleSessionACloseDoesNotAffectB(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	sessionA := ingestor.NewSession()
	shortLease := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-A", 1, producerprotocol.StateWorking, now)
	shortLease.LeaseExpiresAt = now.Add(10 * time.Second)
	if _, err := sessionA.Apply(shortLease); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Second) // past lease + grace period; A's connection never closes.
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}

	sessionB := ingestor.NewSession()
	instB, err := sessionB.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-B", 1, producerprotocol.StateIdle, clock.Now()))
	if err != nil {
		t.Fatalf("B's takeover after lease-driven retirement rejected: %v", err)
	}
	if instB.Revisions.ProducerEpoch != "epoch-B" || instB.Status != instancepresence.RuntimeAlive {
		t.Fatalf("instance = %#v", instB)
	}
	recAfterTakeover, ok := generationOf(t, ingestor, "inst-a")
	if !ok || !recAfterTakeover.live || recAfterTakeover.owner != sessionB.owner || recAfterTakeover.currentEpoch != "epoch-B" {
		t.Fatalf("B did not become the recognized owner: rec=%#v", recAfterTakeover)
	}

	// A finally "notices" its connection died and closes.
	sessionA.Close()
	recStillB, ok := generationOf(t, ingestor, "inst-a")
	if !ok || !recStillB.live || recStillB.owner != sessionB.owner || recStillB.currentEpoch != "epoch-B" {
		t.Fatalf("A's belated close after B's takeover disturbed B: rec=%#v", recStillB)
	}
	finalInst, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if finalInst.Revisions.ProducerEpoch != "epoch-B" || finalInst.State != instancepresence.StateIdle {
		t.Fatalf("A's belated close disturbed B's state: %#v", finalInst)
	}
}

// TestIngestorConcurrentSameEpochClaimsExactlyOneWins races several
// sessions presenting the identical (tool, epoch) for a brand-new instance
// id against each other under -race, complementing
// TestIngestorTwoConcurrentGenerationsCannotBothWin (which races distinct
// epochs): exactly one must become the recognized owner.
func TestIngestorConcurrentSameEpochClaimsExactlyOneWins(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	const attempts = 8
	var wait sync.WaitGroup
	results := make([]error, attempts)
	wait.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func(index int) {
			defer wait.Done()
			session := ingestor.NewSession()
			msg := ingestMessage(producerprotocol.ToolClaude, "inst-race", "epoch-shared", 1, producerprotocol.StateIdle, now)
			_, results[index] = session.Apply(msg)
		}(index)
	}
	wait.Wait()

	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrGenerationStillActive):
			// expected for every loser.
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (results=%v)", wins, results)
	}
	if len(registry.ActiveInstances()) != 1 {
		t.Fatalf("active instances = %d, want 1", len(registry.ActiveInstances()))
	}
	rec, ok := generationOf(t, ingestor, "inst-race")
	if !ok || !rec.live {
		t.Fatalf("generation record missing or not live: rec=%#v ok=%v", rec, ok)
	}
}

// TestIngestorInvalidFirstReportLeavesRegistryAndGenerationsEmpty is the
// direct regression test for risk 2's transactionality requirement for a
// previously-unknown instance id: claimGeneration tentatively creates a
// generation record before the registry has validated anything, so a
// report that instanceregistry.ProducerReport.validate rejects (here: a
// zero observed_at) must roll that tentative record back, leaving both the
// registry and the generation map exactly as if the attempt never happened.
func TestIngestorInvalidFirstReportLeavesRegistryAndGenerationsEmpty(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	invalid := ingestMessage(producerprotocol.ToolClaude, "inst-new", "epoch-1", 1, producerprotocol.StateWorking, now)
	invalid.ObservedAt = time.Time{}
	if _, err := session.Apply(invalid); err == nil {
		t.Fatal("invalid first report (zero observed_at) was accepted")
	}
	if _, getErr := registry.Get("inst-new"); getErr == nil {
		t.Fatal("invalid first report created a registry instance")
	}
	if _, ok := generationOf(t, ingestor, "inst-new"); ok {
		t.Fatal("invalid first report left a generation record behind")
	}
}

// TestIngestorFailedTakeoverLeavesCurrentEpochUnretiredThenCorrectedRetryWorks
// covers risk 2's takeover-specific transactionality requirement: a
// takeover attempt that the registry rejects must leave the PREVIOUS
// (still rightfully current) epoch exactly as it was — current and NOT
// retired — and the connection attempting the new epoch must still be able
// to retry with a corrected report afterwards.
func TestIngestorFailedTakeoverLeavesCurrentEpochUnretiredThenCorrectedRetryWorks(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	sessionA := ingestor.NewSession()
	if _, err := sessionA.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-A", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	sessionA.Close()

	sessionB := ingestor.NewSession()
	invalidTakeover := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-B", 1, producerprotocol.StateIdle, now)
	invalidTakeover.ObservedAt = time.Time{}
	if _, err := sessionB.Apply(invalidTakeover); err == nil {
		t.Fatal("invalid takeover report (zero observed_at) was accepted")
	}

	rec, ok := generationOf(t, ingestor, "inst-a")
	if !ok {
		t.Fatal("generation record disappeared after failed takeover")
	}
	if rec.currentEpoch != "epoch-A" || rec.live {
		t.Fatalf("failed takeover left current epoch/live wrong: rec=%#v", rec)
	}
	if _, retired := rec.retiredEpochs["epoch-A"]; retired {
		t.Fatal("failed takeover retired the still-current epoch-A")
	}
	instAfterFailedTakeover, getErr := registry.Get("inst-a")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if instAfterFailedTakeover.Revisions.ProducerEpoch != "epoch-A" || instAfterFailedTakeover.State != instancepresence.StateWorking {
		t.Fatalf("failed takeover mutated the registry instance: %#v", instAfterFailedTakeover)
	}

	// B retries with a corrected report using the same epoch: must now succeed.
	corrected := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-B", 1, producerprotocol.StateIdle, now)
	instB, err := sessionB.Apply(corrected)
	if err != nil {
		t.Fatalf("corrected retry of epoch-B rejected: %v", err)
	}
	if instB.Revisions.ProducerEpoch != "epoch-B" || instB.State != instancepresence.StateIdle {
		t.Fatalf("instance = %#v", instB)
	}
}

// TestIngestorFailedTakeoverDoesNotBlockADifferentNewEpoch complements the
// above: a failed takeover attempt under epoch-B must leave no residue
// (bogus retired-epoch entry, stuck live flag, ...) that would block an
// entirely different, genuinely new epoch-C from taking over normally
// afterwards.
func TestIngestorFailedTakeoverDoesNotBlockADifferentNewEpoch(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	sessionA := ingestor.NewSession()
	if _, err := sessionA.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-A", 1, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	sessionA.Close()

	sessionB := ingestor.NewSession()
	invalidTakeover := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-B", 1, producerprotocol.StateIdle, now)
	invalidTakeover.ObservedAt = time.Time{}
	if _, err := sessionB.Apply(invalidTakeover); err == nil {
		t.Fatal("invalid takeover report accepted")
	}

	sessionC := ingestor.NewSession()
	instC, err := sessionC.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-C", 1, producerprotocol.StateWorking, now))
	if err != nil {
		t.Fatalf("epoch-C takeover rejected after B's failed attempt: %v", err)
	}
	if instC.Revisions.ProducerEpoch != "epoch-C" {
		t.Fatalf("instance = %#v", instC)
	}
}

// TestIngestorStaleOrConflictingReportDoesNotChangeOwnerLiveOrRetired
// covers risk 2's requirement that a rejected report from the connection
// that ALREADY owns a generation (the "nothing to roll back" fast path in
// claimGeneration) really does leave owner/live/retiredEpochs untouched,
// not just the registry.
func TestIngestorStaleOrConflictingReportDoesNotChangeOwnerLiveOrRetired(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()

	session := ingestor.NewSession()
	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 5, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	before, ok := generationOf(t, ingestor, "inst-a")
	if !ok {
		t.Fatal("missing generation record")
	}

	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 4, producerprotocol.StateError, now)); err == nil {
		t.Fatal("stale revision accepted")
	}
	conflicting := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 5, producerprotocol.StateAttention, now)
	conflicting.ObservedAt = now.Add(time.Second)
	if _, err := session.Apply(conflicting); err == nil {
		t.Fatal("conflicting same-revision payload accepted")
	}

	after, ok := generationOf(t, ingestor, "inst-a")
	if !ok {
		t.Fatal("generation record disappeared")
	}
	if after.owner != before.owner || after.live != before.live ||
		after.currentEpoch != before.currentEpoch || len(after.retiredEpochs) != len(before.retiredEpochs) {
		t.Fatalf("rejected report changed generation record: before=%#v after=%#v", before, after)
	}
}

func TestIngestorEveryAcceptedMessageRenewsLease(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	msg := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateIdle, now)
	msg.LeaseExpiresAt = now.Add(20 * time.Second)
	if _, err := session.Apply(msg); err != nil {
		t.Fatal(err)
	}
	clock.Advance(15 * time.Second)
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	if len(registry.ActiveInstances()) != 1 {
		t.Fatal("instance unexpectedly missing before its lease should have expired")
	}

	renew := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 2, producerprotocol.StateIdle, clock.Now())
	renew.LeaseExpiresAt = clock.Now().Add(20 * time.Second)
	if _, err := session.Apply(renew); err != nil {
		t.Fatal(err)
	}
	clock.Advance(15 * time.Second)
	result, err := registry.ExpireLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SuspectMissing) != 0 || len(result.Ended) != 0 {
		t.Fatalf("expiry result = %#v, want no transitions after renewal", result)
	}
}

func TestIngestorRejectsLeaseExceedingMaximum(t *testing.T) {
	registry, clock := newIngestTestRegistry(t) // MaximumProducerLeaseDuration: 5 minutes.
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	session := ingestor.NewSession()

	msg := ingestMessage(producerprotocol.ToolClaude, "inst-a", "epoch-1", 1, producerprotocol.StateWorking, now)
	msg.LeaseExpiresAt = now.Add(time.Hour)
	_, err := session.Apply(msg)
	if !errors.Is(err, instanceregistry.ErrLeaseExceedsMaximum) {
		t.Fatalf("error = %v, want ErrLeaseExceedsMaximum", err)
	}
	if len(registry.ActiveInstances()) != 0 {
		t.Fatal("rejected report created an instance")
	}
}

func TestClassifyIngestErrorNeverLeaksInstanceID(t *testing.T) {
	registry, clock := newIngestTestRegistry(t)
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	const marker = "leak-canary-instance-id-9f2b1c"
	session := ingestor.NewSession()

	if _, err := session.Apply(ingestMessage(producerprotocol.ToolClaude, marker, "epoch-1", 5, producerprotocol.StateWorking, now)); err != nil {
		t.Fatal(err)
	}
	_, staleErr := session.Apply(ingestMessage(producerprotocol.ToolClaude, marker, "epoch-1", 1, producerprotocol.StateIdle, now))
	otherSession := ingestor.NewSession()
	_, crossToolErr := otherSession.Apply(ingestMessage(producerprotocol.ToolCodex, marker, "epoch-1", 6, producerprotocol.StateIdle, now))
	_, epochErr := otherSession.Apply(ingestMessage(producerprotocol.ToolClaude, marker, "epoch-2", 1, producerprotocol.StateIdle, now))

	// A retired-epoch reactivation attempt: close the original session,
	// let a new epoch take over (retiring epoch-1), then have the
	// original epoch try again.
	session.Close()
	takeoverSession := ingestor.NewSession()
	if _, err := takeoverSession.Apply(ingestMessage(producerprotocol.ToolClaude, marker, "epoch-3", 1, producerprotocol.StateIdle, now)); err != nil {
		t.Fatal(err)
	}
	retiredSession := ingestor.NewSession()
	_, retiredErr := retiredSession.Apply(ingestMessage(producerprotocol.ToolClaude, marker, "epoch-1", 6, producerprotocol.StateError, now))

	// A future-dated observed_at, past the registry's clock skew budget
	// (this registry leaves MaximumClockSkew unconfigured, so this instead
	// exercises the reanchored max-lease check — still an error path whose
	// classification must stay marker-free). Sent by takeoverSession itself
	// (the actual, already-live owner of epoch-3): a fresh session
	// presenting the same already-owned epoch would now correctly be
	// rejected as ErrGenerationStillActive by the owner check instead (see
	// TestIngestorSecondSessionSameEpochRejectedWhileFirstIsLive).
	skewed := ingestMessage(producerprotocol.ToolClaude, marker, "epoch-3", 2, producerprotocol.StateWorking, now.Add(time.Hour))
	skewed.LeaseExpiresAt = skewed.ObservedAt.Add(time.Minute)
	_, skewErr := takeoverSession.Apply(skewed)

	for name, err := range map[string]error{
		"stale": staleErr, "crossTool": crossToolErr, "epoch": epochErr,
		"retired": retiredErr, "leaseAnchor": skewErr,
	} {
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		code := ClassifyIngestError(err)
		if code == "" || code == "internal_error" {
			t.Fatalf("%s: unexpected classification %q for %v", name, code, err)
		}
		if strings.Contains(code, marker) {
			t.Fatalf("%s: classification leaked instance id: %q", name, code)
		}
	}
	if ClassifyIngestError(retiredErr) != "producer_epoch_retired" {
		t.Fatalf("retired classification = %q, want producer_epoch_retired", ClassifyIngestError(retiredErr))
	}
	if ClassifyIngestError(skewErr) != "lease_exceeds_maximum" {
		t.Fatalf("skew classification = %q, want lease_exceeds_maximum", ClassifyIngestError(skewErr))
	}
}

func TestNewIngestorValidatesArguments(t *testing.T) {
	registry, _ := newIngestTestRegistry(t)
	if _, err := NewIngestor(nil, "host", "collector"); err == nil {
		t.Fatal("nil registry accepted")
	}
	if _, err := NewIngestor(registry, "", "collector"); err == nil {
		t.Fatal("empty host ID accepted")
	}
	if _, err := NewIngestor(registry, "host", ""); err == nil {
		t.Fatal("empty collector ID accepted")
	}
}
