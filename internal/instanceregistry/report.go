package instanceregistry

import (
	"errors"
	"fmt"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

// ProducerReport is a single normalized report from a producer: everything
// ApplyProducerReport needs to create or update one instance, atomically,
// under one lock. Unlike Registration/RuntimeMutation/HookStateMutation
// (which split a "runtime" track from a "hook" track for the process-
// observation + hook-signal flow), a report carries one unified revision
// and state: a producer-protocol-style producer has no such split, and
// folding identity creation, tool/epoch/revision enforcement, state, and
// lease deadline into a single atomic step means no reader can ever
// observe a partially-applied report — not even the intermediate state
// between "runtime renewed" and "state updated" that two separate calls
// would expose.
type ProducerReport struct {
	InstanceID    instancepresence.InstanceID
	Tool          instancepresence.ToolKind
	Source        instancepresence.SourceDescriptor
	Runtime       instancepresence.RuntimeIdentity
	ProducerEpoch instancepresence.ProducerEpoch
	// Revision is a single producer-owned counter, applied to both
	// RuntimeRevision and HookRevision on the resulting Instance so
	// existing wire projections (presencev2.Revisions) stay meaningful.
	Revision       uint64
	State          instancepresence.EffectiveState
	ObservedAt     time.Time
	LeaseExpiresAt time.Time
}

func (report ProducerReport) validate() error {
	checks := []struct {
		name string
		err  error
	}{
		{"instance ID", report.InstanceID.Validate()},
		{"tool", report.Tool.Validate()},
		{"source", report.Source.Validate()},
		{"runtime", report.Runtime.Validate()},
		{"producer epoch", report.ProducerEpoch.Validate()},
		{"state", report.State.Validate()},
	}
	for _, check := range checks {
		if check.err != nil {
			return fmt.Errorf("%s: %w", check.name, check.err)
		}
	}
	if report.Revision == 0 {
		return errors.New("revision must be positive")
	}
	if report.ObservedAt.IsZero() {
		return errors.New("observed_at must not be zero")
	}
	if report.LeaseExpiresAt.IsZero() {
		return errors.New("lease_expires_at must not be zero")
	}
	if !report.LeaseExpiresAt.After(report.ObservedAt) {
		return errors.New("lease_expires_at must be strictly after observed_at")
	}
	return nil
}

// ReportOutcome classifies what ApplyProducerReport did, so callers (and
// tests) can distinguish the cases without inspecting the returned
// Instance or comparing timestamps.
type ReportOutcome int

const (
	// ReportRegistered means the instance id was unknown and a new
	// instance was created.
	ReportRegistered ReportOutcome = iota
	// ReportApplied means an existing instance was updated: either an
	// ordinary same-epoch revision advance, or a new-generation takeover
	// (see ApplyProducerReport).
	ReportApplied
	// ReportDuplicate means the report exactly repeated the last accepted
	// report for this instance (same epoch, same revision, identical
	// content); nothing was mutated.
	ReportDuplicate
)

// reportPayload is the full content of one accepted report, used both to
// detect an exact duplicate (idempotent no-op) and to detect the same
// revision reused with different content (rejected, never applied).
type reportPayload struct {
	ProducerEpoch  instancepresence.ProducerEpoch
	Revision       uint64
	State          instancepresence.EffectiveState
	ObservedAt     time.Time
	LeaseExpiresAt time.Time
}

// ApplyProducerReport atomically creates or updates the instance identified
// by report.InstanceID. Every enforcement — tool identity, producer epoch,
// revision ordering, state, lease deadline, idempotency — happens under one
// registry lock acquisition, so no reader (CanonicalSnapshot, Presentation,
// ActiveInstances) can ever observe a state this call only partially
// applied.
//
// Three cases:
//
//  1. Unknown instance id: a new instance is created (like Register, but
//     unified: it is immediately in the reported State, not a separate
//     "idle then hook-applied" two-step).
//  2. Known instance id, same stored ProducerEpoch: an ordinary update.
//     Revision must be strictly greater than the last accepted revision;
//     the same revision is idempotent only if the entire report is
//     byte-identical to the last accepted one, otherwise it is rejected;
//     a lower revision is always rejected. A rejected report leaves State,
//     Revisions, Lifecycle (including lease), Slot, Tool, and ProducerEpoch
//     byte-for-byte unchanged.
//  3. Known instance id, different stored ProducerEpoch: a new-generation
//     takeover. Tool must still match (cross-tool takeover is always
//     rejected, regardless of epoch). The instance's Slot is preserved
//     when still free, Revisions and State reset to exactly what this
//     report says, and Status returns to Alive even if the prior
//     generation had reached RuntimeSuspectMissing or RuntimeEnded. This
//     is registry-level mechanism only: whether a given epoch change is a
//     legitimate takeover (the previous generation's connection actually
//     went away) is a broker/connection-layer decision made before this
//     call — this method does not know about connections and always
//     accepts a new epoch for a tool-matching instance.
//
// Callers that must prevent two concurrent "new epoch" attempts from both
// succeeding (only the first should win) need their own upstream
// coordination: this call alone serializes storage, not who is allowed to
// attempt it.
func (registry *Registry) ApplyProducerReport(report ProducerReport) (instancepresence.Instance, ReportOutcome, error) {
	if err := report.validate(); err != nil {
		return instancepresence.Instance{}, 0, fmt.Errorf("apply producer report: %w", err)
	}
	now, err := registry.now()
	if err != nil {
		return instancepresence.Instance{}, 0, err
	}
	if registry.maximumProducerLease <= 0 {
		return instancepresence.Instance{}, 0, errors.New("registry not configured for producer reports: maximum producer lease duration must be positive")
	}
	// Clock skew and report age are both checked against the registry's own
	// now, never against report.ObservedAt itself — a report only ever
	// supplies data about itself, never a trustworthy substitute for the
	// broker's clock.
	if registry.maximumClockSkew > 0 && report.ObservedAt.After(now.Add(registry.maximumClockSkew)) {
		return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrClockSkewTooLarge, "")
	}
	if registry.maximumReportAge > 0 && report.ObservedAt.Before(now.Add(-registry.maximumReportAge)) {
		return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrReportTooOld, "")
	}
	if !report.LeaseExpiresAt.After(now) {
		return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrLeaseAlreadyExpired, "")
	}
	// Anchored to the registry's own now, not to report.ObservedAt: a
	// producer that lies about ObservedAt (setting it far in the future,
	// alongside a matchingly-far-future LeaseExpiresAt) must not be able to
	// make a lease span that is actually far beyond the maximum look small
	// by measuring it against its own claimed observation time instead of
	// real broker time.
	if report.LeaseExpiresAt.After(now.Add(registry.maximumProducerLease)) {
		return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrLeaseExceedsMaximum, "")
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	payload := reportPayload{
		ProducerEpoch: report.ProducerEpoch, Revision: report.Revision, State: report.State,
		ObservedAt: report.ObservedAt, LeaseExpiresAt: report.LeaseExpiresAt,
	}

	record, exists := registry.instances[report.InstanceID]
	if !exists {
		return registry.createFromReport(report, payload, now)
	}
	if record.instance.Tool != report.Tool {
		return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrToolMismatch, "")
	}
	if record.instance.Revisions.ProducerEpoch != report.ProducerEpoch {
		return registry.takeoverFromReport(record, report, payload, now)
	}
	return registry.applySameGenerationReport(record, report, payload, now)
}

