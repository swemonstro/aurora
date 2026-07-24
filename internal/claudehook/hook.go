package claudehook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/status"
)

const (
	DefaultRelayURL = "http://127.0.0.1:8080"
	DefaultSource   = "claude-code"
	RelayURLEnv     = "AURORA_RELAY_URL"
	SourceEnv       = "AURORA_SOURCE"
	CaptureHooksEnv = "AURORA_CAPTURE_HOOKS"
)

type Config struct {
	RelayURL string
	Source   string
}

type Event struct {
	HookEventName    string `json:"hook_event_name"`
	SessionID        string `json:"session_id"`
	NotificationType string `json:"notification_type"`
	ToolName         string `json:"tool_name"`
	// ToolUseID and TranscriptPath are present on PreToolUse AskUserQuestion
	// and power transcript-based Esc recovery. They are never logged.
	ToolUseID      string `json:"tool_use_id"`
	TranscriptPath string `json:"transcript_path"`
}

type EventAction struct {
	State  status.State
	Remove bool
}

func ConfigFromEnv(getenv func(string) string) Config {
	config := Config{
		RelayURL: DefaultRelayURL,
		Source:   DefaultSource,
	}
	if relayURL := strings.TrimSpace(getenv(RelayURLEnv)); relayURL != "" {
		config.RelayURL = relayURL
	}
	if source := strings.TrimSpace(getenv(SourceEnv)); source != "" {
		config.Source = source
	}
	return config
}

func CaptureDirectoryFromEnv(getenv func(string) string) string {
	return strings.TrimSpace(getenv(CaptureHooksEnv))
}

func ReadInput(reader io.Reader) ([]byte, error) {
	input, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read Claude hook input: %w", err)
	}
	return input, nil
}

func Capture(directory string, input []byte) error {
	if strings.TrimSpace(directory) == "" {
		return nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create hook capture directory: %w", err)
	}

	event := "unknown"
	if parsedEvent, err := ParseEvent(input); err == nil && parsedEvent.HookEventName != "" {
		event = sanitizeFilenamePart(parsedEvent.HookEventName)
	}
	prefix := time.Now().UTC().Format("20060102T150405.000000000Z") + "_" + event + "_"

	file, err := os.CreateTemp(directory, prefix+"*.json")
	if err != nil {
		return fmt.Errorf("create hook capture file: %w", err)
	}
	filename := file.Name()
	success := false
	defer func() {
		if !success {
			_ = file.Close()
			_ = os.Remove(filename)
		}
	}()

	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("set hook capture permissions: %w", err)
	}
	written, err := file.Write(input)
	if err != nil {
		return fmt.Errorf("write hook capture: %w", err)
	}
	if written != len(input) {
		return fmt.Errorf("write hook capture: wrote %d of %d bytes", written, len(input))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close hook capture: %w", err)
	}
	success = true
	return nil
}

func ParseEvent(input []byte) (Event, error) {
	var event Event
	decoder := json.NewDecoder(bytes.NewReader(input))
	if err := decoder.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("decode Claude hook event: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Event{}, fmt.Errorf("input contains more than one JSON value")
		}
		return Event{}, fmt.Errorf("decode trailing Claude hook input: %w", err)
	}
	event.HookEventName = strings.TrimSpace(event.HookEventName)
	event.SessionID = strings.TrimSpace(event.SessionID)
	event.NotificationType = strings.TrimSpace(event.NotificationType)
	event.ToolName = strings.TrimSpace(event.ToolName)
	event.ToolUseID = strings.TrimSpace(event.ToolUseID)
	event.TranscriptPath = strings.TrimSpace(event.TranscriptPath)
	return event, nil
}

func sanitizeFilenamePart(value string) string {
	const maxLength = 64
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
		if result.Len() >= maxLength {
			break
		}
	}
	if result.Len() == 0 {
		return "unknown"
	}
	return result.String()
}

func MapEvent(event Event) (EventAction, bool) {
	switch event.HookEventName {
	case "UserPromptSubmit":
		return EventAction{State: status.Working}, true
	case "Notification":
		switch event.NotificationType {
		case "permission_prompt":
			return EventAction{State: status.Attention}, true
		case "idle_prompt":
			return EventAction{State: status.Idle}, true
		default:
			return EventAction{State: status.Attention}, true
		}
	case "PreToolUse":
		if event.ToolName == "AskUserQuestion" {
			return EventAction{State: status.Attention}, true
		}
		return EventAction{State: status.Working}, true
	case "PostToolUse":
		return EventAction{State: status.Working}, true
	case "PostToolUseFailure":
		// Defensive fallback only: Esc on AskUserQuestion does not currently
		// emit this hook (recovery is transcript-based). If a Claude version
		// ever sends PostToolUseFailure for AskUserQuestion, map it to idle.
		// Other tool failures must not invent idle.
		if event.ToolName == "AskUserQuestion" {
			return EventAction{State: status.Idle}, true
		}
		return EventAction{}, false
	case "Stop":
		return EventAction{State: status.Idle}, true
	case "StopFailure":
		return EventAction{State: status.Error}, true
	case "SessionEnd":
		return EventAction{Remove: true}, true
	}
	return EventAction{}, false
}
