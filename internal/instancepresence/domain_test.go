package instancepresence

import (
	"reflect"
	"testing"
	"time"
)

func fixedTestTime() time.Time {
	return time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
}

func validInstance(id InstanceID, pid uint64, startedAt time.Time) Instance {
	now := fixedTestTime()
	return Instance{
		ID:     id,
		Tool:   ToolClaude,
		Source: SourceDescriptor{Provider: "claude", Profile: "default", CollectorID: "collector-alpha"},
		Runtime: RuntimeIdentity{
			HostID:      "host-alpha",
			BootID:      "boot-alpha",
			RootProcess: ProcessIdentity{PID: pid, StartedAt: startedAt},
		},
		Status: RuntimeAlive,
		State:  StateIdle,
		Slot:   Slot{Namespace: "default", Index: pid, AssignedAt: now},
		Lifecycle: LifecycleTimestamps{
			DiscoveredAt:   now,
			LastSeenAt:     now,
			LeaseExpiresAt: now.Add(time.Minute),
			StateChangedAt: now,
		},
		Revisions: Revisions{ProducerEpoch: "epoch-alpha", RuntimeRevision: 1},
	}
}

func TestSameSourceDoesNotDefineInstanceIdentity(t *testing.T) {
	now := fixedTestTime()
	first := validInstance("instance-alpha", 101, now)
	second := validInstance("instance-beta", 102, now.Add(time.Second))

	if first.Source != second.Source {
		t.Fatal("fixture sources unexpectedly differ")
	}
	registry := map[InstanceID]Instance{first.ID: first, second.ID: second}
	if len(registry) != 2 {
		t.Fatalf("same-source instances collapsed: got %d entries", len(registry))
	}
	if reflect.TypeOf(SourceDescriptor{}) == reflect.TypeOf(InstanceID("")) {
		t.Fatal("source descriptor and instance ID unexpectedly share a type")
	}
}

func TestProcessIdentityRequiresStartTime(t *testing.T) {
	if err := (ProcessIdentity{PID: 101}).Validate(); err == nil {
		t.Fatal("PID without start time was accepted")
	}
}

func TestPIDReuseCreatesDifferentRuntimeIdentity(t *testing.T) {
	first := RuntimeIdentity{HostID: "host-alpha", BootID: "boot-alpha", RootProcess: ProcessIdentity{PID: 101, StartedAt: fixedTestTime()}}
	second := first
	second.RootProcess.StartedAt = fixedTestTime().Add(time.Minute)

	if first == second {
		t.Fatal("PID reuse with a new start time produced the same runtime identity")
	}
}

func TestEffectiveStateUsesIdleProcessBaseAndHookClaim(t *testing.T) {
	tests := []struct {
		name  string
		claim HookClaim
		want  EffectiveState
	}{
		{name: "no claim", claim: NoHookClaim, want: StateIdle},
		{name: "working", claim: ClaimWorking, want: StateWorking},
		{name: "attention", claim: ClaimAttention, want: StateAttention},
		{name: "error", claim: ClaimError, want: StateError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, active, err := Effective(RuntimeAlive, test.claim, false)
			if err != nil || !active || got != test.want {
				t.Fatalf("Effective = %q, %t, %v; want %q, true, nil", got, active, err, test.want)
			}
			// A process observation changes runtime ownership only. Retaining the
			// claim proves a poll cannot lower hook-owned state.
			gotAfterPoll, active, err := Effective(RuntimeAlive, test.claim, false)
			if err != nil || !active || gotAfterPoll != test.want {
				t.Fatalf("Effective after process update = %q, %t, %v; want %q", gotAfterPoll, active, err, test.want)
			}
		})
	}
}

func TestHookIdleClearsClaim(t *testing.T) {
	claim, err := ApplyHookState(StateIdle)
	if err != nil || claim != NoHookClaim {
		t.Fatalf("ApplyHookState(idle) = %q, %v; want empty claim", claim, err)
	}
	state, active, err := Effective(RuntimeAlive, claim, false)
	if err != nil || !active || state != StateIdle {
		t.Fatalf("Effective after hook idle = %q, %t, %v", state, active, err)
	}
}

