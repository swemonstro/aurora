package runtimepresence

import (
	"fmt"
	"os"
	"time"

	"github.com/swemonstro/aurora/internal/codextrust"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

// codexStartupTrustState tracks per-generation Codex trust observation so an
// initial Unknown poll can later become NotTrusted (pending) without reopening
// a generation that already resolved (trusted or hooked).
type codexStartupTrustState uint8

const (
	codexTrustUnresolved codexStartupTrustState = iota + 1
	codexTrustPending
	codexTrustResolved
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
	// codexStartupTrust tracks post-baseline interactive Codex generations only.
	codexStartupTrust map[instancepresence.InstanceID]codexStartupTrustState
	// userHome resolves ~/.codex when CODEX_HOME is unset. Tests may override.
	userHome func() (string, error)
	// projectTrust observes Codex config.toml trust. Tests may override.
	projectTrust func(codexHomeEnv, projectDir, userHome string) codextrust.Status
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
		codexStartupTrust: make(map[instancepresence.InstanceID]codexStartupTrustState),
		userHome:          os.UserHomeDir,
		projectTrust:      codextrust.ProjectTrust,
	}, nil
}

// ObserverStartedAt reports the baseline used for startup-pending decisions.
func (sync *RegistrySync) ObserverStartedAt() time.Time { return sync.observerStartedAt }

// ApplyRecognition registers/renews secure families and ends missing ones.
// Handles 0→1→2→1→0 without mixing instances (identity is host+boot+pid+start).
// Recognized families map to RuntimeAlive or RuntimeSuspended from root stop state.
//
// Startup-pending uses a generation start time that is strictly after the
// observer baseline:
//   - Claude: Candidate.Runtime.RootProcess.StartedAt
//   - Codex:  Family.LaunchProcess.StartedAt (outermost matched Codex process,
//     e.g. Node launcher — never a pre-baseline terminal shell)
//
// Codex trust may resolve after an initial Unknown poll without inventing hook
// revisions (unresolved → pending → resolved).
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
			pending, trustState, track := sync.startupAtRegister(family, userHome)
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
					// Install tracking before renew so Unknown→NotTrusted still works
					// after an exact registration retry. Roll back map/known if renew fails.
					_, hadKnown := sync.known[id]
					_, hadTrust := sync.codexStartupTrust[id]
					sync.known[id] = existing.Revisions.RuntimeRevision
					installedTrust := false
					if track {
						if !hadTrust {
							sync.codexStartupTrust[id] = trustState
							installedTrust = true
						}
					}
					if renewErr := sync.renew(id, status, now, family, userHome); renewErr != nil {
						if !hadKnown {
							delete(sync.known, id)
						}
						if installedTrust {
							delete(sync.codexStartupTrust, id)
						}
						return renewErr
					}
					continue
				}
				return fmt.Errorf("register %s: %w", id, err)
			}
			// Register succeeded: only then record known + trust tracking.
			sync.known[id] = 1
			if track {
				sync.codexStartupTrust[id] = trustState
			}
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
		delete(sync.codexStartupTrust, id)
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
	// nextTrust is applied only after a successful registry mutation.
	var nextTrust codexStartupTrustState
	var commitTrust bool

	if trustState, tracked := sync.codexStartupTrust[id]; tracked {
		existing, err := sync.registry.Get(id)
		if err != nil {
			return fmt.Errorf("renew %s: %w", id, err)
		}
		switch trustState {
		case codexTrustUnresolved:
			if existing.Revisions.HookRevision > 0 || existing.HookClaim != instancepresence.NoHookClaim {
				// Hook already owns the instance; resolve after ordinary renew.
				nextTrust, commitTrust = codexTrustResolved, true
				break
			}
			switch sync.observeCodexTrust(family, userHome) {
			case codextrust.Unknown:
				// Stay unresolved; ordinary renew, no map change.
			case codextrust.Trusted:
				nextTrust, commitTrust = codexTrustResolved, true
			case codextrust.NotTrusted:
				action = renewSet
				keySuffix = string(status) + "|trust-pending"
				// Final map state decided from mutation return value.
			}
		case codexTrustPending:
			if existing.Revisions.HookRevision > 0 || existing.HookClaim != instancepresence.NoHookClaim || !existing.StartupPending {
				nextTrust, commitTrust = codexTrustResolved, true
				break
			}
			switch sync.observeCodexTrust(family, userHome) {
			case codextrust.Trusted:
				action = renewClear
				keySuffix = string(status) + "|trust-cleared"
				nextTrust, commitTrust = codexTrustResolved, true
			case codextrust.Unknown, codextrust.NotTrusted:
				// Keep pending; ordinary renew, no map change.
			}
		case codexTrustResolved:
			// Never re-evaluate trust or re-activate pending.
		}
	}

	mutation := presencev2.RuntimeMutation{
		ProducerEpoch: sync.producerEpoch, RuntimeRevision: next,
		Status: status, ObservedAt: now,
		IdempotencyKey: fmt.Sprintf("runtime-lease|%s|%d|%s", id, next, keySuffix),
	}
	var (
		err    error
		result instancepresence.Instance
	)
	switch action {
	case renewClear:
		result, err = sync.registry.ApplyRuntimeMutationClearingStartup(id, mutation)
	case renewSet:
		result, err = sync.registry.ApplyRuntimeMutationSettingStartupPending(id, mutation)
	default:
		result, err = sync.registry.ApplyRuntimeMutation(id, mutation)
	}
	if err != nil {
		// Leave known + codexStartupTrust unchanged on failure.
		return fmt.Errorf("renew %s: %w", id, err)
	}

	// Commit map transitions only after a successful registry mutation.
	if action == renewSet {
		if result.StartupPending &&
			result.Revisions.HookRevision == 0 &&
			result.HookClaim == instancepresence.NoHookClaim {
			sync.codexStartupTrust[id] = codexTrustPending
		} else {
			// Hook or other transition won the race; do not leave unresolved.
			sync.codexStartupTrust[id] = codexTrustResolved
		}
	} else if commitTrust {
		sync.codexStartupTrust[id] = nextTrust
	}
	sync.known[id] = next
	return nil
}

