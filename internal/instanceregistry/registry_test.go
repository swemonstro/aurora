package instanceregistry

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

type fakeClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.RLock()
	defer clock.mu.RUnlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

func testTime() time.Time { return time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC) }

func newTestRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: testTime()}
	registry, err := New(Config{
		Clock: clock, SlotNamespace: "default", LeaseDuration: 10 * time.Second, GracePeriod: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return registry, clock
}

func registration(id string, pid uint64) Registration {
	return Registration{
		InstanceID: instancepresence.InstanceID(id), Tool: instancepresence.ToolCodex,
		Source: instancepresence.SourceDescriptor{Provider: "codex-api", Profile: "default", CollectorID: "collector-a"},
		Runtime: instancepresence.RuntimeIdentity{
			HostID: "host-a", BootID: "boot-a",
			RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: testTime().Add(time.Duration(pid) * time.Millisecond)},
		},
		ProducerEpoch: "epoch-a", RuntimeRevision: 1, ObservedAt: testTime(), IdempotencyKey: "register-" + id,
	}
}

func runtimeMutation(revision uint64, status instancepresence.RuntimeStatus, key string) presencev2.RuntimeMutation {
	return presencev2.RuntimeMutation{
		ProducerEpoch: "epoch-a", RuntimeRevision: instancepresence.RuntimeRevision(revision),
		Status: status, ObservedAt: testTime().Add(time.Duration(revision) * time.Second), IdempotencyKey: key,
	}
}

func hookMutation(revision uint64, state instancepresence.EffectiveState, key string) presencev2.HookStateMutation {
	return presencev2.HookStateMutation{
		ProducerEpoch: "epoch-a", HookRevision: instancepresence.HookRevision(revision),
		State: state, ObservedAt: testTime().Add(time.Duration(revision) * time.Second), IdempotencyKey: key,
	}
}

func mustRegister(t *testing.T, registry *Registry, value Registration) instancepresence.Instance {
	t.Helper()
	instance, err := registry.Register(value)
	if err != nil {
		t.Fatalf("Register(%q) error = %v", value.InstanceID, err)
	}
	return instance
}

func TestRegistrationRequiresCanonicalIdentityFields(t *testing.T) {
	registry, _ := newTestRegistry(t)
	tests := []struct {
		name   string
		mutate func(*Registration)
	}{
		{"instance ID", func(value *Registration) { value.InstanceID = "" }},
		{"tool", func(value *Registration) { value.Tool = "" }},
		{"source", func(value *Registration) { value.Source = instancepresence.SourceDescriptor{} }},
		{"runtime", func(value *Registration) { value.Runtime = instancepresence.RuntimeIdentity{} }},
		{"producer epoch", func(value *Registration) { value.ProducerEpoch = "" }},
		{"runtime revision", func(value *Registration) { value.RuntimeRevision = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := registration("instance-a", 101)
			test.mutate(&value)
			if _, err := registry.Register(value); err == nil {
				t.Fatal("invalid registration was accepted")
			}
		})
	}
}

func TestRegistrationIdentityAndIdempotency(t *testing.T) {
	registry, clock := newTestRegistry(t)
	firstRegistration := registration("instance-a", 101)
	first := mustRegister(t, registry, firstRegistration)
	clock.Advance(time.Second)
	retry := mustRegister(t, registry, firstRegistration)
	if !reflect.DeepEqual(first, retry) {
		t.Fatalf("exact registration retry changed instance:\nfirst = %#v\nretry = %#v", first, retry)
	}

	t.Run("same instance ID with another runtime", func(t *testing.T) {
		conflict := registration("instance-a", 202)
		if _, err := registry.Register(conflict); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("Register() error = %v, want identity conflict", err)
		}
	})

	t.Run("same active runtime with another instance ID", func(t *testing.T) {
		conflict := firstRegistration
		conflict.InstanceID = "instance-b"
		conflict.IdempotencyKey = "register-instance-b"
		conflict.Runtime.RootProcess.StartedAt = conflict.Runtime.RootProcess.StartedAt.In(time.FixedZone("other", 2*60*60))
		if _, err := registry.Register(conflict); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("Register() error = %v, want identity conflict", err)
		}
	})
}

