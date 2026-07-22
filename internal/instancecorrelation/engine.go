package instancecorrelation

import (
	"errors"
	"sort"
)

type Engine struct {
	config Config
}

func New(config Config) (*Engine, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Engine{config: config}, nil
}

func (engine *Engine) Correlate(input CorrelationInput) (CorrelationResult, error) {
	if engine == nil {
		return CorrelationResult{}, errors.New("correlation engine must not be nil")
	}
	if err := input.Validate(); err != nil {
		return CorrelationResult{}, err
	}
	input.EvaluatedAt = canonicalTime(input.EvaluatedAt)
	for index := range input.Runtimes {
		input.Runtimes[index] = normalizeRuntimeTime(input.Runtimes[index])
	}
	sort.Slice(input.Runtimes, func(first, second int) bool {
		return input.Runtimes[first].Ref() < input.Runtimes[second].Ref()
	})
	normalized := normalizeHooks(input.Hooks)
	result := CorrelationResult{
		Summary:           Summary{Runtimes: len(input.Runtimes), Hooks: uniqueHookCount(input.Hooks)},
		Proposals:         []MatchProposal{},
		Ambiguous:         []AmbiguousMatch{},
		Rejected:          append([]RejectedMatch{}, normalized.rejected...),
		UnmatchedHooks:    []UnmatchedHook{},
		UnmatchedRuntimes: []UnmatchedRuntime{},
		SupersededHooks:   append([]SupersededHook{}, normalized.superseded...),
		Diagnostics:       append([]ReasonCode{}, normalized.diagnostics...),
	}

	if len(input.Runtimes) > engine.config.MaximumCandidateSize || len(normalized.current) > engine.config.MaximumCandidateSize {
		result.Diagnostics = uniqueReasonCodes(append(result.Diagnostics, ReasonCandidateLimitExceeded))
		for _, hook := range normalized.current {
			result.UnmatchedHooks = append(result.UnmatchedHooks, UnmatchedHook{
				HookRef: hook.Ref(), Reasons: []ReasonCode{ReasonCandidateLimitExceeded},
			})
		}
		for _, runtime := range input.Runtimes {
			result.UnmatchedRuntimes = append(result.UnmatchedRuntimes, UnmatchedRuntime{
				RuntimeRef: runtime.Ref(), Reasons: []ReasonCode{ReasonCandidateLimitExceeded},
			})
		}
		result.Summary.Rejected = rejectedHookCount(result)
		result.Risk = evaluateRisk(input.ExpectedMatches, result.Proposals)
		return result, nil
	}

	explicitOwners := make(map[string]int)
	runtimeOwners := make(map[string]int)
	for runtimeIndex, runtime := range input.Runtimes {
		setOwner(runtimeOwners, runtimeKey(runtime.Candidate.Runtime), runtimeIndex)
		for _, member := range runtime.Candidate.Members {
			setOwner(explicitOwners, processKey(member), runtimeIndex)
		}
	}

	pairs := make([][]scoredPair, len(normalized.current))
	allPairs := make([][]scoredPair, len(normalized.current))
	for hookIndex, hook := range normalized.current {
		for runtimeIndex, runtime := range input.Runtimes {
			pair := scorePair(engine.config, input.EvaluatedAt, hook, runtime, explicitOwners, runtimeOwners, runtimeIndex)
			pair.hookIndex = hookIndex
			allPairs[hookIndex] = append(allPairs[hookIndex], pair)
			if len(pair.conflicts) > 0 {
				result.Rejected = append(result.Rejected, RejectedMatch{
					HookRef: hook.Ref(), RuntimeRef: runtime.Ref(), Reasons: pair.conflicts,
				})
				continue
			}
			if pair.confidence == ConfidenceRejected {
				continue
			}
			pairs[hookIndex] = append(pairs[hookIndex], pair)
		}
	}

	best := solveAssignments(len(normalized.current), len(input.Runtimes), pairs, excludedPair{-1, -1})
	ambiguousHooks := make(map[int]struct{})
	ambiguousRuntimes := make(map[int]struct{})
	for hookIndex, runtimeIndex := range best.assignment {
		if runtimeIndex < 0 {
			continue
		}
		alternative := solveAssignments(len(normalized.current), len(input.Runtimes), pairs, excludedPair{hookIndex, runtimeIndex})
		if alternative.score < best.score-engine.config.AmbiguousScoreDelta {
			continue
		}
		for index := range best.assignment {
			if best.assignment[index] == alternative.assignment[index] {
				continue
			}
			ambiguousHooks[index] = struct{}{}
			if best.assignment[index] >= 0 {
				ambiguousRuntimes[best.assignment[index]] = struct{}{}
			}
			if alternative.assignment[index] >= 0 {
				ambiguousRuntimes[alternative.assignment[index]] = struct{}{}
			}
		}
	}

	if len(ambiguousHooks) > 0 || len(ambiguousRuntimes) > 0 {
		ambiguous := AmbiguousMatch{BestScore: best.score, Confidence: ConfidenceAmbiguous}
		for index := range ambiguousHooks {
			ambiguous.HookRefs = append(ambiguous.HookRefs, normalized.current[index].Ref())
		}
		for index := range ambiguousRuntimes {
			ambiguous.RuntimeRefs = append(ambiguous.RuntimeRefs, input.Runtimes[index].Ref())
		}
		sort.Strings(ambiguous.HookRefs)
		sort.Strings(ambiguous.RuntimeRefs)
		ambiguous.Reasons = append(ambiguous.Reasons, ReasonAmbiguousTopScore)
		if len(ambiguous.HookRefs) > 1 {
			ambiguous.Reasons = append(ambiguous.Reasons, ReasonCompetingHook)
		}
		if len(ambiguous.RuntimeRefs) > 1 || len(ambiguous.HookRefs) > 1 {
			ambiguous.Reasons = append(ambiguous.Reasons, ReasonCompetingRuntime)
		}
		ambiguous.Reasons = uniqueReasonCodes(ambiguous.Reasons)
		result.Ambiguous = append(result.Ambiguous, ambiguous)
	}

	matchedHooks := make(map[int]struct{})
	matchedRuntimes := make(map[int]struct{})
	for hookIndex, runtimeIndex := range best.assignment {
		if runtimeIndex < 0 {
			continue
		}
		if _, uncertain := ambiguousHooks[hookIndex]; uncertain {
			continue
		}
		if _, uncertain := ambiguousRuntimes[runtimeIndex]; uncertain {
			continue
		}
		pair, exists := findPair(pairs[hookIndex], runtimeIndex)
		if !exists {
			continue
		}
		result.Proposals = append(result.Proposals, MatchProposal{
			HookRef: normalized.current[hookIndex].Ref(), RuntimeRef: input.Runtimes[runtimeIndex].Ref(),
			Score: pair.score, Confidence: pair.confidence, Reasons: pair.reasons,
			WouldBindUnderCurrentThreshold: pair.confidence == ConfidenceExact || pair.confidence == ConfidenceStrong,
			RequiresReview:                 pair.confidence == ConfidenceWeak,
		})
		matchedHooks[hookIndex] = struct{}{}
		matchedRuntimes[runtimeIndex] = struct{}{}
	}

	for hookIndex, hook := range normalized.current {
		if _, matched := matchedHooks[hookIndex]; matched {
			continue
		}
		if _, uncertain := ambiguousHooks[hookIndex]; uncertain {
			continue
		}
		reasons := unmatchedHookReasons(allPairs[hookIndex], pairs[hookIndex])
		result.UnmatchedHooks = append(result.UnmatchedHooks, UnmatchedHook{HookRef: hook.Ref(), Reasons: reasons})
	}
	for runtimeIndex, runtime := range input.Runtimes {
		if _, matched := matchedRuntimes[runtimeIndex]; matched {
			continue
		}
		if _, uncertain := ambiguousRuntimes[runtimeIndex]; uncertain {
			continue
		}
		reasons := unmatchedRuntimeReasons(runtimeIndex, allPairs, pairs)
		result.UnmatchedRuntimes = append(result.UnmatchedRuntimes, UnmatchedRuntime{RuntimeRef: runtime.Ref(), Reasons: reasons})
	}

	sortResult(&result)
	for _, proposal := range result.Proposals {
		switch proposal.Confidence {
		case ConfidenceExact:
			result.Summary.Exact++
		case ConfidenceStrong:
			result.Summary.Strong++
		case ConfidenceWeak:
			result.Summary.Weak++
		}
	}
	for _, ambiguous := range result.Ambiguous {
		result.Summary.Ambiguous += len(ambiguous.HookRefs)
	}
	result.Summary.Rejected = rejectedHookCount(result)
	result.Risk = evaluateRisk(input.ExpectedMatches, result.Proposals)
	return result, nil
}

