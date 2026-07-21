package codexhook

import (
	"strings"
	"testing"

	"github.com/swemonstro/aurora/internal/status"
)

func TestParseEventFields(t *testing.T) {
	event, err := ParseEvent([]byte(
		`{"hook_event_name":" PermissionRequest ","session_id":" session-a ","turn_id":" turn-a ","transcript_path":" /tmp/session.jsonl ","source":" startup ","tool_name":" Bash ","ignored":"value"}`,
	))
	if err != nil {
		t.Fatalf("ParseEvent returned error: %v", err)
	}

	if event.HookEventName != "PermissionRequest" {
		t.Fatalf("HookEventName = %q", event.HookEventName)
	}
	if event.SessionID != "session-a" {
		t.Fatalf("SessionID = %q", event.SessionID)
	}
	if event.TurnID != "turn-a" {
		t.Fatalf("TurnID = %q", event.TurnID)
	}
	if event.TranscriptPath != "/tmp/session.jsonl" {
		t.Fatalf("TranscriptPath = %q", event.TranscriptPath)
	}
	if event.Source != "startup" {
		t.Fatalf("Source = %q", event.Source)
	}
	if event.ToolName != "Bash" {
		t.Fatalf("ToolName = %q", event.ToolName)
	}
}

func TestParseEventRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "invalid JSON", input: `{`},
		{name: "multiple values", input: `{}` + "\n" + `{}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseEvent([]byte(test.input)); err == nil {
				t.Fatal("ParseEvent returned no error")
			}
		})
	}
}

func TestReadInput(t *testing.T) {
	input, err := ReadInput(strings.NewReader(`{"hook_event_name":"Stop"}`))
	if err != nil {
		t.Fatalf("ReadInput returned error: %v", err)
	}
	if got := string(input); got != `{"hook_event_name":"Stop"}` {
		t.Fatalf("input = %q", got)
	}
}

func TestMapEvent(t *testing.T) {
	tests := []struct {
		name      string
		eventName string
		state     status.State
		remove    bool
		supported bool
	}{
		{name: "session start", eventName: "SessionStart", state: status.Idle, supported: true},
		{name: "prompt", eventName: "UserPromptSubmit", state: status.Working, supported: true},
		{name: "tool begins", eventName: "PreToolUse", state: status.Working, supported: true},
		{name: "approval", eventName: "PermissionRequest", state: status.Attention, supported: true},
		{name: "tool ends", eventName: "PostToolUse", state: status.Working, supported: true},
		{name: "turn stops", eventName: "Stop", state: status.Idle, supported: true},
		{name: "synthetic session end", eventName: "SessionEnd", remove: true, supported: true},
		{name: "unknown", eventName: "FutureEvent"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, supported := MapEvent(Event{HookEventName: test.eventName})

			if supported != test.supported {
				t.Fatalf("supported = %t, want %t", supported, test.supported)
			}
			if !supported {
				return
			}
			if action.State != test.state {
				t.Fatalf("State = %q, want %q", action.State, test.state)
			}
			if action.Remove != test.remove {
				t.Fatalf("Remove = %t, want %t", action.Remove, test.remove)
			}
		})
	}
}
