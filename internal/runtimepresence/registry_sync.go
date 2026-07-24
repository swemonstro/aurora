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
		userHome:          os.UserHomeDir,
		projectTrust:      codextrust.ProjectTrust,
	}, nil
}

// ObserverStartedAt reports the baseline used for startup-pending decisions.
func (sync *RegistrySync) ObserverStartedAt() time.Time { return sync.observerStartedAt }

// ApplyRecognition registers/renews secure families and ends missing ones.
// Handles 0→1→2→1→0 without mixing instances (identity is host+boot+pid+start).
// Recognized families map to RuntimeAlive or RuntimeSuspended from root stop state.
// Claude/Codex startup-pending is set only for post-baseline generations; Codex
// trust becomes idle when config.toml reports trust_level=trusted (no hook rev).
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
			registration := instanceregistry.Registration{
				InstanceID: id, Tool: candidate.Tool, Source: sync.source,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: sync.hostID, BootID: bootID, RootProcess: candidate.Runtime.RootProcess,
				},
				ProducerEpoch: sync.producerEpoch, RuntimeRevision: 1, ObservedAt: now,
				IdempotencyKey: fmt.Sprintf("runtime-register|%s|1", id),
				Status:         status,
				StartupPending: sync.startupPendingAtRegister(family, userHome),
			}
			if _, err := sync.registry.Register(registration); err != nil {
				// Exact retry of identical registration is OK; identity conflicts are hard errors.
				if existing, getErr := sync.registry.Get(id); getErr == nil {
					sync.known[id] = existing.Revisions.RuntimeRevision
					// Still apply current observed status if registration already exists.
					if renewErr := sync.renew(id, status, now, family, userHome); renewErr != nil {
						return renewErr
					}
					continue
				}
				return fmt.Errorf("register %s: %w", id, err)
			}
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
	clearStartup := false
	if existing, err := sync.registry.Get(id); err == nil && existing.StartupPending {
		// Only clear when trust observation positively reports trusted.
		// unknown/not_trusted preserve pending; never re-activate after clear.
		if family.Candidate.Tool == instancepresence.ToolCodex {
			if sync.observeCodexTrust(family, userHome) == codextrust.Trusted {
				clearStartup = true
			}
		}
	}
	keySuffix := string(status)
	if clearStartup {
		keySuffix = string(status) + "|trust-cleared"
	}
	mutation := presencev2.RuntimeMutation{
		ProducerEpoch: sync.producerEpoch, RuntimeRevision: next,
		Status: status, ObservedAt: now,
		IdempotencyKey: fmt.Sprintf("runtime-lease|%s|%d|%s", id, next, keySuffix),
	}
	var err error
	if clearStartup {
		_, err = sync.registry.ApplyRuntimeMutationClearingStartup(id, mutation)
	} else {
		_, err = sync.registry.ApplyRuntimeMutation(id, mutation)
	}
	if err != nil {
		return fmt.Errorf("renew %s: %w", id, err)
	}
	sync.known[id] = next
	return nil
}

// KnownCount reports tracked instances (tests).
func (sync *RegistrySync) KnownCount() int { return len(sync.known) }

func (sync *RegistrySync) startupPendingAtRegister(family runtimerecognition.Family, userHome string) bool {
	start := family.Candidate.Runtime.RootProcess.StartedAt
	if start.IsZero() || sync.observerStartedAt.IsZero() || !start.After(sync.observerStartedAt) {
		return false
	}
	switch family.Candidate.Tool {
	case instancepresence.ToolClaude:
		return true
	case instancepresence.ToolCodex:
		if !codextrust.InteractiveArgv(family.Argv) {
			return false
		}
		// unknown → fail-safe not pending (no false attention).
		// trusted → not pending. not_trusted → pending.
		return sync.observeCodexTrust(family, userHome) == codextrust.NotTrusted
	default:
		return false
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
