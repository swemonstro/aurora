// Package presencev2 defines the transport-neutral v2 wire contract. It does
// not register HTTP routes or implement relay storage.
package presencev2

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

const APIVersion = 2

type Presence string

const (
	PresenceSleeping Presence = "sleeping"
	PresenceActive   Presence = "active"
)

type Source struct {
	Provider    string `json:"provider"`
	Profile     string `json:"profile"`
	CollectorID string `json:"collector_id"`
}

func (source Source) Validate() error {
	return (instancepresence.SourceDescriptor{
		Provider: source.Provider, Profile: source.Profile, CollectorID: source.CollectorID,
	}).Validate()
}

type Slot struct {
	Namespace string `json:"namespace"`
	Index     uint64 `json:"index"`
}

func (slot Slot) Validate() error {
	if strings.TrimSpace(slot.Namespace) == "" {
		return errors.New("slot namespace must not be empty")
	}
	return nil
}

type Revisions struct {
	ProducerEpoch   instancepresence.ProducerEpoch   `json:"producer_epoch"`
	RuntimeRevision instancepresence.RuntimeRevision `json:"runtime_revision"`
	HookRevision    instancepresence.HookRevision    `json:"hook_revision"`
}

type Instance struct {
	InstanceID     instancepresence.InstanceID     `json:"instance_id"`
	Tool           instancepresence.ToolKind       `json:"tool"`
	Source         Source                          `json:"source"`
	State          instancepresence.EffectiveState `json:"state"`
	Slot           Slot                            `json:"slot"`
	Revisions      Revisions                       `json:"revisions"`
	DiscoveredAt   time.Time                       `json:"discovered_at"`
	StateChangedAt time.Time                       `json:"state_changed_at"`
	LeaseExpiresAt time.Time                       `json:"lease_expires_at"`
}

func (instance Instance) Validate() error {
	if err := instance.InstanceID.Validate(); err != nil {
		return err
	}
	if err := instance.Tool.Validate(); err != nil {
		return err
	}
	if err := instance.Source.Validate(); err != nil {
		return err
	}
	if err := instance.State.Validate(); err != nil {
		return err
	}
	if err := instance.Slot.Validate(); err != nil {
		return err
	}
	if err := instance.Revisions.ProducerEpoch.Validate(); err != nil {
		return err
	}
	if instance.Revisions.RuntimeRevision == 0 {
		return errors.New("runtime revision must be positive")
	}
	if instance.Revisions.HookRevision == 0 && instance.State != instancepresence.StateIdle {
		return errors.New("non-idle state requires a positive hook revision")
	}
	if instance.DiscoveredAt.IsZero() || instance.StateChangedAt.IsZero() || instance.LeaseExpiresAt.IsZero() {
		return errors.New("instance lifecycle timestamps must not be zero")
	}
	if instance.StateChangedAt.Before(instance.DiscoveredAt) {
		return errors.New("state-changed time precedes discovery")
	}
	if instance.LeaseExpiresAt.Before(instance.DiscoveredAt) {
		return errors.New("lease expiry precedes discovery")
	}
	return nil
}

type SlotSummary struct {
	Namespace   string `json:"namespace"`
	ActiveCount uint64 `json:"active_count"`
}

// CanonicalSnapshot is always a full active-instance snapshot. Sleeping is a
// register/presentation status and therefore never appears in Instance.State.
type CanonicalSnapshot struct {
	APIVersion  int         `json:"api_version"`
	GeneratedAt time.Time   `json:"generated_at"`
	Presence    Presence    `json:"presence"`
	Instances   []Instance  `json:"instances"`
	Slots       SlotSummary `json:"slots"`
}

