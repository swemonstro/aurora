package instanceregistry

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

type Registry struct {
	mu             sync.RWMutex
	clock          instancepresence.Clock
	namespace      string
	leaseDuration  time.Duration
	gracePeriod    time.Duration
	instances      map[instancepresence.InstanceID]*record
	activeRuntimes map[runtimeKey]instancepresence.InstanceID
}

type runtimeKey struct {
	hostID    string
	bootID    instancepresence.BootIdentity
	pid       uint64
	startedAt time.Time
}

type record struct {
	instance           instancepresence.Instance
	registration       Registration
	runtimePayload     runtimePayload
	hookPayload        hookPayload
	runtimeIdempotency map[string]runtimePayload
	hookIdempotency    map[string]hookPayload
}

type runtimePayload struct {
	ProducerEpoch   instancepresence.ProducerEpoch
	RuntimeRevision instancepresence.RuntimeRevision
	Status          instancepresence.RuntimeStatus
	ObservedAt      time.Time
}

type hookPayload struct {
	ProducerEpoch instancepresence.ProducerEpoch
	HookRevision  instancepresence.HookRevision
	State         instancepresence.EffectiveState
	ObservedAt    time.Time
}

func New(config Config) (*Registry, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Registry{
		clock: config.Clock, namespace: config.SlotNamespace,
		leaseDuration: config.LeaseDuration, gracePeriod: config.GracePeriod,
		instances:      make(map[instancepresence.InstanceID]*record),
		activeRuntimes: make(map[runtimeKey]instancepresence.InstanceID),
	}, nil
}

// Register creates an alive idle instance. An exact retry returns the current
// record, even if later mutations have advanced it.
func (registry *Registry) Register(registration Registration) (instancepresence.Instance, error) {
	if err := registration.validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("register: %w", err)
	}
	now, err := registry.now()
	if err != nil {
		return instancepresence.Instance{}, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if existing, ok := registry.instances[registration.InstanceID]; ok {
		if reflect.DeepEqual(existing.registration, registration) {
			return cloneInstance(existing.instance), nil
		}
		return instancepresence.Instance{}, domainError("register", registration.InstanceID, ErrIdentityConflict, "instance ID is already registered with different registration data")
	}
	if id, ok := registry.activeRuntimes[keyForRuntime(registration.Runtime)]; ok {
		return instancepresence.Instance{}, domainError("register", registration.InstanceID, ErrIdentityConflict, fmt.Sprintf("runtime is active as %q", id))
	}

	occupied := make([]instancepresence.Slot, 0, len(registry.activeRuntimes))
	for _, existing := range registry.instances {
		if existing.instance.Status.Active() {
			occupied = append(occupied, existing.instance.Slot)
		}
	}
	index, err := instancepresence.LowestAvailableSlot(registry.namespace, occupied)
	if err != nil {
		return instancepresence.Instance{}, fmt.Errorf("register slot: %w", err)
	}
	status := registration.initialStatus()
	effective, active, err := instancepresence.Effective(status, instancepresence.NoHookClaim, registration.StartupPending)
	if err != nil || !active {
		return instancepresence.Instance{}, fmt.Errorf("register effective state: %w", err)
	}
	instance := instancepresence.Instance{
		ID: registration.InstanceID, Tool: registration.Tool, Source: registration.Source,
		Runtime: registration.Runtime, Status: status,
		HookClaim: instancepresence.NoHookClaim, State: effective,
		Slot: instancepresence.Slot{Namespace: registry.namespace, Index: index, AssignedAt: now},
		Lifecycle: instancepresence.LifecycleTimestamps{
			DiscoveredAt: now, LastSeenAt: now, LeaseExpiresAt: now.Add(registry.leaseDuration), StateChangedAt: now,
		},
		Revisions:      instancepresence.Revisions{ProducerEpoch: registration.ProducerEpoch, RuntimeRevision: registration.RuntimeRevision},
		StartupPending: registration.StartupPending,
	}
	if err := instance.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("register invariant: %w", err)
	}
	payload := runtimePayload{
		ProducerEpoch: registration.ProducerEpoch, RuntimeRevision: registration.RuntimeRevision,
		Status: status, ObservedAt: registration.ObservedAt,
	}
	registry.instances[instance.ID] = &record{
		instance: instance, registration: registration, runtimePayload: payload,
		runtimeIdempotency: map[string]runtimePayload{registration.IdempotencyKey: payload},
		hookIdempotency:    make(map[string]hookPayload),
	}
	registry.activeRuntimes[keyForRuntime(instance.Runtime)] = instance.ID
	return cloneInstance(instance), nil
}

