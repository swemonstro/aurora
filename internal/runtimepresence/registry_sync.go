package runtimepresence

import (
	"fmt"
	"os"
	"time"

	"github.com/swemonstro/aurora/internal/claudetrust"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

// RegistrySync keeps canonical v2 instances aligned with secure runtime families.
// Coarse v1 publish remains independent via Agent.
type RegistrySync struct {
	registry      *instanceregistry.Registry
	hostID        string
	producerEpoch instancepresence.ProducerEpoch
	source        instancepresence.SourceDescriptor
	// known maps instance ID → last runtime revision applied by this sync.
	known map[instancepresence.InstanceID]instancepresence.RuntimeRevision
	clock Clock
	// observerStartedAt is the wall-clock moment this sync (observer) began.
	// Runtimes with RootProcess.StartedAt strictly after this baseline may be
	// startup-pending. Pre-existing processes at service restart stay idle
	// without a hook (Claude) or trust metadata (Codex).
	observerStartedAt time.Time
	// userHome resolves ~/.claude.json's directory when needed. Tests may override.
	userHome func() (string, error)
	// claudeTrust observes Claude project presence in ~/.claude.json through /proc/<pid>/root.
	// Tests may override.
	claudeTrust func(pid uint64, userHome, cwd string) claudetrust.Status
}

// NewRegistrySync constructs a multi-instance registry synchronizer.
// The observer baseline is taken from clock() at construction time.
func NewRegistrySync(
	registry *instanceregistry.Registry,
	hostID string,
	epoch instancepresence.ProducerEpoch,
	source instancepresence.SourceDescriptor,
	clock Clock,
) (*RegistrySync, error) {
	if registry == nil {
		return nil, fmt.Errorf("registry must not be nil")
	}
	if hostID == "" {
		return nil, fmt.Errorf("host ID must not be empty")
	}
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}
	baseline := clock().UTC()
	if baseline.IsZero() {
		return nil, fmt.Errorf("observer baseline clock returned zero time")
	}
	return &RegistrySync{
		registry: registry, hostID: hostID, producerEpoch: epoch, source: source,
		known: make(map[instancepresence.InstanceID]instancepresence.RuntimeRevision), clock: clock,
		observerStartedAt: baseline,
		userHome:          os.UserHomeDir,
		claudeTrust:       claudetrust.Observer{ProcRoot: "/proc"}.Observe,
	}, nil
}

// ObserverStartedAt reports the baseline used for startup-pending decisions.
func (sync *RegistrySync) ObserverStartedAt() time.Time { return sync.observerStartedAt }

// ApplyRecognition registers/renews secure families and ends missing ones.
// Handles 0→1→2→1→0 without mixing instances (identity is host+boot+pid+start).
// Recognized families map to RuntimeAlive or RuntimeSuspended from root stop state.
//
// Startup-pending uses a generation start time that is strictly after the
// observer baseline, and only for Claude: a post-baseline generation with no
// observed ~/.claude.json project entry is startup-pending. Codex never sets
// StartupPending at all — see startupAtRegister and
// codexhook.CodexStartupAttention for the shared G.4 semantic both this
// monolith path and internal/codexproducer defer to.
func (sync *RegistrySync) ApplyRecognition(result runtimerecognition.Result, bootID instancepresence.BootIdentity) error {
	now := sync.clock().UTC()
	seen := make(map[instancepresence.InstanceID]struct{})
	userHome := ""
	if sync.userHome != nil {
		if home, err := sync.userHome(); err == nil {
			userHome = home
		}
	}

	for _, family := range result.Families {
		candidate := family.Candidate
		id := candidate.InstanceID
		if id == "" {
			id = runtimerecognition.StableInstanceID(sync.hostID, bootID, candidate.Tool, candidate.Runtime.RootProcess)
		}
		seen[id] = struct{}{}
		status := instancepresence.RuntimeAlive
		if family.Suspended {
			status = instancepresence.RuntimeSuspended
		}
		if _, known := sync.known[id]; !known {
			pending := sync.startupAtRegister(family, userHome)
			registration := instanceregistry.Registration{
				InstanceID: id, Tool: candidate.Tool, Source: sync.source,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: sync.hostID, BootID: bootID, RootProcess: candidate.Runtime.RootProcess,
				},
				ProducerEpoch: sync.producerEpoch, RuntimeRevision: 1, ObservedAt: now,
				IdempotencyKey: fmt.Sprintf("runtime-register|%s|1", id),
				Status:         status,
				StartupPending: pending,
			}
			if _, err := sync.registry.Register(registration); err != nil {
				// Exact retry of identical registration is OK; identity conflicts are hard errors.
				if existing, getErr := sync.registry.Get(id); getErr == nil {
					_, hadKnown := sync.known[id]
					sync.known[id] = existing.Revisions.RuntimeRevision
					if renewErr := sync.renew(id, status, now, family, userHome); renewErr != nil {
						if !hadKnown {
							delete(sync.known, id)
						}
						return renewErr
					}
					continue
				}
				return fmt.Errorf("register %s: %w", id, err)
			}
			// Register succeeded: only then record known.
			sync.known[id] = 1
			continue
		}
		if err := sync.renew(id, status, now, family, userHome); err != nil {
			return err
		}
	}

	for id, revision := range sync.known {
		if _, ok := seen[id]; ok {
			continue
		}
		next := revision + 1
		mutation := presencev2.RuntimeMutation{
			ProducerEpoch: sync.producerEpoch, RuntimeRevision: next,
			Status: instancepresence.RuntimeEnded, ObservedAt: now,
			IdempotencyKey: fmt.Sprintf("runtime-end|%s|%d", id, next),
		}
		if _, err := sync.registry.EndRuntime(id, mutation); err != nil {
			return fmt.Errorf("end %s: %w", id, err)
		}
		delete(sync.known, id)
	}
	return nil
}