func (snapshot CanonicalSnapshot) Validate() error {
	if snapshot.APIVersion != APIVersion {
		return fmt.Errorf("unsupported API version %d", snapshot.APIVersion)
	}
	if snapshot.GeneratedAt.IsZero() {
		return errors.New("snapshot generated time must not be zero")
	}
	if snapshot.Instances == nil {
		return errors.New("full snapshot requires an instance list, even when empty")
	}
	if strings.TrimSpace(snapshot.Slots.Namespace) == "" {
		return errors.New("slot summary namespace must not be empty")
	}
	if snapshot.Slots.ActiveCount != uint64(len(snapshot.Instances)) {
		return errors.New("active count must equal the full instance list length")
	}
	switch snapshot.Presence {
	case PresenceSleeping:
		if len(snapshot.Instances) != 0 {
			return errors.New("sleeping snapshot must have an empty instance list")
		}
	case PresenceActive:
		if len(snapshot.Instances) == 0 {
			return errors.New("active snapshot must contain instances")
		}
	default:
		return fmt.Errorf("unsupported presence %q", snapshot.Presence)
	}

	instanceIDs := make(map[instancepresence.InstanceID]struct{}, len(snapshot.Instances))
	slots := make(map[Slot]struct{}, len(snapshot.Instances))
	for index, instance := range snapshot.Instances {
		if err := instance.Validate(); err != nil {
			return fmt.Errorf("instance %d: %w", index, err)
		}
		if instance.Slot.Namespace != snapshot.Slots.Namespace {
			return fmt.Errorf("instance %q uses a different slot namespace", instance.InstanceID)
		}
		if _, exists := instanceIDs[instance.InstanceID]; exists {
			return fmt.Errorf("duplicate instance ID %q", instance.InstanceID)
		}
		instanceIDs[instance.InstanceID] = struct{}{}
		if _, exists := slots[instance.Slot]; exists {
			return fmt.Errorf("duplicate slot %s/%d", instance.Slot.Namespace, instance.Slot.Index)
		}
		slots[instance.Slot] = struct{}{}
	}
	return nil
}

type Pixel struct {
	Pixel      uint64                          `json:"pixel"`
	InstanceID instancepresence.InstanceID     `json:"instance_id"`
	State      instancepresence.EffectiveState `json:"state"`
}

type Presentation struct {
	APIVersion          int                           `json:"api_version"`
	Mode                string                        `json:"mode"`
	PixelCapacity       uint64                        `json:"pixel_capacity"`
	Pixels              []Pixel                       `json:"pixels"`
	ActiveCount         uint64                        `json:"active_count"`
	VisibleCount        uint64                        `json:"visible_count"`
	OverflowCount       uint64                        `json:"overflow_count"`
	OverflowInstanceIDs []instancepresence.InstanceID `json:"overflow_instance_ids"`
}

func (presentation Presentation) Validate() error {
	if presentation.APIVersion != APIVersion {
		return fmt.Errorf("unsupported API version %d", presentation.APIVersion)
	}
	if presentation.Mode != "slots" {
		return fmt.Errorf("unsupported presentation mode %q", presentation.Mode)
	}
	if presentation.Pixels == nil || presentation.OverflowInstanceIDs == nil {
		return errors.New("presentation requires pixel and overflow lists, even when empty")
	}
	if presentation.VisibleCount != uint64(len(presentation.Pixels)) || presentation.VisibleCount > presentation.PixelCapacity {
		return errors.New("visible count does not match pixel capacity and pixel list")
	}
	if presentation.OverflowCount != uint64(len(presentation.OverflowInstanceIDs)) {
		return errors.New("overflow count does not match overflow instance list")
	}
	if presentation.ActiveCount != presentation.VisibleCount+presentation.OverflowCount {
		return errors.New("active count must equal visible plus overflow")
	}
	seenPixels := make(map[uint64]struct{}, len(presentation.Pixels))
	seenInstances := make(map[instancepresence.InstanceID]struct{}, presentation.ActiveCount)
	for _, pixel := range presentation.Pixels {
		if pixel.Pixel >= presentation.PixelCapacity {
			return fmt.Errorf("pixel %d exceeds capacity", pixel.Pixel)
		}
		if err := pixel.InstanceID.Validate(); err != nil {
			return err
		}
		if err := pixel.State.Validate(); err != nil {
			return err
		}
		if _, exists := seenPixels[pixel.Pixel]; exists {
			return fmt.Errorf("duplicate pixel %d", pixel.Pixel)
		}
		seenPixels[pixel.Pixel] = struct{}{}
		if _, exists := seenInstances[pixel.InstanceID]; exists {
			return fmt.Errorf("duplicate presentation instance ID %q", pixel.InstanceID)
		}
		seenInstances[pixel.InstanceID] = struct{}{}
	}
	for _, id := range presentation.OverflowInstanceIDs {
		if err := id.Validate(); err != nil {
			return err
		}
		if _, exists := seenInstances[id]; exists {
			return fmt.Errorf("duplicate presentation instance ID %q", id)
		}
		seenInstances[id] = struct{}{}
	}
	return nil
}