func TestEndedRuntimeHasNoActiveInstance(t *testing.T) {
	state, active, err := Effective(RuntimeEnded, ClaimWorking, false)
	if err != nil || active || state != "" {
		t.Fatalf("Effective(ended) = %q, %t, %v; want empty, false, nil", state, active, err)
	}
}

func TestStartupPendingIsAttentionUntilHook(t *testing.T) {
	got, active, err := Effective(RuntimeAlive, NoHookClaim, true)
	if err != nil || !active || got != StateAttention {
		t.Fatalf("startup pending = %q, %t, %v; want attention", got, active, err)
	}
	// Explicit hooks win over startup-pending.
	got, active, err = Effective(RuntimeAlive, ClaimWorking, true)
	if err != nil || !active || got != StateWorking {
		t.Fatalf("working claim with startup pending = %q, %t, %v", got, active, err)
	}
	got, active, err = Effective(RuntimeAlive, ClaimError, true)
	if err != nil || !active || got != StateError {
		t.Fatalf("error claim with startup pending = %q, %t, %v", got, active, err)
	}
	// After SessionStart clears claim and startup-pending flag is cleared by registry.
	got, active, err = Effective(RuntimeAlive, NoHookClaim, false)
	if err != nil || !active || got != StateIdle {
		t.Fatalf("post-sessionstart idle = %q, %t, %v", got, active, err)
	}
	// Suspended still attention during startup-pending.
	got, active, err = Effective(RuntimeSuspended, NoHookClaim, true)
	if err != nil || !active || got != StateAttention {
		t.Fatalf("suspended startup = %q, %t, %v", got, active, err)
	}
}

func TestSuspendedRuntimeIsActiveAndMapsToAttention(t *testing.T) {
	if !RuntimeSuspended.Active() {
		t.Fatal("suspended runtime must be active")
	}
	tests := []struct {
		name  string
		claim HookClaim
		want  EffectiveState
	}{
		{name: "idle claim", claim: NoHookClaim, want: StateAttention},
		{name: "working claim", claim: ClaimWorking, want: StateAttention},
		{name: "attention claim", claim: ClaimAttention, want: StateAttention},
		{name: "error claim wins", claim: ClaimError, want: StateError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, active, err := Effective(RuntimeSuspended, test.claim, false)
			if err != nil || !active || got != test.want {
				t.Fatalf("Effective(suspended) = %q, %t, %v; want %q", got, active, err, test.want)
			}
		})
	}
	// Resume restores hook claim presentation.
	got, active, err := Effective(RuntimeAlive, ClaimWorking, false)
	if err != nil || !active || got != StateWorking {
		t.Fatalf("Effective after resume = %q, %t, %v", got, active, err)
	}
}

func TestPresentationStatesAreRejectedAsInstanceStates(t *testing.T) {
	for _, state := range []EffectiveState{"sleeping", "offline"} {
		if err := state.Validate(); err == nil {
			t.Errorf("instance state %q was accepted", state)
		}
	}
	if err := HookClaim(StateIdle).Validate(); err == nil {
		t.Fatal("idle was accepted as a hook claim")
	}
}

func TestInstanceValidationRequiresDerivedState(t *testing.T) {
	instance := validInstance("instance-alpha", 101, fixedTestTime())
	instance.HookClaim = ClaimWorking
	instance.Revisions.HookRevision = 1
	if err := instance.Validate(); err == nil {
		t.Fatal("instance with non-derived effective state was accepted")
	}
	instance.State = StateWorking
	if err := instance.Validate(); err != nil {
		t.Fatalf("valid derived instance rejected: %v", err)
	}
}

