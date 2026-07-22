package instancecorrelation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

var fixtureTime = time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

func TestExactRootAndMemberProcessIdentity(t *testing.T) {
	tests := []struct {
		name       string
		member     bool
		confidence Confidence
		reason     ReasonCode
	}{
		{name: "root is exact", confidence: ConfidenceExact, reason: ReasonRootProcessMatch},
		{name: "member is strong", member: true, confidence: ConfidenceStrong, reason: ReasonMemberProcessMatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
			process := runtime.Candidate.Runtime.RootProcess
			if test.member {
				process = processIdentity(102, time.Second)
				runtime.Candidate.Members = append(runtime.Candidate.Members, process)
			}
			hook := fixtureHook("hook-a", instancepresence.ToolClaude)
			hook.ProcessHint = &process
			result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
			if len(result.Proposals) != 1 || result.Proposals[0].Confidence != test.confidence {
				t.Fatalf("proposals = %#v", result.Proposals)
			}
			assertProposalReason(t, result.Proposals[0], ReasonExactProcessIdentity)
			assertProposalReason(t, result.Proposals[0], test.reason)
		})
	}
}

func TestExactRuntimeIdentity(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolCodex, 201, 0)
	hook := fixtureHook("hook-a", instancepresence.ToolCodex)
	hint := runtime.Candidate.Runtime
	hook.RuntimeHint = &hint
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
	if len(result.Proposals) != 1 || result.Proposals[0].Confidence != ConfidenceExact {
		t.Fatalf("proposals = %#v", result.Proposals)
	}
	assertProposalReason(t, result.Proposals[0], ReasonExactRuntimeIdentity)
}

func TestHardIdentityConflictsBlockSoftScore(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*HookObservation, RuntimeObservation)
		reason ReasonCode
	}{
		{
			name: "process generation", reason: ReasonProcessGenerationConflict,
			mutate: func(hook *HookObservation, runtime RuntimeObservation) {
				value := runtime.Candidate.Runtime.RootProcess
				value.StartedAt = value.StartedAt.Add(time.Second)
				hook.ProcessHint = &value
			},
		},
		{name: "host", reason: ReasonHostConflict, mutate: func(hook *HookObservation, _ RuntimeObservation) { hook.HostID = "host-other" }},
		{name: "boot", reason: ReasonBootConflict, mutate: func(hook *HookObservation, _ RuntimeObservation) { hook.BootID = "boot-other" }},
		{name: "tool", reason: ReasonToolConflict, mutate: func(hook *HookObservation, _ RuntimeObservation) { hook.Tool = instancepresence.ToolCodex }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
			runtime.ProcessGroupOrJob = "group-a"
			runtime.OSSession = "session-a"
			runtime.TerminalFingerprint = "terminal-a"
			hook := fixtureHook("hook-a", instancepresence.ToolClaude)
			hook.ProcessGroupOrJob = "group-a"
			hook.OSSession = "session-a"
			hook.TerminalFingerprint = "terminal-a"
			test.mutate(&hook, runtime)
			result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
			if len(result.Proposals) != 0 || !hasRejectedReason(result, test.reason) {
				t.Fatalf("result = %#v, want conflict %q", result, test.reason)
			}
		})
	}
}

