package runtimepresence

import (
	"fmt"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

// RegistrySync keeps canonical v2 instances aligned with secure runtime families.
// Coarse v1 publish remains independent via Agent.
type RegistrySync struct {
	registry      *instanceregistry.Registry
	hostID        string
	producerEpoch instancepresence.ProducerEpoch
	source        instancepresence.SourceDescriptor
	// known maps instance ID → last runtime revision applied by this sync.
	known map[instancepresence.InstanceID]instancepresence.RuntimeRevision
	clock Clock
}

// NewRegistrySync constructs a multi-instance registry synchronizer.
func NewRegistrySync(
	registry *instanceregistry.Registry,
	hostID string,
	epoch instancepresence.ProducerEpoch,
	source instancepresence.SourceDescriptor,
	clock Clock,
) (*RegistrySync, error) {
	if registry == nil {
		return nil, fmt.Errorf("registry must not be nil")
	}
	if hostID == "" {
		return nil, fmt.Errorf("host ID must not be empty")
	}
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}
	return &RegistrySync{
		registry: registry, hostID: hostID, producerEpoch: epoch, source: source,
		known: make(map[instancepresence.InstanceID]instancepresence.RuntimeRevision), clock: clock,
	}, nil
}

// ApplyRecognition registers/renews secure families and ends missing ones.
// Handles 0→1→2→1→0 without mixing instances (identity is host+boot+pid+start).
func (sync *RegistrySync) ApplyRecognition(result runtimerecognition.Result, bootID instancepresence.BootIdentity) error {
	now := sync.clock().UTC()
	seen := make(map[instancepresence.InstanceID]struct{})

	for _, family := range result.Families {
		candidate := family.Candidate
		id := candidate.InstanceID
		if id == "" {
			id = runtimerecognition.StableInstanceID(sync.hostID, bootID, candidate.Tool, candidate.Runtime.RootProcess)
		}
		seen[id] = struct{}{}
		if _, known := sync.known[id]; !known {
			registration := instanceregistry.Registration{
				InstanceID: id, Tool: candidate.Tool, Source: sync.source,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: sync.hostID, BootID: bootID, RootProcess: candidate.Runtime.RootProcess,
				},
				ProducerEpoch: sync.producerEpoch, RuntimeRevision: 1, ObservedAt: now,
				IdempotencyKey: fmt.Sprintf("runtime-register|%s|1", id),
			}
			if _, err := sync.registry.Register(registration); err != nil {
				// Exact retry of identical registration is OK; identity conflicts are hard errors.
				if existing, getErr := sync.registry.Get(id); getErr == nil {
					sync.known[id] = existing.Revisions.RuntimeRevision
					continue
				}
				return fmt.Errorf("register %s: %w", id, err)
			}
			sync.known[id] = 1
			continue
		}
		next := sync.known[id] + 1
		mutation := presencev2.RuntimeMutation{
			ProducerEpoch: sync.producerEpoch, RuntimeRevision: next,
			Status: instancepresence.RuntimeAlive, ObservedAt: now,
			IdempotencyKey: fmt.Sprintf("runtime-lease|%s|%d", id, next),
		}
		if _, err := sync.registry.ApplyRuntimeMutation(id, mutation); err != nil {
			return fmt.Errorf("renew %s: %w", id, err)
		}
		sync.known[id] = next
	}

	for id, revision := range sync.known {
		if _, ok := seen[id]; ok {
			continue
		}
		next := revision + 1
		mutation := presencev2.RuntimeMutation{
			ProducerEpoch: sync.producerEpoch, RuntimeRevision: next,
			Status: instancepresence.RuntimeEnded, ObservedAt: now,
			IdempotencyKey: fmt.Sprintf("runtime-end|%s|%d", id, next),
		}
		if _, err := sync.registry.EndRuntime(id, mutation); err != nil {
			return fmt.Errorf("end %s: %w", id, err)
		}
		delete(sync.known, id)
	}
	return nil
}

// KnownCount reports tracked instances (tests).
func (sync *RegistrySync) KnownCount() int { return len(sync.known) }

// Ensure time import used when clock advances in tests via external.
var _ = time.Second
