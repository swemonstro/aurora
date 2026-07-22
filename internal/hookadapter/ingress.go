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
type IngressObservation struct {
	Tool           instancepresence.ToolKind
	HookSessionRef instancepresence.OpaqueIdentity
	Lifecycle      instancecorrelation.Lifecycle
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
	return nil
}

// IngressFromLifecycle maps an agent lifecycle result to the minimal ingress.
func IngressFromLifecycle(tool instancepresence.ToolKind, sessionID string, remove bool, state status.State) (IngressObservation, error) {
	lifecycle := instancecorrelation.LifecycleActive
	if remove {
		lifecycle = instancecorrelation.LifecycleEnded
	} else if state == status.Idle {
		lifecycle = instancecorrelation.LifecycleIdle
	}
	observation := IngressObservation{
		Tool:           tool,
		HookSessionRef: instancepresence.OpaqueIdentity(sessionID),
		Lifecycle:      lifecycle,
	}
	if err := observation.Validate(); err != nil {
		return IngressObservation{}, err
	}
	return observation, nil
}