func TestMissingOptionalFieldsAreNotConflicts(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	process := runtime.Candidate.Runtime.RootProcess
	hook.ProcessHint = &process
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
	if len(result.Proposals) != 1 || len(result.Rejected) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestWeakSignalsCannotCreateMatchAlone(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*HookObservation, *RuntimeObservation)
	}{
		{
			name: "source and tool",
			mutate: func(hook *HookObservation, runtime *RuntimeObservation) {
				source := fixtureSource()
				hook.Source, runtime.Source = &source, &source
			},
		},
		{
			name: "terminal",
			mutate: func(hook *HookObservation, runtime *RuntimeObservation) {
				hook.TerminalFingerprint, runtime.TerminalFingerprint = "terminal-a", "terminal-a"
				runtime.Candidate.Runtime.RootProcess.StartedAt = fixtureTime.Add(-time.Minute)
				runtime.Candidate.Members[0] = runtime.Candidate.Runtime.RootProcess
			},
		},
		{name: "time proximity", mutate: func(*HookObservation, *RuntimeObservation) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
			hook := fixtureHook("hook-a", instancepresence.ToolClaude)
			test.mutate(&hook, &runtime)
			result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
			if len(result.Proposals) != 0 || len(result.UnmatchedHooks) != 1 || !containsReason(result.UnmatchedHooks[0].Reasons, ReasonInsufficientEvidence) {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestEveryScoreContributionHasReasonAndThresholdsAreExplicit(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	runtime.ProcessGroupOrJob = "group-a"
	runtime.OSSession = "session-a"
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	hook.ProcessGroupOrJob = "group-a"
	hook.OSSession = "session-a"
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
	if len(result.Proposals) != 1 || result.Proposals[0].Confidence != ConfidenceWeak || !result.Proposals[0].RequiresReview {
		t.Fatalf("proposals = %#v", result.Proposals)
	}
	sum := 0
	for _, reason := range result.Proposals[0].Reasons {
		sum += reason.Points
	}
	if sum != result.Proposals[0].Score {
		t.Fatalf("reason points = %d, score = %d", sum, result.Proposals[0].Score)
	}
	if result.Proposals[0].WouldBindUnderCurrentThreshold {
		t.Fatal("weak observe-only proposal unexpectedly marked would-bind")
	}
}

func TestAmbiguousScoreDeltaProducesNoArbitraryProposal(t *testing.T) {
	runtimeA := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	runtimeB := fixtureRuntime("runtime-b", instancepresence.ToolClaude, 201, 0)
	for _, runtime := range []*RuntimeObservation{&runtimeA, &runtimeB} {
		runtime.ProcessGroupOrJob = "group-a"
		runtime.OSSession = "session-a"
	}
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	hook.ProcessGroupOrJob = "group-a"
	hook.OSSession = "session-a"
	result := correlateFixture(t, []RuntimeObservation{runtimeB, runtimeA}, []HookObservation{hook})
	if len(result.Proposals) != 0 || len(result.Ambiguous) != 1 || len(result.Ambiguous[0].RuntimeRefs) != 2 {
		t.Fatalf("result = %#v", result)
	}
}

func TestNearTieWithinConfiguredDeltaIsAmbiguous(t *testing.T) {
	runtimeA := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	runtimeB := fixtureRuntime("runtime-b", instancepresence.ToolClaude, 201, 0)
	for _, runtime := range []*RuntimeObservation{&runtimeA, &runtimeB} {
		runtime.ProcessGroupOrJob = "group-a"
		runtime.OSSession = "session-a"
	}
	runtimeA.TerminalFingerprint = "terminal-a"
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	hook.ProcessGroupOrJob, hook.OSSession, hook.TerminalFingerprint = "group-a", "session-a", "terminal-a"
	config := DefaultConfig()
	config.AmbiguousScoreDelta = config.Weights.TerminalMatch
	engine, _ := New(config)
	result, err := engine.Correlate(CorrelationInput{
		EvaluatedAt: fixtureTime.Add(10 * time.Second),
		Runtimes:    []RuntimeObservation{runtimeA, runtimeB}, Hooks: []HookObservation{hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposals) != 0 || len(result.Ambiguous) != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestWeakThresholdBoundary(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, -time.Minute)
	runtime.ProcessGroupOrJob = "group-a"
	runtime.ObservedAt = fixtureTime.Add(-time.Minute)
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	hook.ProcessGroupOrJob = "group-a"
	config := DefaultConfig()
	config.Weights.ProcessGroupMatch = 50
	config.MinimumWeakScore = config.Weights.ToolMatch + config.Weights.ProcessGroupMatch
	engine, _ := New(config)
	input := CorrelationInput{EvaluatedAt: fixtureTime.Add(10 * time.Second), Runtimes: []RuntimeObservation{runtime}, Hooks: []HookObservation{hook}}
	result, err := engine.Correlate(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposals) != 1 || result.Proposals[0].Score != config.MinimumWeakScore {
		t.Fatalf("boundary result = %#v", result)
	}
	config.MinimumWeakScore++
	engine, _ = New(config)
	result, err = engine.Correlate(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposals) != 0 {
		t.Fatalf("above-boundary result = %#v", result)
	}
}

func TestCompetingHooksDoNotBothWinSameRuntime(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	first := fixtureHook("hook-a", instancepresence.ToolClaude)
	second := fixtureHook("hook-b", instancepresence.ToolClaude)
	process := runtime.Candidate.Runtime.RootProcess
	first.ProcessHint, second.ProcessHint = &process, &process
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{second, first})
	if len(result.Proposals) != 0 || len(result.Ambiguous) != 1 ||
		len(result.Ambiguous[0].HookRefs) != 2 || len(result.Ambiguous[0].RuntimeRefs) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !containsReason(result.Ambiguous[0].Reasons, ReasonCompetingHook) {
		t.Fatalf("ambiguous reasons = %#v", result.Ambiguous[0].Reasons)
	}
}

func TestRiskSummaryMeasuresLabeledFalsePositiveAndNegative(t *testing.T) {
	runtimeA := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	runtimeB := fixtureRuntime("runtime-b", instancepresence.ToolClaude, 201, 0)
	hookA := fixtureHook("hook-a", instancepresence.ToolClaude)
	hookB := fixtureHook("hook-b", instancepresence.ToolClaude)
	processA, processB := runtimeA.Candidate.Runtime.RootProcess, runtimeB.Candidate.Runtime.RootProcess
	hookA.ProcessHint, hookB.ProcessHint = &processA, &processB
	engine, _ := New(DefaultConfig())
	result, err := engine.Correlate(CorrelationInput{
		EvaluatedAt: fixtureTime.Add(10 * time.Second),
		Runtimes:    []RuntimeObservation{runtimeA, runtimeB}, Hooks: []HookObservation{hookA, hookB},
		ExpectedMatches: []ExpectedMatch{
			{HookRef: hookA.Ref(), RuntimeRef: runtimeB.Ref()},
			{HookRef: hookB.Ref()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Risk.FalsePositive != 2 || result.Risk.FalseNegative != 1 || result.Risk.Labeled != 2 {
		t.Fatalf("risk = %#v", result.Risk)
	}
}

func TestCorrelationIsIndependentOfInputOrder(t *testing.T) {
	runtimeA := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	runtimeB := fixtureRuntime("runtime-b", instancepresence.ToolCodex, 201, 0)
	hookA := fixtureHook("hook-a", instancepresence.ToolClaude)
	hookB := fixtureHook("hook-b", instancepresence.ToolCodex)
	processA, processB := runtimeA.Candidate.Runtime.RootProcess, runtimeB.Candidate.Runtime.RootProcess
	hookA.ProcessHint, hookB.ProcessHint = &processA, &processB
	first := correlateFixture(t, []RuntimeObservation{runtimeA, runtimeB}, []HookObservation{hookA, hookB})
	second := correlateFixture(t, []RuntimeObservation{runtimeB, runtimeA}, []HookObservation{hookB, hookA})
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("ordered result differs:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestCandidateLimitIsExplicitAndConservative(t *testing.T) {
	config := DefaultConfig()
	config.MaximumCandidateSize = 1
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	input := CorrelationInput{
		EvaluatedAt: fixtureTime.Add(10 * time.Second),
		Runtimes: []RuntimeObservation{
			fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0),
			fixtureRuntime("runtime-b", instancepresence.ToolClaude, 201, 0),
		},
		Hooks: []HookObservation{fixtureHook("hook-a", instancepresence.ToolClaude)},
	}
	result, err := engine.Correlate(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposals) != 0 || !containsReason(result.Diagnostics, ReasonCandidateLimitExceeded) {
		t.Fatalf("result = %#v", result)
	}
}

func TestConfigValidation(t *testing.T) {
	config := DefaultConfig()
	config.AmbiguousScoreDelta = -1
	if _, err := New(config); err == nil {
		t.Fatal("negative ambiguous delta accepted")
	}
	config = DefaultConfig()
	config.MinimumStrongScore = config.MinimumWeakScore
	if _, err := New(config); err == nil {
		t.Fatal("contradictory thresholds accepted")
	}
	config = DefaultConfig()
	config.Weights.ToolMatch = -1
	if _, err := New(config); err == nil {
		t.Fatal("negative weight accepted")
	}
}

func correlateFixture(t *testing.T, runtimes []RuntimeObservation, hooks []HookObservation) CorrelationResult {
	t.Helper()
	engine, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Correlate(CorrelationInput{
		EvaluatedAt: fixtureTime.Add(10 * time.Second), Runtimes: runtimes, Hooks: hooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func fixtureRuntime(ref string, tool instancepresence.ToolKind, pid uint64, startOffset time.Duration) RuntimeObservation {
	root := processIdentity(pid, startOffset)
	return RuntimeObservation{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: instancepresence.InstanceID(ref), Tool: tool,
			Runtime: instancepresence.RuntimeIdentity{HostID: "host-a", BootID: "boot-a", RootProcess: root},
			Members: []instancepresence.ProcessIdentity{root},
		},
		ObservedAt: fixtureTime.Add(5 * time.Second), Lifecycle: LifecycleActive,
	}
}

func fixtureHook(ref string, tool instancepresence.ToolKind) HookObservation {
	return HookObservation{
		Tool: tool, HookSessionRef: instancepresence.OpaqueIdentity(ref), ProducerEpoch: "epoch-a",
		Revision: 1, IdempotencyKey: "key-" + ref, ObservedAt: fixtureTime.Add(6 * time.Second), Lifecycle: LifecycleActive,
	}
}

func processIdentity(pid uint64, offset time.Duration) instancepresence.ProcessIdentity {
	return instancepresence.ProcessIdentity{PID: pid, StartedAt: fixtureTime.Add(offset)}
}

func fixtureSource() instancepresence.SourceDescriptor {
	return instancepresence.SourceDescriptor{Provider: "provider-a", Profile: "profile-a", CollectorID: "collector-a"}
}

func assertProposalReason(t *testing.T, proposal MatchProposal, reason ReasonCode) {
	t.Helper()
	for _, actual := range proposal.Reasons {
		if actual.Code == reason {
			return
		}
	}
	t.Fatalf("proposal reasons = %#v, want %q", proposal.Reasons, reason)
}

func hasRejectedReason(result CorrelationResult, reason ReasonCode) bool {
	for _, rejected := range result.Rejected {
		if containsReason(rejected.Reasons, reason) {
			return true
		}
	}
	return false
}

func containsReason(reasons []ReasonCode, reason ReasonCode) bool {
	for _, actual := range reasons {
		if actual == reason {
			return true
		}
	}
	return false
}
