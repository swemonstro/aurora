package instancecorrelation

type excludedPair struct {
	hookIndex    int
	runtimeIndex int
}

type assignmentSolution struct {
	score      int
	assignment []int
}

func solveAssignments(hookCount, runtimeCount int, pairs [][]scoredPair, excluded excludedPair) assignmentSolution {
	states := map[uint64]assignmentSolution{0: {assignment: make([]int, 0, hookCount)}}
	for hookIndex := 0; hookIndex < hookCount; hookIndex++ {
		next := make(map[uint64]assignmentSolution)
		for mask, state := range states {
			unmatched := assignmentSolution{score: state.score, assignment: appendAssignment(state.assignment, -1)}
			keepBest(next, mask, unmatched)
			for _, pair := range pairs[hookIndex] {
				if excluded.hookIndex == hookIndex && excluded.runtimeIndex == pair.runtimeIndex {
					continue
				}
				bit := uint64(1) << pair.runtimeIndex
				if mask&bit != 0 {
					continue
				}
				candidate := assignmentSolution{
					score:      state.score + pair.score,
					assignment: appendAssignment(state.assignment, pair.runtimeIndex),
				}
				keepBest(next, mask|bit, candidate)
			}
		}
		states = next
	}
	best := assignmentSolution{score: -1}
	for _, state := range states {
		if betterSolution(state, best) {
			best = state
		}
	}
	if best.score < 0 {
		return assignmentSolution{assignment: make([]int, hookCount)}
	}
	_ = runtimeCount
	return best
}

func appendAssignment(values []int, value int) []int {
	result := make([]int, len(values)+1)
	copy(result, values)
	result[len(values)] = value
	return result
}

func keepBest(states map[uint64]assignmentSolution, mask uint64, candidate assignmentSolution) {
	current, exists := states[mask]
	if !exists || betterSolution(candidate, current) {
		states[mask] = candidate
	}
}

func betterSolution(candidate, current assignmentSolution) bool {
	if candidate.score != current.score {
		return candidate.score > current.score
	}
	if current.assignment == nil {
		return true
	}
	for index := range candidate.assignment {
		candidateValue, currentValue := candidate.assignment[index], current.assignment[index]
		if candidateValue == currentValue {
			continue
		}
		if candidateValue < 0 {
			return false
		}
		if currentValue < 0 {
			return true
		}
		return candidateValue < currentValue
	}
	return false
}
