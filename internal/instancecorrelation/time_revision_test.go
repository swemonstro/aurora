package instancecorrelation

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestStaleAndLifecycleObservationsAreRejected(t *testing.T) {
	tests := []struct {
		name          string
		mutateRuntime func(*RuntimeObservation)
		mutateHook    func(*HookObservation)
		evaluatedAt   time.Time
		reason        ReasonCode
	}{
		{
			name: "stale hook", evaluatedAt: fixtureTime.Add(10 * time.Minute), reason: ReasonStaleHook,
			mutateRuntime: func(runtime *RuntimeObservation) { runtime.ObservedAt = fixtureTime.Add(9 * time.Minute) },
		},
		{
			name: "stale runtime", evaluatedAt: fixtureTime.Add(10 * time.Minute), reason: ReasonStaleRuntime,
			mutateHook: func(hook *HookObservation) { hook.ObservedAt = fixtureTime.Add(9 * time.Minute) },
		},
		{
			name: "ended hook against active runtime", evaluatedAt: fixtureTime.Add(10 * time.Second), reason: ReasonLifecycleConflict,
			mutateHook: func(hook *HookObservation) { hook.Lifecycle = LifecycleEnded },
		},
		{
			name: "active hook against ended runtime", evaluatedAt: fixtureTime.Add(10 * time.Second), reason: ReasonLifecycleConflict,
			mutateRuntime: func(runtime *RuntimeObservation) {
				runtime.Lifecycle = LifecycleEnded
				endedAt := fixtureTime.Add(7 * time.Second)
				runtime.EndedAt = &endedAt
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
			hook := fixtureHook("hook-a", instancepresence.ToolClaude)
			process := runtime.Candidate.Runtime.RootProcess
			hook.ProcessHint = &process
			if test.mutateRuntime != nil {
				test.mutateRuntime(&runtime)
			}
			if test.mutateHook != nil {
				test.mutateHook(&hook)
			}
			engine, _ := New(DefaultConfig())
			result, err := engine.Correlate(CorrelationInput{EvaluatedAt: test.evaluatedAt, Runtimes: []RuntimeObservation{runtime}, Hooks: []HookObservation{hook}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Proposals) != 0 || !hasRejectedReason(result, test.reason) {
				t.Fatalf("result = %#v, want %q", result, test.reason)
			}
		})
	}
}

func TestHookLeadToleranceAndImpossibleTimeOrder(t *testing.T) {
	for _, test := range []struct {
		name       string
		rootOffset time.Duration
		wantMatch  bool
	}{
		{name: "within tolerance", rootOffset: 7 * time.Second, wantMatch: true},
		{name: "outside tolerance", rootOffset: 9 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, test.rootOffset)
			runtime.ObservedAt = fixtureTime.Add(9 * time.Second)
			hook := fixtureHook("hook-a", instancepresence.ToolClaude)
			process := runtime.Candidate.Runtime.RootProcess
			hook.ProcessHint = &process
			result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
			if (len(result.Proposals) == 1) != test.wantMatch {
				t.Fatalf("result = %#v", result)
			}
			if !test.wantMatch && !hasRejectedReason(result, ReasonLifecycleConflict) {
				t.Fatalf("result = %#v, want lifecycle conflict", result)
			}
		})
	}
}

func TestTimesNormalizeToUTCAndDropMonotonicMetadata(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	location := time.FixedZone("fixture-zone", 2*60*60)
	process := runtime.Candidate.Runtime.RootProcess
	process.StartedAt = process.StartedAt.In(location)
	hook.ProcessHint = &process
	hook.ObservedAt = hook.ObservedAt.In(location)
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook})
	if len(result.Proposals) != 1 || result.Proposals[0].Confidence != ConfidenceExact {
		t.Fatalf("result = %#v", result)
	}
}

func TestIdenticalRetryDeduplicatesAcrossIdempotencyKeys(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	process := runtime.Candidate.Runtime.RootProcess
	hook.ProcessHint = &process
	retry := hook
	retry.IdempotencyKey = "retry-key"
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{retry, hook})
	if len(result.Proposals) != 1 || len(result.Rejected) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestSameRevisionDifferentPayloadIsReported(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	hook := fixtureHook("hook-a", instancepresence.ToolClaude)
	conflict := hook
	conflict.Lifecycle = LifecycleIdle
	conflict.IdempotencyKey = "different-key"
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{hook, conflict})
	if len(result.Proposals) != 0 || !hasRejectedReason(result, ReasonSameRevisionConflict) {
		t.Fatalf("result = %#v", result)
	}
}

func TestReusedIdempotencyKeyWithDifferentObservationIsReported(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	first := fixtureHook("hook-a", instancepresence.ToolClaude)
	second := first
	second.Revision = 2
	second.Lifecycle = LifecycleIdle
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{first, second})
	if len(result.Proposals) != 0 || !hasRejectedReason(result, ReasonIdempotencyConflict) {
		t.Fatalf("result = %#v", result)
	}
}

func TestLowerRevisionIsSuperseded(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	newer := fixtureHook("hook-a", instancepresence.ToolClaude)
	newer.Revision = 2
	process := runtime.Candidate.Runtime.RootProcess
	newer.ProcessHint = &process
	older := newer
	older.Revision = 1
	older.IdempotencyKey = "older"
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{older, newer})
	if len(result.Proposals) != 1 || len(result.SupersededHooks) != 1 ||
		!containsReason(result.SupersededHooks[0].ReasonCodes, ReasonOutOfOrderRevision) {
		t.Fatalf("result = %#v", result)
	}
}

func TestDifferentProducerEpochsAreReportedWithoutOrdering(t *testing.T) {
	runtime := fixtureRuntime("runtime-a", instancepresence.ToolClaude, 101, 0)
	first := fixtureHook("hook-a", instancepresence.ToolClaude)
	second := first
	second.ProducerEpoch = "epoch-b"
	second.Revision = 99
	result := correlateFixture(t, []RuntimeObservation{runtime}, []HookObservation{first, second})
	if len(result.Proposals) != 0 || !hasRejectedReason(result, ReasonProducerEpochConflict) {
		t.Fatalf("result = %#v", result)
	}
}
