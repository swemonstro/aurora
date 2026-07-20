package status

import (
	"fmt"
	"strings"
	"time"
)

type State string

const (
	Working   State = "working"
	Attention State = "attention"
	Error     State = "error"
	Idle      State = "idle"
)

type Message struct {
	Version   int       `json:"version"`
	Source    string    `json:"source"`
	State     State     `json:"state"`
	Timestamp time.Time `json:"timestamp"`
}

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
