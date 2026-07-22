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
