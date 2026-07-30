package runtimerecognition_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

func TestRecognizePreservesWrapperNodeNativeFamilies(t *testing.T) {
	for _, test := range []struct {
		name, launch, native string
		tool                 instancepresence.ToolKind
	}{
		{"Claude", "launch:anthropic-claude-code", "claude-native-worker", instancepresence.ToolClaude},
		{"Codex", "launch:openai-codex", "codex-linux-x86_64", instancepresence.ToolCodex},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := runtimerecognition.Recognize(fixtureSnapshot(test.launch, test.native), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Families) != 1 || len(result.Observations) != 1 {
				t.Fatalf("result = %#v", result)
			}
			family := result.Families[0]
			if family.Candidate.Tool != test.tool || family.Candidate.Runtime.RootProcess.PID != 101 || len(family.Candidate.Members) != 3 || family.Shape != "wrapper+node_launcher+native_child" {
				t.Fatalf("family = %#v", family)
			}
			if !contains(family.ReasonCodes, runtimerecognition.ReasonIdentifiedAgentFamily) {
				t.Fatalf("reason codes = %v", family.ReasonCodes)
			}
		})
	}
}

func TestSourceUsesAtomicRuntimeSnapshotAndPerSampleBootID(t *testing.T) {
	first := fixtureSnapshot("launch:anthropic-claude-code", "claude-native-worker")
	second := fixtureSnapshot("launch:anthropic-claude-code", "claude-native-worker")
	second.BootID = "boot-b"
	sourceImpl := &atomicSource{snapshots: []runtimerecognition.Snapshot{first, second}}
	source, err := runtimerecognition.NewSource(sourceImpl, "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		t.Fatal(err)
	}
	firstObservations, err := source.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondObservations, err := source.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sourceImpl.calls != 2 || firstObservations[0].Candidate.Runtime.BootID != "boot-a" || secondObservations[0].Candidate.Runtime.BootID != "boot-b" || firstObservations[0].Candidate.InstanceID == secondObservations[0].Candidate.InstanceID {
		t.Fatalf("atomic source results = %#v %#v, calls=%d", firstObservations, secondObservations, sourceImpl.calls)
	}
}

func TestRecognizeGenerationSafeParentsAndIDs(t *testing.T) {
	start := fixtureTime()
	oldParent := process(101, start, nil, 1, "claude", "claude")
	newParent := process(101, start.Add(time.Hour), nil, 1, "claude", "claude")
	wrongParent := oldParent.Process
	child := process(102, start.Add(2*time.Hour), &wrongParent, 101, "claude-native-worker", "claude-native-worker")
	snapshot := runtimeSnapshot([]runtimerecognition.ProcessObservation{newParent, child})
	result, err := runtimerecognition.Recognize(snapshot, "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Families) != 0 || len(result.UncertainFamilies) != 1 {
		t.Fatalf("wrong generation was not kept conservative: %#v", result)
	}
	oldResult, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{oldParent}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		t.Fatal(err)
	}
	newResult, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{newParent}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		t.Fatal(err)
	}
	if oldResult.Families[0].Candidate.InstanceID == newResult.Families[0].Candidate.InstanceID {
		t.Fatalf("reused PID collided: %#v %#v", oldResult, newResult)
	}
	repeated, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{newParent}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil || repeated.Families[0].Candidate.InstanceID != newResult.Families[0].Candidate.InstanceID {
		t.Fatalf("ID not stable: %#v, %v", repeated, err)
	}
}

func TestRecognizeTreatsReusedParentPIDAsAmbiguous(t *testing.T) {
	start := fixtureTime()
	missingParent := instancepresence.ProcessIdentity{PID: 999, StartedAt: start}
	reused := process(999, start.Add(time.Hour), nil, 1, "worker", "worker")
	child := process(102, start.Add(2*time.Hour), &missingParent, 999, "claude", "claude")
	if child.Parent == nil || child.Parent.PID != reused.Process.PID || child.Parent.StartedAt.Equal(reused.Process.StartedAt) || child.ParentPIDHint != reused.Process.PID {
		t.Fatalf("fixture does not contain a missing parent generation and a reused PID: %#v %#v", child, reused)
	}
	result, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{reused, child}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Families) != 0 || len(result.UncertainFamilies) != 1 {
		t.Fatalf("reused parent PID became a safe family: %#v", result)
	}
	family := result.UncertainFamilies[0]
	wantReasons := []runtimerecognition.ReasonCode{runtimerecognition.ReasonAmbiguousRoot, runtimerecognition.ReasonRootMissingChildAlive}
	if family.Tool != instancepresence.ToolClaude || len(family.PossibleRoots) != 1 || family.PossibleRoots[0] != child.Process || len(family.Members) != 1 || family.Members[0] != child.Process || !reflect.DeepEqual(family.ReasonCodes, wantReasons) {
		t.Fatalf("uncertain family = %#v", family)
	}
}

