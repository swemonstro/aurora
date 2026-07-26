package grokpresence

import (
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestParseStructuralEventWorkingPhases(t *testing.T) {
	for _, phase := range []string{
		"waiting_for_model",
		"streaming_reasoning",
		"streaming_text",
		"tool_execution",
	} {
		line := `{"type":"phase_changed","phase":"` + phase +
			`","ts":"2026-07-25T14:40:21.171Z"}`

		observation, ok := ParseStructuralEvent([]byte(line))
		if !ok {
			t.Fatalf("phase %q was not classified", phase)
		}
		if observation.State != instancepresence.StateWorking {
			t.Fatalf("phase %q state = %q", phase, observation.State)
		}
	}
}

func TestParseStructuralEventPermissionPromptIsAttention(t *testing.T) {
	line := []byte(
		`{"type":"phase_changed","phase":"permission_prompt",` +
			`"ts":"2026-07-25T14:40:22.000Z"}`,
	)

	observation, ok := ParseStructuralEvent(line)
	if !ok || observation.State != instancepresence.StateAttention {
		t.Fatalf("observation = %#v ok=%t", observation, ok)
	}
}

func TestParseStructuralEventTerminalOutcomesAreIdle(t *testing.T) {
	for _, outcome := range []string{"completed", "cancelled"} {
		line := `{"type":"turn_ended","outcome":"` + outcome +
			`","ts":"2026-07-25T14:40:23.000Z"}`

		observation, ok := ParseStructuralEvent([]byte(line))
		if !ok || observation.State != instancepresence.StateIdle {
			t.Fatalf(
				"outcome %q observation = %#v ok=%t",
				outcome,
				observation,
				ok,
			)
		}
	}
}

func TestStructuralStatesPreservesRecognizedOrder(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"phase_changed","phase":"waiting_for_model","ts":"2026-07-25T14:40:21Z"}`,
		`{"type":"future_event","ts":"2026-07-25T14:40:22Z"}`,
		`{"type":"phase_changed","phase":"permission_prompt","ts":"2026-07-25T14:40:23Z"}`,
		`{"type":"turn_ended","outcome":"completed","ts":"2026-07-25T14:40:24Z"}`,
	}, "\n")

	got := StructuralStates([]byte(data))
	want := []instancepresence.EffectiveState{
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
	}

	if len(got) != len(want) {
		t.Fatalf("len(StructuralStates()) = %d want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].State != want[index] {
			t.Fatalf("state[%d] = %q want %q", index, got[index].State, want[index])
		}
	}
}

func TestLatestStructuralStateUsesLastRecognizedRecord(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"phase_changed","phase":"waiting_for_model","ts":"2026-07-25T14:40:21Z"}`,
		`{"type":"phase_changed","phase":"permission_prompt","ts":"2026-07-25T14:40:22Z"}`,
		`{"type":"phase_changed","phase":"future_unknown_phase","ts":"2026-07-25T14:40:23Z"}`,
		`{"type":"future_event","ts":"2026-07-25T14:40:24Z"}`,
	}, "\n")

	observation, ok := LatestStructuralState([]byte(data))
	if !ok || observation.State != instancepresence.StateAttention {
		t.Fatalf("observation = %#v ok=%t", observation, ok)
	}

	wantTime := time.Date(2026, 7, 25, 14, 40, 22, 0, time.UTC)
	if !observation.ObservedAt.Equal(wantTime) {
		t.Fatalf("ObservedAt = %s want %s", observation.ObservedAt, wantTime)
	}
}

func TestLatestStructuralStateCompletedSequenceEndsIdle(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"phase_changed","phase":"waiting_for_model","ts":"2026-07-25T14:40:21.171Z"}`,
		`{"type":"phase_changed","phase":"streaming_reasoning","ts":"2026-07-25T14:40:22.543Z"}`,
		`{"type":"phase_changed","phase":"streaming_text","ts":"2026-07-25T14:40:22.658Z"}`,
		`{"type":"turn_ended","outcome":"completed","ts":"2026-07-25T14:40:22.700Z"}`,
	}, "\n")

	observation, ok := LatestStructuralState([]byte(data))
	if !ok || observation.State != instancepresence.StateIdle {
		t.Fatalf("observation = %#v ok=%t", observation, ok)
	}
}

func TestUnknownMalformedAndMissingTimestampAreIgnored(t *testing.T) {
	for _, line := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"type":"phase_changed","phase":"unknown","ts":"2026-07-25T14:40:21Z"}`),
		[]byte(`{"type":"turn_ended","outcome":"failed","ts":"2026-07-25T14:40:21Z"}`),
		[]byte(`{"type":"phase_changed","phase":"streaming_text"}`),
		[]byte(`{"type":"phase_changed","phase":"streaming_text","ts":"invalid"}`),
	} {
		if observation, ok := ParseStructuralEvent(line); ok {
			t.Fatalf("line %q classified as %#v", line, observation)
		}
	}
}

func TestOversizedRecordDoesNotHideLaterValidRecord(t *testing.T) {
	oversized := strings.Repeat("x", MaxStructuralLineBytes+1)
	valid := `{"type":"turn_ended","outcome":"cancelled","ts":"2026-07-25T14:40:21Z"}`

	observation, ok := LatestStructuralState(
		[]byte(oversized + "\n" + valid),
	)
	if !ok || observation.State != instancepresence.StateIdle {
		t.Fatalf("observation = %#v ok=%t", observation, ok)
	}
}

func TestBoundedTailDiscardsPartialFirstRecord(t *testing.T) {
	prefix := strings.Repeat("x", MaxStructuralTailBytes)
	valid := `{"type":"phase_changed","phase":"permission_prompt","ts":"2026-07-25T14:40:21Z"}`

	observation, ok := LatestStructuralState(
		[]byte(prefix + "\n" + valid),
	)
	if !ok || observation.State != instancepresence.StateAttention {
		t.Fatalf("observation = %#v ok=%t", observation, ok)
	}
}

func TestContentFieldsAreIgnored(t *testing.T) {
	line := []byte(
		`{"type":"phase_changed","phase":"streaming_text",` +
			`"ts":"2026-07-25T14:40:21Z",` +
			`"prompt":"do-not-read","response":"also-secret"}`,
	)

	observation, ok := ParseStructuralEvent(line)
	if !ok {
		t.Fatal("structural event was not classified")
	}
	if strings.Contains(observation.IdempotencyKey, "secret") ||
		strings.Contains(observation.IdempotencyKey, "do-not-read") {
		t.Fatalf("content leaked into observation: %#v", observation)
	}
}