func setOwner(owners map[string]int, key string, owner int) {
	if previous, exists := owners[key]; exists && previous != owner {
		owners[key] = -1
		return
	}
	owners[key] = owner
}

func uniqueHookCount(hooks []HookObservation) int {
	refs := make(map[string]struct{}, len(hooks))
	for _, hook := range hooks {
		refs[hook.Ref()] = struct{}{}
	}
	return len(refs)
}

func rejectedHookCount(result CorrelationResult) int {
	classified := make(map[string]struct{})
	for _, proposal := range result.Proposals {
		classified[proposal.HookRef] = struct{}{}
	}
	for _, ambiguous := range result.Ambiguous {
		for _, ref := range ambiguous.HookRefs {
			classified[ref] = struct{}{}
		}
	}
	rejected := make(map[string]struct{})
	for _, match := range result.Rejected {
		if _, alreadyClassified := classified[match.HookRef]; !alreadyClassified {
			rejected[match.HookRef] = struct{}{}
		}
	}
	return len(rejected)
}

func evaluateRisk(labels []ExpectedMatch, proposals []MatchProposal) RiskSummary {
	result := RiskSummary{Labeled: len(labels)}
	proposed := make(map[string]string, len(proposals))
	for _, proposal := range proposals {
		proposed[proposal.HookRef] = proposal.RuntimeRef
	}
	for _, label := range labels {
		actual := proposed[label.HookRef]
		switch {
		case label.RuntimeRef == "" && actual == "":
			result.TrueNegative++
		case label.RuntimeRef == "" && actual != "":
			result.FalsePositive++
		case actual == label.RuntimeRef:
			result.TruePositive++
		case actual == "":
			result.FalseNegative++
		default:
			result.FalsePositive++
			result.FalseNegative++
		}
	}
	return result
}

