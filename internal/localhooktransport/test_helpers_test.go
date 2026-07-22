package localhooktransport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
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
	samples []linuxprocess.Sample
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (source *fakeSnapshots) Observe(ctx context.Context) (linuxprocess.Sample, error) {
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
			return linuxprocess.Sample{}, ctx.Err()
		}
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	index := source.calls
	if index >= len(source.samples) {
		index = len(source.samples) - 1
	}
	source.calls++
	return source.samples[index], nil
}

func (source *fakeSnapshots) Calls() int {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return source.calls
}

func newTestReceiver(t *testing.T, clock *testClock, source SnapshotSource) (*Receiver, Config) {
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

func testSample(runtimes ...instancecorrelation.RuntimeObservation) linuxprocess.Sample {
	sample := linuxprocess.Sample{
		Snapshot: instancepresence.ProcessSnapshot{ObservedAt: testTime, Processes: []instancepresence.ProcessObservation{}},
		Families: []linuxprocess.Family{},
	}
	for _, runtime := range runtimes {
		sample.Families = append(sample.Families, linuxprocess.Family{Candidate: runtime.Candidate})
		for _, member := range runtime.Candidate.Members {
			observation := instancepresence.ProcessObservation{
				Process: member, ExecutableIdentity: "exe:fixture", OwnerIdentity: "owner:fixture",
			}
			if member.PID == runtime.Candidate.Runtime.RootProcess.PID && member.StartedAt.Equal(runtime.Candidate.Runtime.RootProcess.StartedAt) {
				observation.ProcessGroupOrJob = runtime.ProcessGroupOrJob
				observation.OSSession = runtime.OSSession
				observation.TerminalFingerprint = runtime.TerminalFingerprint
			}
			sample.Snapshot.Processes = append(sample.Snapshot.Processes, observation)
		}
	}
	return sample
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