func (registry *Registry) Get(id instancepresence.InstanceID) (instancepresence.Instance, error) {
	if err := id.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("get: %w", err)
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	record, ok := registry.instances[id]
	if !ok {
		return instancepresence.Instance{}, domainError("get", id, ErrNotFound, "")
	}
	return cloneInstance(record.instance), nil
}

// ActiveInstances returns a detached, deterministic canonical domain snapshot.
func (registry *Registry) ActiveInstances() []instancepresence.Instance {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	instances := make([]instancepresence.Instance, 0, len(registry.activeRuntimes))
	for _, record := range registry.instances {
		if record.instance.Status.Active() {
			instances = append(instances, cloneInstance(record.instance))
		}
	}
	sortInstances(instances)
	return instances
}

func (registry *Registry) ApplyRuntimeMutation(id instancepresence.InstanceID, mutation presencev2.RuntimeMutation) (instancepresence.Instance, error) {
	return registry.applyRuntimeMutation(id, mutation, false)
}

// ApplyRuntimeMutationClearingStartup updates runtime status and permanently
// clears StartupPending without touching HookRevision. Used when Codex project
// trust becomes trusted (not a hook event).
func (registry *Registry) ApplyRuntimeMutationClearingStartup(id instancepresence.InstanceID, mutation presencev2.RuntimeMutation) (instancepresence.Instance, error) {
	return registry.applyRuntimeMutation(id, mutation, true)
}

func (registry *Registry) applyRuntimeMutation(id instancepresence.InstanceID, mutation presencev2.RuntimeMutation, clearStartupPending bool) (instancepresence.Instance, error) {
	if err := id.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("runtime mutation instance ID: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("runtime mutation: %w", err)
	}
	now, err := registry.now()
	if err != nil {
		return instancepresence.Instance{}, err
	}
	payload := runtimePayload{mutation.ProducerEpoch, mutation.RuntimeRevision, mutation.Status, mutation.ObservedAt}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.instances[id]
	if !ok {
		return instancepresence.Instance{}, domainError("runtime mutation", id, ErrNotFound, "")
	}
	if err := checkIdempotency("runtime mutation", id, mutation.IdempotencyKey, payload, record.runtimeIdempotency); err != nil {
		return instancepresence.Instance{}, err
	}
	order, err := instancepresence.CompareRuntimeRevision(record.instance.Revisions.ProducerEpoch, record.instance.Revisions.RuntimeRevision, mutation.ProducerEpoch, mutation.RuntimeRevision)
	if err != nil {
		return instancepresence.Instance{}, domainError("runtime mutation", id, ErrEpochConflict, err.Error())
	}
	switch order {
	case instancepresence.RevisionOlder:
		return instancepresence.Instance{}, domainError("runtime mutation", id, ErrStaleRevision, "runtime revision is older than current")
	case instancepresence.RevisionSame:
		if !reflect.DeepEqual(payload, record.runtimePayload) {
			return instancepresence.Instance{}, domainError("runtime mutation", id, ErrRevisionConflict, "runtime revision was reused with a different payload")
		}
		record.runtimeIdempotency[mutation.IdempotencyKey] = payload
		return cloneInstance(record.instance), nil
	}
	if record.instance.Status == instancepresence.RuntimeEnded {
		return instancepresence.Instance{}, domainError("runtime mutation", id, ErrRuntimeEnded, "ordinary mutation cannot reactivate an ended runtime")
	}

	record.runtimeIdempotency[mutation.IdempotencyKey] = payload
	record.runtimePayload = payload
	record.instance.Revisions.RuntimeRevision = mutation.RuntimeRevision
	if mutation.Status == instancepresence.RuntimeEnded {
		registry.endLocked(record, now)
	} else {
		previousState := record.instance.State
		// Preserve HookClaim across alive/suspended transitions; recompute State.
		// StartupPending is never re-activated once cleared for a generation.
		record.instance.Status = mutation.Status
		if clearStartupPending {
			record.instance.StartupPending = false
		}
		record.instance.Lifecycle.LastSeenAt = now
		record.instance.Lifecycle.LeaseExpiresAt = now.Add(registry.leaseDuration)
		effective, active, err := instancepresence.Effective(
			record.instance.Status, record.instance.HookClaim, record.instance.StartupPending,
		)
		if err != nil {
			return instancepresence.Instance{}, err
		}
		if !active {
			return instancepresence.Instance{}, domainError("runtime mutation", id, ErrRuntimeEnded, "runtime status is not active")
		}
		record.instance.State = effective
		if record.instance.State != previousState {
			record.instance.Lifecycle.StateChangedAt = now
		}
	}
	if err := record.instance.Validate(); err != nil {
		panic(fmt.Sprintf("instanceregistry runtime invariant: %v", err))
	}
	return cloneInstance(record.instance), nil
}

