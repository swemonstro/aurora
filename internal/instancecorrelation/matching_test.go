package instancecorrelation

import "testing"

func TestGlobalAssignmentBeatsPerHookGreedy(t *testing.T) {
	pairs := [][]scoredPair{
		{{runtimeIndex: 0, score: 100}, {runtimeIndex: 1, score: 90}},
		{{runtimeIndex: 0, score: 95}, {runtimeIndex: 1, score: 1}},
	}
	solution := solveAssignments(2, 2, pairs, excludedPair{-1, -1})
	if solution.score != 185 || solution.assignment[0] != 1 || solution.assignment[1] != 0 {
		t.Fatalf("global solution = %#v", solution)
	}
}

func TestAssignmentTieBreakIsDeterministic(t *testing.T) {
	pairs := [][]scoredPair{{{runtimeIndex: 1, score: 100}, {runtimeIndex: 0, score: 100}}}
	solution := solveAssignments(1, 2, pairs, excludedPair{-1, -1})
	if solution.assignment[0] != 0 {
		t.Fatalf("tie-break assignment = %#v", solution.assignment)
	}
}
