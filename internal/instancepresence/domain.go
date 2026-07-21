// Package instancepresence defines the platform-neutral per-instance presence
// domain. It contains no process discovery, persistence, transport, or OS code.
package instancepresence

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type InstanceID string

func (id InstanceID) Validate() error {
	if strings.TrimSpace(string(id)) == "" {
		return errors.New("instance ID must not be empty")
	}
	return nil
}

type ToolKind string

const (
	ToolClaude ToolKind = "claude"
	ToolCodex  ToolKind = "codex"
)

func (kind ToolKind) Validate() error {
	switch kind {
	case ToolClaude, ToolCodex:
		return nil
	default:
		return fmt.Errorf("unsupported tool kind %q", kind)
	}
}

// SourceDescriptor describes integration and configuration provenance. It is
// metadata and must never be used as instance or runtime identity.
type SourceDescriptor struct {
	Provider    string
	Profile     string
	CollectorID string
}

func (source SourceDescriptor) Validate() error {
	if strings.TrimSpace(source.Provider) == "" {
		return errors.New("source provider must not be empty")
	}
	if strings.TrimSpace(source.Profile) == "" {
		return errors.New("source profile must not be empty")
	}
	if strings.TrimSpace(source.CollectorID) == "" {
		return errors.New("source collector ID must not be empty")
	}
	return nil
}