func (registry *Registry) EndRuntime(id instancepresence.InstanceID, mutation presencev2.RuntimeMutation) (instancepresence.Instance, error) {
	if mutation.Status != instancepresence.RuntimeEnded {
		return instancepresence.Instance{}, fmt.Errorf("end runtime requires status %q", instancepresence.RuntimeEnded)
	}
	return registry.ApplyRuntimeMutation(id, mutation)
}

// ApplyHookMutation applies an explicit producer-owned HookRevision from a
// presence v2 HookStateMutation. Callers that need local runtime-owned
// sequencing (Package 6 binding) should use ApplyNextHookMutation instead.
func (registry *Registry) ApplyHookMutation(id instancepresence.InstanceID, mutation presencev2.HookStateMutation) (instancepresence.Instance, error) {
	if err := id.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("hook mutation instance ID: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("hook mutation: %w", err)
	}
	now, err := registry.now()
	if err != nil {
		return instancepresence.Instance{}, err
	}
	payload := hookPayload{mutation.ProducerEpoch, mutation.HookRevision, mutation.State, mutation.ObservedAt}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.instances[id]
	if !ok {
		return instancepresence.Instance{}, domainError("hook mutation", id, ErrNotFound, "")
	}
	if err := checkIdempotency("hook mutation", id, mutation.IdempotencyKey, payload, record.hookIdempotency); err != nil {
		return instancepresence.Instance{}, err
	}
	order, err := instancepresence.CompareHookRevision(record.instance.Revisions.ProducerEpoch, record.instance.Revisions.HookRevision, mutation.ProducerEpoch, mutation.HookRevision)
	if err != nil {
		return instancepresence.Instance{}, domainError("hook mutation", id, ErrEpochConflict, err.Error())
	}
	switch order {
	case instancepresence.RevisionOlder:
		return instancepresence.Instance{}, domainError("hook mutation", id, ErrStaleRevision, "hook revision is older than current")
	case instancepresence.RevisionSame:
		if !reflect.DeepEqual(payload, record.hookPayload) {
			return instancepresence.Instance{}, domainError("hook mutation", id, ErrRevisionConflict, "hook revision was reused with a different payload")
		}
		record.hookIdempotency[mutation.IdempotencyKey] = payload
		return cloneInstance(record.instance), nil
	}
	if record.instance.Status == instancepresence.RuntimeEnded {
		return instancepresence.Instance{}, domainError("hook mutation", id, ErrRuntimeEnded, "hook mutation cannot reactivate an ended runtime")
	}

	return registry.applyHookLocked(record, mutation.ProducerEpoch, mutation.HookRevision, mutation.State, mutation.ObservedAt, mutation.IdempotencyKey, now)
}

// ApplyNextHookMutation assigns the next HookRevision for a runtime instance
// under the registry lock. HookRevision is owned and sequenced per InstanceID
// by the registry; ingress/session stream revisions must not be supplied here.
//
// Idempotency: a repeated key with the same logical payload (epoch, state,
// observedAt) returns the current instance without advancing revision. A key
// reused with a different logical payload is ErrIdempotencyConflict.
func (registry *Registry) ApplyNextHookMutation(
	id instancepresence.InstanceID,
	producerEpoch instancepresence.ProducerEpoch,
	state instancepresence.EffectiveState,
	observedAt time.Time,
	idempotencyKey string,
) (instancepresence.Instance, error) {
	if err := id.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("next hook mutation instance ID: %w", err)
	}
	if err := producerEpoch.Validate(); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("next hook mutation producer epoch: %w", err)
	}
	if _, err := instancepresence.ApplyHookState(state); err != nil {
		return instancepresence.Instance{}, fmt.Errorf("next hook mutation state: %w", err)
	}
	if observedAt.IsZero() {
		return instancepresence.Instance{}, fmt.Errorf("next hook mutation observation time must not be zero")
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return instancepresence.Instance{}, fmt.Errorf("next hook mutation idempotency key must not be empty")
	}
	now, err := registry.now()
	if err != nil {
		return instancepresence.Instance{}, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, ok := registry.instances[id]
	if !ok {
		return instancepresence.Instance{}, domainError("next hook mutation", id, ErrNotFound, "")
	}
	if previous, seen := record.hookIdempotency[idempotencyKey]; seen {
		if previous.ProducerEpoch == producerEpoch && previous.State == state && previous.ObservedAt.Equal(observedAt) {
			return cloneInstance(record.instance), nil
		}
		return instancepresence.Instance{}, domainError("next hook mutation", id, ErrIdempotencyConflict, "idempotency key was reused with a different payload")
	}
	if producerEpoch != record.instance.Revisions.ProducerEpoch {
		return instancepresence.Instance{}, domainError("next hook mutation", id, ErrEpochConflict, "producer epochs are not directly comparable")
	}
	if record.instance.Status == instancepresence.RuntimeEnded {
		return instancepresence.Instance{}, domainError("next hook mutation", id, ErrRuntimeEnded, "hook mutation cannot reactivate an ended runtime")
	}

	next := record.instance.Revisions.HookRevision + 1
	return registry.applyHookLocked(record, producerEpoch, next, state, observedAt, idempotencyKey, now)
}

