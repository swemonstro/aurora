package localhooktransport

import (
	"errors"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/status"
)

type AdapterMetadata struct {
	ProducerEpoch  instancepresence.ProducerEpoch
	Revision       uint64
	IdempotencyKey string
	ObservedAt     time.Time
}

func ClaudeObservation(event claudehook.Event, metadata AdapterMetadata) (HookObservation, error) {
	action, supported := claudehook.MapEvent(event)
	if !supported {
		return HookObservation{}, errors.New("unsupported Claude lifecycle event")
	}
	return observationFromLifecycle(instancepresence.ToolClaude, event.SessionID, action.Remove, action.State, metadata)
}

func CodexObservation(event codexhook.Event, metadata AdapterMetadata) (HookObservation, error) {
	action, supported := codexhook.MapEvent(event)
	if !supported {
		return HookObservation{}, errors.New("unsupported Codex lifecycle event")
	}
	return observationFromLifecycle(instancepresence.ToolCodex, event.SessionID, action.Remove, action.State, metadata)
}

func observationFromLifecycle(tool instancepresence.ToolKind, sessionID string, remove bool, state status.State, metadata AdapterMetadata) (HookObservation, error) {
	lifecycle := instancecorrelation.LifecycleActive
	if remove {
		lifecycle = instancecorrelation.LifecycleEnded
	} else if state == status.Idle {
		lifecycle = instancecorrelation.LifecycleIdle
	}
	observation := HookObservation{
		Tool: tool, HookSessionRef: instancepresence.OpaqueIdentity(sessionID),
		ProducerEpoch: metadata.ProducerEpoch, Revision: metadata.Revision,
		IdempotencyKey: metadata.IdempotencyKey, ObservedAt: canonicalTime(metadata.ObservedAt),
		Lifecycle: lifecycle,
	}
	if err := observation.domain().Validate(); err != nil {
		return HookObservation{}, err
	}
	return observation, nil
}