func TestRecognizeConservativeFamilyCases(t *testing.T) {
	t.Run("short lived unknown intermediate", func(t *testing.T) {
		start := fixtureTime()
		root := process(101, start, nil, 1, "bash", "bash")
		root.LaunchIdentities = []instancepresence.OpaqueIdentity{"launch:openai-codex"}
		middleParent := root.Process
		middle := process(102, start.Add(time.Second), &middleParent, 101, "sh", "sh")
		nativeParent := middle.Process
		native := process(103, start.Add(2*time.Second), &nativeParent, 102, "codex-linux-x86_64", "codex-linux-x86_64")
		result, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{native, root, middle}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
		if err != nil || len(result.Families) != 1 || len(result.Families[0].Candidate.Members) != 3 {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		for index, want := range []uint64{101, 102, 103} {
			if got := result.Families[0].Candidate.Members[index].PID; got != want {
				t.Fatalf("member order = %#v", result.Families[0].Candidate.Members)
			}
		}
		rootCount := 0
		for _, member := range result.Families[0].Candidate.Members {
			if member == result.Families[0].Candidate.Runtime.RootProcess {
				rootCount++
			}
		}
		if rootCount != 1 {
			t.Fatalf("root count = %d", rootCount)
		}
	})
	t.Run("disappeared intermediate is uncertain", func(t *testing.T) {
		start := fixtureTime()
		root := process(101, start, nil, 1, "bash", "bash")
		root.ParentPIDHint = 1
		root.LaunchIdentities = []instancepresence.OpaqueIdentity{"launch:openai-codex"}
		missing := instancepresence.ProcessIdentity{PID: 102, StartedAt: start.Add(time.Second)}
		native := process(103, start.Add(2*time.Second), &missing, 102, "codex-linux-x86_64", "codex-linux-x86_64")
		result, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{root, native}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
		if err != nil || len(result.Families) != 0 || len(result.UncertainFamilies) != 1 {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		family := result.UncertainFamilies[0]
		if family.Tool != instancepresence.ToolCodex || len(family.PossibleRoots) != 2 || len(family.Members) != 2 || family.PossibleRoots[0] != root.Process || family.PossibleRoots[1] != native.Process || !contains(family.ReasonCodes, runtimerecognition.ReasonAmbiguousRoot) || !contains(family.ReasonCodes, runtimerecognition.ReasonMultipleRoots) {
			t.Fatalf("uncertain family=%#v", family)
		}
	})
	t.Run("parallel and deterministic", func(t *testing.T) {
		for _, test := range []struct {
			tool       instancepresence.ToolKind
			executable string
		}{{instancepresence.ToolClaude, "claude"}, {instancepresence.ToolCodex, "codex"}} {
			t.Run(string(test.tool), func(t *testing.T) {
				start := fixtureTime()
				first := process(202, start.Add(time.Second), nil, 1, test.executable, test.executable)
				second := process(101, start, nil, 1, test.executable, test.executable)
				first.ProcessGroupOrJob, first.OSSession = "pgrp:202", "session:20"
				result, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{first, second}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
				if err != nil || len(result.Families) != 2 {
					t.Fatalf("result=%#v err=%v", result, err)
				}
				for index, wantRoot := range []instancepresence.ProcessIdentity{second.Process, first.Process} {
					family := result.Families[index]
					if family.Candidate.Tool != test.tool || family.Candidate.Runtime.RootProcess != wantRoot || len(family.Candidate.Members) != 1 || family.Candidate.Members[0] != wantRoot || family.Shape != string(runtimerecognition.RoleDirect) {
						t.Fatalf("family %d = %#v", index, family)
					}
				}
				if result.Families[0].Candidate.InstanceID == result.Families[1].Candidate.InstanceID {
					t.Fatalf("parallel candidates collided: %#v", result.Families)
				}
			})
		}
	})
	t.Run("missing root with one living child", func(t *testing.T) {
		start := fixtureTime()
		child := process(102, start, nil, 999, "claude", "claude")
		nativeParent := child.Process
		native := process(103, start.Add(time.Second), &nativeParent, 102, "claude-native-worker", "claude-native-worker")
		result, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{native, child}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
		if err != nil || len(result.Families) != 1 || !contains(result.Families[0].ReasonCodes, runtimerecognition.ReasonRootMissingChildAlive) {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})
}

func TestRecognizeRejectsInvalidRecognizersAndConflicts(t *testing.T) {
	snapshot := runtimeSnapshot([]runtimerecognition.ProcessObservation{process(101, fixtureTime(), nil, 1, "x", "x")})
	if _, err := runtimerecognition.Recognize(snapshot, "host-a"); err == nil {
		t.Fatal("empty recognizer list accepted")
	}
	if _, err := runtimerecognition.NewSource(nil, "host-a", claudehook.RuntimeRecognizer()); err == nil {
		t.Fatal("nil source accepted")
	}
	var nilSource *nilSnapshotSource
	if _, err := runtimerecognition.NewSource(nilSource, "host-a", claudehook.RuntimeRecognizer()); err == nil {
		t.Fatal("typed nil source accepted")
	}
	var nilRecognizer *nilRuntimeRecognizer
	if _, err := runtimerecognition.Recognize(snapshot, "host-a", nilRecognizer); err == nil {
		t.Fatal("typed nil recognizer accepted")
	}
	for _, recognizer := range []runtimerecognition.AgentRuntimeRecognizer{badRecognizer{runtimerecognition.Recognition{}}, badRecognizer{runtimerecognition.Recognition{Tool: instancepresence.ToolClaude, Role: "bad", Priority: 1}}, badRecognizer{runtimerecognition.Recognition{Tool: instancepresence.ToolClaude, Role: runtimerecognition.RoleDirect, Priority: 9}}} {
		if _, err := runtimerecognition.Recognize(snapshot, "host-a", recognizer); err == nil {
			t.Fatalf("invalid recognizer accepted: %#v", recognizer)
		}
	}
	result, err := runtimerecognition.Recognize(snapshot, "host-a", badRecognizer{runtimerecognition.Recognition{Tool: instancepresence.ToolClaude, Role: runtimerecognition.RoleDirect, Priority: 2}}, badRecognizer{runtimerecognition.Recognition{Tool: instancepresence.ToolCodex, Role: runtimerecognition.RoleDirect, Priority: 2}})
	if err != nil || len(result.Families) != 0 || result.UnknownProcesses != 1 {
		t.Fatalf("conflict = %#v, %v", result, err)
	}
}

func TestRecognizerClassificationSignalsRemainConservative(t *testing.T) {
	tests := []struct {
		name, comm, executable string
		launches               []instancepresence.OpaqueIdentity
		want                   instancepresence.ToolKind
		role                   runtimerecognition.Role
	}{
		{"Claude direct", "claude", "other", nil, instancepresence.ToolClaude, runtimerecognition.RoleDirect},
		{"Claude uppercase", "Claude", "other", nil, instancepresence.ToolClaude, runtimerecognition.RoleDirect},
		{"Claude code uppercase", "CLAUDE-CODE", "other", nil, instancepresence.ToolClaude, runtimerecognition.RoleDirect},
		{"Claude direct plus native comm", "claude-native-worker", "claude", nil, instancepresence.ToolClaude, runtimerecognition.RoleNative},
		{"Claude Aurora executable", "aurora-claude", "aurora-claude", nil, instancepresence.ToolClaude, runtimerecognition.RoleDirect},
		{"Codex native", "x", "CODEX-LINUX-X86_64", nil, instancepresence.ToolCodex, runtimerecognition.RoleNative},
		{"Codex Aurora executable", "aurora-codex", "aurora-codex", nil, instancepresence.ToolCodex, runtimerecognition.RoleDirect},
		{"Claude node package", "node", "node", []instancepresence.OpaqueIdentity{"launch:anthropic-claude-code"}, instancepresence.ToolClaude, runtimerecognition.RoleNode},
		{"aurora codex wrapper", "sh", "sh", []instancepresence.OpaqueIdentity{"launch:aurora-codex"}, instancepresence.ToolCodex, runtimerecognition.RoleWrapper},
		{"aurora claude wrapper", "sh", "sh", []instancepresence.OpaqueIdentity{"launch:aurora-claude"}, instancepresence.ToolClaude, runtimerecognition.RoleWrapper},
		{"unknown node", "node", "node", nil, "", ""},
		{"conflicting agent markers", "sh", "sh", []instancepresence.OpaqueIdentity{"launch:aurora-claude", "launch:aurora-codex"}, "", ""},
		{"conflicting executable signals", "CLAUDE", "CODEX", nil, "", ""},
		{"Claude executable wins over Codex launch", "claude", "claude", []instancepresence.OpaqueIdentity{"launch:openai-codex"}, instancepresence.ToolClaude, runtimerecognition.RoleDirect},
		{"Codex executable wins over Claude launch", "codex", "codex", []instancepresence.OpaqueIdentity{"launch:anthropic-claude-code"}, instancepresence.ToolCodex, runtimerecognition.RoleDirect},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := process(101, fixtureTime(), nil, 1, test.comm, test.executable)
			p.LaunchIdentities = test.launches
			result, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{p}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if result.UnknownProcesses != 1 {
					t.Fatalf("unknown = %#v", result)
				}
				return
			}
			if len(result.Families) != 1 || result.Families[0].Candidate.Tool != test.want || result.Families[0].Shape != string(test.role) {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

func TestRecognizerNormalizesMixedCaseExecutablePrefixes(t *testing.T) {
	for _, test := range []struct {
		name, comm, executable string
		tool                   instancepresence.ToolKind
		role                   runtimerecognition.Role
	}{
		{"Claude uppercase prefix", "EXE:CLAUDE", "Exe:Claude-Code", instancepresence.ToolClaude, runtimerecognition.RoleDirect},
		{"Codex uppercase prefix", "other", "EXE:CODEX-LINUX-X86_64", instancepresence.ToolCodex, runtimerecognition.RoleNative},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := process(101, fixtureTime(), nil, 1, "other", "other")
			p.CommIdentity = instancepresence.OpaqueIdentity(test.comm)
			p.ExecutableIdentity = instancepresence.OpaqueIdentity(test.executable)
			result, err := runtimerecognition.Recognize(runtimeSnapshot([]runtimerecognition.ProcessObservation{p}), "host-a", claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
			if err != nil || len(result.Families) != 1 || result.Families[0].Candidate.Tool != test.tool || result.Families[0].Shape != string(test.role) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestRecognizeIgnoresCodexUtilityCommands(t *testing.T) {
	start := fixtureTime()
	for _, test := range []struct {
		name string
		argv []string
	}{
		{name: "app server stdio", argv: []string{"codex", "app-server", "--stdio"}},
		{name: "version", argv: []string{"codex", "--version"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			utility := process(501, start, nil, 1, "codex", "codex")
			utility.Argv = test.argv

			result, err := runtimerecognition.Recognize(
				runtimeSnapshot([]runtimerecognition.ProcessObservation{utility}),
				"host-a",
				codexhook.RuntimeRecognizer(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Families) != 0 || len(result.Observations) != 0 {
				t.Fatalf("utility command created runtime family: %#v", result)
			}
		})
	}
}

func TestRecognizeCodexUtilitiesDoNotAddRuntimeBesideEstablishedCodex(t *testing.T) {
	start := fixtureTime()
	interactive := process(601, start, nil, 1, "codex", "codex")
	interactive.Argv = []string{"codex", "fix the tests"}
	utility := process(602, start.Add(time.Minute), nil, 1, "codex", "codex")
	utility.Argv = []string{"codex", "app-server", "--stdio"}
	utility.ProcessGroupOrJob = "pgrp:utility"
	utility.OSSession = "session:utility"

	result, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{interactive, utility}),
		"host-a",
		codexhook.RuntimeRecognizer(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Families) != 1 || len(result.Observations) != 1 {
		t.Fatalf("families = %#v", result.Families)
	}
	family := result.Families[0]
	if family.Candidate.Runtime.RootProcess != interactive.Process {
		t.Fatalf("root = %#v, want established interactive process", family.Candidate.Runtime.RootProcess)
	}
	beforeID := family.Candidate.InstanceID

	withoutUtility, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{interactive}),
		"host-a",
		codexhook.RuntimeRecognizer(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutUtility.Families) != 1 {
		t.Fatalf("without utility = %#v", withoutUtility)
	}
	if withoutUtility.Families[0].Candidate.InstanceID != beforeID ||
		withoutUtility.Families[0].Candidate.Runtime.RootProcess != interactive.Process {
		t.Fatalf("established Codex identity changed: with=%#v without=%#v", family, withoutUtility.Families[0])
	}
}

func TestRecognizePreservesInteractiveAndExecCodexFamilies(t *testing.T) {
	start := fixtureTime()
	first := process(701, start, nil, 1, "codex", "codex")
	first.Argv = []string{"codex", "fix the tests"}
	second := process(702, start.Add(time.Minute), nil, 1, "codex", "codex")
	second.Argv = []string{"codex", "exec", "go test ./..."}
	second.ProcessGroupOrJob = "pgrp:exec"
	second.OSSession = "session:exec"

	result, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{first, second}),
		"host-a",
		codexhook.RuntimeRecognizer(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Families) != 2 || len(result.Observations) != 2 {
		t.Fatalf("families = %#v", result.Families)
	}
	if result.Families[0].Candidate.Runtime.RootProcess != first.Process ||
		result.Families[1].Candidate.Runtime.RootProcess != second.Process {
		t.Fatalf("roots = %#v", result.Families)
	}
	if result.Families[0].Candidate.InstanceID == result.Families[1].Candidate.InstanceID {
		t.Fatalf("separate Codex runtimes collided: %#v", result.Families)
	}
}

type atomicSource struct {
	snapshots []runtimerecognition.Snapshot
	calls     int
}

func (source *atomicSource) RuntimeSnapshot(context.Context) (runtimerecognition.Snapshot, error) {
	if source.calls >= len(source.snapshots) {
		return runtimerecognition.Snapshot{}, errors.New("no snapshot")
	}
	result := source.snapshots[source.calls]
	source.calls++
	return result, nil
}

type badRecognizer struct {
	recognition runtimerecognition.Recognition
}

type nilRuntimeRecognizer struct{}

func (*nilRuntimeRecognizer) Recognize(runtimerecognition.ProcessObservation) (runtimerecognition.Recognition, bool) {
	return runtimerecognition.Recognition{}, false
}

type nilSnapshotSource struct{}

func (*nilSnapshotSource) RuntimeSnapshot(context.Context) (runtimerecognition.Snapshot, error) {
	return runtimerecognition.Snapshot{}, nil
}

func (r badRecognizer) Recognize(runtimerecognition.ProcessObservation) (runtimerecognition.Recognition, bool) {
	return r.recognition, true
}

func fixtureSnapshot(launch, native string) runtimerecognition.Snapshot {
	start := fixtureTime()
	root := process(101, start, nil, 1, "bash", "bash")
	root.LaunchIdentities = []instancepresence.OpaqueIdentity{instancepresence.OpaqueIdentity(launch)}
	rootID := root.Process
	node := process(102, start.Add(time.Second), &rootID, 101, "node", "node")
	node.LaunchIdentities = []instancepresence.OpaqueIdentity{instancepresence.OpaqueIdentity(launch)}
	nodeID := node.Process
	child := process(103, start.Add(2*time.Second), &nodeID, 102, native, native)
	return runtimeSnapshot([]runtimerecognition.ProcessObservation{root, node, child})
}
func TestRecognizeSuspendedRootOnly(t *testing.T) {
	// Agent-neutral: suspension is derived only from the recognized root's
	// stop state (T/t), for every RuntimeRecognizer (Claude + Codex) — not
	// from hook event mappers in claudehook/codexhook.
	start := fixtureTime()
	recognizers := []runtimerecognition.AgentRuntimeRecognizer{
		claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer(),
	}
	for _, test := range []struct {
		name   string
		tool   instancepresence.ToolKind
		exe    string
		native string
		launch string
		pid    uint64
	}{
		{name: "Claude", tool: instancepresence.ToolClaude, exe: "claude", native: "claude-native-worker", launch: "launch:anthropic-claude-code", pid: 201},
		{name: "Codex", tool: instancepresence.ToolCodex, exe: "codex", native: "codex-linux-x86_64", launch: "launch:openai-codex", pid: 211},
	} {
		t.Run(test.name+"/root_stopped", func(t *testing.T) {
			root := process(test.pid, start, nil, 0, test.exe, test.exe)
			root.Suspended = true
			result, err := runtimerecognition.Recognize(
				runtimeSnapshot([]runtimerecognition.ProcessObservation{root}),
				"host-a", recognizers...,
			)
			if err != nil || len(result.Families) != 1 {
				t.Fatalf("result = %#v err=%v", result, err)
			}
			family := result.Families[0]
			if family.Candidate.Tool != test.tool {
				t.Fatalf("tool = %q, want %q", family.Candidate.Tool, test.tool)
			}
			if !family.Suspended {
				t.Fatal("root T/t must suspend family")
			}
		})
		t.Run(test.name+"/unrecognized_helper_stopped_only", func(t *testing.T) {
			aliveRoot := process(test.pid+100, start, nil, 0, test.exe, test.exe)
			aliveRootID := aliveRoot.Process
			stoppedChild := process(test.pid+101, start.Add(time.Second), &aliveRootID, test.pid+100, "helper", "helper")
			stoppedChild.Suspended = true
			result, err := runtimerecognition.Recognize(
				runtimeSnapshot([]runtimerecognition.ProcessObservation{aliveRoot, stoppedChild}),
				"host-a", recognizers...,
			)
			if err != nil || len(result.Families) != 1 {
				t.Fatalf("result = %#v err=%v", result, err)
			}
			family := result.Families[0]
			if family.Candidate.Tool != test.tool {
				t.Fatalf("tool = %q, want %q", family.Candidate.Tool, test.tool)
			}
			if family.Suspended {
				t.Fatal("stopped helper child must not suspend family")
			}
			if family.Candidate.Runtime.RootProcess.PID != test.pid+100 {
				t.Fatalf("root = %#v", family.Candidate.Runtime.RootProcess)
			}
		})
		// Recognized multi-process family: only the root stop state matters.
		// A stopped native member must not alone suspend Claude or Codex.
		t.Run(test.name+"/recognized_member_stopped_only", func(t *testing.T) {
			root := process(test.pid+200, start, nil, 1, "bash", "bash")
			root.LaunchIdentities = []instancepresence.OpaqueIdentity{instancepresence.OpaqueIdentity(test.launch)}
			rootID := root.Process
			node := process(test.pid+201, start.Add(time.Second), &rootID, test.pid+200, "node", "node")
			node.LaunchIdentities = []instancepresence.OpaqueIdentity{instancepresence.OpaqueIdentity(test.launch)}
			nodeID := node.Process
			native := process(test.pid+202, start.Add(2*time.Second), &nodeID, test.pid+201, test.native, test.native)
			native.Suspended = true
			result, err := runtimerecognition.Recognize(
				runtimeSnapshot([]runtimerecognition.ProcessObservation{root, node, native}),
				"host-a", recognizers...,
			)
			if err != nil || len(result.Families) != 1 {
				t.Fatalf("result = %#v err=%v", result, err)
			}
			family := result.Families[0]
			if family.Candidate.Tool != test.tool {
				t.Fatalf("tool = %q, want %q", family.Candidate.Tool, test.tool)
			}
			if len(family.Candidate.Members) != 3 {
				t.Fatalf("members = %#v", family.Candidate.Members)
			}
			if family.Suspended {
				t.Fatal("stopped recognized member must not suspend family when root is alive")
			}
			if family.Candidate.Runtime.RootProcess.PID != test.pid+200 {
				t.Fatalf("root = %#v", family.Candidate.Runtime.RootProcess)
			}
		})
	}
}

func TestRecognizeSuspendedIndependenceClaudeAndCodex(t *testing.T) {
	// Concurrent multi-agent: stopping Claude root must not suspend Codex, and vice versa.
	start := fixtureTime()
	recognizers := []runtimerecognition.AgentRuntimeRecognizer{
		claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer(),
	}
	claude := process(401, start, nil, 0, "claude", "claude")
	codex := process(402, start.Add(time.Second), nil, 0, "codex", "codex")

	claude.Suspended = true
	codex.Suspended = false
	result, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{claude, codex}),
		"host-a", recognizers...,
	)
	if err != nil || len(result.Families) != 2 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
	byTool := map[instancepresence.ToolKind]bool{}
	for _, family := range result.Families {
		byTool[family.Candidate.Tool] = family.Suspended
	}
	if !byTool[instancepresence.ToolClaude] {
		t.Fatal("Claude root stopped must be suspended")
	}
	if byTool[instancepresence.ToolCodex] {
		t.Fatal("Codex must stay non-suspended when only Claude root is stopped")
	}

	claude.Suspended = false
	codex.Suspended = true
	result, err = runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{claude, codex}),
		"host-a", recognizers...,
	)
	if err != nil || len(result.Families) != 2 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
	byTool = map[instancepresence.ToolKind]bool{}
	for _, family := range result.Families {
		byTool[family.Candidate.Tool] = family.Suspended
	}
	if byTool[instancepresence.ToolClaude] {
		t.Fatal("Claude must stay non-suspended when only Codex root is stopped")
	}
	if !byTool[instancepresence.ToolCodex] {
		t.Fatal("Codex root stopped must be suspended")
	}
}

func TestRecognizePropagatesLaunchProcessCodexMetadataOnly(t *testing.T) {
	start := fixtureTime()
	root := process(201, start, nil, 0, "codex", "codex")
	root.WorkingDirectory = "/tmp/root-project"
	root.EnvCodexHome = "/tmp/root-codex-home"
	root.Argv = []string{"codex", "exec"}
	rootID := root.Process
	child := process(202, start.Add(time.Second), &rootID, 201, "helper", "helper")
	child.WorkingDirectory = "/tmp/child-project"
	child.EnvCodexHome = "/tmp/child-codex-home"
	child.Argv = []string{"helper", "noise"}

	// Unrelated parallel codex must not leak into the first family.
	other := process(301, start.Add(2*time.Second), nil, 0, "codex", "codex")
	other.WorkingDirectory = "/tmp/other-project"
	other.EnvCodexHome = "/tmp/other-home"
	other.Argv = []string{"codex"}

	result, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{root, child, other}),
		"host-a", codexhook.RuntimeRecognizer(),
	)
	if err != nil || len(result.Families) != 2 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
	var familyA, familyB *runtimerecognition.Family
	for i := range result.Families {
		switch result.Families[i].Candidate.Runtime.RootProcess.PID {
		case 201:
			familyA = &result.Families[i]
		case 301:
			familyB = &result.Families[i]
		}
	}
	if familyA == nil || familyB == nil {
		t.Fatalf("families = %#v", result.Families)
	}
	if familyA.LaunchProcess.PID != 201 {
		t.Fatalf("LaunchProcess = %#v, want pid 201", familyA.LaunchProcess)
	}
	if familyA.WorkingDirectory != "/tmp/root-project" || familyA.EnvCodexHome != "/tmp/root-codex-home" {
		t.Fatalf("family A metadata = cwd=%q home=%q", familyA.WorkingDirectory, familyA.EnvCodexHome)
	}
	if !reflect.DeepEqual(familyA.Argv, []string{"codex", "exec"}) {
		t.Fatalf("family A argv = %#v", familyA.Argv)
	}
	if familyA.WorkingDirectory == familyB.WorkingDirectory || familyA.EnvCodexHome == familyB.EnvCodexHome {
		t.Fatal("parallel Codex families must keep independent metadata")
	}
	if familyB.WorkingDirectory != "/tmp/other-project" || familyB.EnvCodexHome != "/tmp/other-home" {
		t.Fatalf("family B metadata = %#v", familyB)
	}
	// Child metadata must not be selected for the launch family.
	if familyA.WorkingDirectory == "/tmp/child-project" || familyA.EnvCodexHome == "/tmp/child-codex-home" {
		t.Fatal("child metadata leaked into launch family")
	}
}