func findPair(pairs []scoredPair, runtimeIndex int) (scoredPair, bool) {
	for _, pair := range pairs {
		if pair.runtimeIndex == runtimeIndex {
			return pair, true
		}
	}
	return scoredPair{}, false
}

func unmatchedHookReasons(all, eligible []scoredPair) []ReasonCode {
	if len(eligible) > 0 {
		return []ReasonCode{ReasonCompetingRuntime}
	}
	var reasons []ReasonCode
	for _, pair := range all {
		reasons = append(reasons, pair.conflicts...)
	}
	if len(reasons) == 0 {
		reasons = append(reasons, ReasonInsufficientEvidence)
	}
	return uniqueReasonCodes(reasons)
}

func unmatchedRuntimeReasons(runtimeIndex int, all, eligible [][]scoredPair) []ReasonCode {
	var reasons []ReasonCode
	hasEligible := false
	for hookIndex := range all {
		if _, exists := findPair(eligible[hookIndex], runtimeIndex); exists {
			hasEligible = true
		}
		if pair, exists := findPair(all[hookIndex], runtimeIndex); exists {
			reasons = append(reasons, pair.conflicts...)
		}
	}
	if hasEligible {
		return []ReasonCode{ReasonCompetingHook}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, ReasonInsufficientEvidence)
	}
	return uniqueReasonCodes(reasons)
}

func sortResult(result *CorrelationResult) {
	sort.Slice(result.Proposals, func(first, second int) bool {
		return result.Proposals[first].HookRef < result.Proposals[second].HookRef
	})
	sort.Slice(result.Rejected, func(first, second int) bool {
		if result.Rejected[first].HookRef != result.Rejected[second].HookRef {
			return result.Rejected[first].HookRef < result.Rejected[second].HookRef
		}
		return result.Rejected[first].RuntimeRef < result.Rejected[second].RuntimeRef
	})
	sort.Slice(result.UnmatchedHooks, func(first, second int) bool {
		return result.UnmatchedHooks[first].HookRef < result.UnmatchedHooks[second].HookRef
	})
	sort.Slice(result.UnmatchedRuntimes, func(first, second int) bool {
		return result.UnmatchedRuntimes[first].RuntimeRef < result.UnmatchedRuntimes[second].RuntimeRef
	})
	result.Diagnostics = uniqueReasonCodes(result.Diagnostics)
}
