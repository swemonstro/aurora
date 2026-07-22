package linuxprocess

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestSnapshotDiffReportsExitAndPIDReuse(t *testing.T) {
	start := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	observed := start.Add(time.Minute)
	old := processObservation(101, start)
	survivor := processObservation(202, start)
	reused := processObservation(101, start.Add(30*time.Second))
	previous := instancepresence.ProcessSnapshot{ObservedAt: start, Processes: []instancepresence.ProcessObservation{survivor, old}}
	current := instancepresence.ProcessSnapshot{ObservedAt: observed, Processes: []instancepresence.ProcessObservation{reused, survivor}}

	result, err := DiffSamples(Sample{Snapshot: previous}, Sample{Snapshot: current})
	if err != nil {
		t.Fatalf("DiffSamples() error = %v", err)
	}
	if len(result.Exits) != 1 || !sameProcess(result.Exits[0].Process, old.Process) {
		t.Fatalf("exits = %#v", result.Exits)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != ReasonPIDReused {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
}

func TestConservativeDiffSuppressesUnreadablePIDExit(t *testing.T) {
	start := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	previous := Sample{Snapshot: instancepresence.ProcessSnapshot{
		ObservedAt: start, Processes: []instancepresence.ProcessObservation{processObservation(101, start)},
	}}
	current := Sample{
		Snapshot:      instancepresence.ProcessSnapshot{ObservedAt: start.Add(time.Second), Processes: []instancepresence.ProcessObservation{}},
		uncertainPIDs: map[uint64]struct{}{101: {}},
	}
	result, err := DiffSamples(previous, current)
	if err != nil || len(result.Exits) != 0 {
		t.Fatalf("conservative diff = %#v, %v", result, err)
	}
}

func TestSnapshotDiffSortsExitsDeterministically(t *testing.T) {
	start := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	previous := instancepresence.ProcessSnapshot{
		ObservedAt: start,
		Processes: []instancepresence.ProcessObservation{
			processObservation(303, start), processObservation(101, start),
		},
	}
	current := instancepresence.ProcessSnapshot{ObservedAt: start.Add(time.Second), Processes: []instancepresence.ProcessObservation{}}
	exits, err := DiffSnapshots(previous, current, current.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(exits) != 2 || exits[0].Process.PID != 101 || exits[1].Process.PID != 303 {
		t.Fatalf("sorted exits = %#v", exits)
	}
}

func processObservation(pid uint64, started time.Time) instancepresence.ProcessObservation {
	return instancepresence.ProcessObservation{
		Process:            instancepresence.ProcessIdentity{PID: pid, StartedAt: started},
		ExecutableIdentity: "exe:test", OwnerIdentity: "uid:1000",
	}
}
