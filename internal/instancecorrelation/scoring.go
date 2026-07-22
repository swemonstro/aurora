package instancecorrelation

import (
	"sort"
	"strconv"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

type scoredPair struct {
	hookIndex    int
	runtimeIndex int
	score        int
	confidence   Confidence
	reasons      []Reason
	conflicts    []ReasonCode
}

func scorePair(config Config, evaluatedAt time.Time, hook HookObservation, runtime RuntimeObservation, explicitOwners map[string]int, runtimeOwners map[string]int, runtimeIndex int) scoredPair {
	pair := scoredPair{runtimeIndex: runtimeIndex}
	add := func(code ReasonCode, points int) {
		for _, reason := range pair.reasons {
			if reason.Code == code {
				return
			}
		}
		pair.score += points
		pair.reasons = append(pair.reasons, Reason{Code: code, Points: points})
	}
	conflict := func(code ReasonCode) {
		for _, existing := range pair.conflicts {
			if existing == code {
				return
			}
		}
		pair.conflicts = append(pair.conflicts, code)
	}

	candidate := runtime.Candidate
	if hook.Tool != candidate.Tool {
		conflict(ReasonToolConflict)
	} else {
		add(ReasonToolMatch, config.Weights.ToolMatch)
	}

	hookHost := hook.HostID
	if hook.RuntimeHint != nil && hookHost == "" {
		hookHost = hook.RuntimeHint.HostID
	}
	if hookHost != "" {
		if hookHost != candidate.Runtime.HostID {
			conflict(ReasonHostConflict)
		} else {
			add(ReasonHostMatch, config.Weights.HostMatch)
		}
	}
	hookBoot := hook.BootID
	if hook.RuntimeHint != nil && hookBoot == "" {
		hookBoot = hook.RuntimeHint.BootID
	}
	if hookBoot != "" {
		if hookBoot != candidate.Runtime.BootID {
			conflict(ReasonBootConflict)
		} else {
			add(ReasonBootMatch, config.Weights.BootMatch)
		}
	}

	exact := false
	hardPositive := false
	if hook.RuntimeHint != nil {
		key := runtimeKey(*hook.RuntimeHint)
		if owner, exists := runtimeOwners[key]; exists && owner >= 0 && owner != runtimeIndex {
			conflict(ReasonExplicitProcessConflict)
		}
		if sameRuntime(*hook.RuntimeHint, candidate.Runtime) {
			add(ReasonExactRuntimeIdentity, config.Weights.ExactRuntimeIdentity)
			exact, hardPositive = true, true
		} else if hook.RuntimeHint.RootProcess.PID == candidate.Runtime.RootProcess.PID &&
			!hook.RuntimeHint.RootProcess.StartedAt.Equal(candidate.Runtime.RootProcess.StartedAt) {
			conflict(ReasonProcessGenerationConflict)
		} else {
			conflict(ReasonExplicitProcessConflict)
		}
	}
	if hook.ProcessHint != nil {
		key := processKey(*hook.ProcessHint)
		if owner, exists := explicitOwners[key]; exists && owner >= 0 && owner != runtimeIndex {
			conflict(ReasonExplicitProcessConflict)
		}
		matched := false
		for _, member := range candidate.Members {
			if member.PID == hook.ProcessHint.PID && !member.StartedAt.Equal(hook.ProcessHint.StartedAt) {
				conflict(ReasonProcessGenerationConflict)
			}
			if !sameProcess(member, *hook.ProcessHint) {
				continue
			}
			matched, hardPositive = true, true
			add(ReasonExactProcessIdentity, 0)
			if sameProcess(member, candidate.Runtime.RootProcess) {
				add(ReasonRootProcessMatch, config.Weights.RootProcessIdentity)
				exact = true
			} else {
				add(ReasonMemberProcessMatch, config.Weights.MemberProcessIdentity)
			}
		}
		if !matched {
			conflict(ReasonExplicitProcessConflict)
		}
	}
	if hook.ParentOrRootPIDHint != nil {
		add(ReasonMissingProcessStartTime, 0)
		if *hook.ParentOrRootPIDHint == candidate.Runtime.RootProcess.PID {
			add(ReasonPIDOnlyHint, config.Weights.PIDOnlyHint)
		}
	}

	matchOpaque := func(hookValue, runtimeValue instancepresence.OpaqueIdentity, code ReasonCode, points int) {
		if hookValue != "" && runtimeValue != "" && hookValue == runtimeValue {
			add(code, points)
		}
	}
	matchOpaque(hook.ProcessGroupOrJob, runtime.ProcessGroupOrJob, ReasonProcessGroupMatch, config.Weights.ProcessGroupMatch)
	matchOpaque(hook.OSSession, runtime.OSSession, ReasonOSSessionMatch, config.Weights.OSSessionMatch)
	matchOpaque(hook.TerminalFingerprint, runtime.TerminalFingerprint, ReasonTerminalMatch, config.Weights.TerminalMatch)

	if absoluteDuration(hook.ObservedAt.Sub(candidate.Runtime.RootProcess.StartedAt)) <= config.MaximumStartTimeDelta {
		add(ReasonStartTimeClose, config.Weights.StartTimeClose)
	}
	if absoluteDuration(hook.ObservedAt.Sub(runtime.ObservedAt)) <= config.MaximumStartTimeDelta {
		add(ReasonObservationTimeClose, config.Weights.ObservationTimeClose)
	}
	if candidate.Runtime.RootProcess.StartedAt.After(hook.ObservedAt.Add(config.AllowedHookLead)) {
		conflict(ReasonLifecycleConflict)
	}
	if evaluatedAt.Sub(hook.ObservedAt) > config.MaximumObservationAge {
		conflict(ReasonStaleHook)
	}
	if evaluatedAt.Sub(runtime.ObservedAt) > config.MaximumObservationAge {
		conflict(ReasonStaleRuntime)
	}
	if hook.ObservedAt.After(evaluatedAt.Add(config.AllowedHookLead)) || runtime.ObservedAt.After(evaluatedAt.Add(config.AllowedHookLead)) {
		conflict(ReasonLifecycleConflict)
	}
	if (hook.Lifecycle == LifecycleEnded) != (runtime.Lifecycle == LifecycleEnded) {
		conflict(ReasonLifecycleConflict)
	}

	sort.Slice(pair.reasons, func(first, second int) bool { return pair.reasons[first].Code < pair.reasons[second].Code })
	pair.conflicts = uniqueReasonCodes(pair.conflicts)
	switch {
	case len(pair.conflicts) > 0:
		pair.confidence = ConfidenceRejected
	case exact:
		pair.confidence = ConfidenceExact
	case hardPositive && pair.score >= config.MinimumStrongScore:
		pair.confidence = ConfidenceStrong
	case pair.score >= config.MinimumStrongScore:
		pair.confidence = ConfidenceStrong
	case pair.score >= config.MinimumWeakScore:
		pair.confidence = ConfidenceWeak
	default:
		pair.confidence = ConfidenceRejected
	}
	return pair
}

func sameProcess(first, second instancepresence.ProcessIdentity) bool {
	return first.PID == second.PID && first.StartedAt.Equal(second.StartedAt)
}

func sameRuntime(first, second instancepresence.RuntimeIdentity) bool {
	return first.HostID == second.HostID && first.BootID == second.BootID && sameProcess(first.RootProcess, second.RootProcess)
}

func processKey(process instancepresence.ProcessIdentity) string {
	return strconv.FormatUint(process.PID, 10) + "/" + process.StartedAt.UTC().Format(time.RFC3339Nano)
}

func runtimeKey(runtime instancepresence.RuntimeIdentity) string {
	return runtime.HostID + "/" + string(runtime.BootID) + "/" + processKey(runtime.RootProcess)
}

func absoluteDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
