package codexhook

import (
	"errors"
	"strings"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

func LocalHookObservation(event Event, metadata hookadapter.Metadata) (hookadapter.Observation, error) {
	action, supported := MapEvent(event)
	if !supported {
		return hookadapter.Observation{}, errors.New("unsupported Codex lifecycle event")
	}
	return hookadapter.ObservationFromLifecycle(instancepresence.ToolCodex, event.SessionID, action.Remove, action.State, metadata)
}

func LaunchIdentityRules() []runtimerecognition.LaunchIdentityRule {
	return []runtimerecognition.LaunchIdentityRule{
		{Mode: runtimerecognition.LaunchRulePackagePath, Value: "@openai/codex", Identity: "launch:openai-codex", Argument: runtimerecognition.LaunchArgumentEntrypoint, Launchers: []string{"node", "nodejs"}},
		{Mode: runtimerecognition.LaunchRuleExactBasename, Value: "aurora-codex", Identity: "launch:aurora-codex", Argument: runtimerecognition.LaunchArgumentArgv0},
		{Mode: runtimerecognition.LaunchRuleExactBasename, Value: "aurora-codex", Identity: "launch:aurora-codex", Argument: runtimerecognition.LaunchArgumentEntrypoint, Launchers: []string{"sh", "bash", "zsh", "fish", "env", "npx"}},
	}
}

func RuntimeRecognizer() runtimerecognition.AgentRuntimeRecognizer { return runtimeRecognizer{} }

type runtimeRecognizer struct{}

func (runtimeRecognizer) Recognize(process runtimerecognition.ProcessObservation) (runtimerecognition.Recognition, bool) {
	direct, native := false, false
	for _, executable := range processNames(process) {
		if executable == "codex" || executable == "aurora-codex" || strings.HasPrefix(executable, "codex-") {
			direct = true
			native = native || strings.Contains(executable, "native") || strings.HasPrefix(executable, "codex-linux-")
		}
	}
	if direct {
		role := runtimerecognition.RoleDirect
		if native {
			role = runtimerecognition.RoleNative
		}
		return runtimerecognition.Recognition{Tool: instancepresence.ToolCodex, Role: role, Priority: runtimerecognition.PriorityExecutable}, true
	}
	if hasLaunchIdentity(process, "launch:openai-codex") {
		if hasProcessName(process, "node", "nodejs") {
			return runtimerecognition.Recognition{Tool: instancepresence.ToolCodex, Role: runtimerecognition.RoleNode, Priority: runtimerecognition.PriorityLaunch}, true
		}
		return runtimerecognition.Recognition{Tool: instancepresence.ToolCodex, Role: runtimerecognition.RoleWrapper, Priority: runtimerecognition.PriorityLaunch}, true
	}
	if hasLaunchIdentity(process, "launch:aurora-codex") {
		return runtimerecognition.Recognition{Tool: instancepresence.ToolCodex, Role: runtimerecognition.RoleWrapper, Priority: runtimerecognition.PriorityLaunch}, true
	}
	return runtimerecognition.Recognition{}, false
}

func hasLaunchIdentity(process runtimerecognition.ProcessObservation, want instancepresence.OpaqueIdentity) bool {
	for _, identity := range process.LaunchIdentities {
		if identity == want {
			return true
		}
	}
	return false
}

func processNames(process runtimerecognition.ProcessObservation) []string {
	return []string{normalizedProcessName(process.ExecutableIdentity), normalizedProcessName(process.CommIdentity)}
}

func normalizedProcessName(identity instancepresence.OpaqueIdentity) string {
	return strings.TrimPrefix(strings.ToLower(string(identity)), "exe:")
}

func hasProcessName(process runtimerecognition.ProcessObservation, names ...string) bool {
	for _, got := range processNames(process) {
		for _, want := range names {
			if got == want {
				return true
			}
		}
	}
	return false
}
