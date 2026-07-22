package localhooktransport

import (
	"fmt"
	"testing"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestClaudeAndCodexAdaptersAreSanitized(t *testing.T) {
	metadata := AdapterMetadata{
		ProducerEpoch: "epoch-fixture", Revision: 1,
		IdempotencyKey: "idem-fixture", ObservedAt: testTime,
	}
	tests := []struct {
		name      string
		construct func() (HookObservation, error)
		tool      instancepresence.ToolKind
		lifecycle instancecorrelation.Lifecycle
	}{
		{
			name: "Claude active", tool: instancepresence.ToolClaude, lifecycle: instancecorrelation.LifecycleActive,
			construct: func() (HookObservation, error) {
				return ClaudeObservation(claudehook.Event{HookEventName: "UserPromptSubmit", SessionID: "claude-session", ToolName: "ignored"}, metadata)
			},
		},
		{
			name: "Claude idle", tool: instancepresence.ToolClaude, lifecycle: instancecorrelation.LifecycleIdle,
			construct: func() (HookObservation, error) {
				return ClaudeObservation(claudehook.Event{HookEventName: "Stop", SessionID: "claude-session"}, metadata)
			},
		},
		{
			name: "Codex ended", tool: instancepresence.ToolCodex, lifecycle: instancecorrelation.LifecycleEnded,
			construct: func() (HookObservation, error) {
				return CodexObservation(codexhook.Event{
					HookEventName: "SessionEnd", SessionID: "codex-session",
					TranscriptPath: "/sensitive/ignored", Source: "ignored", TurnID: "ignored",
				}, metadata)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation, err := test.construct()
			if err != nil {
				t.Fatal(err)
			}
			if observation.Tool != test.tool || observation.Lifecycle != test.lifecycle {
				t.Fatalf("observation = %#v", observation)
			}
			if observation.ProcessHint != nil || observation.RuntimeHint != nil || observation.HostID != "" || observation.BootID != "" {
				t.Fatalf("adapter guessed process identity: %#v", observation)
			}
		})
	}
}

func TestAdaptersRejectUnsupportedEventsAndMissingSession(t *testing.T) {
	metadata := AdapterMetadata{ProducerEpoch: "epoch-fixture", Revision: 1, ObservedAt: testTime}
	if _, err := ClaudeObservation(claudehook.Event{HookEventName: "unsupported", SessionID: "session"}, metadata); err == nil {
		t.Fatal("unsupported Claude event accepted")
	}
	if _, err := CodexObservation(codexhook.Event{HookEventName: "SessionStart"}, metadata); err == nil {
		t.Fatal("Codex event without session accepted")
	}
}

func TestForbiddenObservationFieldsAreUnknown(t *testing.T) {
	base, err := EncodeRequestJSON(testRequest("request-01", instancepresence.ToolClaude))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"prompt", "transcript", "cwd", "argv", "environment", "uid", "metadata"} {
		t.Run(field, func(t *testing.T) {
			payload := string(base)
			payload = payload[:len(payload)-2] + fmt.Sprintf(",%q:%q}}", field, "forbidden")
			if _, err := DecodeRequestJSON([]byte(payload)); errorCode(err) != CodeUnknownField {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