// ProcessIdentity is generation-safe only when PID and StartedAt are both set.
type ProcessIdentity struct {
	PID       uint64    `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func (process ProcessIdentity) Validate() error {
	if process.PID == 0 {
		return errors.New("process PID must be positive")
	}
	if process.StartedAt.IsZero() {
		return errors.New("process start time must not be zero")
	}
	return nil
}

type RuntimeIdentity struct {
	HostID      string
	BootID      BootIdentity
	RootProcess ProcessIdentity
}

func (runtime RuntimeIdentity) Validate() error {
	if strings.TrimSpace(runtime.HostID) == "" {
		return errors.New("runtime host ID must not be empty")
	}
	if err := runtime.BootID.Validate(); err != nil {
		return err
	}
	if err := runtime.RootProcess.Validate(); err != nil {
		return fmt.Errorf("runtime root process: %w", err)
	}
	return nil
}

type RuntimeStatus string

const (
	RuntimeAlive          RuntimeStatus = "alive"
	RuntimeSuspectMissing RuntimeStatus = "suspect_missing"
	RuntimeEnded          RuntimeStatus = "ended"
)

func (status RuntimeStatus) Validate() error {
	switch status {
	case RuntimeAlive, RuntimeSuspectMissing, RuntimeEnded:
		return nil
	default:
		return fmt.Errorf("unsupported runtime status %q", status)
	}
}

// Active reports whether the runtime still belongs in the active canonical
// snapshot. Suspect-missing remains active until its grace period ends.
func (status RuntimeStatus) Active() bool {
	return status == RuntimeAlive || status == RuntimeSuspectMissing
}

type EffectiveState string

const (
	StateIdle      EffectiveState = "idle"
	StateWorking   EffectiveState = "working"
	StateAttention EffectiveState = "attention"
	StateError     EffectiveState = "error"
)

func (state EffectiveState) Validate() error {
	switch state {
	case StateIdle, StateWorking, StateAttention, StateError:
		return nil
	default:
		return fmt.Errorf("unsupported instance state %q", state)
	}
}

// HookClaim is empty when the hook layer has no claim. Idle is deliberately not
// a claim: an idle hook event clears the claim and reveals the process base.
type HookClaim string

const (
	NoHookClaim    HookClaim = ""
	ClaimWorking   HookClaim = "working"
	ClaimAttention HookClaim = "attention"
	ClaimError     HookClaim = "error"
)

func (claim HookClaim) Validate() error {
	switch claim {
	case NoHookClaim, ClaimWorking, ClaimAttention, ClaimError:
		return nil
	default:
		return fmt.Errorf("unsupported hook claim %q", claim)
	}
}

// ApplyHookState maps the wire-level idle convenience to claim removal.
func ApplyHookState(state EffectiveState) (HookClaim, error) {
	if err := state.Validate(); err != nil {
		return NoHookClaim, err
	}
	switch state {
	case StateIdle:
		return NoHookClaim, nil
	case StateWorking:
		return ClaimWorking, nil
	case StateAttention:
		return ClaimAttention, nil
	case StateError:
		return ClaimError, nil
	default:
		panic("validated state was not handled")
	}
}

// Effective derives status from the process-owned idle base and optional hook
// claim. The bool is false when the runtime is no longer active.
func Effective(runtime RuntimeStatus, claim HookClaim) (EffectiveState, bool, error) {
	if err := runtime.Validate(); err != nil {
		return "", false, err
	}
	if err := claim.Validate(); err != nil {
		return "", false, err
	}
	if !runtime.Active() {
		return "", false, nil
	}
	if claim == NoHookClaim {
		return StateIdle, true, nil
	}
	return EffectiveState(claim), true, nil
}

type Slot struct {
	Namespace  string
	Index      uint64
	AssignedAt time.Time
}

func (slot Slot) Validate() error {
	if strings.TrimSpace(slot.Namespace) == "" {
		return errors.New("slot namespace must not be empty")
	}
	if slot.AssignedAt.IsZero() {
		return errors.New("slot assignment time must not be zero")
	}
	return nil
}

type LifecycleTimestamps struct {
	DiscoveredAt   time.Time
	LastSeenAt     time.Time
	LeaseExpiresAt time.Time
	StateChangedAt time.Time
	EndedAt        *time.Time
	SlotReleasedAt *time.Time
}

func (lifecycle LifecycleTimestamps) Validate(status RuntimeStatus) error {
	if lifecycle.DiscoveredAt.IsZero() {
		return errors.New("lifecycle discovered time must not be zero")
	}
	if lifecycle.LastSeenAt.IsZero() {
		return errors.New("lifecycle last-seen time must not be zero")
	}
	if lifecycle.LeaseExpiresAt.IsZero() {
		return errors.New("lifecycle lease expiry must not be zero")
	}
	if lifecycle.StateChangedAt.IsZero() {
		return errors.New("lifecycle state-changed time must not be zero")
	}
	if lifecycle.LastSeenAt.Before(lifecycle.DiscoveredAt) {
		return errors.New("lifecycle last-seen time precedes discovery")
	}
	if lifecycle.StateChangedAt.Before(lifecycle.DiscoveredAt) {
		return errors.New("lifecycle state-changed time precedes discovery")
	}
	if lifecycle.LeaseExpiresAt.Before(lifecycle.LastSeenAt) {
		return errors.New("lifecycle lease expiry precedes last-seen time")
	}
	if status == RuntimeEnded && lifecycle.EndedAt == nil {
		return errors.New("ended runtime requires ended time")
	}
	if status != RuntimeEnded && lifecycle.EndedAt != nil {
		return errors.New("active runtime must not have ended time")
	}
	if lifecycle.EndedAt != nil {
		if lifecycle.EndedAt.Before(lifecycle.DiscoveredAt) {
			return errors.New("lifecycle ended time precedes discovery")
		}
		if lifecycle.EndedAt.Before(lifecycle.LastSeenAt) {
			return errors.New("lifecycle ended time precedes last-seen time")
		}
	}
	if lifecycle.SlotReleasedAt != nil {
		if lifecycle.EndedAt == nil {
			return errors.New("slot release requires ended time")
		}
		if lifecycle.SlotReleasedAt.Before(*lifecycle.EndedAt) {
			return errors.New("slot release precedes ended time")
		}
	}
	return nil
}

type RuntimeRevision uint64
type HookRevision uint64
type ProducerEpoch string

func (epoch ProducerEpoch) Validate() error {
	if strings.TrimSpace(string(epoch)) == "" {
		return errors.New("producer epoch must not be empty")
	}
	return nil
}

type Revisions struct {
	ProducerEpoch   ProducerEpoch
	RuntimeRevision RuntimeRevision
	HookRevision    HookRevision
}

type Instance struct {
	ID        InstanceID
	Tool      ToolKind
	Source    SourceDescriptor
	Runtime   RuntimeIdentity
	Status    RuntimeStatus
	HookClaim HookClaim
	State     EffectiveState
	Slot      Slot
	Lifecycle LifecycleTimestamps
	Revisions Revisions
}

func (instance Instance) Validate() error {
	checks := []struct {
		name string
		err  error
	}{
		{"instance ID", instance.ID.Validate()},
		{"tool", instance.Tool.Validate()},
		{"source", instance.Source.Validate()},
		{"runtime", instance.Runtime.Validate()},
		{"runtime status", instance.Status.Validate()},
		{"hook claim", instance.HookClaim.Validate()},
		{"slot", instance.Slot.Validate()},
		{"producer epoch", instance.Revisions.ProducerEpoch.Validate()},
		{"lifecycle", instance.Lifecycle.Validate(instance.Status)},
	}
	for _, check := range checks {
		if check.err != nil {
			return fmt.Errorf("%s: %w", check.name, check.err)
		}
	}
	if instance.Revisions.RuntimeRevision == 0 {
		return errors.New("runtime revision must be positive")
	}
	if instance.Revisions.HookRevision == 0 && instance.HookClaim != NoHookClaim {
		return errors.New("hook claim requires a positive hook revision")
	}

	effective, active, err := Effective(instance.Status, instance.HookClaim)
	if err != nil {
		return err
	}
	if active {
		if instance.State != effective {
			return fmt.Errorf("effective state %q does not match derived state %q", instance.State, effective)
		}
	} else if instance.State != "" {
		return errors.New("ended runtime must not have an instance state")
	}
	if !active && instance.HookClaim != NoHookClaim {
		return errors.New("ended runtime must not retain a hook claim")
	}
	return nil
}
