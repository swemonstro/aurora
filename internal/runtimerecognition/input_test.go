package runtimerecognition

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestLaunchIdentityRulesUseNarrowMatching(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		index     int
		rule      LaunchIdentityRule
		want      bool
	}{
		{"Claude wrapper argv0", []string{"/opaque/aurora-claude"}, 0, LaunchIdentityRule{Mode: LaunchRuleExactBasename, Value: "aurora-claude", Identity: "wrapper", Argument: LaunchArgumentArgv0}, true},
		{"exact wrapper argv0", []string{"/opaque/aurora-codex"}, 0, LaunchIdentityRule{Mode: LaunchRuleExactBasename, Value: "aurora-codex", Identity: "wrapper", Argument: LaunchArgumentArgv0}, true},
		{"wrapper in later option", []string{"node", "--output=/tmp/aurora-codex"}, 1, LaunchIdentityRule{Mode: LaunchRuleExactBasename, Value: "aurora-codex", Identity: "wrapper", Argument: LaunchArgumentArgv0}, false},
		{"wrapper cache option", []string{"node", "--cache=aurora-claude"}, 1, LaunchIdentityRule{Mode: LaunchRuleExactBasename, Value: "aurora-claude", Identity: "wrapper", Argument: LaunchArgumentArgv0}, false},
		{"wrapper substring is not a match", []string{"/opaque/not-aurora-codex-helper"}, 0, LaunchIdentityRule{Mode: LaunchRuleExactBasename, Value: "aurora-codex", Identity: "wrapper", Argument: LaunchArgumentArgv0}, false},
		{"package entrypoint", []string{"node", "/work/node_modules/@openai/codex/bin.js"}, 1, LaunchIdentityRule{Mode: LaunchRulePackagePath, Value: "@openai/codex", Identity: "package", Argument: LaunchArgumentEntrypoint, Launchers: []string{"node"}}, true},
		{"package cache option", []string{"node", "--cache=/tmp/@openai/codex/data"}, 1, LaunchIdentityRule{Mode: LaunchRulePackagePath, Value: "@openai/codex", Identity: "package", Argument: LaunchArgumentEntrypoint, Launchers: []string{"node"}}, false},
		{"Claude package output option", []string{"node", "--output=/tmp/@anthropic-ai/claude-code/result"}, 1, LaunchIdentityRule{Mode: LaunchRulePackagePath, Value: "@anthropic-ai/claude-code", Identity: "package", Argument: LaunchArgumentEntrypoint, Launchers: []string{"node"}}, false},
		{"package wrong launcher", []string{"bash", "/work/node_modules/@openai/codex/bin.js"}, 1, LaunchIdentityRule{Mode: LaunchRulePackagePath, Value: "@openai/codex", Identity: "package", Argument: LaunchArgumentEntrypoint, Launchers: []string{"node"}}, false},
		{"package substring is not a segment", []string{"node", "/work/node_modules/@openai/codex-helper/bin.js"}, 1, LaunchIdentityRule{Mode: LaunchRulePackagePath, Value: "@openai/codex", Identity: "package", Argument: LaunchArgumentEntrypoint, Launchers: []string{"node"}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.rule.Matches(test.arguments, test.index); got != test.want {
				t.Fatalf("Matches(%q, %d)=%t, want %t", test.arguments, test.index, got, test.want)
			}
		})
	}
}

func TestRecognitionInputRejectsContradictoryIdentityEvidence(t *testing.T) {
	start := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	base := ProcessObservation{Process: instancepresence.ProcessIdentity{PID: 102, StartedAt: start}, CommIdentity: "exe:node", ExecutableIdentity: "exe:node", OwnerIdentity: "uid:1000"}
	tests := []struct {
		name   string
		mutate func(*ProcessObservation)
	}{
		{"self parent", func(value *ProcessObservation) {
			value.Parent = &instancepresence.ProcessIdentity{PID: 102, StartedAt: start}
		}},
		{"reused PID parent", func(value *ProcessObservation) {
			value.Parent = &instancepresence.ProcessIdentity{PID: 102, StartedAt: start.Add(-time.Second)}
		}},
		{"future parent", func(value *ProcessObservation) {
			value.Parent = &instancepresence.ProcessIdentity{PID: 101, StartedAt: start.Add(time.Second)}
		}},
		{"inconsistent parent PID hint", func(value *ProcessObservation) {
			value.Parent = &instancepresence.ProcessIdentity{PID: 101, StartedAt: start.Add(-time.Second)}
			value.ParentPIDHint = 999
		}},
		{"whitespace comm", func(value *ProcessObservation) { value.CommIdentity = "exe: node" }},
		{"whitespace executable", func(value *ProcessObservation) { value.ExecutableIdentity = "exe: node" }},
		{"whitespace launch identity", func(value *ProcessObservation) {
			value.LaunchIdentities = []instancepresence.OpaqueIdentity{"launch: bad"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatalf("invalid input accepted: %#v", value)
			}
		})
	}
	if err := (ProcessObservation{Process: base.Process, ParentPIDHint: 999, CommIdentity: base.CommIdentity, ExecutableIdentity: base.ExecutableIdentity, OwnerIdentity: base.OwnerIdentity}).Validate(); err != nil {
		t.Fatalf("conservative parent hint rejected: %v", err)
	}
}

func TestRecognitionInputKeepsParentAndLaunchEvidenceOutsideWireModel(t *testing.T) {
	start := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	parent := instancepresence.ProcessIdentity{PID: 101, StartedAt: start}
	input := ProcessObservation{
		Process: instancepresence.ProcessIdentity{PID: 102, StartedAt: start.Add(time.Second)}, Parent: &parent, ParentPIDHint: 101,
		CommIdentity: "exe:node", ExecutableIdentity: "exe:node", LaunchIdentities: []instancepresence.OpaqueIdentity{"launch:openai-codex"}, OwnerIdentity: "uid:1000",
	}
	if err := (Snapshot{ObservedAt: start.Add(2 * time.Second), BootID: "boot-a", Processes: []ProcessObservation{input}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if input.Parent == nil || len(input.LaunchIdentities) != 1 {
		t.Fatalf("recognition evidence lost: %#v", input)
	}
}
