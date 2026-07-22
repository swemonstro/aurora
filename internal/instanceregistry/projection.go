package instanceregistry

import (
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

func (registry *Registry) CanonicalSnapshot() (presencev2.CanonicalSnapshot, error) {
	now, err := registry.now()
	if err != nil {
		return presencev2.CanonicalSnapshot{}, err
	}
	instances := registry.ActiveInstances()
	wireInstances := make([]presencev2.Instance, len(instances))
	for index, instance := range instances {
		wireInstances[index] = projectInstance(instance)
	}
	presence := presencev2.PresenceActive
	if len(wireInstances) == 0 {
		presence = presencev2.PresenceSleeping
	}
	return presencev2.CanonicalSnapshot{
		APIVersion: presencev2.APIVersion, GeneratedAt: now, Presence: presence,
		Instances: wireInstances,
		Slots:     presencev2.SlotSummary{Namespace: registry.namespace, ActiveCount: uint64(len(wireInstances))},
	}, nil
}

func (registry *Registry) Presentation(pixelCapacity uint64) (presencev2.Presentation, error) {
	instances := registry.ActiveInstances()
	presentation := presencev2.Presentation{
		APIVersion: presencev2.APIVersion, Mode: "slots", PixelCapacity: pixelCapacity,
		Pixels: []presencev2.Pixel{}, ActiveCount: uint64(len(instances)),
		OverflowInstanceIDs: []instancepresence.InstanceID{},
	}
	for _, instance := range instances {
		if instance.Slot.Index < pixelCapacity {
			presentation.Pixels = append(presentation.Pixels, presencev2.Pixel{
				Pixel: instance.Slot.Index, InstanceID: instance.ID, State: instance.State,
			})
		} else {
			presentation.OverflowInstanceIDs = append(presentation.OverflowInstanceIDs, instance.ID)
		}
	}
	presentation.VisibleCount = uint64(len(presentation.Pixels))
	presentation.OverflowCount = uint64(len(presentation.OverflowInstanceIDs))
	return presentation, nil
}

func projectInstance(instance instancepresence.Instance) presencev2.Instance {
	return presencev2.Instance{
		InstanceID: instance.ID, Tool: instance.Tool,
		Source: presencev2.Source{
			Provider: instance.Source.Provider, Profile: instance.Source.Profile, CollectorID: instance.Source.CollectorID,
		},
		State: instance.State,
		Slot:  presencev2.Slot{Namespace: instance.Slot.Namespace, Index: instance.Slot.Index},
		Revisions: presencev2.Revisions{
			ProducerEpoch:   instance.Revisions.ProducerEpoch,
			RuntimeRevision: instance.Revisions.RuntimeRevision,
			HookRevision:    instance.Revisions.HookRevision,
		},
		DiscoveredAt:   instance.Lifecycle.DiscoveredAt,
		StateChangedAt: instance.Lifecycle.StateChangedAt,
		LeaseExpiresAt: instance.Lifecycle.LeaseExpiresAt,
	}
}