func TestInstancesWithSameSourceRemainSeparate(t *testing.T) {
	registry, _ := newTestRegistry(t)
	first := mustRegister(t, registry, registration("instance-a", 101))
	second := mustRegister(t, registry, registration("instance-b", 202))
	if first.Source != second.Source {
		t.Fatal("fixture sources differ")
	}
	if first.ID == second.ID || first.Slot.Index == second.Slot.Index {
		t.Fatalf("same-source instances collided: %#v %#v", first, second)
	}
	snapshot := registry.ActiveInstances()
	if len(snapshot) != 2 {
		t.Fatalf("active instance count = %d, want 2", len(snapshot))
	}
}

func TestRuntimeRevisionRules(t *testing.T) {
	registry, clock := newTestRegistry(t)
	mustRegister(t, registry, registration("instance-a", 101))
	clock.Advance(3 * time.Second)
	accepted := runtimeMutation(2, instancepresence.RuntimeAlive, "runtime-2")
	accepted.ObservedAt = testTime().Add(-time.Hour)
	updated, err := registry.ApplyRuntimeMutation("instance-a", accepted)
	if err != nil {
		t.Fatalf("first mutation error = %v", err)
	}
	if !updated.Lifecycle.LastSeenAt.Equal(clock.Now()) || !updated.Lifecycle.LeaseExpiresAt.Equal(clock.Now().Add(10*time.Second)) {
		t.Fatalf("server lifecycle did not use injected clock: %#v", updated.Lifecycle)
	}

	stale := runtimeMutation(1, instancepresence.RuntimeAlive, "stale")
	if _, err := registry.ApplyRuntimeMutation("instance-a", stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale mutation error = %v", err)
	}
	if _, err := registry.ApplyRuntimeMutation("instance-a", accepted); err != nil {
		t.Fatalf("identical duplicate error = %v", err)
	}
	samePayloadNewKey := accepted
	samePayloadNewKey.IdempotencyKey = "runtime-2-retry"
	if _, err := registry.ApplyRuntimeMutation("instance-a", samePayloadNewKey); err != nil {
		t.Fatalf("same payload with new key error = %v", err)
	}
	conflict := accepted
	conflict.Status = instancepresence.RuntimeSuspectMissing
	conflict.IdempotencyKey = "runtime-2-conflict"
	if _, err := registry.ApplyRuntimeMutation("instance-a", conflict); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("same revision conflict error = %v", err)
	}
	reusedKey := runtimeMutation(3, instancepresence.RuntimeAlive, "runtime-2")
	if _, err := registry.ApplyRuntimeMutation("instance-a", reusedKey); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reused idempotency key error = %v", err)
	}
	differentEpoch := runtimeMutation(3, instancepresence.RuntimeAlive, "other-epoch")
	differentEpoch.ProducerEpoch = "epoch-b"
	if _, err := registry.ApplyRuntimeMutation("instance-a", differentEpoch); !errors.Is(err, ErrEpochConflict) {
		t.Fatalf("epoch conflict error = %v", err)
	}
}

