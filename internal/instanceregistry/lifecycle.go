package instanceregistry

import (
	"fmt"
	"sort"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

// ExpireLeases applies all transitions due at Clock.Now. The boundary is
// inclusive: a lease is expired when now is equal to LeaseExpiresAt.
func (registry *Registry) ExpireLeases() (ExpiryResult, error) {
	now, err := registry.now()
	if err != nil {
		return ExpiryResult{}, err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()

	type transition struct {
		id   instancepresence.InstanceID
		slot uint64
	}
	var suspect, ended []transition
	for id, record := range registry.instances {
		instance := &record.instance
		switch instance.Status {
		case instancepresence.RuntimeAlive, instancepresence.RuntimeSuspended:
			if !now.Before(instance.Lifecycle.LeaseExpiresAt) {
				instance.Status = instancepresence.RuntimeSuspectMissing
				// Recompute effective state after lease-driven status change.
				if effective, active, err := instancepresence.Effective(instance.Status, instance.HookClaim); err == nil && active {
					if instance.State != effective {
						instance.State = effective
						instance.Lifecycle.StateChangedAt = now
					}
				}
				suspect = append(suspect, transition{id, instance.Slot.Index})
			}
		case instancepresence.RuntimeSuspectMissing:
			endAt := instance.Lifecycle.LeaseExpiresAt.Add(registry.gracePeriod)
			if !now.Before(endAt) {
				// Expiry is server-owned lifecycle state and does not invent a
				// producer revision. The last accepted producer payload remains.
				instance.Status = instancepresence.RuntimeEnded
				instance.HookClaim = instancepresence.NoHookClaim
				instance.State = ""
				instance.Lifecycle.EndedAt = timePointer(now)
				instance.Lifecycle.SlotReleasedAt = timePointer(now)
				delete(registry.activeRuntimes, keyForRuntime(instance.Runtime))
				ended = append(ended, transition{id, instance.Slot.Index})
			}
		}
		if err := instance.Validate(); err != nil {
			panic(fmt.Sprintf("instanceregistry expiry invariant: %v", err))
		}
	}
	sortTransitions := func(values []transition) []instancepresence.InstanceID {
		sort.Slice(values, func(first, second int) bool {
			if values[first].slot != values[second].slot {
				return values[first].slot < values[second].slot
			}
			return values[first].id < values[second].id
		})
		ids := make([]instancepresence.InstanceID, len(values))
		for index := range values {
			ids[index] = values[index].id
		}
		return ids
	}
	return ExpiryResult{SuspectMissing: sortTransitions(suspect), Ended: sortTransitions(ended)}, nil
}