func TestLifecycleTimestampOrdering(t *testing.T) {
	discovered := fixedTestTime()
	lastSeen := discovered.Add(time.Minute)
	ended := lastSeen.Add(time.Minute)
	released := ended.Add(time.Minute)

	validActive := LifecycleTimestamps{
		DiscoveredAt: discovered, LastSeenAt: lastSeen,
		StateChangedAt: discovered.Add(30 * time.Second), LeaseExpiresAt: lastSeen.Add(time.Minute),
	}
	if err := validActive.Validate(RuntimeAlive); err != nil {
		t.Fatalf("valid active lifecycle rejected: %v", err)
	}
	validEnded := validActive
	validEnded.EndedAt = &ended
	validEnded.SlotReleasedAt = &released
	if err := validEnded.Validate(RuntimeEnded); err != nil {
		t.Fatalf("valid ended lifecycle rejected: %v", err)
	}

	tests := []struct {
		name      string
		lifecycle LifecycleTimestamps
		status    RuntimeStatus
	}{
		{name: "last seen before discovery", lifecycle: withLastSeen(validActive, discovered.Add(-time.Second)), status: RuntimeAlive},
		{name: "state change before discovery", lifecycle: withStateChanged(validActive, discovered.Add(-time.Second)), status: RuntimeAlive},
		{name: "lease before last seen", lifecycle: withLeaseExpiry(validActive, lastSeen.Add(-time.Second)), status: RuntimeAlive},
		{name: "ended before discovery", lifecycle: withEnded(validActive, discovered.Add(-time.Second)), status: RuntimeEnded},
		{name: "ended before last seen", lifecycle: withEnded(validActive, lastSeen.Add(-time.Second)), status: RuntimeEnded},
		{name: "slot release without end", lifecycle: withSlotReleased(validActive, released), status: RuntimeAlive},
		{name: "slot release before end", lifecycle: withSlotReleased(validEnded, ended.Add(-time.Second)), status: RuntimeEnded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.lifecycle.Validate(test.status); err == nil {
				t.Fatal("invalid lifecycle was accepted")
			}
		})
	}
}

func TestInstanceRevisionInvariants(t *testing.T) {
	t.Run("runtime revision must be positive", func(t *testing.T) {
		instance := validInstance("instance-alpha", 101, fixedTestTime())
		instance.Revisions.RuntimeRevision = 0
		if err := instance.Validate(); err == nil {
			t.Fatal("zero runtime revision was accepted")
		}
	})

	t.Run("claim requires hook revision", func(t *testing.T) {
		instance := validInstance("instance-alpha", 101, fixedTestTime())
		instance.HookClaim = ClaimWorking
		instance.State = StateWorking
		if err := instance.Validate(); err == nil {
			t.Fatal("hook claim with zero hook revision was accepted")
		}
	})

	t.Run("zero hook revision before first event", func(t *testing.T) {
		instance := validInstance("instance-alpha", 101, fixedTestTime())
		if err := instance.Validate(); err != nil {
			t.Fatalf("zero hook revision without claim rejected: %v", err)
		}
	})

	t.Run("positive hook revision after claim clear", func(t *testing.T) {
		instance := validInstance("instance-alpha", 101, fixedTestTime())
		instance.Revisions.HookRevision = 2
		if err := instance.Validate(); err != nil {
			t.Fatalf("cleared claim with positive hook revision rejected: %v", err)
		}
	})

	t.Run("ended runtime cannot retain claim", func(t *testing.T) {
		instance := validInstance("instance-alpha", 101, fixedTestTime())
		ended := instance.Lifecycle.LastSeenAt.Add(time.Second)
		instance.Status = RuntimeEnded
		instance.Lifecycle.EndedAt = &ended
		instance.HookClaim = ClaimError
		instance.Revisions.HookRevision = 3
		instance.State = ""
		if err := instance.Validate(); err == nil {
			t.Fatal("ended runtime retained a hook claim")
		}
	})
}

func withLastSeen(value LifecycleTimestamps, timestamp time.Time) LifecycleTimestamps {
	value.LastSeenAt = timestamp
	return value
}

func withStateChanged(value LifecycleTimestamps, timestamp time.Time) LifecycleTimestamps {
	value.StateChangedAt = timestamp
	return value
}

func withLeaseExpiry(value LifecycleTimestamps, timestamp time.Time) LifecycleTimestamps {
	value.LeaseExpiresAt = timestamp
	return value
}

func withEnded(value LifecycleTimestamps, timestamp time.Time) LifecycleTimestamps {
	value.EndedAt = &timestamp
	return value
}

func withSlotReleased(value LifecycleTimestamps, timestamp time.Time) LifecycleTimestamps {
	value.SlotReleasedAt = &timestamp
	return value
}