func TestRecognizeShellNodeNativeChoosesNodeLaunchProcess(t *testing.T) {
	// Production shape: old bash terminal as family root, new Node codex
	// launcher, native child. Shell has launch identity so it is RootProcess
	// but remains a terminal name so LaunchProcess selection skips it.
	baseline := fixtureTime()
	shellStart := baseline.Add(-2 * time.Hour)
	nodeStart := baseline.Add(time.Second)
	nativeStart := baseline.Add(2 * time.Second)

	shell := process(100, shellStart, nil, 1, "bash", "bash")
	shell.Argv = []string{"-bash"}
	shell.LaunchIdentities = []instancepresence.OpaqueIdentity{"launch:openai-codex"}
	shell.ProcessGroupOrJob = "pgrp:shared"
	shell.OSSession = "session:shared"
	shell.WorkingDirectory = "/tmp/old-shell-cwd"
	shellID := shell.Process

	node := process(200, nodeStart, &shellID, 100, "node", "node")
	node.LaunchIdentities = []instancepresence.OpaqueIdentity{"launch:openai-codex"}
	node.ProcessGroupOrJob = "pgrp:shared"
	node.OSSession = "session:shared"
	node.WorkingDirectory = "/tmp/untrusted-project"
	node.EnvCodexHome = "/tmp/codex-home"
	node.Argv = []string{"codex"}
	nodeID := node.Process

	native := process(201, nativeStart, &nodeID, 200, "codex-linux-x86_64", "codex-linux-x86_64")
	native.ProcessGroupOrJob = "pgrp:shared"
	native.OSSession = "session:shared"
	native.WorkingDirectory = "/tmp/native-cwd"
	native.EnvCodexHome = "/tmp/native-home"
	native.Argv = []string{"codex-linux-x86_64"}

	result, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{shell, node, native}),
		"host-a", codexhook.RuntimeRecognizer(),
	)
	if err != nil || len(result.Families) != 1 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
	family := result.Families[0]
	if family.Candidate.Runtime.RootProcess != shell.Process {
		t.Fatalf("RootProcess = %#v, want shell", family.Candidate.Runtime.RootProcess)
	}
	if family.LaunchProcess != node.Process {
		t.Fatalf("LaunchProcess = %#v, want node", family.LaunchProcess)
	}
	if family.WorkingDirectory != "/tmp/untrusted-project" || family.EnvCodexHome != "/tmp/codex-home" {
		t.Fatalf("metadata from node expected: cwd=%q home=%q", family.WorkingDirectory, family.EnvCodexHome)
	}
	if !reflect.DeepEqual(family.Argv, []string{"codex"}) {
		t.Fatalf("Argv = %#v, want [codex] from node (not shell -bash)", family.Argv)
	}
}