type RuntimeMutation struct {
	ProducerEpoch   instancepresence.ProducerEpoch   `json:"producer_epoch"`
	RuntimeRevision instancepresence.RuntimeRevision `json:"runtime_revision"`
	Status          instancepresence.RuntimeStatus   `json:"status"`
	ObservedAt      time.Time                        `json:"observed_at"`
	IdempotencyKey  string                           `json:"idempotency_key"`
}

func (mutation RuntimeMutation) Validate() error {
	if err := mutation.ProducerEpoch.Validate(); err != nil {
		return err
	}
	if err := mutation.Status.Validate(); err != nil {
		return err
	}
	if mutation.RuntimeRevision == 0 {
		return errors.New("runtime revision must be positive")
	}
	if mutation.ObservedAt.IsZero() {
		return errors.New("runtime observation time must not be zero")
	}
	if strings.TrimSpace(mutation.IdempotencyKey) == "" {
		return errors.New("idempotency key must not be empty")
	}
	return nil
}

type HookStateMutation struct {
	ProducerEpoch  instancepresence.ProducerEpoch  `json:"producer_epoch"`
	HookRevision   instancepresence.HookRevision   `json:"hook_revision"`
	State          instancepresence.EffectiveState `json:"state"`
	ObservedAt     time.Time                       `json:"observed_at"`
	IdempotencyKey string                          `json:"idempotency_key"`
}

func (mutation HookStateMutation) Validate() error {
	if err := mutation.ProducerEpoch.Validate(); err != nil {
		return err
	}
	if _, err := instancepresence.ApplyHookState(mutation.State); err != nil {
		return err
	}
	if mutation.HookRevision == 0 {
		return errors.New("hook revision must be positive")
	}
	if mutation.ObservedAt.IsZero() {
		return errors.New("hook observation time must not be zero")
	}
	if strings.TrimSpace(mutation.IdempotencyKey) == "" {
		return errors.New("idempotency key must not be empty")
	}
	return nil
}

type MutationResponse struct {
	InstanceID     instancepresence.InstanceID     `json:"instance_id"`
	EffectiveState instancepresence.EffectiveState `json:"effective_state"`
	Revisions      Revisions                       `json:"revisions"`
}

func (response MutationResponse) Validate() error {
	if err := response.InstanceID.Validate(); err != nil {
		return err
	}
	if err := response.EffectiveState.Validate(); err != nil {
		return err
	}
	if err := response.Revisions.ProducerEpoch.Validate(); err != nil {
		return err
	}
	if response.Revisions.RuntimeRevision == 0 {
		return errors.New("runtime revision must be positive")
	}
	if response.Revisions.HookRevision == 0 && response.EffectiveState != instancepresence.StateIdle {
		return errors.New("non-idle effective state requires a positive hook revision")
	}
	return nil
}

type ErrorCode string

const (
	ErrorInvalidRequest      ErrorCode = "invalid_request"
	ErrorInvalidInstance     ErrorCode = "invalid_instance"
	ErrorInstanceNotFound    ErrorCode = "instance_not_found"
	ErrorRevisionConflict    ErrorCode = "revision_conflict"
	ErrorIdempotencyConflict ErrorCode = "idempotency_conflict"
	ErrorUnauthorized        ErrorCode = "unauthorized"
	ErrorForbidden           ErrorCode = "forbidden"
	ErrorUnsupportedVersion  ErrorCode = "unsupported_version"
	ErrorInternal            ErrorCode = "internal_error"
)

type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (response Error) Validate() error {
	switch response.Code {
	case ErrorInvalidRequest, ErrorInvalidInstance, ErrorInstanceNotFound,
		ErrorRevisionConflict, ErrorIdempotencyConflict, ErrorUnauthorized,
		ErrorForbidden, ErrorUnsupportedVersion, ErrorInternal:
	default:
		return fmt.Errorf("unsupported v2 error code %q", response.Code)
	}
	if strings.TrimSpace(response.Message) == "" {
		return errors.New("error message must not be empty")
	}
	return nil
}
