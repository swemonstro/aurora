// Package hookadapter contains generic conversion from an agent lifecycle
// result to a transport-neutral ingress observation.
package hookadapter

import (
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/status"
)

type Metadata struct {
	ProducerEpoch  instancepresence.ProducerEpoch
	Revision       uint64
	IdempotencyKey string
	ObservedAt     time.Time
}

// Observation is an allowlisted, transport-neutral ingress record. Agent
// adapters cannot add process or runtime hints through this type.
type Observation struct {
	Tool           instancepresence.ToolKind
	HookSessionRef instancepresence.OpaqueIdentity
	ProducerEpoch  instancepresence.ProducerEpoch
	Revision       uint64
	IdempotencyKey string
	ObservedAt     time.Time
	Lifecycle      instancecorrelation.Lifecycle
}

func (observation Observation) Validate() error {
	return (instancecorrelation.HookObservation{
		Tool: observation.Tool, HookSessionRef: observation.HookSessionRef,
		ProducerEpoch: observation.ProducerEpoch, Revision: observation.Revision,
		IdempotencyKey: observation.IdempotencyKey, ObservedAt: observation.ObservedAt,
		Lifecycle: observation.Lifecycle,
	}).Validate()
}

func ObservationFromLifecycle(tool instancepresence.ToolKind, sessionID string, remove bool, state status.State, metadata Metadata) (Observation, error) {
	lifecycle := instancecorrelation.LifecycleActive
	if remove {
		lifecycle = instancecorrelation.LifecycleEnded
	} else if state == status.Idle {
		lifecycle = instancecorrelation.LifecycleIdle
	}
	observation := Observation{
		Tool: tool, HookSessionRef: instancepresence.OpaqueIdentity(sessionID),
		ProducerEpoch: metadata.ProducerEpoch, Revision: metadata.Revision,
		IdempotencyKey: metadata.IdempotencyKey, ObservedAt: metadata.ObservedAt.Round(0).UTC(), Lifecycle: lifecycle,
	}
	if err := observation.Validate(); err != nil {
		return Observation{}, err
	}
	return observation, nil
}