func TestHookRevisionRulesAndClaimOwnership(t *testing.T) {
	registry, clock := newTestRegistry(t)
	mustRegister(t, registry, registration("instance-a", 101))
	accepted := hookMutation(1, instancepresence.StateWorking, "hook-1")
	working, err := registry.ApplyHookMutation("instance-a", accepted)
	if err != nil || working.State != instancepresence.StateWorking {
		t.Fatalf("working mutation = %#v, %v", working, err)
	}
	changedAt := working.Lifecycle.StateChangedAt

	// Wire validation rejects revision zero before ordering, so establish a
	// later revision and use the prior positive revision as the stale case.
	clock.Advance(time.Second)
	attention := hookMutation(2, instancepresence.StateAttention, "hook-2")
	if _, err := registry.ApplyHookMutation("instance-a", attention); err != nil {
		t.Fatalf("attention mutation error = %v", err)
	}
	stale := accepted
	stale.IdempotencyKey = "hook-stale"
	if _, err := registry.ApplyHookMutation("instance-a", stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale hook error = %v", err)
	}
	if _, err := registry.ApplyHookMutation("instance-a", attention); err != nil {
		t.Fatalf("identical hook duplicate error = %v", err)
	}
	conflict := attention
	conflict.State = instancepresence.StateError
	conflict.IdempotencyKey = "hook-2-conflict"
	if _, err := registry.ApplyHookMutation("instance-a", conflict); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("same hook revision conflict error = %v", err)
	}

	clock.Advance(time.Second)
	runtime := runtimeMutation(2, instancepresence.RuntimeAlive, "runtime-2")
	afterPoll, err := registry.ApplyRuntimeMutation("instance-a", runtime)
	if err != nil || afterPoll.State != instancepresence.StateAttention {
		t.Fatalf("runtime poll lowered hook state: %#v, %v", afterPoll, err)
	}
	if !afterPoll.Lifecycle.StateChangedAt.After(changedAt) {
		t.Fatal("hook effective-state transition did not advance state-changed-at")
	}

	clock.Advance(time.Second)
	idle, err := registry.ApplyHookMutation("instance-a", hookMutation(3, instancepresence.StateIdle, "hook-3"))
	if err != nil || idle.State != instancepresence.StateIdle || idle.HookClaim != instancepresence.NoHookClaim {
		t.Fatalf("hook idle did not clear claim: %#v, %v", idle, err)
	}
}

func TestHookMutationIdempotencyEpochAndEndedRuntime(t *testing.T) {
	t.Run("same revision and payload with new idempotency key is an idempotent retry", func(t *testing.T) {
		registry, _ := newTestRegistry(t)
		mustRegister(t, registry, registration("instance-a", 101))
		accepted := hookMutation(1, instancepresence.StateWorking, "hook-1")
		first, err := registry.ApplyHookMutation("instance-a", accepted)
		if err != nil {
			t.Fatalf("first hook mutation error = %v", err)
		}

		retry := accepted
		retry.IdempotencyKey = "hook-1-retry"
		got, err := registry.ApplyHookMutation("instance-a", retry)
		if err != nil {
			t.Fatalf("same hook payload with new idempotency key error = %v", err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("idempotent retry changed instance:\nfirst = %#v\nretry = %#v", first, got)
		}
	})

	t.Run("reused idempotency key with another payload conflicts", func(t *testing.T) {
		registry, _ := newTestRegistry(t)
		mustRegister(t, registry, registration("instance-a", 101))
		if _, err := registry.ApplyHookMutation("instance-a", hookMutation(1, instancepresence.StateWorking, "hook-1")); err != nil {
			t.Fatalf("first hook mutation error = %v", err)
		}

		reusedKey := hookMutation(2, instancepresence.StateAttention, "hook-1")
		if _, err := registry.ApplyHookMutation("instance-a", reusedKey); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("reused hook idempotency key error = %v, want %v", err, ErrIdempotencyConflict)
		}
	})

	t.Run("different producer epoch conflicts", func(t *testing.T) {
		registry, _ := newTestRegistry(t)
		mustRegister(t, registry, registration("instance-a", 101))
		mutation := hookMutation(1, instancepresence.StateWorking, "hook-other-epoch")
		mutation.ProducerEpoch = "epoch-b"
		if _, err := registry.ApplyHookMutation("instance-a", mutation); !errors.Is(err, ErrEpochConflict) {
			t.Fatalf("hook producer epoch error = %v, want %v", err, ErrEpochConflict)
		}
	})

	t.Run("accepted identical retry remains idempotent after runtime end but a higher revision is rejected", func(t *testing.T) {
		registry, _ := newTestRegistry(t)
		mustRegister(t, registry, registration("instance-a", 101))
		accepted := hookMutation(1, instancepresence.StateWorking, "hook-1")
		if _, err := registry.ApplyHookMutation("instance-a", accepted); err != nil {
			t.Fatalf("accepted hook mutation error = %v", err)
		}
		ended, err := registry.EndRuntime("instance-a", runtimeMutation(2, instancepresence.RuntimeEnded, "end-2"))
		if err != nil {
			t.Fatalf("EndRuntime() error = %v", err)
		}

		retry := accepted
		retry.IdempotencyKey = "hook-1-after-end"
		got, err := registry.ApplyHookMutation("instance-a", retry)
		if err != nil {
			t.Fatalf("accepted hook retry after end error = %v", err)
		}
		if !reflect.DeepEqual(got, ended) {
			t.Fatalf("accepted hook retry changed ended instance:\nended = %#v\nretry = %#v", ended, got)
		}

		newer := hookMutation(2, instancepresence.StateAttention, "hook-2-after-end")
		if _, err := registry.ApplyHookMutation("instance-a", newer); !errors.Is(err, ErrRuntimeEnded) {
			t.Fatalf("new hook revision after end error = %v, want %v", err, ErrRuntimeEnded)
		}
	})
}

