package codexhook

import (
	"reflect"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

func TestLocalHookObservationPreservesCodexMapping(t *testing.T) {
	observation, err := LocalHookObservation(Event{HookEventName: "SessionEnd", SessionID: "session-a", TranscriptPath: "/ignored"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Tool != instancepresence.ToolCodex || observation.Lifecycle != instancecorrelation.LifecycleEnded {
		t.Fatalf("observation = %#v", observation)
	}
	if _, exists := reflect.TypeOf(observation).FieldByName("RuntimeHint"); exists {
		t.Fatal("agent ingress exposes a runtime hint")
	}
	if _, err := LocalHookObservation(Event{HookEventName: "FutureEvent", SessionID: "session-a"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: time.Now().UTC()}); err == nil {
		t.Fatal("unsupported Codex event accepted")
	}
}

func TestLocalHookObservationRejectsMissingCodexSession(t *testing.T) {
	if _, err := LocalHookObservation(Event{HookEventName: "SessionEnd"}, hookadapter.Metadata{ProducerEpoch: "epoch-a", Revision: 1, ObservedAt: time.Now()}); err == nil {
		t.Fatal("missing session accepted")
	}
}

func TestLocalIngressObservationRejectsSessionEndAndMapsLifecycle(t *testing.T) {
	if _, err := LocalIngressObservation(Event{HookEventName: "SessionEnd", SessionID: "session-a"}); err == nil {
		t.Fatal("Codex SessionEnd accepted for Package 6 ingress")
	}
	observation, err := LocalIngressObservation(Event{HookEventName: "UserPromptSubmit", SessionID: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Tool != instancepresence.ToolCodex || observation.HookSessionRef != "session-a" ||
		observation.Lifecycle != instancecorrelation.LifecycleActive || observation.EffectiveState != instancepresence.StateWorking {
		t.Fatalf("observation = %#v", observation)
	}
	if _, err := LocalIngressObservation(Event{HookEventName: "Stop", SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LocalIngressObservation(Event{HookEventName: "FutureEvent", SessionID: "session-a"}); err == nil {
		t.Fatal("unsupported event accepted")
	}
}

func TestCodexWrapperLaunchRulesExcludeNPMAndAllowNPX(t *testing.T) {
	var rule runtimerecognition.LaunchIdentityRule
	for _, candidate := range LaunchIdentityRules() {
		if candidate.Identity == "launch:aurora-codex" && candidate.Argument == runtimerecognition.LaunchArgumentEntrypoint {
			rule = candidate
		}
	}
	if rule.Identity == "" {
		t.Fatal("Codex wrapper entrypoint rule is missing")
	}
	for _, test := range []struct {
		name string
		argv []string
		want bool
	}{
		{"npm direct wrapper is rejected", []string{"npm", "aurora-codex"}, false},
		{"npx wrapper is accepted", []string{"npx", "aurora-codex"}, true},
		{"npm exec is rejected at argv1", []string{"npm", "exec", "--", "aurora-codex"}, false},
		{"later option is rejected", []string{"npx", "--output=aurora-codex"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := rule.Matches(test.argv, 1); got != test.want {
				t.Fatalf("Matches(%q, 1) = %t, want %t", test.argv, got, test.want)
			}
		})
	}
}

func TestCodexCodeModeHostIsNotRecognizedAsSession(t *testing.T) {
	process := runtimerecognition.ProcessObservation{
		CommIdentity:       "exe:codex-code-mode-host",
		ExecutableIdentity: "exe:codex-code-mode-host",
	}

	if recognition, recognized := RuntimeRecognizer().Recognize(process); recognized {
		t.Fatalf(
			"codex-code-mode-host recognized as session: %#v",
			recognition,
		)
	}
}
