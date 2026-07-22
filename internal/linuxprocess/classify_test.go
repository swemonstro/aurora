package linuxprocess

import (
	"strings"
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestToolClassificationUsesSanitizedLimitedSignals(t *testing.T) {
	tests := []struct {
		name   string
		record rawProcess
		tool   instancepresence.ToolKind
		role   processRole
	}{
		{name: "claude binary", record: rawForClassification("claude", "claude", nil), tool: instancepresence.ToolClaude, role: roleDirect},
		{name: "codex native variant", record: rawForClassification("codex-linux-x86_64", "codex", nil), tool: instancepresence.ToolCodex, role: roleNative},
		{name: "claude node package", record: rawForClassification("node", "node", []string{"node", "/opaque/@anthropic-ai/claude-code/cli.js"}), tool: instancepresence.ToolClaude, role: roleNode},
		{name: "codex wrapper", record: rawForClassification("bash", "bash", []string{"bash", "/opaque/aurora-codex"}), tool: instancepresence.ToolCodex, role: roleWrapper},
		{name: "unknown node", record: rawForClassification("node", "node", []string{"node", "/opaque/app.js"}), role: roleUnknown},
		{name: "conflicting hints", record: rawForClassification("node", "node", []string{"node", "claude", "codex"}), role: roleUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classify(test.record)
			if got.tool != test.tool || got.role != test.role {
				t.Fatalf("classify() = %#v, want tool %q role %q", got, test.tool, test.role)
			}
		})
	}
}

func rawForClassification(executable, comm string, argv []string) rawProcess {
	return rawProcess{executableBase: executable, argvPrefix: argv, stat: procStat{Comm: comm}}
}

func TestArgvSignalsDiscardUnknownAndSensitiveValues(t *testing.T) {
	signals := argvSignals([]byte("/opaque/bin/node\x00/opaque/@openai/codex/bin/codex.js\x00--api-key\x00secret-value\x00"))
	for _, signal := range signals {
		if signal == "--api-key" || signal == "secret-value" || strings.Contains(signal, "/opaque/") {
			t.Fatalf("argvSignals retained sensitive value %q", signal)
		}
	}
	if got := toolFromArgv(signals); got != instancepresence.ToolCodex {
		t.Fatalf("toolFromArgv(%v) = %q, want codex", signals, got)
	}
}
