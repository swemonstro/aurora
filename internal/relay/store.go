package relay

import (
	"sync"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

const aggregateSource = "aurora-aggregate"

type Store struct {
	mu        sync.RWMutex
	snapshots map[string]presence.Snapshot
}

func (s *Store) Set(snapshot presence.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.snapshots == nil {
		s.snapshots = make(map[string]presence.Snapshot)
	}
	s.snapshots[snapshot.Source] = snapshot
}

func (s *Store) Remove(source string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.snapshots, source)
}

func (s *Store) Latest() (presence.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.snapshots) == 0 {
		return presence.Snapshot{}, false
	}

	if len(s.snapshots) == 1 {
		for _, snapshot := range s.snapshots {
			return snapshot, true
		}
	}

	aggregate := presence.Snapshot{
		Version: presence.ProtocolVersion,
		Source:  aggregateSource,
		State:   status.Idle,
	}

	for _, snapshot := range s.snapshots {
		currentPriority := statePriority(snapshot.State)
		aggregatePriority := statePriority(aggregate.State)

		if currentPriority > aggregatePriority {
			aggregate.State = snapshot.State
			aggregate.Timestamp = snapshot.Timestamp
			continue
		}

		if currentPriority == aggregatePriority &&
			snapshot.Timestamp.After(aggregate.Timestamp) {
			aggregate.Timestamp = snapshot.Timestamp
		}
	}

	return aggregate, true
}

func statePriority(state status.State) int {
	switch state {
	case status.Error:
		return 4
	case status.Attention:
		return 3
	case status.Working:
		return 2
	case status.Idle:
		return 1
	default:
		return 0
	}
}
