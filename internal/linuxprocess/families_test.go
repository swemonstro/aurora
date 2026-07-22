package linuxprocess

import (
	"reflect"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestWrapperNodeNativeFamilies(t *testing.T) {
	tests := []struct {
		name string
		tool instancepresence.ToolKind
	}{
		{name: "claude", tool: instancepresence.ToolClaude},
		{name: "codex", tool: instancepresence.ToolCodex},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			records := []rawProcess{
				familyRecord(101, 1, 101, 10, test.tool, roleWrapper, 0),
				familyRecord(102, 101, 101, 10, test.tool, roleNode, 1),
				familyRecord(103, 102, 101, 10, test.tool, roleNative, 2),
			}
			families, uncertain := buildFamilies("host-a", "boot-a", records)
			if len(uncertain) != 0 || len(families) != 1 {
				t.Fatalf("families = %#v, uncertain = %#v", families, uncertain)
			}
			candidate := families[0].Candidate
			if candidate.Runtime.RootProcess.PID != 101 || len(candidate.Members) != 3 {
				t.Fatalf("candidate = %#v", candidate)
			}
			if err := candidate.Validate(); err != nil {
				t.Fatalf("candidate validation error = %v", err)
			}
			if families[0].Shape != "wrapper+node_launcher+native_child" {
				t.Fatalf("shape = %q", families[0].Shape)
			}
		})
	}
}

func TestShortLivedIntermediateConnectsKnownFamily(t *testing.T) {
	records := []rawProcess{
		familyRecord(101, 1, 101, 10, instancepresence.ToolCodex, roleWrapper, 0),
		familyRecord(102, 101, 101, 10, "", roleUnknown, 1),
		familyRecord(103, 102, 101, 10, instancepresence.ToolCodex, roleNative, 2),
	}
	families, uncertain := buildFamilies("host-a", "boot-a", records)
	if len(uncertain) != 0 || len(families) != 1 || len(families[0].Candidate.Members) != 3 {
		t.Fatalf("bridged family = %#v, uncertain = %#v", families, uncertain)
	}
}

func TestDisappearedIntermediateProducesUncertainFamily(t *testing.T) {
	records := []rawProcess{
		familyRecord(1, 0, 101, 10, "", roleUnknown, 0),
		familyRecord(101, 1, 101, 10, instancepresence.ToolCodex, roleWrapper, 1),
		// PID 102 was the short-lived intermediate and is absent.
		familyRecord(103, 102, 101, 10, instancepresence.ToolCodex, roleNative, 3),
	}
	families, uncertain := buildFamilies("host-a", "boot-a", records)
	if len(families) != 0 || len(uncertain) != 1 || len(uncertain[0].PossibleRoots) != 2 {
		t.Fatalf("disappeared-intermediate result = %#v, %#v", families, uncertain)
	}
}

func TestParallelSameToolFamiliesRemainSeparateAndSorted(t *testing.T) {
	for _, tool := range []instancepresence.ToolKind{instancepresence.ToolClaude, instancepresence.ToolCodex} {
		t.Run(string(tool), func(t *testing.T) {
			records := []rawProcess{
				familyRecord(202, 2, 202, 20, tool, roleDirect, 2),
				familyRecord(101, 1, 101, 10, tool, roleDirect, 1),
			}
			families, uncertain := buildFamilies("host-a", "boot-a", records)
			if len(uncertain) != 0 || len(families) != 2 {
				t.Fatalf("families = %#v, uncertain = %#v", families, uncertain)
			}
			if families[0].Candidate.Runtime.RootProcess.PID != 101 ||
				families[1].Candidate.Runtime.RootProcess.PID != 202 {
				t.Fatalf("family order = %#v", families)
			}
			// No source/provider/profile input exists in family construction;
			// those metadata therefore cannot collapse these candidates.
			if families[0].Candidate.InstanceID == families[1].Candidate.InstanceID {
				t.Fatal("parallel candidates collided")
			}
		})
	}
}

func TestFamilyMembersHaveDeterministicOrderAndOneRoot(t *testing.T) {
	records := []rawProcess{
		familyRecord(103, 102, 101, 10, instancepresence.ToolClaude, roleNative, 3),
		familyRecord(101, 1, 101, 10, instancepresence.ToolClaude, roleWrapper, 1),
		familyRecord(102, 101, 101, 10, instancepresence.ToolClaude, roleNode, 2),
	}
	families, _ := buildFamilies("host-a", "boot-a", records)
	got := []uint64{}
	rootCount := 0
	for _, member := range families[0].Candidate.Members {
		got = append(got, member.PID)
		if sameProcess(member, families[0].Candidate.Runtime.RootProcess) {
			rootCount++
		}
	}
	if want := []uint64{101, 102, 103}; !reflect.DeepEqual(got, want) {
		t.Fatalf("member PIDs = %v, want %v", got, want)
	}
	if rootCount != 1 {
		t.Fatalf("root count = %d, want 1", rootCount)
	}
}

func TestMissingRootAndAmbiguousRootsAreConservative(t *testing.T) {
	t.Run("one surviving child becomes observable root", func(t *testing.T) {
		records := []rawProcess{
			familyRecord(102, 999, 100, 10, instancepresence.ToolClaude, roleNode, 2),
			familyRecord(103, 102, 100, 10, instancepresence.ToolClaude, roleNative, 3),
		}
		families, uncertain := buildFamilies("host-a", "boot-a", records)
		if len(uncertain) != 0 || len(families) != 1 || families[0].Candidate.Runtime.RootProcess.PID != 102 {
			t.Fatalf("missing-root result = %#v, %#v", families, uncertain)
		}
		if !containsReason(families[0].ReasonCodes, ReasonRootMissingChildAlive) {
			t.Fatalf("reason codes = %v", families[0].ReasonCodes)
		}
	})

	t.Run("multiple possible roots remain uncertain", func(t *testing.T) {
		records := []rawProcess{
			familyRecord(102, 999, 100, 10, instancepresence.ToolClaude, roleNode, 2),
			familyRecord(103, 999, 100, 10, instancepresence.ToolClaude, roleNative, 3),
		}
		families, uncertain := buildFamilies("host-a", "boot-a", records)
		if len(families) != 0 || len(uncertain) != 1 || len(uncertain[0].PossibleRoots) != 2 {
			t.Fatalf("ambiguous result = %#v, %#v", families, uncertain)
		}
		if !containsReason(uncertain[0].ReasonCodes, ReasonAmbiguousRoot) ||
			!containsReason(uncertain[0].ReasonCodes, ReasonMultipleRoots) {
			t.Fatalf("reason codes = %v", uncertain[0].ReasonCodes)
		}
	})
}

func familyRecord(pid, ppid, group, session uint64, tool instancepresence.ToolKind, role processRole, offset int) rawProcess {
	start := time.Date(2026, 7, 22, 9, 0, offset, 0, time.UTC)
	return rawProcess{
		stat:           procStat{PID: pid, ParentPID: ppid, GroupID: group, SessionID: session},
		identity:       instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
		classification: classification{tool: tool, role: role},
	}
}

func containsReason(codes []ReasonCode, want ReasonCode) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
