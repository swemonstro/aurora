package codexproducer

import (
	"testing"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

func mustMapEvent(t *testing.T, hookEventName, sessionID string) producerprotocol.State {
	t.Helper()
	action, supported := codexhook.MapEvent(codexhook.Event{HookEventName: hookEventName, SessionID: sessionID})
	if !supported {
		t.Fatalf("hook event %q not supported by codexhook.MapEvent", hookEventName)
	}
	mapped, err := MapHookState(action.State)
	if err != nil {
		t.Fatalf("MapHookState(%v): %v", action.State, err)
	}
	return mapped
}

func TestMachine_Discover_DefaultsToIdleAtRevisionOne(t *testing.T) {
	machine := NewMachine()
	state, revision, isNew := machine.Discover("inst-1", "business")
	if state != producerprotocol.StateIdle || revision != 1 || !isNew {
		t.Fatalf("Discover: got state=%v revision=%d isNew=%v", state, revision, isNew)
	}
	// Re-discovering must never reset an already-tracked instance.
	state2, revision2, isNew2 := machine.Discover("inst-1", "business")
	if state2 != producerprotocol.StateIdle || revision2 != 1 || isNew2 {
		t.Fatalf("second Discover must be a no-op: got state=%v revision=%d isNew=%v", state2, revision2, isNew2)
	}
}

func TestMachine_Discover_NeverRegressesAlreadyTrackedInstance(t *testing.T) {
	machine := NewMachine()
	machine.ApplyHookEvent("inst-1", "business", producerprotocol.StateWorking)
	state, revision, isNew := machine.Discover("inst-1", "business")
	if isNew {
		t.Fatalf("Discover must not report isNew for an already-tracked instance")
	}
	if state != producerprotocol.StateWorking || revision != 1 {
		t.Fatalf("Discover must not regress state: got state=%v revision=%d", state, revision)
	}
}

func TestMachine_ApplyHookEvent_FirstEventCanSkipIdle(t *testing.T) {
	// Hook-before-process-discovery: the very first thing ever observed
	// about an instance is "working" (e.g. UserPromptSubmit), with no prior
	// Discover call. It must be reported directly as working at revision 1,
	// never as a fabricated idle-then-working transition.
	machine := NewMachine()
	state, revision, changed := machine.ApplyHookEvent("inst-1", "business", producerprotocol.StateWorking)
	if !changed || state != producerprotocol.StateWorking || revision != 1 {
		t.Fatalf("got state=%v revision=%d changed=%v, want working/1/true", state, revision, changed)
	}
}

func TestMachine_RevisionsIncreaseOnlyOnChange(t *testing.T) {
	machine := NewMachine()
	machine.Discover("inst-1", "business")
	// Idempotent repeat of idle must not bump revision.
	state, revision, changed := machine.ApplyHookEvent("inst-1", "business", producerprotocol.StateIdle)
	if changed {
		t.Fatalf("re-reporting the same state must not be reported as changed")
	}
	if state != producerprotocol.StateIdle || revision != 1 {
		t.Fatalf("got state=%v revision=%d, want idle/1", state, revision)
	}
	// A real transition bumps revision by exactly one.
	state, revision, changed = machine.ApplyHookEvent("inst-1", "business", producerprotocol.StateWorking)
	if !changed || state != producerprotocol.StateWorking || revision != 2 {
		t.Fatalf("got state=%v revision=%d changed=%v, want working/2/true", state, revision, changed)
	}
}

func TestMachine_Renew_BumpsRevisionWithoutChangingState(t *testing.T) {
	machine := NewMachine()
	machine.Discover("inst-1", "business")
	state, revision, ok := machine.Renew("inst-1")
	if !ok || state != producerprotocol.StateIdle || revision != 2 {
		t.Fatalf("Renew: got state=%v revision=%d ok=%v, want idle/2/true", state, revision, ok)
	}
	_, _, ok = machine.Renew("never-tracked")
	if ok {
		t.Fatalf("Renew on an untracked instance must report ok=false")
	}
}

func TestMachine_Forget_StopsTracking(t *testing.T) {
	machine := NewMachine()
	machine.Discover("inst-1", "business")
	machine.Forget("inst-1")
	if _, tracked := machine.Tracked("inst-1"); tracked {
		t.Fatalf("instance must not be tracked after Forget")
	}
	if _, _, ok := machine.Renew("inst-1"); ok {
		t.Fatalf("Renew must fail for a forgotten instance")
	}
}

func TestMachine_RevisionsAreIndependentPerInstance(t *testing.T) {
	machine := NewMachine()
	machine.Discover("inst-1", "business")
	machine.Discover("inst-2", "business")
	machine.ApplyHookEvent("inst-1", "business", producerprotocol.StateWorking)
	machine.ApplyHookEvent("inst-1", "business", producerprotocol.StateAttention)

	state2, revision2, _ := machine.ApplyHookEvent("inst-2", "business", producerprotocol.StateIdle)
	if state2 != producerprotocol.StateIdle || revision2 != 1 {
		t.Fatalf("inst-2 must be untouched by inst-1's transitions: got state=%v revision=%d", state2, revision2)
	}
}

// TestFalseRed regression tests below map directly onto the G.4 spec's
// "FALSE-RED-REGRESSIONSTESTER" list. Machine never accepts a trust/config
// input of any kind (its exported API takes only instance id, source, and
// an already-mapped hook state) — attention is structurally reachable only
// via MapHookState(status.Attention), which codexhook.MapEvent only ever
// produces for a real observed PermissionRequest event. These tests confirm
// the resulting *sequence* of states, not just a final snapshot.

func TestFalseRed_MissingTrustEntry_NeverProducesAttention(t *testing.T) {
	// A brand-new interactive Codex process is discovered by recognition
	// alone (no trust config is ever consulted or even reachable from this
	// package — see doc.go). It must be idle, and stay idle with no hook
	// activity at all, regardless of what any trust config would have said.
	machine := NewMachine()
	var sequence []producerprotocol.State
	state, _, _ := machine.Discover("inst-1", "business")
	sequence = append(sequence, state)
	for _, observed := range sequence {
		if observed == producerprotocol.StateAttention {
			t.Fatalf("missing trust entry must never produce attention; got sequence %v", sequence)
		}
	}
	if sequence[len(sequence)-1] != producerprotocol.StateIdle {
		t.Fatalf("expected idle, got %v", sequence)
	}
}

func TestFalseRed_UntrustedProjectNoObservedQuestion_NeverProducesAttention(t *testing.T) {
	// Same as above: "untrusted" is a config fact this package cannot even
	// observe (it never imports internal/codextrust), so it can never
	// trigger attention. Modeled the same way as the missing-entry case
	// because both are, from this package's point of view, identical:
	// process discovered, zero hook events observed.
	machine := NewMachine()
	state, revision, isNew := machine.Discover("inst-1", "api")
	if !isNew || state != producerprotocol.StateIdle || revision != 1 {
		t.Fatalf("got state=%v revision=%d isNew=%v, want idle/1/true", state, revision, isNew)
	}
}

func TestFalseRed_StartsWorkingDirectly_NoIntermediateAttention(t *testing.T) {
	machine := NewMachine()
	var sequence []producerprotocol.State

	discovered, _, _ := machine.Discover("inst-1", "business")
	sequence = append(sequence, discovered)

	working := mustMapEvent(t, "UserPromptSubmit", "session-1")
	state, _, changed := machine.ApplyHookEvent("inst-1", "business", working)
	if !changed {
		t.Fatalf("expected a state change to working")
	}
	sequence = append(sequence, state)

	for _, observed := range sequence {
		if observed == producerprotocol.StateAttention {
			t.Fatalf("starting work directly must never pass through attention; got sequence %v", sequence)
		}
	}
	if sequence[len(sequence)-1] != producerprotocol.StateWorking {
		t.Fatalf("expected final state working, got sequence %v", sequence)
	}
}

func TestFalseRed_ActualPermissionRequest_ProducesAttention(t *testing.T) {
	machine := NewMachine()
	machine.Discover("inst-1", "business")
	attention := mustMapEvent(t, "PermissionRequest", "session-1")
	state, revision, changed := machine.ApplyHookEvent("inst-1", "business", attention)
	if !changed || state != producerprotocol.StateAttention || revision != 2 {
		t.Fatalf("got state=%v revision=%d changed=%v, want attention/2/true", state, revision, changed)
	}
}

func TestFalseRed_PermissionRequestThenEscCancel_ReturnsToIdle(t *testing.T) {
	// Esc/cancel is modeled via the Stop hook event, which codexhook.MapEvent
	// maps unconditionally to idle — see state.go's doc comment for why this
	// package does not read the transcript for turn_aborted detection.
	machine := NewMachine()
	var sequence []producerprotocol.State

	discovered, _, _ := machine.Discover("inst-1", "business")
	sequence = append(sequence, discovered)

	attention := mustMapEvent(t, "PermissionRequest", "session-1")
	state, _, _ := machine.ApplyHookEvent("inst-1", "business", attention)
	sequence = append(sequence, state)

	idle := mustMapEvent(t, "Stop", "session-1")
	state, _, changed := machine.ApplyHookEvent("inst-1", "business", idle)
	if !changed {
		t.Fatalf("expected a state change back to idle")
	}
	sequence = append(sequence, state)

	want := []producerprotocol.State{producerprotocol.StateIdle, producerprotocol.StateAttention, producerprotocol.StateIdle}
	if len(sequence) != len(want) {
		t.Fatalf("got sequence %v, want %v", sequence, want)
	}
	for index := range want {
		if sequence[index] != want[index] {
			t.Fatalf("got sequence %v, want %v", sequence, want)
		}
	}
}

func TestFalseRed_ConcurrentSessionUnaffectedThroughout(t *testing.T) {
	machine := NewMachine()
	machine.Discover("inst-1", "business")
	machine.Discover("inst-2", "business")

	type step struct {
		hookEvent string
		wantState producerprotocol.State
	}
	steps := []step{
		{"UserPromptSubmit", producerprotocol.StateWorking},
		{"PermissionRequest", producerprotocol.StateAttention},
		{"Stop", producerprotocol.StateIdle},
	}

	for _, current := range steps {
		mapped := mustMapEvent(t, current.hookEvent, "session-1")
		state, _, _ := machine.ApplyHookEvent("inst-1", "business", mapped)
		if state != current.wantState {
			t.Fatalf("inst-1: got %v, want %v", state, current.wantState)
		}
		// inst-2 must be byte-for-byte unchanged after every single step.
		other, revision, tracked := func() (producerprotocol.State, uint64, bool) {
			s, _ := machine.Tracked("inst-2")
			if s == "" {
				return "", 0, false
			}
			snap := machine.Snapshot()
			for _, entry := range snap {
				if entry.InstanceID == "inst-2" {
					return entry.State, entry.Revision, true
				}
			}
			return "", 0, false
		}()
		if !tracked || other != producerprotocol.StateIdle || revision != 1 {
			t.Fatalf("inst-2 must remain idle/1 throughout inst-1's transitions; got state=%v revision=%d tracked=%v", other, revision, tracked)
		}
	}
}