func TestAllHookClaimsSurviveRuntimePoll(t *testing.T) {
	for index, state := range []instancepresence.EffectiveState{
		instancepresence.StateWorking, instancepresence.StateAttention, instancepresence.StateError,
	} {
		t.Run(string(state), func(t *testing.T) {
			registry, _ := newTestRegistry(t)
			mustRegister(t, registry, registration("instance-a", uint64(100+index)))
			if _, err := registry.ApplyHookMutation("instance-a", hookMutation(1, state, "hook-1")); err != nil {
				t.Fatal(err)
			}
			got, err := registry.ApplyRuntimeMutation("instance-a", runtimeMutation(2, instancepresence.RuntimeAlive, "runtime-2"))
			if err != nil || got.State != state {
				t.Fatalf("runtime update state = %q, %v, want %q", got.State, err, state)
			}
		})
	}
}

func TestEndAndLeaseLifecycle(t *testing.T) {
	t.Run("explicit end", func(t *testing.T) {
		registry, _ := newTestRegistry(t)
		mustRegister(t, registry, registration("instance-a", 101))
		ended, err := registry.EndRuntime("instance-a", runtimeMutation(2, instancepresence.RuntimeEnded, "end-2"))
		if err != nil || ended.Status != instancepresence.RuntimeEnded {
			t.Fatalf("EndRuntime() = %#v, %v", ended, err)
		}
		if len(registry.ActiveInstances()) != 0 {
			t.Fatal("ended runtime remained in active snapshot")
		}
		if duplicate, err := registry.EndRuntime("instance-a", runtimeMutation(2, instancepresence.RuntimeEnded, "end-2")); err != nil || duplicate.Status != instancepresence.RuntimeEnded {
			t.Fatalf("duplicate end = %#v, %v", duplicate, err)
		}
		if _, err := registry.ApplyRuntimeMutation("instance-a", runtimeMutation(3, instancepresence.RuntimeAlive, "revive")); !errors.Is(err, ErrRuntimeEnded) {
			t.Fatalf("runtime revival error = %v", err)
		}
		if _, err := registry.ApplyHookMutation("instance-a", hookMutation(1, instancepresence.StateWorking, "revive-hook")); !errors.Is(err, ErrRuntimeEnded) {
			t.Fatalf("hook revival error = %v", err)
		}
	})

	t.Run("lease expiry clears hook at end", func(t *testing.T) {
		registry, clock := newTestRegistry(t)
		mustRegister(t, registry, registration("instance-a", 101))
		if _, err := registry.ApplyHookMutation("instance-a", hookMutation(1, instancepresence.StateError, "hook-1")); err != nil {
			t.Fatal(err)
		}
		clock.Advance(10 * time.Second)
		result, err := registry.ExpireLeases()
		if err != nil || !reflect.DeepEqual(result.SuspectMissing, []instancepresence.InstanceID{"instance-a"}) {
			t.Fatalf("suspect expiry = %#v, %v", result, err)
		}
		suspect, _ := registry.Get("instance-a")
		if suspect.Status != instancepresence.RuntimeSuspectMissing || suspect.State != instancepresence.StateError {
			t.Fatalf("suspect instance = %#v", suspect)
		}
		clock.Advance(5 * time.Second)
		result, err = registry.ExpireLeases()
		if err != nil || !reflect.DeepEqual(result.Ended, []instancepresence.InstanceID{"instance-a"}) {
			t.Fatalf("ended expiry = %#v, %v", result, err)
		}
		ended, _ := registry.Get("instance-a")
		if ended.Status != instancepresence.RuntimeEnded || ended.HookClaim != instancepresence.NoHookClaim {
			t.Fatalf("expired tombstone = %#v", ended)
		}
		if len(registry.ActiveInstances()) != 0 {
			t.Fatal("hook claim kept expired runtime active")
		}
	})

	t.Run("higher revision recovers suspect before grace", func(t *testing.T) {
		registry, clock := newTestRegistry(t)
		mustRegister(t, registry, registration("instance-a", 101))
		clock.Advance(10 * time.Second)
		if _, err := registry.ExpireLeases(); err != nil {
			t.Fatal(err)
		}
		recovered, err := registry.ApplyRuntimeMutation("instance-a", runtimeMutation(2, instancepresence.RuntimeAlive, "recover-2"))
		if err != nil || recovered.Status != instancepresence.RuntimeAlive {
			t.Fatalf("recovery = %#v, %v", recovered, err)
		}
		clock.Advance(5 * time.Second)
		if result, err := registry.ExpireLeases(); err != nil || len(result.Ended) != 0 || recovered.Status == instancepresence.RuntimeEnded {
			t.Fatalf("recovered runtime ended during old grace: %#v, %v", result, err)
		}
	})
}