func TestRecognizeNativeOnlyLaunchProcess(t *testing.T) {
	start := fixtureTime()
	native := process(55, start, nil, 1, "codex-linux-x86_64", "codex-linux-x86_64")
	native.WorkingDirectory = "/tmp/only-native"
	native.Argv = []string{"codex-linux-x86_64"}
	result, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{native}),
		"host-a", codexhook.RuntimeRecognizer(),
	)
	if err != nil || len(result.Families) != 1 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
	if result.Families[0].LaunchProcess.PID != 55 {
		t.Fatalf("LaunchProcess = %#v", result.Families[0].LaunchProcess)
	}
	if result.Families[0].WorkingDirectory != "/tmp/only-native" {
		t.Fatalf("cwd = %q", result.Families[0].WorkingDirectory)
	}
}

func TestRecognizeClaudeMetadataStaysOnRoot(t *testing.T) {
	// Claude must not use LaunchProcess-style child selection: root cwd/argv win.
	start := fixtureTime()
	root := process(401, start, nil, 1, "claude", "claude")
	root.WorkingDirectory = "/tmp/claude-root-cwd"
	root.EnvCodexHome = "/tmp/claude-root-home"
	root.Argv = []string{"claude", "root-arg"}
	rootID := root.Process
	child := process(402, start.Add(time.Second), &rootID, 401, "claude-native-worker", "claude-native-worker")
	child.WorkingDirectory = "/tmp/claude-child-cwd"
	child.EnvCodexHome = "/tmp/claude-child-home"
	child.Argv = []string{"claude-native-worker", "child-arg"}

	result, err := runtimerecognition.Recognize(
		runtimeSnapshot([]runtimerecognition.ProcessObservation{root, child}),
		"host-a", claudehook.RuntimeRecognizer(),
	)
	if err != nil || len(result.Families) != 1 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
	family := result.Families[0]
	if family.Candidate.Tool != instancepresence.ToolClaude {
		t.Fatalf("tool = %q", family.Candidate.Tool)
	}
	if family.Candidate.Runtime.RootProcess.PID != 401 {
		t.Fatalf("RootProcess = %#v", family.Candidate.Runtime.RootProcess)
	}
	if family.WorkingDirectory != "/tmp/claude-root-cwd" || family.EnvCodexHome != "/tmp/claude-root-home" {
		t.Fatalf("Claude metadata must stay on root: cwd=%q home=%q", family.WorkingDirectory, family.EnvCodexHome)
	}
	if !reflect.DeepEqual(family.Argv, []string{"claude", "root-arg"}) {
		t.Fatalf("Claude Argv = %#v, want root argv", family.Argv)
	}
	if family.WorkingDirectory == "/tmp/claude-child-cwd" || family.EnvCodexHome == "/tmp/claude-child-home" {
		t.Fatal("Claude child metadata must not replace root metadata")
	}
}

func runtimeSnapshot(processes []runtimerecognition.ProcessObservation) runtimerecognition.Snapshot {
	return runtimerecognition.Snapshot{ObservedAt: fixtureTime().Add(10 * time.Second), BootID: "boot-a", Processes: processes}
}
func process(pid uint64, started time.Time, parent *instancepresence.ProcessIdentity, parentHint uint64, comm, executable string) runtimerecognition.ProcessObservation {
	return runtimerecognition.ProcessObservation{Process: instancepresence.ProcessIdentity{PID: pid, StartedAt: started}, Parent: parent, ParentPIDHint: parentHint, CommIdentity: instancepresence.OpaqueIdentity("exe:" + comm), ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + executable), ProcessGroupOrJob: "pgrp:101", OSSession: "session:10", OwnerIdentity: "owner:fixture"}
}
func fixtureTime() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }
func contains(codes []runtimerecognition.ReasonCode, want runtimerecognition.ReasonCode) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
