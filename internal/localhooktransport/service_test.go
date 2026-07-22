package localhooktransport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestReceiverCorrelationOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		runtimes   func() []instancecorrelation.RuntimeObservation
		mutate     func(*Request, []instancecorrelation.RuntimeObservation)
		confidence instancecorrelation.Confidence
		ambiguous  bool
		rejected   bool
	}{
		{
			name: "exact root", runtimes: oneClaudeRuntime,
			mutate: func(request *Request, runtimes []instancecorrelation.RuntimeObservation) {
				root := runtimes[0].Candidate.Runtime.RootProcess
				request.Observation.ProcessHint = &ProcessIdentity{PID: root.PID, StartedAt: root.StartedAt}
			}, confidence: instancecorrelation.ConfidenceExact,
		},
		{
			name: "strong member", runtimes: func() []instancecorrelation.RuntimeObservation {
				runtime := oneClaudeRuntime()[0]
				runtime.Candidate.Members = append(runtime.Candidate.Members, instancepresence.ProcessIdentity{PID: 102, StartedAt: testTime.Add(-500 * time.Millisecond)})
				return []instancecorrelation.RuntimeObservation{runtime}
			},
			mutate: func(request *Request, runtimes []instancecorrelation.RuntimeObservation) {
				member := runtimes[0].Candidate.Members[1]
				request.Observation.ProcessHint = &ProcessIdentity{PID: member.PID, StartedAt: member.StartedAt}
			}, confidence: instancecorrelation.ConfidenceStrong,
		},
		{
			name: "weak review", runtimes: func() []instancecorrelation.RuntimeObservation {
				runtime := oneClaudeRuntime()[0]
				runtime.ProcessGroupOrJob, runtime.OSSession = "group-a", "session-a"
				return []instancecorrelation.RuntimeObservation{runtime}
			},
			mutate: func(request *Request, _ []instancecorrelation.RuntimeObservation) {
				request.Observation.ProcessGroupOrJob, request.Observation.OSSession = "group-a", "session-a"
			}, confidence: instancecorrelation.ConfidenceWeak,
		},
		{
			name: "ambiguous", runtimes: func() []instancecorrelation.RuntimeObservation {
				first, second := testRuntime("runtime-a", instancepresence.ToolClaude, 101), testRuntime("runtime-b", instancepresence.ToolClaude, 201)
				first.ProcessGroupOrJob, first.OSSession = "group-a", "session-a"
				second.ProcessGroupOrJob, second.OSSession = "group-a", "session-a"
				return []instancecorrelation.RuntimeObservation{first, second}
			},
			mutate: func(request *Request, _ []instancecorrelation.RuntimeObservation) {
				request.Observation.ProcessGroupOrJob, request.Observation.OSSession = "group-a", "session-a"
			}, ambiguous: true,
		},
		{
			name: "rejected host conflict", runtimes: oneClaudeRuntime,
			mutate: func(request *Request, runtimes []instancecorrelation.RuntimeObservation) {
				root := runtimes[0].Candidate.Runtime.RootProcess
				request.Observation.ProcessHint = &ProcessIdentity{PID: root.PID, StartedAt: root.StartedAt}
				request.Observation.HostID = "host-other"
			}, rejected: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &testClock{now: testTime}
			runtimes := test.runtimes()
			receiver, _ := newTestReceiver(t, clock, &fakeSnapshots{samples: [][]instancecorrelation.RuntimeObservation{testSample(runtimes...)}})
			request := testRequest("request-01", instancepresence.ToolClaude)
			test.mutate(&request, runtimes)
			response := receiver.Handle(context.Background(), request)
			if !response.NoBindingPerformed {
				t.Fatal("receiver reported a performed binding")
			}
			switch {
			case test.ambiguous:
				if len(response.Ambiguous) != 1 || len(response.Proposals) != 0 {
					t.Fatalf("response = %#v", response)
				}
			case test.rejected:
				if len(response.Rejected) == 0 || len(response.Proposals) != 0 {
					t.Fatalf("response = %#v", response)
				}
			default:
				if len(response.Proposals) != 1 || response.Proposals[0].Confidence != test.confidence {
					t.Fatalf("response = %#v", response)
				}
				if test.confidence == instancecorrelation.ConfidenceWeak && !response.Proposals[0].Review {
					t.Fatal("weak proposal does not require review")
				}
			}
		})
	}
}

func TestReceiverResponseDoesNotSerializeProcessOrSourceData(t *testing.T) {
	clock := &testClock{now: testTime}
	runtime := oneClaudeRuntime()[0]
	receiver, _ := newTestReceiver(t, clock, &fakeSnapshots{samples: [][]instancecorrelation.RuntimeObservation{testSample(runtime)}})
	request := testRequest("request-01", instancepresence.ToolClaude)
	root := runtime.Candidate.Runtime.RootProcess
	request.Observation.ProcessHint = &ProcessIdentity{PID: root.PID, StartedAt: root.StartedAt}
	response := receiver.Handle(context.Background(), request)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pid", "started_at", "host_id", "boot_id", "process_group_or_job", "os_session", "terminal_fingerprint", "provider", "profile", "source"} {
		if strings.Contains(string(data), `"`+forbidden+`"`) {
			t.Fatalf("response contains %q: %s", forbidden, data)
		}
	}
}

