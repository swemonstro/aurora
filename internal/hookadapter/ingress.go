package hookadapter

import (
	"errors"
	"fmt"
	"strings"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/status"
)

// IngressObservation is the minimal transport-neutral Package 6 ingress.
// It intentionally excludes producer epoch, revision, observed-at, process
// hints, runtime hints, and any agent event name or free metadata.
//
// EffectiveState carries exact non-ended hook status (idle/working/attention/
// error). Ended observations leave EffectiveState empty.
type IngressObservation struct {
	Tool           instancepresence.ToolKind
	HookSessionRef instancepresence.OpaqueIdentity
	Lifecycle      instancecorrelation.Lifecycle
	EffectiveState instancepresence.EffectiveState
}

func (observation IngressObservation) Validate() error {
	if err := observation.Tool.Validate(); err != nil {
		return fmt.Errorf("ingress tool: %w", err)
	}
	if strings.TrimSpace(string(observation.HookSessionRef)) == "" {
		return errors.New("ingress hook session reference must not be empty")
	}
	if strings.TrimSpace(string(observation.HookSessionRef)) != string(observation.HookSessionRef) {
		return errors.New("ingress hook session reference must not contain surrounding whitespace")
	}
	if err := observation.Lifecycle.Validate(); err != nil {
		return fmt.Errorf("ingress lifecycle: %w", err)
	}
	if observation.Lifecycle == instancecorrelation.LifecycleEnded {
		if observation.EffectiveState != "" {
			return errors.New("ended ingress must not carry an effective state")
		}
		return nil
	}
	if err := observation.EffectiveState.Validate(); err != nil {
		return fmt.Errorf("ingress effective state: %w", err)
	}
	return nil
}

// IngressFromLifecycle maps an agent lifecycle result to the minimal ingress,
// including exact effective state for non-ended events.
func IngressFromLifecycle(tool instancepresence.ToolKind, sessionID string, remove bool, state status.State) (IngressObservation, error) {
	if remove {
		observation := IngressObservation{
			Tool:           tool,
			HookSessionRef: instancepresence.OpaqueIdentity(sessionID),
			Lifecycle:      instancecorrelation.LifecycleEnded,
		}
		if err := observation.Validate(); err != nil {
			return IngressObservation{}, err
		}
		return observation, nil
	}
	effective, err := effectiveStateFromStatus(state)
	if err != nil {
		return IngressObservation{}, err
	}
	lifecycle := instancecorrelation.LifecycleActive
	if state == status.Idle {
		lifecycle = instancecorrelation.LifecycleIdle
	}
	observation := IngressObservation{
		Tool:           tool,
		HookSessionRef: instancepresence.OpaqueIdentity(sessionID),
		Lifecycle:      lifecycle,
		EffectiveState: effective,
	}
	if err := observation.Validate(); err != nil {
		return IngressObservation{}, err
	}
	return observation, nil
}

func effectiveStateFromStatus(state status.State) (instancepresence.EffectiveState, error) {
	switch state {
	case status.Idle:
		return instancepresence.StateIdle, nil
	case status.Working:
		return instancepresence.StateWorking, nil
	case status.Attention:
		return instancepresence.StateAttention, nil
	case status.Error:
		return instancepresence.StateError, nil
	default:
		return "", fmt.Errorf("unsupported status state %q", state)
	}
}
