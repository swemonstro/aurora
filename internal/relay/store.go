package relay

import (
	"sync"

	"github.com/swemonstro/aurora/internal/presence"
)

type Store struct {
	mu       sync.RWMutex
	snapshot presence.Snapshot
	hasValue bool
}

func (s *Store) Set(snapshot presence.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshot = snapshot
	s.hasValue = true
}

func (s *Store) Latest() (presence.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.snapshot, s.hasValue
}
