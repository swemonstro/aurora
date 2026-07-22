package linuxprocess

import (
	"fmt"
	"sort"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

type DiffResult struct {
	Exits       []instancepresence.ProcessExit
	Diagnostics []Diagnostic
}

// DiffSnapshots reports generation-safe exits. A reused PID with a different
// start time is one old exit plus one new process in the current snapshot.
func DiffSnapshots(previous, current instancepresence.ProcessSnapshot, observedAt time.Time) ([]instancepresence.ProcessExit, error) {
	if err := previous.Validate(); err != nil {
		return nil, fmt.Errorf("previous snapshot: %w", err)
	}
	if err := current.Validate(); err != nil {
		return nil, fmt.Errorf("current snapshot: %w", err)
	}
	if err := validObservedAt(observedAt); err != nil {
		return nil, err
	}
	currentProcesses := make(map[string]struct{}, len(current.Processes))
	for _, process := range current.Processes {
		currentProcesses[processKey(process.Process)] = struct{}{}
	}
	exits := make([]instancepresence.ProcessExit, 0)
	for _, process := range previous.Processes {
		if _, exists := currentProcesses[processKey(process.Process)]; !exists {
			exits = append(exits, instancepresence.ProcessExit{Process: process.Process, ObservedAt: observedAt})
		}
	}
	sort.Slice(exits, func(first, second int) bool {
		return processKey(exits[first].Process) < processKey(exits[second].Process)
	})
	return exits, nil
}

// DiffSamples suppresses an apparent exit when the current scan marked that
// PID uncertain because of a process race or unreadable proc data.
func DiffSamples(previous, current Sample) (DiffResult, error) {
	exits, err := DiffSnapshots(previous.Snapshot, current.Snapshot, current.Snapshot.ObservedAt)
	if err != nil {
		return DiffResult{}, err
	}
	filtered := exits[:0]
	for _, exit := range exits {
		if _, uncertain := current.uncertainPIDs[exit.Process.PID]; !uncertain {
			filtered = append(filtered, exit)
		}
	}
	diagnostics := make([]Diagnostic, 0, 1)
	currentByPID := make(map[uint64]instancepresence.ProcessIdentity, len(current.Snapshot.Processes))
	for _, process := range current.Snapshot.Processes {
		currentByPID[process.Process.PID] = process.Process
	}
	for _, process := range previous.Snapshot.Processes {
		if replacement, exists := currentByPID[process.Process.PID]; exists && !sameProcess(process.Process, replacement) {
			diagnostics = append(diagnostics, Diagnostic{Code: ReasonPIDReused, Count: 1})
		}
	}
	return DiffResult{Exits: filtered, Diagnostics: diagnostics}, nil
}
