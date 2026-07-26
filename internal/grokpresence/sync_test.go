package grokpresence

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

type syncCall struct {
	id    instancepresence.InstanceID
	state instancepresence.EffectiveState
}

type fakeSyncRegistry struct {
	instances []instancepresence.Instance
	calls     []syncCall
	replay    bool
}

func (registry *fakeSyncRegistry) ActiveInstances() []instancepresence.Instance {
	return append([]instancepresence.Instance{}, registry.instances...)
}

func (registry *fakeSyncRegistry) ApplyNextHookMutation(
	id instancepresence.InstanceID,
	_ instancepresence.ProducerEpoch,
	state instancepresence.EffectiveState,
	_ time.Time,
	_ string,
) (instancepresence.Instance, error) {
	registry.calls = append(registry.calls, syncCall{id: id, state: state})

	for index := range registry.instances {
		if registry.instances[index].ID != id {
			continue
		}

		if registry.replay {
			return registry.instances[index], nil
		}

		claim, err := instancepresence.ApplyHookState(state)
		if err != nil {
			return instancepresence.Instance{}, err
		}
		registry.instances[index].HookClaim = claim
		registry.instances[index].Revisions.HookRevision++
		return registry.instances[index], nil
	}

	return instancepresence.Instance{}, errors.New("instance not found")
}

type fakeStateReader struct {
	states map[uint64][]EventObservation
	errors map[uint64]error
}

func (reader fakeStateReader) Observations(
	_ context.Context,
	process instancepresence.ProcessIdentity,
) ([]EventObservation, error) {
	if err := reader.errors[process.PID]; err != nil {
		return nil, err
	}
	return append([]EventObservation{}, reader.states[process.PID]...), nil
}

func TestStateSyncMutatesOnlyGrok(t *testing.T) {
	registry := &fakeSyncRegistry{instances: []instancepresence.Instance{
		testSyncInstance("claude-a", instancepresence.ToolClaude, 100),
		testSyncInstance("grok-a", instancepresence.ToolGrok, 200),
	}}
	reader := fakeStateReader{states: map[uint64][]EventObservation{
		100: testSyncObservations(instancepresence.StateWorking),
		200: testSyncObservations(instancepresence.StateWorking),
	}}

	syncer := StateSync{Registry: registry, Reader: reader}
	result := syncer.Apply(context.Background())

	if result.Examined != 1 || result.Mutated != 1 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(registry.calls) != 1 || registry.calls[0].id != "grok-a" {
		t.Fatalf("calls = %#v", registry.calls)
	}
}

func TestStateSyncBaselinesThenPreservesNewTransitions(t *testing.T) {
	registry := &fakeSyncRegistry{instances: []instancepresence.Instance{
		testSyncInstance("grok-a", instancepresence.ToolGrok, 200),
	}}
	reader := fakeStateReader{states: map[uint64][]EventObservation{
		200: testSyncObservations(
			instancepresence.StateWorking,
			instancepresence.StateWorking,
			instancepresence.StateAttention,
			instancepresence.StateIdle,
		),
	}}

	notified := 0
	syncer := StateSync{
		Registry: registry,
		Reader:   reader,
		Notify:   func() { notified++ },
	}

	syncer.EstablishBaseline(registry.instances)

	first := syncer.Apply(context.Background())
	if first.Examined != 1 || first.Mutated != 1 ||
		len(first.Errors) != 0 || notified != 1 {
		t.Fatalf("first=%#v notified=%d", first, notified)
	}
	if len(registry.calls) != 1 ||
		registry.calls[0].state != instancepresence.StateIdle {
		t.Fatalf("baseline calls = %#v", registry.calls)
	}

	reader.states[200] = testSyncObservations(
		instancepresence.StateWorking,
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
	)

	second := syncer.Apply(context.Background())
	if second.Examined != 1 || second.Mutated != 3 ||
		len(second.Errors) != 0 || notified != 4 {
		t.Fatalf("second=%#v notified=%d", second, notified)
	}

	want := []instancepresence.EffectiveState{
		instancepresence.StateIdle,
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
	}
	if len(registry.calls) != len(want) {
		t.Fatalf("calls = %#v", registry.calls)
	}
	for index := range want {
		if registry.calls[index].state != want[index] {
			t.Fatalf("call[%d] = %#v want %q", index, registry.calls[index], want[index])
		}
	}

	third := syncer.Apply(context.Background())
	if third.Mutated != 0 || notified != 4 || len(registry.calls) != 4 {
		t.Fatalf("third=%#v notified=%d calls=%#v", third, notified, registry.calls)
	}
}