func (sync *RegistrySync) renew(
	id instancepresence.InstanceID,
	status instancepresence.RuntimeStatus,
	now time.Time,
	family runtimerecognition.Family,
	userHome string,
) error {
	next := sync.known[id] + 1
	type renewStartupAction uint8
	const (
		renewLeave renewStartupAction = iota
		renewClear
		renewSet
	)
	action := renewLeave
	keySuffix := string(status)
	var existing instancepresence.Instance
	haveExisting := false
	getExisting := func() (instancepresence.Instance, error) {
		if haveExisting {
			return existing, nil
		}
		inst, err := sync.registry.Get(id)
		if err != nil {
			return instancepresence.Instance{}, err
		}
		existing = inst
		haveExisting = true
		return existing, nil
	}
	// Codex has no startup-pending observer here at all: codexhook.CodexStartupAttention
	// is always false, so there is nothing to (re-)evaluate on renew — a Codex
	// instance's StartupPending is set to false once at registration (see
	// startupAtRegister) and never revisited. This is the single shared
	// semantic with internal/codexproducer; see codexhook.CodexStartupAttention's
	// doc comment.

	// Claude trust observer: only post-baseline generations participate.
	if family.Candidate.Tool == instancepresence.ToolClaude &&
		family.Candidate.Runtime.RootProcess.StartedAt.After(sync.observerStartedAt) {
		inst, err := getExisting()
		if err != nil {
			return fmt.Errorf("renew %s: %w", id, err)
		}
		// Once a hook owns the instance, never reactivate Claude startup pending.
		if inst.Revisions.HookRevision == 0 &&
			inst.HookClaim == instancepresence.NoHookClaim {
			observe := sync.claudeTrust
			if observe == nil {
				observe = claudetrust.Observer{ProcRoot: "/proc"}.Observe
			}
			switch observe(family.Candidate.Runtime.RootProcess.PID, userHome, family.WorkingDirectory) {
			case claudetrust.ProjectMissing:
				if !inst.StartupPending {
					action = renewSet
					keySuffix = string(status) + "|claude-startup-pending"
				}
			case claudetrust.ProjectPresent:
				if inst.StartupPending {
					action = renewClear
					keySuffix = string(status) + "|claude-startup-cleared"
				}
			case claudetrust.Unknown:
				// Leave unchanged: do not invent attention or clear pending on uncertainty.
			}
		}
	}

	mutation := presencev2.RuntimeMutation{
		ProducerEpoch: sync.producerEpoch, RuntimeRevision: next,
		Status: status, ObservedAt: now,
		IdempotencyKey: fmt.Sprintf("runtime-lease|%s|%d|%s", id, next, keySuffix),
	}
	var err error
	switch action {
	case renewClear:
		_, err = sync.registry.ApplyRuntimeMutationClearingStartup(id, mutation)
	case renewSet:
		_, err = sync.registry.ApplyRuntimeMutationSettingStartupPending(id, mutation)
	default:
		_, err = sync.registry.ApplyRuntimeMutation(id, mutation)
	}
	if err != nil {
		// Leave known unchanged on failure.
		return fmt.Errorf("renew %s: %w", id, err)
	}
	sync.known[id] = next
	return nil
}

// KnownCount reports tracked instances (tests).
func (sync *RegistrySync) KnownCount() int { return len(sync.known) }

// startupAtRegister decides StartupPending at first registration.
//
// Claude uses RootProcess.StartedAt (unchanged): a post-baseline generation
// with no observed ~/.claude.json project entry is startup-pending, exactly
// as before.
//
// Codex never sets StartupPending here, regardless of baseline, trust
// configuration, or process interactivity — see
// codexhook.CodexStartupAttention, the single shared function both this
// monolith path and internal/codexproducer defer to for that decision. This
// is the G.4 false-red fix: a missing or explicitly untrusted Codex project
// trust entry is not, by itself, evidence of an observed question.
func (sync *RegistrySync) startupAtRegister(family runtimerecognition.Family, userHome string) (pending bool) {
	switch family.Candidate.Tool {
	case instancepresence.ToolClaude:
		start := family.Candidate.Runtime.RootProcess.StartedAt
		if start.IsZero() || sync.observerStartedAt.IsZero() || !start.After(sync.observerStartedAt) {
			return false
		}
		observe := sync.claudeTrust
		if observe == nil {
			observe = claudetrust.Observer{ProcRoot: "/proc"}.Observe
		}
		return observe(family.Candidate.Runtime.RootProcess.PID, userHome, family.WorkingDirectory) == claudetrust.ProjectMissing
	case instancepresence.ToolCodex:
		return codexhook.CodexStartupAttention()
	default:
		return false
	}
}

// claudeStartupPending is retained for tests covering Claude-only baseline rules.
func claudeStartupPending(tool instancepresence.ToolKind, processStarted, observerStarted time.Time) bool {
	if tool != instancepresence.ToolClaude {
		return false
	}
	if processStarted.IsZero() || observerStarted.IsZero() {
		return false
	}
	return processStarted.After(observerStarted)
}

// Ensure time import used when clock advances in tests via external.
var _ = time.Second
