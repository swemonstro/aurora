package instancepresence

import (
	"errors"
	"fmt"
	"strings"
)

type RevisionOrder int

const (
	RevisionOlder RevisionOrder = -1
	RevisionSame  RevisionOrder = 0
	RevisionNewer RevisionOrder = 1
)

func CompareRuntimeRevision(currentEpoch ProducerEpoch, current RuntimeRevision, incomingEpoch ProducerEpoch, incoming RuntimeRevision) (RevisionOrder, error) {
	return compareRevision(currentEpoch, uint64(current), incomingEpoch, uint64(incoming))
}

func CompareHookRevision(currentEpoch ProducerEpoch, current HookRevision, incomingEpoch ProducerEpoch, incoming HookRevision) (RevisionOrder, error) {
	return compareRevision(currentEpoch, uint64(current), incomingEpoch, uint64(incoming))
}

// compareRevision compares one owner layer within a producer epoch. Epoch
// changes require re-registration and are intentionally not ordered here.
func compareRevision(currentEpoch ProducerEpoch, current uint64, incomingEpoch ProducerEpoch, incoming uint64) (RevisionOrder, error) {
	if err := currentEpoch.Validate(); err != nil {
		return RevisionSame, fmt.Errorf("current epoch: %w", err)
	}
	if err := incomingEpoch.Validate(); err != nil {
		return RevisionSame, fmt.Errorf("incoming epoch: %w", err)
	}
	if currentEpoch != incomingEpoch {
		return RevisionSame, errors.New("producer epochs are not directly comparable")
	}
	switch {
	case incoming < current:
		return RevisionOlder, nil
	case incoming > current:
		return RevisionNewer, nil
	default:
		return RevisionSame, nil
	}
}

// LowestAvailableSlot returns the lowest unused logical slot. It deliberately
// has no physical-capacity input; capacity belongs to presentation.
func LowestAvailableSlot(namespace string, occupied []Slot) (uint64, error) {
	if strings.TrimSpace(namespace) == "" {
		return 0, errors.New("slot namespace must not be empty")
	}
	used := make(map[uint64]struct{}, len(occupied))
	for _, slot := range occupied {
		if slot.Namespace != namespace {
			continue
		}
		if _, exists := used[slot.Index]; exists {
			return 0, fmt.Errorf("duplicate occupied slot %s/%d", namespace, slot.Index)
		}
		used[slot.Index] = struct{}{}
	}
	for index := uint64(0); ; index++ {
		if _, exists := used[index]; !exists {
			return index, nil
		}
	}
}