func (registry *Registry) createFromReport(report ProducerReport, payload reportPayload, now time.Time) (instancepresence.Instance, ReportOutcome, error) {
	if id, ok := registry.activeRuntimes[keyForRuntime(report.Runtime)]; ok {
		return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrIdentityConflict, fmt.Sprintf("runtime is active as %q", id))
	}
	index, err := instancepresence.LowestAvailableSlot(registry.namespace, registry.occupiedSlots())
	if err != nil {
		return instancepresence.Instance{}, 0, fmt.Errorf("apply producer report slot: %w", err)
	}
	claim, err := instancepresence.ApplyHookState(report.State)
	if err != nil {
		return instancepresence.Instance{}, 0, err
	}
	instance := instancepresence.Instance{
		ID: report.InstanceID, Tool: report.Tool, Source: report.Source, Runtime: report.Runtime,
		Status: instancepresence.RuntimeAlive, HookClaim: claim, State: report.State,
		Slot: instancepresence.Slot{Namespace: registry.namespace, Index: index, AssignedAt: now},
		Lifecycle: instancepresence.LifecycleTimestamps{
			DiscoveredAt: now, LastSeenAt: now, LeaseExpiresAt: report.LeaseExpiresAt, StateChangedAt: now,
		},
		Revisions: instancepresence.Revisions{
			ProducerEpoch:   report.ProducerEpoch,
			RuntimeRevision: instancepresence.RuntimeRevision(report.Revision),
			HookRevision:    instancepresence.HookRevision(report.Revision),
		},
	}
	if err := instance.Validate(); err != nil {
		return instancepresence.Instance{}, 0, fmt.Errorf("apply producer report invariant: %w", err)
	}
	registry.instances[instance.ID] = &record{instance: instance, lastReport: &payload}
	registry.activeRuntimes[keyForRuntime(instance.Runtime)] = instance.ID
	return cloneInstance(instance), ReportRegistered, nil
}

