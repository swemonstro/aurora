package codexhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/swemonstro/aurora/internal/status"
)

type Event struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	TurnID        string `json:"turn_id"`
	Source        string `json:"source"`
	ToolName      string `json:"tool_name"`
}

type EventAction struct {
	State  status.State
	Remove bool
}

func ReadInput(reader io.Reader) ([]byte, error) {
	input, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read Codex hook input: %w", err)
	}
	return input, nil
}

func ParseEvent(input []byte) (Event, error) {
	var event Event

	decoder := json.NewDecoder(bytes.NewReader(input))
	if err := decoder.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("decode Codex hook event: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Event{}, fmt.Errorf("input contains more than one JSON value")
		}
		return Event{}, fmt.Errorf("decode trailing Codex hook input: %w", err)
	}

	event.HookEventName = strings.TrimSpace(event.HookEventName)
	event.SessionID = strings.TrimSpace(event.SessionID)
	event.TurnID = strings.TrimSpace(event.TurnID)
	event.Source = strings.TrimSpace(event.Source)
	event.ToolName = strings.TrimSpace(event.ToolName)

	return event, nil
}

func MapEvent(event Event) (EventAction, bool) {
	switch event.HookEventName {
	case "SessionStart":
		return EventAction{State: status.Idle}, true
	case "UserPromptSubmit":
		return EventAction{State: status.Working}, true
	case "PreToolUse":
		return EventAction{State: status.Working}, true
	case "PermissionRequest":
		return EventAction{State: status.Attention}, true
	case "PostToolUse":
		return EventAction{State: status.Working}, true
	case "Stop":
		return EventAction{State: status.Idle}, true
	case "SessionEnd":
		return EventAction{Remove: true}, true
	default:
		return EventAction{}, false
	}
}
