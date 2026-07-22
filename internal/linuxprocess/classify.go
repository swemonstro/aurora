package linuxprocess

import (
	"path/filepath"
	"strings"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

type processRole string

const (
	roleUnknown processRole = "unknown"
	roleDirect  processRole = "direct"
	roleWrapper processRole = "wrapper"
	roleNode    processRole = "node_launcher"
	roleNative  processRole = "native_child"
)

type classification struct {
	tool instancepresence.ToolKind
	role processRole
}

func classify(record rawProcess) classification {
	base := normalizeName(record.executableBase)
	comm := normalizeName(record.stat.Comm)
	if isClaudeBinary(base) || isClaudeBinary(comm) {
		role := roleDirect
		if strings.Contains(base, "native") || strings.Contains(comm, "native") {
			role = roleNative
		}
		return classification{tool: instancepresence.ToolClaude, role: role}
	}
	if isCodexBinary(base) || isCodexBinary(comm) {
		role := roleDirect
		if strings.Contains(base, "native") || strings.Contains(comm, "native") || strings.Contains(base, "linux-") {
			role = roleNative
		}
		return classification{tool: instancepresence.ToolCodex, role: role}
	}

	tool := toolFromArgv(record.argvPrefix)
	if tool == "" {
		return classification{role: roleUnknown}
	}
	switch base {
	case "node", "nodejs":
		return classification{tool: tool, role: roleNode}
	case "sh", "bash", "zsh", "fish", "env", "npm", "npx":
		return classification{tool: tool, role: roleWrapper}
	default:
		return classification{tool: tool, role: roleWrapper}
	}
}

func toolFromArgv(arguments []string) instancepresence.ToolKind {
	var claude, codex bool
	for _, argument := range arguments {
		lower := strings.ToLower(argument)
		base := normalizeName(filepath.Base(argument))
		claude = claude || isClaudeBinary(base) || strings.Contains(lower, "@anthropic-ai/claude-code")
		codex = codex || isCodexBinary(base) || strings.Contains(lower, "@openai/codex")
	}
	if claude == codex {
		return ""
	}
	if claude {
		return instancepresence.ToolClaude
	}
	return instancepresence.ToolCodex
}

func isClaudeBinary(name string) bool {
	return name == "claude" || name == "claude-code" || name == "aurora-claude" || strings.HasPrefix(name, "claude-native-")
}

func isCodexBinary(name string) bool {
	return name == "codex" || name == "aurora-codex" || strings.HasPrefix(name, "codex-")
}

func normalizeName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
