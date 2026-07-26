// Package instanceregistry implements the transport- and OS-neutral in-memory
// state machine for canonical per-instance presence.
package instanceregistry

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

type Config struct {
	Clock         instancepresence.Clock
	SlotNamespace string
	LeaseDuration time.Duration
	GracePeriod   time.Duration
	// MaximumProducerLeaseDuration bounds how far into the future
	// (LeaseExpiresAt - broker's own now) ApplyProducerReport accepts, so a
	// producer cannot grant itself permanent state. It is anchored to the
	// registry's own clock at call time, not to the report's own
	// (producer-supplied) ObservedAt — a report cannot widen its effective
	// lease budget by lying about ObservedAt. It is unused by
	// Register/ApplyRuntimeMutation/ApplyHookMutation (which compute their
	// own lease from LeaseDuration above), so callers that never use
	// ApplyProducerReport may leave it zero; ApplyProducerReport itself
	// refuses to run without a positive value configured.
	MaximumProducerLeaseDuration time.Duration
	// MaximumClockSkew bounds how far into the future a report's ObservedAt
	// may be relative to the registry's own clock, guarding against a
	// producer (or a compromised one) whose clock is fast or that is lying
	// about the current time. Zero disables the check — ApplyProducerReport
	// runs without it configured, unlike MaximumProducerLeaseDuration.
	MaximumClockSkew time.Duration
	// MaximumReportAge bounds how far into the past a report's ObservedAt
	// may be relative to the registry's own clock, guarding against a
	// stale or replayed report being applied long after it was produced.
	// Zero disables the check.
	MaximumReportAge time.Duration
}

func (config Config) validate() error {
	if config.Clock == nil {
		return errors.New("registry clock must not be nil")
	}
	if strings.TrimSpace(config.SlotNamespace) == "" {
		return errors.New("slot namespace must not be empty")
	}
	if config.LeaseDuration <= 0 {
		return errors.New("lease duration must be positive")
	}
	if config.GracePeriod <= 0 {
		return errors.New("grace period must be positive")
	}
	if config.MaximumProducerLeaseDuration < 0 {
		return errors.New("maximum producer lease duration must not be negative")
	}
	if config.MaximumClockSkew < 0 {
		return errors.New("maximum clock skew must not be negative")
	}
	if config.MaximumReportAge < 0 {
		return errors.New("maximum report age must not be negative")
	}
	return nil
}

// Registration is the producer-supplied identity of one runtime generation.
// Lease and lifecycle timestamps are server-owned and derived from Config.Clock.
type Registration struct {
	InstanceID      instancepresence.InstanceID
	Tool            instancepresence.ToolKind
	Source          instancepresence.SourceDescriptor
	Runtime         instancepresence.RuntimeIdentity
	ProducerEpoch   instancepresence.ProducerEpoch
	RuntimeRevision instancepresence.RuntimeRevision
	ObservedAt      time.Time
	IdempotencyKey  string
	// Status is the initial runtime status. Empty defaults to RuntimeAlive.
	// Only RuntimeAlive and RuntimeSuspended are valid at registration so a
	// newly discovered stopped process can open as attention without a
	// temporary idle presentation.
	Status instancepresence.RuntimeStatus
	// StartupPending marks a runtime born after the observer baseline that is
	// still in a startup trust dialog (Claude or interactive Codex). Pre-existing
	// processes and non-interactive Codex leave this false.
	StartupPending bool
}

func (registration Registration) validate() error {
	checks := []struct {
		name string
		err  error
	}{
		{"instance ID", registration.InstanceID.Validate()},
		{"tool", registration.Tool.Validate()},
		{"source", registration.Source.Validate()},
		{"runtime", registration.Runtime.Validate()},
		{"producer epoch", registration.ProducerEpoch.Validate()},
	}
	for _, check := range checks {
		if check.err != nil {
			return fmt.Errorf("%s: %w", check.name, check.err)
		}
	}
	if registration.RuntimeRevision == 0 {
		return errors.New("runtime revision must be positive")
	}
	if registration.ObservedAt.IsZero() {
		return errors.New("registration observation time must not be zero")
	}
	if strings.TrimSpace(registration.IdempotencyKey) == "" {
		return errors.New("registration idempotency key must not be empty")
	}
	switch registration.Status {
	case "", instancepresence.RuntimeAlive, instancepresence.RuntimeSuspended:
	default:
		return fmt.Errorf("registration status %q is not allowed", registration.Status)
	}
	return nil
}

func (registration Registration) initialStatus() instancepresence.RuntimeStatus {
	if registration.Status == "" {
		return instancepresence.RuntimeAlive
	}
	return registration.Status
}

// ExpiryResult reports deterministic lifecycle transitions performed by one
// expiry scan. Returned slices are sorted by logical slot, then instance ID.
type ExpiryResult struct {
	SuspectMissing []instancepresence.InstanceID
	Ended          []instancepresence.InstanceID
}
