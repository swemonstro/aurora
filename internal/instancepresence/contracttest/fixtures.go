// Package contracttest provides immutable-on-load, OS-neutral process fixtures
// for adapter and correlation contract tests.
package contracttest

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

//go:embed testdata/process_scenarios.json
var processScenariosJSON string

type ProcessScenario struct {
	Name                string                              `json:"name"`
	Description         string                              `json:"description"`
	Snapshots           []instancepresence.ProcessSnapshot  `json:"snapshots"`
	ExpectedFamilies    []ExpectedRuntimeFamily             `json:"expected_families"`
	Hook                *instancepresence.HookObservation   `json:"hook,omitempty"`
	ExpectedCorrelation *instancepresence.CorrelationResult `json:"expected_correlation,omitempty"`
}

type ExpectedRuntimeFamily struct {
	InstanceID instancepresence.InstanceID        `json:"instance_id"`
	Tool       instancepresence.ToolKind          `json:"tool"`
	Root       instancepresence.ProcessIdentity   `json:"root"`
	Members    []instancepresence.ProcessIdentity `json:"members"`
}

// RuntimeCandidate converts expected fixture output into the domain contract.
// Host and boot values are synthetic because fixtures are intentionally OS-neutral.
func (family ExpectedRuntimeFamily) RuntimeCandidate() instancepresence.RuntimeCandidate {
	return instancepresence.RuntimeCandidate{
		InstanceID: family.InstanceID,
		Tool:       family.Tool,
		Runtime: instancepresence.RuntimeIdentity{
			HostID: "fixture-host", BootID: "fixture-boot", RootProcess: family.Root,
		},
		Members: family.Members,
	}
}

// ProcessScenarios returns a fresh decode so tests cannot share mutable fixture
// state. The embedded data contains only opaque synthetic identifiers.
func ProcessScenarios() ([]ProcessScenario, error) {
	var scenarios []ProcessScenario
	if err := json.Unmarshal([]byte(processScenariosJSON), &scenarios); err != nil {
		return nil, fmt.Errorf("decode process fixtures: %w", err)
	}
	return scenarios, nil
}
