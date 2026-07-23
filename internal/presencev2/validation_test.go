package presencev2

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func fixedAPITestTime() time.Time {
	return time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
}

func validAPIInstance() Instance {
	now := fixedAPITestTime()
	return Instance{
		InstanceID:   "instance-alpha",
		Tool:         instancepresence.ToolClaude,
		Source:       Source{Provider: "claude", Profile: "default", CollectorID: "collector-alpha"},
		State:        instancepresence.StateIdle,
		Slot:         Slot{Namespace: "default", Index: 0},
		Revisions:    Revisions{ProducerEpoch: "epoch-alpha", RuntimeRevision: 1},
		DiscoveredAt: now, StateChangedAt: now, LeaseExpiresAt: now.Add(time.Minute),
	}
}

func TestEmptyCanonicalSnapshotIsValidSleepingData(t *testing.T) {
	snapshot := CanonicalSnapshot{
		APIVersion: APIVersion, GeneratedAt: fixedAPITestTime(), Presence: PresenceSleeping,
		Instances: []Instance{}, Slots: SlotSummary{Namespace: "default", ActiveCount: 0},
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("empty canonical snapshot rejected: %v", err)
	}
}

func TestCanonicalSnapshotRequiresFullConsistentList(t *testing.T) {
	instance := validAPIInstance()
	snapshot := CanonicalSnapshot{
		APIVersion: APIVersion, GeneratedAt: fixedAPITestTime(), Presence: PresenceActive,
		Instances: []Instance{instance}, Slots: SlotSummary{Namespace: "default", ActiveCount: 2},
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("snapshot with partial active_count was accepted")
	}
}

func TestMultipleInstancesMayShareSource(t *testing.T) {
	first := validAPIInstance()
	second := validAPIInstance()
	second.InstanceID = "instance-beta"
	second.Slot.Index = 1
	snapshot := CanonicalSnapshot{
		APIVersion: APIVersion, GeneratedAt: fixedAPITestTime(), Presence: PresenceActive,
		Instances: []Instance{first, second}, Slots: SlotSummary{Namespace: "default", ActiveCount: 2},
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("same-source instances rejected: %v", err)
	}
}

func TestWireInstanceLifecycleAndRevisionValidation(t *testing.T) {
	t.Run("zero hook revision is valid before first hook", func(t *testing.T) {
		instance := validAPIInstance()
		if instance.Revisions.HookRevision != 0 || instance.State != instancepresence.StateIdle {
			t.Fatal("test fixture does not represent a pre-hook idle instance")
		}
		if err := instance.Validate(); err != nil {
			t.Fatalf("pre-hook wire instance rejected: %v", err)
		}
	})

	t.Run("attention with zero hook revision is valid", func(t *testing.T) {
		// RuntimeSuspended projects to attention without any hook event.
		instance := validAPIInstance()
		instance.State = instancepresence.StateAttention
		instance.Revisions.HookRevision = 0
		if err := instance.Validate(); err != nil {
			t.Fatalf("suspended-derived attention rejected: %v", err)
		}
	})

	for _, state := range []instancepresence.EffectiveState{
		instancepresence.StateWorking, instancepresence.StateError,
	} {
		t.Run(string(state)+" with zero hook revision is rejected", func(t *testing.T) {
			instance := validAPIInstance()
			instance.State = state
			instance.Revisions.HookRevision = 0
			if err := instance.Validate(); err == nil {
				t.Fatalf("%s with zero hook revision was accepted", state)
			}
		})
	}

	tests := []struct {
		name   string
		mutate func(*Instance)
	}{
		{name: "zero runtime revision", mutate: func(instance *Instance) { instance.Revisions.RuntimeRevision = 0 }},
		{name: "state change before discovery", mutate: func(instance *Instance) { instance.StateChangedAt = instance.DiscoveredAt.Add(-time.Second) }},
		{name: "lease before discovery", mutate: func(instance *Instance) { instance.LeaseExpiresAt = instance.DiscoveredAt.Add(-time.Second) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := validAPIInstance()
			test.mutate(&instance)
			if err := instance.Validate(); err == nil {
				t.Fatal("invalid wire instance was accepted")
			}
		})
	}

	for _, state := range []instancepresence.EffectiveState{
		instancepresence.StateIdle, instancepresence.StateWorking,
		instancepresence.StateAttention, instancepresence.StateError,
	} {
		t.Run(string(state)+" with positive hook revision is valid", func(t *testing.T) {
			instance := validAPIInstance()
			instance.State = state
			instance.Revisions.HookRevision = 1
			if err := instance.Validate(); err != nil {
				t.Fatalf("%s with positive hook revision rejected: %v", state, err)
			}
		})
	}
}

func TestMutationResponseRevisionValidation(t *testing.T) {
	base := MutationResponse{
		InstanceID: "instance-alpha", EffectiveState: instancepresence.StateIdle,
		Revisions: Revisions{ProducerEpoch: "epoch-alpha", RuntimeRevision: 1},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("idle response before first hook rejected: %v", err)
	}
	cleared := base
	cleared.Revisions.HookRevision = 2
	if err := cleared.Validate(); err != nil {
		t.Fatalf("idle response after claim clear rejected: %v", err)
	}

	t.Run("runtime revision must be positive", func(t *testing.T) {
		response := base
		response.Revisions.RuntimeRevision = 0
		if err := response.Validate(); err == nil {
			t.Fatal("zero runtime revision was accepted")
		}
	})

	t.Run("attention with zero hook revision is valid", func(t *testing.T) {
		response := base
		response.EffectiveState = instancepresence.StateAttention
		response.Revisions.HookRevision = 0
		if err := response.Validate(); err != nil {
			t.Fatalf("attention response with zero hook revision rejected: %v", err)
		}
	})

	for _, state := range []instancepresence.EffectiveState{
		instancepresence.StateWorking, instancepresence.StateError,
	} {
		t.Run(string(state)+" with zero hook revision is rejected", func(t *testing.T) {
			response := base
			response.EffectiveState = state
			response.Revisions.HookRevision = 0
			if err := response.Validate(); err == nil {
				t.Fatalf("%s with zero hook revision was accepted", state)
			}
		})
	}

	for _, state := range []instancepresence.EffectiveState{
		instancepresence.StateIdle, instancepresence.StateWorking,
		instancepresence.StateAttention, instancepresence.StateError,
	} {
		t.Run(string(state)+" with positive hook revision is valid", func(t *testing.T) {
			response := base
			response.EffectiveState = state
			response.Revisions.HookRevision = 1
			if err := response.Validate(); err != nil {
				t.Fatalf("%s with positive hook revision rejected: %v", state, err)
			}
		})
	}
}

func TestCanonicalSnapshotAllowsSuspendedAttentionWithoutHook(t *testing.T) {
	instance := validAPIInstance()
	instance.State = instancepresence.StateAttention
	instance.Revisions.HookRevision = 0
	snapshot := CanonicalSnapshot{
		APIVersion: APIVersion, GeneratedAt: fixedAPITestTime(), Presence: PresenceActive,
		Instances: []Instance{instance}, Slots: SlotSummary{Namespace: "default", ActiveCount: 1},
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("canonical snapshot with suspended-derived attention rejected: %v", err)
	}
}
