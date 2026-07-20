package claudehook

import (
	"testing"

	"github.com/swemonstro/aurora/internal/status"
)

func TestMapEvent(t *testing.T) {
	tests := []struct {
		name   string
		event  Event
		state  status.State
		remove bool
	}{
		{name: "prompt", event: Event{HookEventName: "UserPromptSubmit"}, state: status.Working},
		{name: "permission notification", event: Event{HookEventName: "Notification", NotificationType: "permission_prompt"}, state: status.Attention},
		{name: "idle notification", event: Event{HookEventName: "Notification", NotificationType: "idle_prompt"}, state: status.Attention},
		{name: "unknown notification", event: Event{HookEventName: "Notification", NotificationType: "future_type"}, state: status.Attention},
		{name: "missing notification type", event: Event{HookEventName: "Notification"}, state: status.Attention},
		{name: "stop", event: Event{HookEventName: "Stop"}, state: status.Attention},
		{name: "failure", event: Event{HookEventName: "StopFailure"}, state: status.Error},
		{name: "session end removes", event: Event{HookEventName: "SessionEnd"}, remove: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, supported := MapEvent(test.event)
			if !supported {
				t.Fatal("MapEvent reported supported event as unsupported")
			}
			if action.State != test.state || action.Remove != test.remove {
				t.Fatalf("action = %#v, want state=%q remove=%t", action, test.state, test.remove)
			}
		})
	}
}

func TestMapEventRejectsUnsupportedEvent(t *testing.T) {
	action, supported := MapEvent(Event{HookEventName: "PreToolUse"})
	if supported {
		t.Fatalf("supported = true with action %#v", action)
	}
}

func TestParseEventRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseEvent([]byte(`{"hook_event_name":`)); err == nil {
		t.Fatal("ParseEvent returned no error")
	}
}

func TestParseEventRejectsEmptyInput(t *testing.T) {
	if _, err := ParseEvent(nil); err == nil {
		t.Fatal("ParseEvent returned no error")
	}
}

func TestConfigFromEnv(t *testing.T) {
	values := map[string]string{
		RelayURLEnv: " http://127.0.0.1:9090 ",
		SourceEnv:   " custom-claude ",
	}
	config := ConfigFromEnv(func(key string) string { return values[key] })

	if config.RelayURL != "http://127.0.0.1:9090" {
		t.Fatalf("RelayURL = %q", config.RelayURL)
	}
	if config.Source != "custom-claude" {
		t.Fatalf("Source = %q", config.Source)
	}
}

func TestConfigFromEnvUsesDefaults(t *testing.T) {
	config := ConfigFromEnv(func(string) string { return "" })
	if config.RelayURL != DefaultRelayURL || config.Source != DefaultSource {
		t.Fatalf("config = %#v", config)
	}
}
