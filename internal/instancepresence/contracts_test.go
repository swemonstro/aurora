package instancepresence

import (
	"reflect"
	"testing"
	"time"
)

func TestUncertainCorrelationHasNoMutationTarget(t *testing.T) {
	results := []CorrelationResult{
		{Outcome: CorrelationAmbiguous, Candidates: []InstanceID{"instance-alpha", "instance-beta"}, Evidence: []EvidenceKind{EvidenceCWD, EvidenceTerminal}},
		{Outcome: CorrelationUnmatched},
	}
	states := map[InstanceID]EffectiveState{"instance-alpha": StateIdle, "instance-beta": StateIdle}

	for _, result := range results {
		if err := result.Validate(); err != nil {
			t.Fatalf("valid correlation rejected: %v", err)
		}
		if target, ok := result.Target(); ok {
			states[target] = StateWorking
		}
	}
	for id, state := range states {
		if state != StateIdle {
			t.Errorf("uncertain correlation mutated %s to %s", id, state)
		}
	}
}

func TestUniqueCorrelationSelectsExactlyOneTarget(t *testing.T) {
	result := CorrelationResult{
		Outcome:    CorrelationUniquelyMatched,
		InstanceID: "instance-alpha",
		Candidates: []InstanceID{"instance-alpha"},
		Evidence:   []EvidenceKind{EvidenceAncestor},
	}
	target, ok := result.Target()
	if !ok || target != "instance-alpha" {
		t.Fatalf("Target = %q, %t", target, ok)
	}
}

func TestCorrelationEvidenceValidation(t *testing.T) {
	base := CorrelationResult{
		Outcome: CorrelationUniquelyMatched, InstanceID: "instance-alpha",
		Candidates: []InstanceID{"instance-alpha"}, Evidence: []EvidenceKind{EvidenceAncestor},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}

	tests := []struct {
		name     string
		evidence []EvidenceKind
	}{
		{name: "missing evidence", evidence: nil},
		{name: "unknown evidence", evidence: []EvidenceKind{"nearest_pid"}},
		{name: "duplicate evidence", evidence: []EvidenceKind{EvidenceAncestor, EvidenceAncestor}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := base
			result.Evidence = test.evidence
			if err := result.Validate(); err == nil {
				t.Fatal("invalid correlation evidence was accepted")
			}
		})
	}
}

func TestObservationContractValidation(t *testing.T) {
	startedAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	observedAt := startedAt.Add(time.Second)
	process := ProcessObservation{
		Process:            ProcessIdentity{PID: 101, StartedAt: startedAt},
		ExecutableIdentity: "executable-alpha", OwnerIdentity: "owner-alpha",
	}
	snapshot := ProcessSnapshot{ObservedAt: observedAt, Processes: []ProcessObservation{process}}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("valid process snapshot rejected: %v", err)
	}
	exit := ProcessExit{Process: process.Process, ObservedAt: observedAt}
	if err := exit.Validate(); err != nil {
		t.Fatalf("valid process exit rejected: %v", err)
	}
	candidate := RuntimeCandidate{
		InstanceID: "instance-alpha", Tool: ToolClaude,
		Runtime: RuntimeIdentity{HostID: "host-alpha", BootID: "boot-alpha", RootProcess: process.Process},
		Members: []ProcessIdentity{process.Process},
	}
	if err := candidate.Validate(); err != nil {
		t.Fatalf("valid runtime candidate rejected: %v", err)
	}
	hook := HookObservation{
		Tool: ToolClaude, HookSessionRef: "hook-alpha", ObservedAt: observedAt,
		VerifiedAncestors: []ProcessIdentity{process.Process},
	}
	if err := hook.Validate(); err != nil {
		t.Fatalf("valid hook observation rejected: %v", err)
	}

	tests := []struct {
		name     string
		validate func() error
	}{
		{name: "snapshot missing time", validate: func() error { value := snapshot; value.ObservedAt = time.Time{}; return value.Validate() }},
		{name: "snapshot invalid process", validate: func() error {
			value := snapshot
			value.Processes = []ProcessObservation{process}
			value.Processes[0].Process.StartedAt = time.Time{}
			return value.Validate()
		}},
		{name: "exit missing time", validate: func() error { value := exit; value.ObservedAt = time.Time{}; return value.Validate() }},
		{name: "exit invalid process", validate: func() error { value := exit; value.Process.PID = 0; return value.Validate() }},
		{name: "candidate missing instance ID", validate: func() error { value := candidate; value.InstanceID = ""; return value.Validate() }},
		{name: "candidate invalid tool", validate: func() error { value := candidate; value.Tool = "other"; return value.Validate() }},
		{name: "candidate invalid runtime", validate: func() error { value := candidate; value.Runtime.BootID = ""; return value.Validate() }},
		{name: "candidate without members", validate: func() error { value := candidate; value.Members = nil; return value.Validate() }},
		{name: "candidate invalid member", validate: func() error {
			value := candidate
			value.Members = []ProcessIdentity{{PID: 101}}
			return value.Validate()
		}},
		{name: "hook invalid tool", validate: func() error { value := hook; value.Tool = "other"; return value.Validate() }},
		{name: "hook missing reference", validate: func() error { value := hook; value.HookSessionRef = " "; return value.Validate() }},
		{name: "hook missing time", validate: func() error { value := hook; value.ObservedAt = time.Time{}; return value.Validate() }},
		{name: "hook invalid ancestor", validate: func() error {
			value := hook
			value.VerifiedAncestors = []ProcessIdentity{{PID: 101}}
			return value.Validate()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(); err == nil {
				t.Fatal("invalid contract value was accepted")
			}
		})
	}
}