func (registry *Registry) applySameGenerationReport(record *record, report ProducerReport, payload reportPayload, now time.Time) (instancepresence.Instance, ReportOutcome, error) {
	if record.instance.Status == instancepresence.RuntimeEnded {
		// This generation is already retired; only a new epoch (a
		// takeover) may bring this instance id back.
		return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrRuntimeEnded, "")
	}
	if record.lastReport != nil {
		switch {
		case report.Revision < record.lastReport.Revision:
			return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrStaleRevision, "")
		case report.Revision == record.lastReport.Revision:
			if *record.lastReport != payload {
				return instancepresence.Instance{}, 0, domainError("apply producer report", report.InstanceID, ErrRevisionConflict, "")
			}
			return cloneInstance(record.instance), ReportDuplicate, nil
		}
	}
	claim, err := instancepresence.ApplyHookState(report.State)
	if err != nil {
		return instancepresence.Instance{}, 0, err
	}
	previousState := record.instance.State
	record.instance.Status = instancepresence.RuntimeAlive
	record.instance.HookClaim = claim
	record.instance.State = report.State
	record.instance.StartupPending = false
	record.instance.Revisions.RuntimeRevision = instancepresence.RuntimeRevision(report.Revision)
	record.instance.Revisions.HookRevision = instancepresence.HookRevision(report.Revision)
	record.instance.Lifecycle.LastSeenAt = now
	record.instance.Lifecycle.LeaseExpiresAt = report.LeaseExpiresAt
	if record.instance.State != previousState {
		record.instance.Lifecycle.StateChangedAt = now
	}
	if err := record.instance.Validate(); err != nil {
		panic(fmt.Sprintf("instanceregistry report invariant: %v", err))
	}
	record.lastReport = &payload
	return cloneInstance(record.instance), ReportApplied, nil
}

// takeoverFromReport resets an instance to a new producer generation: tool
// identity and (when still free) slot are preserved for continuity, but
// revisions, state, and lease are entirely replaced by report — the prior
// generation's data plays no further part, deliberately: an accepted
// takeover must never let the old epoch influence, block, or partially
// combine with the new one.
func (registry *Registry) takeoverFromReport(record *record, report ProducerReport, payload reportPayload, now time.Time) (instancepresence.Instance, ReportOutcome, error) {
	claim, err := instancepresence.ApplyHookState(report.State)
	if err != nil {
		return instancepresence.Instance{}, 0, err
	}

	slot := record.instance.Slot
	if !record.instance.Status.Active() {
		// The prior generation had already ended; its slot may since have
		// been reassigned to a different active instance. Reuse it only if
		// it is still actually free.
		occupied := registry.occupiedSlotsExcept(record.instance.ID)
		free := true
		for _, other := range occupied {
			if other.Index == slot.Index {
				free = false
				break
			}
		}
		if !free {
			index, slotErr := instancepresence.LowestAvailableSlot(registry.namespace, occupied)
			if slotErr != nil {
				return instancepresence.Instance{}, 0, fmt.Errorf("apply producer report slot: %w", slotErr)
			}
			slot = instancepresence.Slot{Namespace: registry.namespace, Index: index, AssignedAt: now}
		}
	}

	delete(registry.activeRuntimes, keyForRuntime(record.instance.Runtime))
	record.instance.Runtime = report.Runtime
	record.instance.Source = report.Source
	record.instance.Slot = slot
	record.instance.Status = instancepresence.RuntimeAlive
	record.instance.HookClaim = claim
	record.instance.State = report.State
	record.instance.StartupPending = false
	record.instance.Revisions = instancepresence.Revisions{
		ProducerEpoch:   report.ProducerEpoch,
		RuntimeRevision: instancepresence.RuntimeRevision(report.Revision),
		HookRevision:    instancepresence.HookRevision(report.Revision),
	}
	record.instance.Lifecycle.LastSeenAt = now
	record.instance.Lifecycle.LeaseExpiresAt = report.LeaseExpiresAt
	record.instance.Lifecycle.StateChangedAt = now
	record.instance.Lifecycle.EndedAt = nil
	record.instance.Lifecycle.SlotReleasedAt = nil
	if err := record.instance.Validate(); err != nil {
		panic(fmt.Sprintf("instanceregistry report invariant: %v", err))
	}
	registry.activeRuntimes[keyForRuntime(record.instance.Runtime)] = record.instance.ID
	record.lastReport = &payload
	return cloneInstance(record.instance), ReportApplied, nil
}

func (registry *Registry) occupiedSlots() []instancepresence.Slot {
	occupied := make([]instancepresence.Slot, 0, len(registry.instances))
	for _, existing := range registry.instances {
		if existing.instance.Status.Active() {
			occupied = append(occupied, existing.instance.Slot)
		}
	}
	return occupied
}

func (registry *Registry) occupiedSlotsExcept(excludeID instancepresence.InstanceID) []instancepresence.Slot {
	occupied := make([]instancepresence.Slot, 0, len(registry.instances))
	for id, existing := range registry.instances {
		if id == excludeID {
			continue
		}
		if existing.instance.Status.Active() {
			occupied = append(occupied, existing.instance.Slot)
		}
	}
	return occupied
}
