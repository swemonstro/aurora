package status

import (
	"fmt"
	"strings"
)

type State string

const (
	Working   State = "working"
	Attention State = "attention"
	Error     State = "error"
	Idle      State = "idle"
)

func Normalize(event string) (State, error) {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "started", "start", "running", "working":
		return Working, nil
	case "completed", "complete", "finished", "stop", "attention":
		return Attention, nil
	case "failed", "failure", "error":
		return Error, nil
	case "idle":
		return Idle, nil
	default:
		return "", fmt.Errorf("unsupported event %q", event)
	}
}