func (registry *Registry) applyHookLocked(
	record *record,
	producerEpoch instancepresence.ProducerEpoch,
	hookRevision instancepresence.HookRevision,
	state instancepresence.EffectiveState,
	observedAt time.Time,
	idempotencyKey string,
	now time.Time,
) (instancepresence.Instance, error) {
	claim, err := instancepresence.ApplyHookState(state)
	if err != nil {
		return instancepresence.Instance{}, err
	}
	// Persist the wire request state in the hook payload for idempotency, but
	// derive Instance.State from RuntimeStatus + HookClaim so suspended
	// processes present as attention while claims are preserved.
	// Any accepted hook ends Claude startup-pending (SessionStart, prompt, …).
	payload := hookPayload{producerEpoch, hookRevision, state, observedAt}
	previousState := record.instance.State
	record.hookIdempotency[idempotencyKey] = payload
	record.hookPayload = payload
	record.instance.Revisions.HookRevision = hookRevision
	record.instance.HookClaim = claim
	record.instance.StartupPending = false
	effective, active, err := instancepresence.Effective(record.instance.Status, claim, false)
	if err != nil {
		return instancepresence.Instance{}, err
	}
	if !active {
		return instancepresence.Instance{}, domainError("hook mutation", record.instance.ID, ErrRuntimeEnded, "runtime is not active")
	}
	record.instance.State = effective
	if record.instance.State != previousState {
		record.instance.Lifecycle.StateChangedAt = now
	}
	if err := record.instance.Validate(); err != nil {
		panic(fmt.Sprintf("instanceregistry hook invariant: %v", err))
	}
	return cloneInstance(record.instance), nil
}

func checkIdempotency[T comparable](op string, id instancepresence.InstanceID, key string, payload T, seen map[string]T) error {
	if previous, ok := seen[key]; ok && previous != payload {
		return domainError(op, id, ErrIdempotencyConflict, "idempotency key was reused with a different payload")
	}
	return nil
}

func (registry *Registry) endLocked(record *record, now time.Time) {
	record.instance.Status = instancepresence.RuntimeEnded
	record.instance.HookClaim = instancepresence.NoHookClaim
	record.instance.StartupPending = false
	record.instance.State = ""
	record.instance.Lifecycle.LastSeenAt = now
	record.instance.Lifecycle.LeaseExpiresAt = now
	record.instance.Lifecycle.EndedAt = timePointer(now)
	record.instance.Lifecycle.SlotReleasedAt = timePointer(now)
	delete(registry.activeRuntimes, keyForRuntime(record.instance.Runtime))
}

func (registry *Registry) now() (time.Time, error) {
	now := registry.clock.Now()
	if now.IsZero() {
		return time.Time{}, fmt.Errorf("registry clock returned zero time")
	}
	return now, nil
}

func timePointer(value time.Time) *time.Time { return &value }

func keyForRuntime(runtime instancepresence.RuntimeIdentity) runtimeKey {
	return runtimeKey{
		hostID: runtime.HostID, bootID: runtime.BootID, pid: runtime.RootProcess.PID,
		startedAt: runtime.RootProcess.StartedAt.Round(0).UTC(),
	}
}

func cloneInstance(instance instancepresence.Instance) instancepresence.Instance {
	if instance.Lifecycle.EndedAt != nil {
		instance.Lifecycle.EndedAt = timePointer(*instance.Lifecycle.EndedAt)
	}
	if instance.Lifecycle.SlotReleasedAt != nil {
		instance.Lifecycle.SlotReleasedAt = timePointer(*instance.Lifecycle.SlotReleasedAt)
	}
	return instance
}

func sortInstances(instances []instancepresence.Instance) {
	sort.Slice(instances, func(first, second int) bool {
		if instances[first].Slot.Namespace != instances[second].Slot.Namespace {
			return instances[first].Slot.Namespace < instances[second].Slot.Namespace
		}
		if instances[first].Slot.Index != instances[second].Slot.Index {
			return instances[first].Slot.Index < instances[second].Slot.Index
		}
		return instances[first].ID < instances[second].ID
	})
}