func TestProcessSnapshotMemberUniqueness(t *testing.T) {
	startedAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	observedAt := startedAt.Add(time.Second)
	process := ProcessObservation{
		Process:            ProcessIdentity{PID: 101, StartedAt: startedAt},
		ExecutableIdentity: "executable-alpha", OwnerIdentity: "owner-alpha",
	}

	if err := (ProcessSnapshot{ObservedAt: observedAt, Processes: []ProcessObservation{}}).Validate(); err != nil {
		t.Fatalf("empty process snapshot rejected: %v", err)
	}
	duplicate := ProcessSnapshot{ObservedAt: observedAt, Processes: []ProcessObservation{process, process}}
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate process identities were accepted")
	}
}

func TestRuntimeCandidateRootAndMemberUniqueness(t *testing.T) {
	startedAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	root := ProcessIdentity{PID: 101, StartedAt: startedAt}
	child := ProcessIdentity{PID: 102, StartedAt: startedAt.Add(time.Second)}
	base := RuntimeCandidate{
		InstanceID: "instance-alpha", Tool: ToolClaude,
		Runtime: RuntimeIdentity{HostID: "host-alpha", BootID: "boot-alpha", RootProcess: root},
		Members: []ProcessIdentity{root, child},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("candidate containing root exactly once rejected: %v", err)
	}

	tests := []struct {
		name    string
		members []ProcessIdentity
	}{
		{name: "root missing", members: []ProcessIdentity{child}},
		{name: "root duplicated", members: []ProcessIdentity{root, root}},
		{name: "child duplicated", members: []ProcessIdentity{root, child, child}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Members = test.members
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid runtime candidate members were accepted")
			}
		})
	}
}

func TestSlotAllocationLeavesGapAndUsesLowestFreeIndex(t *testing.T) {
	assignedAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	occupied := []Slot{
		{Namespace: "default", Index: 0, AssignedAt: assignedAt},
		{Namespace: "default", Index: 2, AssignedAt: assignedAt},
	}

	if occupied[1].Index != 2 {
		t.Fatal("middle release compacted an existing slot")
	}
	index, err := LowestAvailableSlot("default", occupied)
	if err != nil || index != 1 {
		t.Fatalf("LowestAvailableSlot = %d, %v; want 1, nil", index, err)
	}
}

func TestCanonicalSlotModelHasNoPixelCapacity(t *testing.T) {
	if _, ok := reflect.TypeOf(Slot{}).FieldByName("PixelCapacity"); ok {
		t.Fatal("physical pixel capacity leaked into canonical Slot")
	}
}

func TestDomainObservationsExposeNoRawContentFields(t *testing.T) {
	forbidden := map[string]bool{
		"Argv": true, "CWD": true, "TranscriptPath": true, "Prompt": true,
		"TerminalOutput": true, "Environment": true, "Env": true,
	}
	for _, contract := range []reflect.Type{
		reflect.TypeOf(ProcessObservation{}), reflect.TypeOf(HookObservation{}),
	} {
		for index := 0; index < contract.NumField(); index++ {
			if forbidden[contract.Field(index).Name] {
				t.Errorf("%s exposes forbidden field %s", contract.Name(), contract.Field(index).Name)
			}
		}
	}
}

func TestRevisionsOrderPerOwnerLayerWithoutWallClock(t *testing.T) {
	epoch := ProducerEpoch("epoch-alpha")
	runtimeOrder, err := CompareRuntimeRevision(epoch, RuntimeRevision(4), epoch, RuntimeRevision(5))
	if err != nil || runtimeOrder != RevisionNewer {
		t.Fatalf("runtime revision order = %v, %v", runtimeOrder, err)
	}
	hookOrder, err := CompareHookRevision(epoch, HookRevision(8), epoch, HookRevision(7))
	if err != nil || hookOrder != RevisionOlder {
		t.Fatalf("hook revision order = %v, %v", hookOrder, err)
	}
	if _, err := CompareRuntimeRevision(epoch, 5, "epoch-beta", 100); err == nil {
		t.Fatal("different producer epochs were ordered without re-registration")
	}
}
