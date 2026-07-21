package contracttest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestProcessFixturesAreValidAndComplete(t *testing.T) {
	scenarios, err := ProcessScenarios()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"one_claude":                             false,
		"two_parallel_claude":                    false,
		"codex_node_native_family":               false,
		"two_parallel_codex_families":            false,
		"pid_reuse":                              false,
		"process_disappears_without_session_end": false,
		"ambiguous_shared_fingerprints":          false,
		"strong_ancestor_match":                  false,
		"weak_signals_unmatched":                 false,
	}
	for _, scenario := range scenarios {
		if _, ok := want[scenario.Name]; !ok {
			t.Errorf("unexpected scenario %q", scenario.Name)
		} else {
			want[scenario.Name] = true
		}
		if len(scenario.Snapshots) == 0 {
			t.Errorf("scenario %q has no snapshots", scenario.Name)
		}
		for _, snapshot := range scenario.Snapshots {
			if err := snapshot.Validate(); err != nil {
				t.Errorf("scenario %q has invalid snapshot: %v", scenario.Name, err)
			}
		}
		for _, family := range scenario.ExpectedFamilies {
			if err := family.RuntimeCandidate().Validate(); err != nil {
				t.Errorf("scenario %q has invalid runtime candidate: %v", scenario.Name, err)
			}
		}
		if scenario.Hook != nil {
			if err := scenario.Hook.Validate(); err != nil {
				t.Errorf("scenario %q has invalid hook observation: %v", scenario.Name, err)
			}
		}
		if scenario.ExpectedCorrelation != nil {
			if err := scenario.ExpectedCorrelation.Validate(); err != nil {
				t.Errorf("scenario %q has invalid expected correlation: %v", scenario.Name, err)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("required process fixture %q is missing", name)
		}
	}
}

func TestFixturesContainNoForbiddenRawDataFields(t *testing.T) {
	var document any
	if err := json.Unmarshal([]byte(processScenariosJSON), &document); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"argv": true, "cwd": true, "transcript_path": true, "prompt": true,
		"terminal_output": true, "environment": true, "env": true,
	}
	var visit func(any)
	visit = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[strings.ToLower(key)] {
					t.Errorf("fixture contains forbidden field %q", key)
				}
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(document)
}

func TestPIDReuseFixtureHasDifferentGeneration(t *testing.T) {
	scenario := findScenario(t, "pid_reuse")
	first := scenario.Snapshots[0].Processes[0].Process
	second := scenario.Snapshots[1].Processes[0].Process
	if first.PID != second.PID || first.StartedAt.Equal(second.StartedAt) {
		t.Fatalf("PID reuse fixture generations = %#v and %#v", first, second)
	}
}

func TestCorrelationFixturesEncodeConservativeOutcomes(t *testing.T) {
	ambiguous := findScenario(t, "ambiguous_shared_fingerprints")
	if ambiguous.ExpectedCorrelation.Outcome != instancepresence.CorrelationAmbiguous {
		t.Fatalf("ambiguous fixture outcome = %q", ambiguous.ExpectedCorrelation.Outcome)
	}
	strong := findScenario(t, "strong_ancestor_match")
	if strong.ExpectedCorrelation.Outcome != instancepresence.CorrelationUniquelyMatched ||
		strong.ExpectedCorrelation.InstanceID != "instance-claude-alpha" {
		t.Fatalf("strong fixture correlation = %#v", strong.ExpectedCorrelation)
	}
	weak := findScenario(t, "weak_signals_unmatched")
	if weak.ExpectedCorrelation.Outcome != instancepresence.CorrelationUnmatched {
		t.Fatalf("weak fixture outcome = %q", weak.ExpectedCorrelation.Outcome)
	}
}

func findScenario(t *testing.T, name string) ProcessScenario {
	t.Helper()
	scenarios, err := ProcessScenarios()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range scenarios {
		if scenario.Name == name {
			return scenario
		}
	}
	t.Fatalf("scenario %q not found", name)
	return ProcessScenario{}
}
