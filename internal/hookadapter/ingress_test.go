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
	if observation.Tool != instancepresence.ToolClaude || observation.HookSessionRef != "session-a" || observation.Lifecycle != instancecorrelation.LifecycleActive {
		t.Fatalf("observation = %#v", observation)
	}

	idle, err := IngressFromLifecycle(instancepresence.ToolCodex, "session-b", false, status.Idle)
	if err != nil || idle.Lifecycle != instancecorrelation.LifecycleIdle {
		t.Fatalf("idle = %#v err=%v", idle, err)
	}
	ended, err := IngressFromLifecycle(instancepresence.ToolCodex, "session-c", true, status.Working)
	if err != nil || ended.Lifecycle != instancecorrelation.LifecycleEnded {
		t.Fatalf("ended = %#v err=%v", ended, err)
	}

	if _, err := IngressFromLifecycle(instancepresence.ToolClaude, "", false, status.Working); err == nil {
		t.Fatal("empty session accepted")
	}
	if _, err := IngressFromLifecycle(instancepresence.ToolClaude, " session-a", false, status.Working); err == nil {
		t.Fatal("whitespace session accepted")
	}
	if err := (IngressObservation{Tool: "hermes", HookSessionRef: "session-a", Lifecycle: instancecorrelation.LifecycleActive}).Validate(); err == nil {
		t.Fatal("unsupported tool accepted")
	}
	if err := (IngressObservation{Tool: instancepresence.ToolClaude, HookSessionRef: "session-a", Lifecycle: "paused"}).Validate(); err == nil {
		t.Fatal("unsupported lifecycle accepted")
	}
}
