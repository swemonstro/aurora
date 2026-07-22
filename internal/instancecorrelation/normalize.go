package instancecorrelation

import (
	"reflect"
	"sort"
	"time"
)

type normalizedHooks struct {
	current     []HookObservation
	rejected    []RejectedMatch
	superseded  []SupersededHook
	diagnostics []ReasonCode
}

func normalizeHooks(observations []HookObservation) normalizedHooks {
	groups := make(map[string][]HookObservation)
	for _, observation := range observations {
		observation = normalizeHookTime(observation)
		groups[observation.Ref()] = append(groups[observation.Ref()], observation)
	}
	refs := make([]string, 0, len(groups))
	for ref := range groups {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	var result normalizedHooks
	for _, ref := range refs {
		group := groups[ref]
		epochs := make(map[instanceEpoch]struct{})
		for _, observation := range group {
			epochs[instanceEpoch(observation.ProducerEpoch)] = struct{}{}
		}
		if len(epochs) != 1 {
			result.rejected = append(result.rejected, RejectedMatch{
				HookRef: ref, Reasons: []ReasonCode{ReasonProducerEpochConflict},
			})
			result.diagnostics = append(result.diagnostics, ReasonProducerEpochConflict)
			continue
		}
		if idempotencyConflict(group) {
			result.rejected = append(result.rejected, RejectedMatch{
				HookRef: ref, Reasons: []ReasonCode{ReasonIdempotencyConflict},
			})
			result.diagnostics = append(result.diagnostics, ReasonIdempotencyConflict)
			continue
		}

		byRevision := make(map[uint64][]HookObservation)
		for _, observation := range group {
			byRevision[observation.Revision] = append(byRevision[observation.Revision], observation)
		}
		revisions := make([]uint64, 0, len(byRevision))
		for revision := range byRevision {
			revisions = append(revisions, revision)
		}
		sort.Slice(revisions, func(first, second int) bool { return revisions[first] > revisions[second] })

		conflictingHighest := false
		for revisionIndex, revision := range revisions {
			values := byRevision[revision]
			if !allPayloadsEqual(values) {
				result.rejected = append(result.rejected, RejectedMatch{
					HookRef: ref, Reasons: []ReasonCode{ReasonSameRevisionConflict},
				})
				result.diagnostics = append(result.diagnostics, ReasonSameRevisionConflict)
				if revisionIndex == 0 {
					conflictingHighest = true
				}
				continue
			}
			if revisionIndex > 0 {
				result.superseded = append(result.superseded, SupersededHook{
					HookRef: ref, Epoch: string(values[0].ProducerEpoch), Revision: revision,
					ReasonCodes: []ReasonCode{ReasonOutOfOrderRevision, ReasonSupersededRevision},
				})
				result.diagnostics = append(result.diagnostics, ReasonOutOfOrderRevision)
			}
		}
		if !conflictingHighest {
			result.current = append(result.current, byRevision[revisions[0]][0])
		}
	}

	sort.Slice(result.rejected, func(first, second int) bool {
		if result.rejected[first].HookRef != result.rejected[second].HookRef {
			return result.rejected[first].HookRef < result.rejected[second].HookRef
		}
		return result.rejected[first].Reasons[0] < result.rejected[second].Reasons[0]
	})
	sort.Slice(result.superseded, func(first, second int) bool {
		if result.superseded[first].HookRef != result.superseded[second].HookRef {
			return result.superseded[first].HookRef < result.superseded[second].HookRef
		}
		return result.superseded[first].Revision > result.superseded[second].Revision
	})
	result.diagnostics = uniqueReasonCodes(result.diagnostics)
	return result
}

type instanceEpoch string

func normalizeHookTime(observation HookObservation) HookObservation {
	observation.ObservedAt = canonicalTime(observation.ObservedAt)
	if observation.ProcessHint != nil {
		value := *observation.ProcessHint
		value.StartedAt = canonicalTime(value.StartedAt)
		observation.ProcessHint = &value
	}
	if observation.RuntimeHint != nil {
		value := *observation.RuntimeHint
		value.RootProcess.StartedAt = canonicalTime(value.RootProcess.StartedAt)
		observation.RuntimeHint = &value
	}
	return observation
}

func normalizeRuntimeTime(observation RuntimeObservation) RuntimeObservation {
	observation.ObservedAt = canonicalTime(observation.ObservedAt)
	observation.Candidate.Runtime.RootProcess.StartedAt = canonicalTime(observation.Candidate.Runtime.RootProcess.StartedAt)
	for index := range observation.Candidate.Members {
		observation.Candidate.Members[index].StartedAt = canonicalTime(observation.Candidate.Members[index].StartedAt)
	}
	if observation.EndedAt != nil {
		value := canonicalTime(*observation.EndedAt)
		observation.EndedAt = &value
	}
	return observation
}

func canonicalTime(value time.Time) time.Time {
	return value.Round(0).UTC()
}

func allPayloadsEqual(values []HookObservation) bool {
	for index := 1; index < len(values); index++ {
		if !sameHookPayload(values[0], values[index]) {
			return false
		}
	}
	return true
}

func idempotencyConflict(values []HookObservation) bool {
	byKey := make(map[string]HookObservation)
	for _, value := range values {
		if value.IdempotencyKey == "" {
			continue
		}
		if previous, exists := byKey[value.IdempotencyKey]; exists {
			first, second := previous, value
			first.IdempotencyKey, second.IdempotencyKey = "", ""
			if !reflect.DeepEqual(first, second) {
				return true
			}
		}
		byKey[value.IdempotencyKey] = value
	}
	return false
}

func sameHookPayload(first, second HookObservation) bool {
	first.IdempotencyKey = ""
	second.IdempotencyKey = ""
	return reflect.DeepEqual(first, second)
}