func TestReceiverReplayDiagnosticsAndNoCorrelationStateTransfer(t *testing.T) {
	clock := &testClock{now: testTime}
	runtimeA, runtimeB := testRuntime("runtime-a", instancepresence.ToolClaude, 101), testRuntime("runtime-b", instancepresence.ToolClaude, 201)
	source := &fakeSnapshots{samples: [][]instancecorrelation.RuntimeObservation{testSample(runtimeA), testSample(runtimeB), testSample(runtimeB)}}
	receiver, config := newTestReceiver(t, clock, source)
	first := testRequest("request-01", instancepresence.ToolClaude)
	rootA := runtimeA.Candidate.Runtime.RootProcess
	first.Observation.ProcessHint = &ProcessIdentity{PID: rootA.PID, StartedAt: rootA.StartedAt}
	if response := receiver.Handle(context.Background(), first); len(response.Proposals) != 1 || response.Proposals[0].RuntimeRef != "runtime-a" {
		t.Fatalf("first response = %#v", response)
	}
	if response := receiver.Handle(context.Background(), first); response.Status != StatusDuplicate || !hasErrorCode(response, CodeDuplicateRequest) {
		t.Fatalf("duplicate response = %#v", response)
	}
	if source.Calls() != 1 {
		t.Fatalf("snapshot calls after retry = %d", source.Calls())
	}
	conflict := first
	conflict.Observation.Revision++
	if response := receiver.Handle(context.Background(), conflict); !hasErrorCode(response, CodeRequestIDConflict) {
		t.Fatalf("conflict response = %#v", response)
	}
	second := testRequest("request-02", instancepresence.ToolClaude)
	rootB := runtimeB.Candidate.Runtime.RootProcess
	second.Observation.ProcessHint = &ProcessIdentity{PID: rootB.PID, StartedAt: rootB.StartedAt}
	if response := receiver.Handle(context.Background(), second); len(response.Proposals) != 1 || response.Proposals[0].RuntimeRef != "runtime-b" {
		t.Fatalf("second response = %#v", response)
	}
	if source.Calls() != 2 {
		t.Fatalf("independent snapshot calls = %d", source.Calls())
	}
	clock.Advance(config.ReplayTTL)
	if response := receiver.Handle(context.Background(), first); response.Status == StatusDuplicate {
		t.Fatalf("expired replay response = %#v", response)
	}
}

func TestReceiverCandidateLimitIsConservative(t *testing.T) {
	clock := &testClock{now: testTime}
	var runtimes []instancecorrelation.RuntimeObservation
	for index := 0; index < 13; index++ {
		runtimes = append(runtimes, testRuntime(fmt.Sprintf("runtime-%02d", index), instancepresence.ToolClaude, uint64(100+index)))
	}
	receiver, _ := newTestReceiver(t, clock, &fakeSnapshots{samples: [][]instancecorrelation.RuntimeObservation{testSample(runtimes...)}})
	response := receiver.Handle(context.Background(), testRequest("request-01", instancepresence.ToolClaude))
	if len(response.Proposals) != 0 || !hasErrorCode(response, CodeCorrelationFailed) || !hasErrorCode(response, CodeInsufficientEvidence) {
		t.Fatalf("response = %#v", response)
	}
}

func TestReceiverReplayOverflowIsConservative(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultConfig(clock)
	config.ReplayCapacity = 1
	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCorrelationService(&fakeSnapshots{samples: [][]instancecorrelation.RuntimeObservation{testSample(), testSample()}}, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(config, service)
	if err != nil {
		t.Fatal(err)
	}
	if response := receiver.Handle(context.Background(), testRequest("request-01", instancepresence.ToolClaude)); response.Status != StatusOK {
		t.Fatalf("first response = %#v", response)
	}
	if response := receiver.Handle(context.Background(), testRequest("request-02", instancepresence.ToolClaude)); response.Status != StatusRejected || !hasErrorCode(response, CodeReplayCacheFull) {
		t.Fatalf("overflow response = %#v", response)
	}
}

func oneClaudeRuntime() []instancecorrelation.RuntimeObservation {
	return []instancecorrelation.RuntimeObservation{testRuntime("runtime-a", instancepresence.ToolClaude, 101)}
}

func hasErrorCode(response Response, code ErrorCode) bool {
	for _, actual := range response.ErrorCodes {
		if actual == code {
			return true
		}
	}
	return false
}
