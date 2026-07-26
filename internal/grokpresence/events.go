package grokpresence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

const (
	// MaxStructuralTailBytes bounds the event history inspected in one pass.
	MaxStructuralTailBytes = 256 * 1024

	// MaxStructuralLineBytes rejects unexpectedly large JSONL records.
	MaxStructuralLineBytes = 64 * 1024
)

// EventObservation is a content-free Grok state observation derived from one
// structural events.jsonl record.
type EventObservation struct {
	State          instancepresence.EffectiveState
	ObservedAt     time.Time
	IdempotencyKey string
}

type structuralEvent struct {
	Type    string `json:"type"`
	Phase   string `json:"phase"`
	Outcome string `json:"outcome"`
	TS      string `json:"ts"`
}

// StructuralStates returns every recognized structural state in JSONL file
// order. Unknown, malformed, oversized, or unsupported records are ignored.
func StructuralStates(data []byte) []EventObservation {
	data = boundedStructuralTail(data)
	if len(data) == 0 {
		return nil
	}

	observations := make([]EventObservation, 0)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		observation, ok := ParseStructuralEvent(line)
		if ok {
			observations = append(observations, observation)
		}
	}
	return observations
}

// LatestStructuralState returns the final recognized structural state.
func LatestStructuralState(data []byte) (EventObservation, bool) {
	observations := StructuralStates(data)
	if len(observations) == 0 {
		return EventObservation{}, false
	}
	return observations[len(observations)-1], true
}

// ParseStructuralEvent classifies one Grok JSONL record while deliberately
// decoding no prompt, response, tool argument, or other content-bearing field.
func ParseStructuralEvent(line []byte) (EventObservation, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || len(line) > MaxStructuralLineBytes {
		return EventObservation{}, false
	}

	var event structuralEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return EventObservation{}, false
	}

	state, marker, ok := classifyStructuralEvent(event)
	if !ok {
		return EventObservation{}, false
	}

	observedAt, err := time.Parse(time.RFC3339Nano, event.TS)
	if err != nil || observedAt.IsZero() {
		return EventObservation{}, false
	}
	observedAt = observedAt.UTC()

	return EventObservation{
		State:      state,
		ObservedAt: observedAt,
		IdempotencyKey: fmt.Sprintf(
			"grok-structural|%s|%s|%s",
			observedAt.Format(time.RFC3339Nano),
			event.Type,
			marker,
		),
	}, true
}

func classifyStructuralEvent(
	event structuralEvent,
) (instancepresence.EffectiveState, string, bool) {
	switch event.Type {
	case "phase_changed":
		switch event.Phase {
		case "waiting_for_model",
			"streaming_reasoning",
			"streaming_text",
			"tool_execution":
			return instancepresence.StateWorking, event.Phase, true

		case "permission_prompt":
			return instancepresence.StateAttention, event.Phase, true

		default:
			return "", "", false
		}

	case "turn_ended":
		switch event.Outcome {
		case "completed", "cancelled":
			return instancepresence.StateIdle, event.Outcome, true

		default:
			return "", "", false
		}

	default:
		return "", "", false
	}
}

func boundedStructuralTail(data []byte) []byte {
	if len(data) <= MaxStructuralTailBytes {
		return data
	}

	tail := data[len(data)-MaxStructuralTailBytes:]
	newline := bytes.IndexByte(tail, '\n')
	if newline < 0 {
		return nil
	}

	// The bounded slice may begin inside a JSONL record. Discard that partial
	// first record and retain only complete records after its newline.
	return tail[newline+1:]
}
