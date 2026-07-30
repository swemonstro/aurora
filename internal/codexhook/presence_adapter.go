package codexhook

import (
	"errors"
	"strings"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

// CodexStartupAttention is the single source of truth for whether a newly
// observed Codex process, before any hook event has been observed for it,
// should be treated as attention (StartupPending). It is always false:
// neither a missing project trust entry nor an explicitly untrusted entry
// is, by itself, evidence that Codex is actually displaying an observed
// question — that would be an inference from configuration, not an
// observation. Both internal/runtimepresence.RegistrySync (the monolith's
// registry/presentation path, still the ESP's live status source) and
// internal/codexproducer (the standalone G.4 shadow producer) must treat
// Codex startup as idle-until-hook by deferring to this single function
// rather than each independently deciding what counts as startup attention,
// so the two can never diverge on this point over time.
//
// Attention may still come from a real observed signal: a PermissionRequest
// hook event (see MapEvent) or an actually observed error. If Codex's real
// startup trust prompt ("Do you trust this folder?") ever gets a reliable
// observed signal in the hook event set, this is the single function to
// change — not a per-caller heuristic layered on top of trust
// configuration, cwd, or process interactivity.
func CodexStartupAttention() bool { return false }

func LocalHookObservation(event Event, metadata hookadapter.Metadata) (hookadapter.Observation, error) {
	action, supported := MapEvent(event)
	if !supported {
		return hookadapter.Observation{}, errors.New("unsupported Codex lifecycle event")
	}
	return hookadapter.ObservationFromLifecycle(instancepresence.ToolCodex, event.SessionID, action.Remove, action.State, metadata)
}

// LocalIngressObservation maps a verified Codex lifecycle event to the minimal
// Package 6 ingress. Provider SessionEnd is intentionally not sent locally;
// the wrapper's synthetic SessionEnd remains a legacy v1-only source.
func LocalIngressObservation(event Event) (hookadapter.IngressObservation, error) {
	if event.HookEventName == "SessionEnd" {
		return hookadapter.IngressObservation{}, errors.New("Codex SessionEnd is not accepted for Package 6 ingress")
	}
	action, supported := MapEvent(event)
	if !supported {
		return hookadapter.IngressObservation{}, errors.New("unsupported Codex lifecycle event")
	}
	return hookadapter.IngressFromLifecycle(instancepresence.ToolCodex, event.SessionID, action.Remove, action.State)
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
		if executable == "codex" ||
			executable == "aurora-codex" ||
			strings.HasPrefix(executable, "codex-linux-") {
			direct = true
			native = native ||
				strings.Contains(executable, "native") ||
				strings.HasPrefix(executable, "codex-linux-")
		}
	}
	if direct {
		if len(process.Argv) >= 2 && (process.Argv[1] == "app-server" || process.Argv[1] == "--version") {
			return runtimerecognition.Recognition{}, false
		}
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
