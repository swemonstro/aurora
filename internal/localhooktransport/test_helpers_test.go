package localhooktransport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

var testTime = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

type testClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *testClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	clock.now = clock.now.Add(duration)
	clock.mutex.Unlock()
}

type fakeSnapshots struct {
	mutex   sync.Mutex
	samples [][]instancecorrelation.RuntimeObservation
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (source *fakeSnapshots) Observe(ctx context.Context) ([]instancecorrelation.RuntimeObservation, error) {
	if source.entered != nil {
		select {
		case source.entered <- struct{}{}:
		default:
		}
	}
	if source.release != nil {
		select {
		case <-source.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	index := source.calls
	if index >= len(source.samples) {
		index = len(source.samples) - 1
	}
	source.calls++
	return append([]instancecorrelation.RuntimeObservation{}, source.samples[index]...), nil
}

func (source *fakeSnapshots) Calls() int {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return source.calls
}

func newTestReceiver(t *testing.T, clock *testClock, source RuntimeObservationSource) (*Receiver, Config) {
	t.Helper()
	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(clock)
	service, err := NewCorrelationService(source, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(config, service)
	if err != nil {
		t.Fatal(err)
	}
	return receiver, config
}

func testSample(runtimes ...instancecorrelation.RuntimeObservation) []instancecorrelation.RuntimeObservation {
	return append([]instancecorrelation.RuntimeObservation{}, runtimes...)
}

func testRuntime(ref string, tool instancepresence.ToolKind, pid uint64) instancecorrelation.RuntimeObservation {
	root := instancepresence.ProcessIdentity{PID: pid, StartedAt: testTime.Add(-time.Second)}
	return instancecorrelation.RuntimeObservation{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: instancepresence.InstanceID(ref), Tool: tool,
			Runtime: instancepresence.RuntimeIdentity{HostID: "host-fixture", BootID: "boot-fixture", RootProcess: root},
			Members: []instancepresence.ProcessIdentity{root},
		},
		ObservedAt: testTime, Lifecycle: instancecorrelation.LifecycleActive,
	}
}

func testRequest(id string, tool instancepresence.ToolKind) Request {
	return Request{
		ProtocolVersion: CurrentProtocolVersion, RequestID: id, Operation: OperationCorrelateObservation,
		Observation: HookObservation{
			Tool: tool, HookSessionRef: "hook-fixture", ProducerEpoch: "epoch-fixture",
			Revision: 1, IdempotencyKey: "idem-fixture", ObservedAt: testTime,
			Lifecycle: instancecorrelation.LifecycleActive,
		},
	}
}