func TestSlotsKeepGapsAndReuseLowestReleased(t *testing.T) {
	registry, clock := newTestRegistry(t)
	for index, id := range []string{"instance-a", "instance-b", "instance-c"} {
		got := mustRegister(t, registry, registration(id, uint64(101+index)))
		if got.Slot.Index != uint64(index) {
			t.Fatalf("%s slot = %d, want %d", id, got.Slot.Index, index)
		}
	}
	clock.Advance(10 * time.Second)
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	beforeRelease := mustRegister(t, registry, registration("instance-d", 404))
	if beforeRelease.Slot.Index != 3 {
		t.Fatalf("slot reused during suspect grace: got %d, want 3", beforeRelease.Slot.Index)
	}
	// Refresh all but the middle instance, then let its grace complete.
	for index, id := range []instancepresence.InstanceID{"instance-a", "instance-c", "instance-d"} {
		if _, err := registry.ApplyRuntimeMutation(id, runtimeMutation(2, instancepresence.RuntimeAlive, fmt.Sprintf("refresh-%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(5 * time.Second)
	if _, err := registry.ExpireLeases(); err != nil {
		t.Fatal(err)
	}
	active := registry.ActiveInstances()
	if got := []uint64{active[0].Slot.Index, active[1].Slot.Index, active[2].Slot.Index}; !reflect.DeepEqual(got, []uint64{0, 2, 3}) {
		t.Fatalf("slots compacted to %v", got)
	}
	replacement := mustRegister(t, registry, registration("instance-e", 505))
	if replacement.Slot.Index != 1 {
		t.Fatalf("replacement slot = %d, want released slot 1", replacement.Slot.Index)
	}
}

func TestReadResultsAreDetached(t *testing.T) {
	registry, _ := newTestRegistry(t)
	mustRegister(t, registry, registration("instance-a", 101))
	got, err := registry.Get("instance-a")
	if err != nil {
		t.Fatal(err)
	}
	got.Source.Provider = "mutated"
	got.Runtime.HostID = "mutated"
	got.Lifecycle.EndedAt = timePointer(testTime())
	list := registry.ActiveInstances()
	list[0].Source.Profile = "mutated"
	list = append(list, instancepresence.Instance{})

	again, _ := registry.Get("instance-a")
	if again.Source.Provider != "codex-api" || again.Source.Profile != "default" || again.Runtime.HostID != "host-a" || again.Lifecycle.EndedAt != nil {
		t.Fatalf("caller mutation leaked into registry: %#v", again)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	registry, _ := newTestRegistry(t)
	_, err := registry.Get("missing")
	var domain *DomainError
	if !errors.Is(err, ErrNotFound) || !errors.As(err, &domain) {
		t.Fatalf("Get() error = %v", err)
	}
}

func TestConcurrentReadsAndMutations(t *testing.T) {
	registry, _ := newTestRegistry(t)
	const count = 32
	for index := 0; index < count; index++ {
		mustRegister(t, registry, registration(fmt.Sprintf("instance-%02d", index), uint64(1000+index)))
	}
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		index := index
		wait.Add(2)
		go func() {
			defer wait.Done()
			for revision := uint64(2); revision < 20; revision++ {
				mutation := runtimeMutation(revision, instancepresence.RuntimeAlive, fmt.Sprintf("runtime-%d-%d", index, revision))
				if _, err := registry.ApplyRuntimeMutation(instancepresence.InstanceID(fmt.Sprintf("instance-%02d", index)), mutation); err != nil {
					t.Errorf("runtime mutation error = %v", err)
					return
				}
			}
		}()
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 50; iteration++ {
				_ = registry.ActiveInstances()
				if _, err := registry.CanonicalSnapshot(); err != nil {
					t.Errorf("canonical snapshot error = %v", err)
					return
				}
				if _, err := registry.Presentation(8); err != nil {
					t.Errorf("presentation error = %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	if got := len(registry.ActiveInstances()); got != count {
		t.Fatalf("active count = %d, want %d", got, count)
	}
}

func TestRegistryAliveSuspendedAlivePreservesSlotAndHookClaim(t *testing.T) {
	registry, clock := newTestRegistry(t)
	mustRegister(t, registry, registration("instance-a", 101))
	working, err := registry.ApplyHookMutation("instance-a", hookMutation(1, instancepresence.StateWorking, "hook-1"))
	if err != nil {
		t.Fatal(err)
	}
	slot := working.Slot
	if working.State != instancepresence.StateWorking || working.HookClaim != instancepresence.ClaimWorking {
		t.Fatalf("working = %#v", working)
	}

	clock.Advance(time.Second)
	suspended, err := registry.ApplyRuntimeMutation("instance-a", runtimeMutation(2, instancepresence.RuntimeSuspended, "suspend-2"))
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Status != instancepresence.RuntimeSuspended {
		t.Fatalf("status = %q", suspended.Status)
	}
	if suspended.State != instancepresence.StateAttention {
		t.Fatalf("suspended state = %q, want attention", suspended.State)
	}
	if suspended.HookClaim != instancepresence.ClaimWorking {
		t.Fatalf("hook claim cleared on suspend: %q", suspended.HookClaim)
	}
	if suspended.Slot != slot {
		t.Fatalf("slot changed: %#v vs %#v", suspended.Slot, slot)
	}
	if suspended.Revisions.HookRevision != 1 {
		t.Fatalf("hook revision changed on suspend: %d", suspended.Revisions.HookRevision)
	}

	// Idle hook under suspend clears claim but effective state stays attention.
	clock.Advance(time.Second)
	idle, err := registry.ApplyHookMutation("instance-a", hookMutation(2, instancepresence.StateIdle, "hook-2"))
	if err != nil {
		t.Fatal(err)
	}
	if idle.HookClaim != instancepresence.NoHookClaim {
		t.Fatalf("claim = %q", idle.HookClaim)
	}
	if idle.State != instancepresence.StateAttention {
		t.Fatalf("idle under suspend state = %q", idle.State)
	}

	clock.Advance(time.Second)
	// Restore working claim then resume.
	if _, err := registry.ApplyHookMutation("instance-a", hookMutation(3, instancepresence.StateWorking, "hook-3")); err != nil {
		t.Fatal(err)
	}
	resumed, err := registry.ApplyRuntimeMutation("instance-a", runtimeMutation(3, instancepresence.RuntimeAlive, "resume-3"))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != instancepresence.RuntimeAlive || resumed.State != instancepresence.StateWorking {
		t.Fatalf("resumed = %#v", resumed)
	}
	if resumed.Slot != slot {
		t.Fatalf("slot after resume = %#v", resumed.Slot)
	}
}

func TestRegisterSuspendedStartsAsAttention(t *testing.T) {
	registry, _ := newTestRegistry(t)
	value := registration("instance-s", 202)
	value.Status = instancepresence.RuntimeSuspended
	inst, err := registry.Register(value)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != instancepresence.RuntimeSuspended || inst.State != instancepresence.StateAttention {
		t.Fatalf("register suspended = %#v", inst)
	}
	if inst.HookClaim != instancepresence.NoHookClaim {
		t.Fatalf("claim = %q", inst.HookClaim)
	}
}

func TestApplyNextHookMutationSequencesPerRuntime(t *testing.T) {
	registry, _ := newTestRegistry(t)
	mustRegister(t, registry, registration("instance-a", 101))
	now := testTime()

	first, err := registry.ApplyNextHookMutation("instance-a", "epoch-a", instancepresence.StateWorking, now, "next-1")
	if err != nil || first.Revisions.HookRevision != 1 || first.State != instancepresence.StateWorking {
		t.Fatalf("first = %#v err=%v", first, err)
	}
	second, err := registry.ApplyNextHookMutation("instance-a", "epoch-a", instancepresence.StateIdle, now.Add(time.Second), "next-2")
	if err != nil || second.Revisions.HookRevision != 2 || second.State != instancepresence.StateIdle {
		t.Fatalf("second = %#v err=%v", second, err)
	}
	// Idempotent retry of the same key does not advance revision.
	retry, err := registry.ApplyNextHookMutation("instance-a", "epoch-a", instancepresence.StateIdle, now.Add(time.Second), "next-2")
	if err != nil || retry.Revisions.HookRevision != 2 {
		t.Fatalf("retry = %#v err=%v", retry, err)
	}
	// Same key, different payload conflicts.
	if _, err := registry.ApplyNextHookMutation("instance-a", "epoch-a", instancepresence.StateWorking, now.Add(2*time.Second), "next-2"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	if _, err := registry.ApplyNextHookMutation("instance-a", "epoch-b", instancepresence.StateWorking, now.Add(3*time.Second), "next-3"); !errors.Is(err, ErrEpochConflict) {
		t.Fatalf("epoch conflict = %v", err)
	}
}

func TestApplyNextHookMutationConcurrentUniqueKeys(t *testing.T) {
	registry, _ := newTestRegistry(t)
	mustRegister(t, registry, registration("instance-a", 101))
	now := testTime()
	const workers = 20
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	wait.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wait.Done()
			_, err := registry.ApplyNextHookMutation(
				"instance-a", "epoch-a", instancepresence.StateWorking,
				now.Add(time.Duration(i)*time.Millisecond),
				fmt.Sprintf("concurrent-%d", i),
			)
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	var okCount int
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent mutation error = %v", err)
		}
		okCount++
	}
	if okCount != workers {
		t.Fatalf("okCount = %d", okCount)
	}
	got, err := registry.Get("instance-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revisions.HookRevision != instancepresence.HookRevision(workers) {
		t.Fatalf("HookRevision = %d, want %d", got.Revisions.HookRevision, workers)
	}
	if got.State != instancepresence.StateWorking {
		t.Fatalf("state = %q", got.State)
	}
}
