// Package sessionbinding implements a server-owned, thread-safe map from
// (tool, hook session) to a verified runtime InstanceID.
//
// New bindings use Reserve → Commit so registry mutation can fail without
// publishing a session binding. Parallel sessions cannot reserve the same runtime.
package sessionbinding

import (
	"errors"
	"fmt"
	"sync"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

var (
	// ErrNotBound indicates the session has no committed binding.
	ErrNotBound = errors.New("session is not bound")
	// ErrConflict indicates a session or runtime identity conflict.
	ErrConflict = errors.New("session binding conflict")
	// ErrNoUniqueLink indicates binding creation requires a unique runtime link.
	ErrNoUniqueLink = errors.New("no unique runtime link")
	// ErrNotReserved indicates Commit/Rollback without an active reservation.
	ErrNotReserved = errors.New("session binding is not reserved")
	// ErrAlreadyBound indicates the session already has a committed binding.
	ErrAlreadyBound = errors.New("session is already bound")
)

// Key identifies one agent hook session stream.
type Key struct {
	Tool    instancepresence.ToolKind
	Session instancepresence.OpaqueIdentity
}

type bindingPhase uint8

const (
	phaseReserved bindingPhase = iota + 1
	phaseCommitted
)

type entry struct {
	runtime instancepresence.InstanceID
	phase   bindingPhase
}

// Registry stores exclusive tool+session → RuntimeRef bindings.
// Two active sessions cannot share the same RuntimeRef (fail-closed),
// including while a reservation is outstanding.
type Registry struct {
	mu       sync.Mutex
	sessions map[Key]entry
	runtimes map[instancepresence.InstanceID]Key
}

// New constructs an empty binding registry.
func New() *Registry {
	return &Registry{
		sessions: make(map[Key]entry),
		runtimes: make(map[instancepresence.InstanceID]Key),
	}
}

// Lookup returns the RuntimeRef for a committed binding only.
// Reservations are invisible to observers.
func (registry *Registry) Lookup(tool instancepresence.ToolKind, session instancepresence.OpaqueIdentity) (instancepresence.InstanceID, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	value, ok := registry.sessions[Key{Tool: tool, Session: session}]
	if !ok || value.phase != phaseCommitted {
		return "", false
	}
	return value.runtime, true
}

// Reserve exclusively claims runtime for a new session until Commit or Rollback.
// The session is not considered bound until Commit.
func (registry *Registry) Reserve(
	tool instancepresence.ToolKind,
	session instancepresence.OpaqueIdentity,
	runtime instancepresence.InstanceID,
) error {
	if err := tool.Validate(); err != nil {
		return fmt.Errorf("session binding tool: %w", err)
	}
	if string(session) == "" {
		return errors.New("session binding session must not be empty")
	}
	if err := runtime.Validate(); err != nil {
		return fmt.Errorf("session binding runtime: %w", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	key := Key{Tool: tool, Session: session}
	if existing, ok := registry.sessions[key]; ok {
		if existing.phase == phaseCommitted {
			return ErrAlreadyBound
		}
		// Same session re-reserve of the same runtime is idempotent.
		if existing.runtime == runtime {
			return nil
		}
		return ErrConflict
	}
	if owner, ok := registry.runtimes[runtime]; ok && owner != key {
		return ErrConflict
	}
	registry.sessions[key] = entry{runtime: runtime, phase: phaseReserved}
	registry.runtimes[runtime] = key
	return nil
}

// Commit publishes a reserved binding so Lookup can see it.
func (registry *Registry) Commit(tool instancepresence.ToolKind, session instancepresence.OpaqueIdentity) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := Key{Tool: tool, Session: session}
	value, ok := registry.sessions[key]
	if !ok || value.phase != phaseReserved {
		return ErrNotReserved
	}
	value.phase = phaseCommitted
	registry.sessions[key] = value
	return nil
}

// Rollback drops a reservation without publishing a binding.
func (registry *Registry) Rollback(tool instancepresence.ToolKind, session instancepresence.OpaqueIdentity) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := Key{Tool: tool, Session: session}
	value, ok := registry.sessions[key]
	if !ok || value.phase != phaseReserved {
		return ErrNotReserved
	}
	delete(registry.sessions, key)
	if owner, ok := registry.runtimes[value.runtime]; ok && owner == key {
		delete(registry.runtimes, value.runtime)
	}
	return nil
}

// Unbind removes a committed session binding. It does not end the runtime.
// Returns the previous RuntimeRef when a committed binding existed.
func (registry *Registry) Unbind(tool instancepresence.ToolKind, session instancepresence.OpaqueIdentity) (instancepresence.InstanceID, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := Key{Tool: tool, Session: session}
	value, ok := registry.sessions[key]
	if !ok || value.phase != phaseCommitted {
		return "", ErrNotBound
	}
	delete(registry.sessions, key)
	if owner, ok := registry.runtimes[value.runtime]; ok && owner == key {
		delete(registry.runtimes, value.runtime)
	}
	return value.runtime, nil
}

// Len reports the number of committed session bindings (test helper).
func (registry *Registry) Len() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	count := 0
	for _, value := range registry.sessions {
		if value.phase == phaseCommitted {
			count++
		}
	}
	return count
}

// ReservedLen reports outstanding reservations (test helper).
func (registry *Registry) ReservedLen() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	count := 0
	for _, value := range registry.sessions {
		if value.phase == phaseReserved {
			count++
		}
	}
	return count
}