// KnownCount reports tracked instances (tests).
func (sync *RegistrySync) KnownCount() int { return len(sync.known) }

// codexTrustStateFor reports tracked trust state (tests).
func (sync *RegistrySync) codexTrustStateFor(id instancepresence.InstanceID) (codexStartupTrustState, bool) {
	state, ok := sync.codexStartupTrust[id]
	return state, ok
}

// startupAtRegister decides StartupPending and optional Codex trust tracking.
// track is true only for post-baseline interactive Codex generations.
//
// Claude uses RootProcess.StartedAt (unchanged). Codex uses LaunchProcess
// (outermost matched Codex process such as the Node launcher), never a
// terminal shell that may predate the observer baseline.
func (sync *RegistrySync) startupAtRegister(family runtimerecognition.Family, userHome string) (pending bool, state codexStartupTrustState, track bool) {
	start := family.Candidate.Runtime.RootProcess.StartedAt
	if family.Candidate.Tool == instancepresence.ToolCodex {
		if family.LaunchProcess.PID != 0 && !family.LaunchProcess.StartedAt.IsZero() {
			start = family.LaunchProcess.StartedAt
		}
	}
	if start.IsZero() || sync.observerStartedAt.IsZero() || !start.After(sync.observerStartedAt) {
		return false, 0, false
	}
	switch family.Candidate.Tool {
	case instancepresence.ToolClaude:
		return true, 0, false
	case instancepresence.ToolCodex:
		// Interactivity from LaunchProcess argv only — never shell argv.
		if !codextrust.InteractiveArgv(family.Argv) {
			return false, 0, false
		}
		switch sync.observeCodexTrust(family, userHome) {
		case codextrust.Unknown:
			return false, codexTrustUnresolved, true
		case codextrust.NotTrusted:
			return true, codexTrustPending, true
		case codextrust.Trusted:
			return false, codexTrustResolved, true
		default:
			return false, codexTrustUnresolved, true
		}
	default:
		return false, 0, false
	}
}

func (sync *RegistrySync) observeCodexTrust(family runtimerecognition.Family, userHome string) codextrust.Status {
	lookup := sync.projectTrust
	if lookup == nil {
		lookup = codextrust.ProjectTrust
	}
	return lookup(family.EnvCodexHome, family.WorkingDirectory, userHome)
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