func TestStateSyncPreservesFirstTurnForNewProcess(t *testing.T) {
	registry := &fakeSyncRegistry{instances: []instancepresence.Instance{
		testSyncInstance("grok-a", instancepresence.ToolGrok, 200),
	}}
	reader := fakeStateReader{states: map[uint64][]EventObservation{
		200: testSyncObservations(
			instancepresence.StateWorking,
			instancepresence.StateAttention,
			instancepresence.StateIdle,
		),
	}}

	notified := 0
	syncer := StateSync{
		Registry: registry,
		Reader:   reader,
		Notify:   func() { notified++ },
	}
	syncer.EstablishBaseline(nil)

	result := syncer.Apply(context.Background())
	if result.Examined != 1 || result.Mutated != 3 ||
		len(result.Errors) != 0 || notified != 3 {
		t.Fatalf("result=%#v notified=%d", result, notified)
	}

	want := []instancepresence.EffectiveState{
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
	}
	if len(registry.calls) != len(want) {
		t.Fatalf("calls=%#v", registry.calls)
	}
	for index := range want {
		if registry.calls[index].state != want[index] {
			t.Fatalf(
				"call[%d]=%#v want %q",
				index,
				registry.calls[index],
				want[index],
			)
		}
	}
}

func TestStateSyncPreservesFirstTurnAfterPrePromptObservation(t *testing.T) {
	registry := &fakeSyncRegistry{instances: []instancepresence.Instance{
		testSyncInstance("grok-a", instancepresence.ToolGrok, 200),
	}}
	reader := fakeStateReader{
		states: map[uint64][]EventObservation{},
	}

	notified := 0
	syncer := StateSync{
		Registry: registry,
		Reader:   reader,
		Notify:   func() { notified++ },
	}

	beforePrompt := syncer.Apply(context.Background())
	if beforePrompt.Examined != 1 || beforePrompt.Mutated != 0 ||
		len(beforePrompt.Errors) != 0 || notified != 0 ||
		len(registry.calls) != 0 {
		t.Fatalf(
			"beforePrompt=%#v notified=%d calls=%#v",
			beforePrompt,
			notified,
			registry.calls,
		)
	}

	reader.states[200] = testSyncObservations(
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
	)

	firstTurn := syncer.Apply(context.Background())
	if firstTurn.Examined != 1 || firstTurn.Mutated != 3 ||
		len(firstTurn.Errors) != 0 || notified != 3 {
		t.Fatalf("firstTurn=%#v notified=%d", firstTurn, notified)
	}

	want := []instancepresence.EffectiveState{
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
	}
	if len(registry.calls) != len(want) {
		t.Fatalf("calls=%#v", registry.calls)
	}
	for index := range want {
		if registry.calls[index].state != want[index] {
			t.Fatalf(
				"call[%d]=%#v want %q",
				index,
				registry.calls[index],
				want[index],
			)
		}
	}
}

func TestStateSyncDoesNotNotifyIdempotentReplay(t *testing.T) {
	registry := &fakeSyncRegistry{
		instances: []instancepresence.Instance{
			testSyncInstance("grok-a", instancepresence.ToolGrok, 200),
		},
		replay: true,
	}
	reader := fakeStateReader{states: map[uint64][]EventObservation{
		200: testSyncObservations(instancepresence.StateWorking),
	}}

	notified := 0
	syncer := StateSync{
		Registry: registry,
		Reader:   reader,
		Notify:   func() { notified++ },
	}
	result := syncer.Apply(context.Background())

	if result.Examined != 1 || result.Mutated != 0 ||
		len(result.Errors) != 0 || notified != 0 {
		t.Fatalf("result=%#v notified=%d", result, notified)
	}
}

func TestStateSyncContinuesAfterOneReaderError(t *testing.T) {
	registry := &fakeSyncRegistry{instances: []instancepresence.Instance{
		testSyncInstance("grok-a", instancepresence.ToolGrok, 200),
		testSyncInstance("grok-b", instancepresence.ToolGrok, 300),
	}}
	reader := fakeStateReader{
		states: map[uint64][]EventObservation{
			300: testSyncObservations(instancepresence.StateAttention),
		},
		errors: map[uint64]error{
			200: errors.New("unreadable"),
		},
	}

	syncer := StateSync{Registry: registry, Reader: reader}
	result := syncer.Apply(context.Background())

	if result.Examined != 2 || result.Mutated != 1 || len(result.Errors) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(registry.calls) != 1 || registry.calls[0].id != "grok-b" {
		t.Fatalf("calls = %#v", registry.calls)
	}
}

func testSyncInstance(
	id instancepresence.InstanceID,
	tool instancepresence.ToolKind,
	pid uint64,
) instancepresence.Instance {
	return instancepresence.Instance{
		ID:     id,
		Tool:   tool,
		Status: instancepresence.RuntimeAlive,
		Runtime: instancepresence.RuntimeIdentity{
			RootProcess: instancepresence.ProcessIdentity{
				PID:       pid,
				StartedAt: time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC),
			},
		},
		Revisions: instancepresence.Revisions{
			ProducerEpoch: "epoch-test",
		},
	}
}

func testSyncObservations(
	states ...instancepresence.EffectiveState,
) []EventObservation {
	base := time.Date(2026, 7, 25, 14, 1, 0, 0, time.UTC)
	observations := make([]EventObservation, 0, len(states))

	for index, state := range states {
		observations = append(observations, EventObservation{
			State:          state,
			ObservedAt:     base.Add(time.Duration(index) * time.Second),
			IdempotencyKey: fmt.Sprintf("grok-test-event-%d", index),
		})
	}
	return observations
}
