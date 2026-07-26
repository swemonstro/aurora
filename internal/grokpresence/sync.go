package grokpresence

import (
	"context"
	"fmt"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

// ActiveRegistry is the registry surface needed by Grok state synchronization.
type ActiveRegistry interface {
	ActiveInstances() []instancepresence.Instance
	ApplyNextHookMutation(
		instancepresence.InstanceID,
		instancepresence.ProducerEpoch,
		instancepresence.EffectiveState,
		time.Time,
		string,
	) (instancepresence.Instance, error)
}

// StructuralStateReader supplies ordered structural observations from one
// generation-verified Grok process.
type StructuralStateReader interface {
	Observations(
		context.Context,
		instancepresence.ProcessIdentity,
	) ([]EventObservation, error)
}

// SyncResult reports one bounded synchronization pass. A failure for one Grok
// instance does not prevent other instances from being examined.
type SyncResult struct {
	Examined uint64
	Mutated  uint64
	Errors   []error
}

type observationCursor struct {
	idempotencyKey string
	prePrompt      bool
}

// StateSync maps structural Grok events onto existing runtime instances.
type StateSync struct {
	Registry ActiveRegistry
	Reader   StructuralStateReader
	Notify   func()

	baseline map[instancepresence.InstanceID]struct{}
	cursors  map[instancepresence.InstanceID]observationCursor
}

// EstablishBaseline records Grok instances present in the first successful
// runtime sweep. Call it before the synchronization loop starts.
func (sync *StateSync) EstablishBaseline(
	instances []instancepresence.Instance,
) {
	baseline := make(map[instancepresence.InstanceID]struct{})
	for _, instance := range instances {
		if instance.Tool == instancepresence.ToolGrok &&
			instance.Status.Active() {
			baseline[instance.ID] = struct{}{}
		}
	}
	sync.baseline = baseline
}

// Apply performs one bounded synchronization pass.
func (sync *StateSync) Apply(ctx context.Context) SyncResult {
	result := SyncResult{}

	if sync.Registry == nil {
		result.Errors = append(result.Errors, fmt.Errorf("grok registry must not be nil"))
		return result
	}
	if sync.Reader == nil {
		result.Errors = append(result.Errors, fmt.Errorf("grok state reader must not be nil"))
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if sync.cursors == nil {
		sync.cursors = make(map[instancepresence.InstanceID]observationCursor)
	}

	active := make(map[instancepresence.InstanceID]struct{})
	for _, instance := range sync.Registry.ActiveInstances() {
		if instance.Tool != instancepresence.ToolGrok ||
			!instance.Status.Active() {
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Errors = append(result.Errors, err)
			return result
		}

		active[instance.ID] = struct{}{}
		result.Examined++

		observations, err := sync.Reader.Observations(
			ctx,
			instance.Runtime.RootProcess,
		)
		if err != nil {
			result.Errors = append(
				result.Errors,
				fmt.Errorf("observe Grok instance %s: %w", instance.ID, err),
			)
			continue
		}

		for _, observation := range sync.pendingObservations(
			instance.ID,
			observations,
		) {
			if err := ctx.Err(); err != nil {
				result.Errors = append(result.Errors, err)
				return result
			}

			beforeRevision := instance.Revisions.HookRevision
			updated, err := sync.Registry.ApplyNextHookMutation(
				instance.ID,
				instance.Revisions.ProducerEpoch,
				observation.State,
				observation.ObservedAt,
				observation.IdempotencyKey,
			)
			if err != nil {
				result.Errors = append(
					result.Errors,
					fmt.Errorf("update Grok instance %s: %w", instance.ID, err),
				)
				// Preserve event order. Do not apply later observations after
				// an earlier observation failed.
				break
			}

			sync.cursors[instance.ID] = observationCursor{
				idempotencyKey: observation.IdempotencyKey,
			}
			if updated.Revisions.HookRevision > beforeRevision {
				result.Mutated++
				if sync.Notify != nil {
					sync.Notify()
				}
			}
			instance = updated
		}
	}

	for id := range sync.cursors {
		if _, ok := active[id]; !ok {
			delete(sync.cursors, id)
		}
	}
	return result
}

func (sync *StateSync) pendingObservations(
	id instancepresence.InstanceID,
	observations []EventObservation,
) []EventObservation {
	cursor, known := sync.cursors[id]

	if len(observations) == 0 {
		if !known {
			// The process has been observed before its first structural event.
			// Preserve that fact so the complete first turn is not discarded.
			sync.cursors[id] = observationCursor{prePrompt: true}
		}
		return nil
	}

	if !known {
		if sync.baseline != nil {
			if _, existedAtBaseline := sync.baseline[id]; !existedAtBaseline {
				// This runtime appeared after the first successful sweep.
				// Preserve its complete first turn despite delayed registration.
				return observations
			}
		}

		// The runtime existed at startup, or no explicit baseline has been
		// established. Restore only the current safe state.
		return observations[len(observations)-1:]
	}

	if cursor.prePrompt {
		// This process was already known before its first event appeared.
		return observations
	}

	// Search backwards so duplicated structural records cannot cause replay.
	for index := len(observations) - 1; index >= 0; index-- {
		if observations[index].IdempotencyKey == cursor.idempotencyKey {
			return observations[index+1:]
		}
	}

	// The bounded tail rotated or the stream changed. Re-baseline safely.
	return observations[len(observations)-1:]
}
