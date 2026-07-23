package hookadapter

import (
	"testing"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/status"
)

func TestIngressObservationValidateAndFromLifecycle(t *testing.T) {
	observation, err := IngressFromLifecycle(instancepresence.ToolClaude, "session-a", false, status.Working)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Tool != instancepresence.ToolClaude || observation.HookSessionRef != "session-a" ||
		observation.Lifecycle != instancecorrelation.LifecycleActive || observation.EffectiveState != instancepresence.StateWorking {
		t.Fatalf("observation = %#v", observation)
	}

	attention, err := IngressFromLifecycle(instancepresence.ToolClaude, "session-a", false, status.Attention)
	if err != nil || attention.EffectiveState != instancepresence.StateAttention || attention.Lifecycle != instancecorrelation.LifecycleActive {
		t.Fatalf("attention = %#v err=%v", attention, err)
	}
	errorState, err := IngressFromLifecycle(instancepresence.ToolClaude, "session-a", false, status.Error)
	if err != nil || errorState.EffectiveState != instancepresence.StateError {
		t.Fatalf("error = %#v err=%v", errorState, err)
	}

	idle, err := IngressFromLifecycle(instancepresence.ToolCodex, "session-b", false, status.Idle)
	if err != nil || idle.Lifecycle != instancecorrelation.LifecycleIdle || idle.EffectiveState != instancepresence.StateIdle {
		t.Fatalf("idle = %#v err=%v", idle, err)
	}
	ended, err := IngressFromLifecycle(instancepresence.ToolCodex, "session-c", true, status.Working)
	if err != nil || ended.Lifecycle != instancecorrelation.LifecycleEnded || ended.EffectiveState != "" {
		t.Fatalf("ended = %#v err=%v", ended, err)
	}

	if _, err := IngressFromLifecycle(instancepresence.ToolClaude, "", false, status.Working); err == nil {
		t.Fatal("empty session accepted")
	}
	if _, err := IngressFromLifecycle(instancepresence.ToolClaude, " session-a", false, status.Working); err == nil {
		t.Fatal("whitespace session accepted")
	}
	if err := (IngressObservation{Tool: "hermes", HookSessionRef: "session-a", Lifecycle: instancecorrelation.LifecycleActive, EffectiveState: instancepresence.StateWorking}).Validate(); err == nil {
		t.Fatal("unsupported tool accepted")
	}
	if err := (IngressObservation{Tool: instancepresence.ToolClaude, HookSessionRef: "session-a", Lifecycle: "paused", EffectiveState: instancepresence.StateWorking}).Validate(); err == nil {
		t.Fatal("unsupported lifecycle accepted")
	}
	if err := (IngressObservation{Tool: instancepresence.ToolClaude, HookSessionRef: "session-a", Lifecycle: instancecorrelation.LifecycleActive}).Validate(); err == nil {
		t.Fatal("active without state accepted")
	}
	if err := (IngressObservation{Tool: instancepresence.ToolClaude, HookSessionRef: "session-a", Lifecycle: instancecorrelation.LifecycleEnded, EffectiveState: instancepresence.StateIdle}).Validate(); err == nil {
		t.Fatal("ended with state accepted")
	}
}
