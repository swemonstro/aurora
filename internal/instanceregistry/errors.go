package instanceregistry

import (
	"errors"
	"fmt"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

var (
	ErrNotFound            = errors.New("instance not found")
	ErrIdentityConflict    = errors.New("instance or runtime identity conflict")
	ErrEpochConflict       = errors.New("producer epoch conflict")
	ErrStaleRevision       = errors.New("stale revision")
	ErrRevisionConflict    = errors.New("revision conflict")
	ErrIdempotencyConflict = errors.New("idempotency conflict")
	ErrRuntimeEnded        = errors.New("runtime has ended")
	// ErrToolMismatch is returned by ApplyProducerReport when a report's
	// Tool does not match the tool the instance id is already registered
	// under. Unlike ErrEpochConflict, this is never a legitimate takeover:
	// cross-tool takeover is always rejected.
	ErrToolMismatch = errors.New("tool does not match the existing instance")
	// ErrLeaseAlreadyExpired is returned by ApplyProducerReport when the
	// report's lease deadline is not after the registry's current time —
	// the report would create an instance that is already expired on
	// arrival.
	ErrLeaseAlreadyExpired = errors.New("lease deadline is not after the current time")
	// ErrLeaseExceedsMaximum is returned by ApplyProducerReport when the
	// report's lease deadline is further into the future than the
	// registry's own clock plus its configured maximum producer lease
	// duration allows, so a producer cannot grant itself permanent state —
	// this is anchored to the registry's now, not to the report's own
	// (producer-supplied) ObservedAt, see Config.MaximumProducerLeaseDuration.
	ErrLeaseExceedsMaximum = errors.New("lease span exceeds the configured maximum")
	// ErrClockSkewTooLarge is returned by ApplyProducerReport when the
	// report's ObservedAt is further into the future than the registry's
	// configured MaximumClockSkew allows, relative to the registry's own
	// clock.
	ErrClockSkewTooLarge = errors.New("observed_at is too far ahead of the current time")
	// ErrReportTooOld is returned by ApplyProducerReport when the report's
	// ObservedAt is further into the past than the registry's configured
	// MaximumReportAge allows, relative to the registry's own clock.
	ErrReportTooOld = errors.New("observed_at is too far behind the current time")
)

// DomainError classifies registry failures while retaining the addressed
// opaque instance ID. Callers should use errors.Is to inspect Kind.
type DomainError struct {
	Op         string
	InstanceID instancepresence.InstanceID
	Kind       error
	Detail     string
}

func (err *DomainError) Error() string {
	if err.Detail == "" {
		return fmt.Sprintf("%s %q: %v", err.Op, err.InstanceID, err.Kind)
	}
	return fmt.Sprintf("%s %q: %v: %s", err.Op, err.InstanceID, err.Kind, err.Detail)
}

func (err *DomainError) Unwrap() error { return err.Kind }

func domainError(op string, id instancepresence.InstanceID, kind error, detail string) error {
	return &DomainError{Op: op, InstanceID: id, Kind: kind, Detail: detail}
}
