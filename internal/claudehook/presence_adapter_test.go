package claudehook

import (
	"reflect"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

func TestLocalHookObservationPreservesClaudeMapping(t *testing.T) {
	observation, err := LocalHookObservation(Event{HookEventName: "Notification", NotificationType: "idle_prompt", SessionID: "session-a"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Tool != instancepresence.ToolClaude || observation.Lifecycle != instancecorrelation.LifecycleIdle {
		t.Fatalf("observation = %#v", observation)
	}
	if _, exists := reflect.TypeOf(observation).FieldByName("ProcessHint"); exists {
		t.Fatal("agent ingress exposes a process hint")
	}
	if _, err := LocalHookObservation(Event{HookEventName: "unsupported", SessionID: "session-a"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: time.Now().UTC()}); err == nil {
		t.Fatal("unsupported Claude event accepted")
	}
}

func TestLocalHookObservationRejectsMissingSessionAndNormalizesTime(t *testing.T) {
	input := time.Date(2026, 7, 22, 12, 0, 0, 123, time.FixedZone("local", 3600))
	if _, err := LocalHookObservation(Event{HookEventName: "Stop"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: input}); err == nil {
		t.Fatal("missing session accepted")
	}
	observation, err := LocalHookObservation(Event{HookEventName: "Stop", SessionID: "session-a"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: input})
	if err != nil || observation.ObservedAt != input.Round(0).UTC() {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

func TestLocalHookObservationMapsClaudeActive(t *testing.T) {
	observation, err := LocalHookObservation(Event{HookEventName: "UserPromptSubmit", SessionID: "session-a"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: time.Now()})
	if err != nil || observation.Lifecycle != instancecorrelation.LifecycleActive {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

func TestLocalIngressObservationMapsClaudeWithoutMetadata(t *testing.T) {
	observation, err := LocalIngressObservation(Event{HookEventName: "UserPromptSubmit", SessionID: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Tool != instancepresence.ToolClaude || observation.HookSessionRef != "session-a" ||
		observation.Lifecycle != instancecorrelation.LifecycleActive || observation.EffectiveState != instancepresence.StateWorking {
		t.Fatalf("observation = %#v", observation)
	}
	if observation, err := LocalIngressObservation(Event{HookEventName: "SessionEnd", SessionID: "session-a"}); err != nil ||
		observation.Lifecycle != instancecorrelation.LifecycleEnded || observation.EffectiveState != "" {
		t.Fatalf("session end = %#v err=%v", observation, err)
	}
	if _, err := LocalIngressObservation(Event{HookEventName: "unsupported", SessionID: "session-a"}); err == nil {
		t.Fatal("unsupported event accepted")
	}
	if _, err := LocalIngressObservation(Event{HookEventName: "Stop"}); err == nil {
		t.Fatal("missing session accepted")
	}
}

func TestLocalIngressObservationAskUserQuestionCancelIsIdle(t *testing.T) {
	attention, err := LocalIngressObservation(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
	})
	if err != nil || attention.EffectiveState != instancepresence.StateAttention || attention.HookSessionRef != "session-a" {
		t.Fatalf("attention = %#v err=%v", attention, err)
	}
	idle, err := LocalIngressObservation(Event{
		HookEventName: "PostToolUseFailure", SessionID: "session-a", ToolName: "AskUserQuestion",
	})
	if err != nil || idle.EffectiveState != instancepresence.StateIdle || idle.Lifecycle != instancecorrelation.LifecycleIdle {
		t.Fatalf("decline idle = %#v err=%v", idle, err)
	}
	if idle.HookSessionRef != "session-a" {
		t.Fatalf("session = %q, want session-a", idle.HookSessionRef)
	}
	if _, err := LocalIngressObservation(Event{
		HookEventName: "PostToolUseFailure", SessionID: "session-a", ToolName: "Bash",
	}); err == nil {
		t.Fatal("Bash PostToolUseFailure must not produce local ingress")
	}
}

func TestClaudeWrapperLaunchRulesExcludeNPMAndAllowNPX(t *testing.T) {
	var rule runtimerecognition.LaunchIdentityRule
	for _, candidate := range LaunchIdentityRules() {
		if candidate.Identity == "launch:aurora-claude" && candidate.Argument == runtimerecognition.LaunchArgumentEntrypoint {
			rule = candidate
		}
	}
	if rule.Identity == "" {
		t.Fatal("Claude wrapper entrypoint rule is missing")
	}
	for _, test := range []struct {
		name string
		argv []string
		want bool
	}{
		{"npm direct wrapper is rejected", []string{"npm", "aurora-claude"}, false},
		{"npx wrapper is accepted", []string{"npx", "aurora-claude"}, true},
		{"npm exec is rejected at argv1", []string{"npm", "exec", "--", "aurora-claude"}, false},
		{"later option is rejected", []string{"npx", "--output=aurora-claude"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := rule.Matches(test.argv, 1); got != test.want {
				t.Fatalf("Matches(%q, 1) = %t, want %t", test.argv, got, test.want)
			}
		})
	}
}
