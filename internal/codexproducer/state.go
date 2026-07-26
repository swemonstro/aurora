package codexproducer

import (
	"errors"
	"sort"
	"sync"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/producerprotocol"
	"github.com/swemonstro/aurora/internal/status"
)

// ErrUnsupportedHookState is returned by MapHookState for any status.State
// value outside the closed set codexhook.MapEvent ever produces.
var ErrUnsupportedHookState = errors.New("codexproducer: unsupported hook state")

// MapHookState converts codexhook's status.State (the type
// codexhook.EventAction.State already carries) to producerprotocol.State.
// The two enums share identical wire values by construction; this function
// exists so no package ever relies on an unchecked string cast between them.
func MapHookState(state status.State) (producerprotocol.State, error) {
	switch state {
	case status.Idle:
		return producerprotocol.StateIdle, nil
	case status.Working:
		return producerprotocol.StateWorking, nil
	case status.Attention:
		return producerprotocol.StateAttention, nil
	case status.Error:
		return producerprotocol.StateError, nil
	default:
		return "", ErrUnsupportedHookState
	}
}

// trackedInstance is this producer's in-memory record for one observed Codex
// instance. It is never persisted: on producer restart every instance is
// re-derived from a fresh process scan (idle at minimum) and hook events
// observed from that point on, under a new ProducerEpoch.
type trackedInstance struct {
	source   SourceLabel
	state    producerprotocol.State
	revision uint64
}

// TrackedInstance is a read-only snapshot of one instance's current state,
// used for periodic full-state reconciliation reporting.
type TrackedInstance struct {
	InstanceID producerprotocol.InstanceID
	Source     SourceLabel
	State      producerprotocol.State
	Revision   uint64
}

// Machine is the Codex-only idle/working/attention state machine. It derives
// state exclusively from two inputs: process recognition (an instance
// exists, defaulting to idle) and observed Codex hook events (SessionStart,
// UserPromptSubmit, PreToolUse, PermissionRequest, PostToolUse, Stop, mapped
// via codexhook.MapEvent). It never consults trust configuration, cwd,
// project metadata, or any other config source — see doc.go for why.
//
// Esc/cancel-after-PermissionRequest is modeled purely via the Stop hook
// event: codexhook.MapEvent already maps Stop to idle unconditionally,
// regardless of the instance's current state, so an aborted turn that Codex
// reports via Stop returns the correct single instance to idle. This package
// deliberately does not read transcript_path or scan the transcript for a
// turn_aborted marker (internal/codexhook.ScanTranscript / PermissionWatch):
// docs/architecture/adapters/codex.md:60-63 documents that transcript-path
// use is legacy v1 permission-recovery logic and "may not be reused by
// Package 6's observation or correlation" — this producer is squarely in
// that observe-only lineage. If a real Codex build ever aborts a turn
// without emitting any further hook event at all, that specific case has no
// currently observed signal and is an explicit, documented G.4 gap (see the
// final report's risk section) rather than a new heuristic.
type Machine struct {
	mu        sync.Mutex
	instances map[producerprotocol.InstanceID]*trackedInstance
}

// NewMachine constructs an empty state machine.
func NewMachine() *Machine {
	return &Machine{instances: make(map[producerprotocol.InstanceID]*trackedInstance)}
}

// Discover ensures id is tracked, defaulting to idle at revision 1 if this is
// the first time this instance has ever been seen (by process recognition,
// with no hook yet delivered). It is a no-op — never resetting state or
// revision — if id is already tracked, so a recognition pass that runs after
// a hook has already reported working/attention can never regress it back to
// idle.
func (machine *Machine) Discover(id producerprotocol.InstanceID, source SourceLabel) (state producerprotocol.State, revision uint64, isNew bool) {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	if existing, tracked := machine.instances[id]; tracked {
		return existing.state, existing.revision, false
	}
	machine.instances[id] = &trackedInstance{source: source, state: producerprotocol.StateIdle, revision: 1}
	return producerprotocol.StateIdle, 1, true
}

// ApplyHookEvent applies one already-mapped Codex hook state to id, creating
// the instance at that exact state (revision 1) if this is the first time
// anything has been observed about it (a hook can arrive before recognition
// catches up with the underlying process — see the package doc's ordering
// notes), or transitioning an already-tracked instance and bumping its
// revision only when the state actually changes. changed reports whether a
// new message must be sent to the broker.
func (machine *Machine) ApplyHookEvent(id producerprotocol.InstanceID, source SourceLabel, mapped producerprotocol.State) (state producerprotocol.State, revision uint64, changed bool) {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	existing, tracked := machine.instances[id]
	if !tracked {
		machine.instances[id] = &trackedInstance{source: source, state: mapped, revision: 1}
		return mapped, 1, true
	}
	if existing.state == mapped {
		return existing.state, existing.revision, false
	}
	existing.state = mapped
	existing.revision++
	return existing.state, existing.revision, true
}

// Renew bumps id's revision without changing its state, for a periodic
// full-state reconciliation heartbeat (a lease renewal): the broker's
// same-revision idempotency check requires a strictly higher revision
// whenever observed_at/lease_expires_at change, even if State does not (see
// internal/instanceregistry's applySameGenerationReport). ok is false if id
// is no longer tracked (already forgotten).
func (machine *Machine) Renew(id producerprotocol.InstanceID) (state producerprotocol.State, revision uint64, ok bool) {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	existing, tracked := machine.instances[id]
	if !tracked {
		return "", 0, false
	}
	existing.revision++
	return existing.state, existing.revision, true
}

// Forget stops tracking id, e.g. because its process disappeared. No message
// is ever sent for this: producerprotocol has no removal message, and the
// broker's own lease expiry (this producer simply stops renewing) is the
// sole mechanism that ends a runtime that goes away. See
// cmd/aurora-presence-broker's -grace-period/-expiry-interval.
func (machine *Machine) Forget(id producerprotocol.InstanceID) {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	delete(machine.instances, id)
}

// Tracked reports whether id is currently tracked, and if so its source.
func (machine *Machine) Tracked(id producerprotocol.InstanceID) (SourceLabel, bool) {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	existing, ok := machine.instances[id]
	if !ok {
		return "", false
	}
	return existing.source, true
}

// Snapshot returns every currently tracked instance, sorted by InstanceID for
// deterministic iteration (tests, and stable send ordering).
func (machine *Machine) Snapshot() []TrackedInstance {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	out := make([]TrackedInstance, 0, len(machine.instances))
	for id, instance := range machine.instances {
		out = append(out, TrackedInstance{InstanceID: id, Source: instance.source, State: instance.state, Revision: instance.revision})
	}
	sort.Slice(out, func(first, second int) bool { return out[first].InstanceID < out[second].InstanceID })
	return out
}

// MapEvent re-exports codexhook.MapEvent so callers driving this machine
// from raw Codex hook events need only import this package, keeping event
// mapping single-sourced (no divergent second Codex hook state machine).
func MapEvent(event codexhook.Event) (codexhook.EventAction, bool) { return codexhook.MapEvent(event) }
