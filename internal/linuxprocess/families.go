package linuxprocess

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

const maxIntermediateDepth = 3

func buildFamilies(hostID string, bootID instancepresence.BootIdentity, records []rawProcess) ([]Family, []UncertainFamily) {
	byPID := make(map[uint64]int, len(records))
	classified := make([]int, 0, len(records))
	for index := range records {
		byPID[records[index].identity.PID] = index
		if records[index].classification.tool != "" {
			classified = append(classified, index)
		}
	}
	sets := newDisjointSet(len(records))
	bridges := make(map[int][]int)
	for _, childIndex := range classified {
		child := records[childIndex]
		currentPID := child.stat.ParentPID
		path := make([]int, 0, maxIntermediateDepth)
		for depth := 0; depth <= maxIntermediateDepth && currentPID != 0; depth++ {
			ancestorIndex, exists := byPID[currentPID]
			if !exists {
				break
			}
			ancestor := records[ancestorIndex]
			if !compatibleExecutionContext(child, ancestor) {
				break
			}
			if ancestor.classification.tool != "" {
				if ancestor.classification.tool == child.classification.tool {
					sets.union(childIndex, ancestorIndex)
					bridges[childIndex] = append(bridges[childIndex], path...)
				}
				break
			}
			path = append(path, ancestorIndex)
			currentPID = ancestor.stat.ParentPID
		}
	}

	components := make(map[int]map[int]struct{})
	for _, index := range classified {
		root := sets.find(index)
		if components[root] == nil {
			components[root] = make(map[int]struct{})
		}
		components[root][index] = struct{}{}
	}
	for child, path := range bridges {
		root := sets.find(child)
		for _, index := range path {
			components[root][index] = struct{}{}
		}
	}

	type component struct {
		tool        instancepresence.ToolKind
		indices     []int
		rootIndices []int
	}
	componentList := make([]component, 0, len(components))
	for _, memberSet := range components {
		indices := make([]int, 0, len(memberSet))
		memberPIDs := make(map[uint64]struct{}, len(memberSet))
		var tool instancepresence.ToolKind
		for index := range memberSet {
			indices = append(indices, index)
			memberPIDs[records[index].identity.PID] = struct{}{}
			if records[index].classification.tool != "" {
				tool = records[index].classification.tool
			}
		}
		sort.Slice(indices, func(first, second int) bool {
			return processKey(records[indices[first]].identity) < processKey(records[indices[second]].identity)
		})
		roots := make([]int, 0, 2)
		for _, index := range indices {
			if _, parentInside := memberPIDs[records[index].stat.ParentPID]; !parentInside {
				roots = append(roots, index)
			}
		}
		componentList = append(componentList, component{tool: tool, indices: indices, rootIndices: roots})
	}

	// Separate components whose roots point at the same missing parent are
	// explicitly uncertain. A process-group match alone is insufficient to
	// choose whether they are siblings or remnants of one vanished family.
	ambiguous := make(map[int]struct{})
	ambiguousGroups := make(map[int][]int)
	missingGroups := make(map[string][]int)
	for index, value := range componentList {
		if len(value.rootIndices) != 1 {
			ambiguous[index] = struct{}{}
			continue
		}
		root := records[value.rootIndices[0]]
		if root.stat.ParentPID == 0 {
			continue
		}
		if _, parentPresent := byPID[root.stat.ParentPID]; parentPresent {
			continue
		}
		key := fmt.Sprintf("%s/%d/%d/%d", value.tool, root.stat.ParentPID, root.stat.GroupID, root.stat.SessionID)
		missingGroups[key] = append(missingGroups[key], index)
	}
	for _, indices := range missingGroups {
		if len(indices) > 1 {
			for _, index := range indices {
				ambiguous[index] = struct{}{}
				ambiguousGroups[index] = indices
			}
		}
	}
	contextGroups := make(map[string][]int)
	contextHasMissing := make(map[string]bool)
	for index, value := range componentList {
		if len(value.rootIndices) != 1 {
			continue
		}
		root := records[value.rootIndices[0]]
		key := fmt.Sprintf("%s/%d/%d", value.tool, root.stat.GroupID, root.stat.SessionID)
		contextGroups[key] = append(contextGroups[key], index)
		if root.stat.ParentPID != 0 {
			_, parentPresent := byPID[root.stat.ParentPID]
			contextHasMissing[key] = contextHasMissing[key] || !parentPresent
		}
	}
	for key, indices := range contextGroups {
		if len(indices) > 1 && contextHasMissing[key] {
			for _, index := range indices {
				ambiguous[index] = struct{}{}
				ambiguousGroups[index] = indices
			}
		}
	}

	families := make([]Family, 0, len(componentList))
	uncertain := make([]UncertainFamily, 0)
	consumedAmbiguous := make(map[int]struct{})
	for index, value := range componentList {
		if _, isAmbiguous := ambiguous[index]; isAmbiguous {
			if _, consumed := consumedAmbiguous[index]; consumed {
				continue
			}
			group := ambiguousGroups[index]
			if len(group) == 0 {
				group = []int{index}
			}
			possibleRoots := make([]instancepresence.ProcessIdentity, 0)
			members := make([]instancepresence.ProcessIdentity, 0)
			for _, componentIndex := range group {
				consumedAmbiguous[componentIndex] = struct{}{}
				for _, rootIndex := range componentList[componentIndex].rootIndices {
					possibleRoots = append(possibleRoots, records[rootIndex].identity)
				}
				for _, memberIndex := range componentList[componentIndex].indices {
					members = append(members, records[memberIndex].identity)
				}
			}
			sortProcesses(possibleRoots)
			sortProcesses(members)
			if len(possibleRoots) == 0 {
				possibleRoots = append(possibleRoots, members...)
			}
			uncertain = append(uncertain, UncertainFamily{
				Tool: value.tool, PossibleRoots: possibleRoots, Members: members,
				ReasonCodes: []ReasonCode{ReasonAmbiguousRoot, ReasonMultipleRoots},
			})
			continue
		}

		rootRecord := records[value.rootIndices[0]]
		members := make([]instancepresence.ProcessIdentity, 0, len(value.indices))
		roles := make(map[processRole]struct{})
		for _, memberIndex := range value.indices {
			members = append(members, records[memberIndex].identity)
			if role := records[memberIndex].classification.role; role != roleUnknown {
				roles[role] = struct{}{}
			}
		}
		sortProcesses(members)
		reason := ReasonCodexFamily
		if value.tool == instancepresence.ToolClaude {
			reason = ReasonClaudeFamily
		}
		reasons := []ReasonCode{reason}
		if rootRecord.stat.ParentPID != 0 {
			if _, parentPresent := byPID[rootRecord.stat.ParentPID]; !parentPresent {
				reasons = append(reasons, ReasonRootMissingChildAlive)
			}
		}
		sortReasonCodes(reasons)
		candidate := instancepresence.RuntimeCandidate{
			InstanceID: observedCandidateID(hostID, bootID, value.tool, rootRecord.identity),
			Tool:       value.tool,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: hostID, BootID: bootID, RootProcess: rootRecord.identity,
			},
			Members: members,
		}
		if err := candidate.Validate(); err != nil {
			panic(fmt.Sprintf("linux process family invariant: %v", err))
		}
		families = append(families, Family{Candidate: candidate, Shape: familyShape(roles), ReasonCodes: reasons})
	}

	sort.Slice(families, func(first, second int) bool {
		if families[first].Candidate.Tool != families[second].Candidate.Tool {
			return families[first].Candidate.Tool < families[second].Candidate.Tool
		}
		return processKey(families[first].Candidate.Runtime.RootProcess) < processKey(families[second].Candidate.Runtime.RootProcess)
	})
	sort.Slice(uncertain, func(first, second int) bool {
		if uncertain[first].Tool != uncertain[second].Tool {
			return uncertain[first].Tool < uncertain[second].Tool
		}
		return processKey(uncertain[first].PossibleRoots[0]) < processKey(uncertain[second].PossibleRoots[0])
	})
	return families, uncertain
}

func compatibleExecutionContext(first, second rawProcess) bool {
	if first.stat.GroupID != 0 && second.stat.GroupID != 0 && first.stat.GroupID != second.stat.GroupID {
		return false
	}
	if first.stat.SessionID != 0 && second.stat.SessionID != 0 && first.stat.SessionID != second.stat.SessionID {
		return false
	}
	return true
}

func observedCandidateID(hostID string, bootID instancepresence.BootIdentity, tool instancepresence.ToolKind, root instancepresence.ProcessIdentity) instancepresence.InstanceID {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", hostID, bootID, tool, root.PID, root.StartedAt.UnixNano())
	digest := sha256.Sum256([]byte(value))
	return instancepresence.InstanceID("observe-" + hex.EncodeToString(digest[:12]))
}

func familyShape(roles map[processRole]struct{}) string {
	parts := make([]string, 0, 4)
	for _, role := range []processRole{roleWrapper, roleNode, roleNative, roleDirect} {
		if _, exists := roles[role]; exists {
			parts = append(parts, string(role))
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "+")
}

type disjointSet struct {
	parents []int
}

func newDisjointSet(size int) *disjointSet {
	parents := make([]int, size)
	for index := range parents {
		parents[index] = index
	}
	return &disjointSet{parents: parents}
}

func (sets *disjointSet) find(value int) int {
	if sets.parents[value] != value {
		sets.parents[value] = sets.find(sets.parents[value])
	}
	return sets.parents[value]
}

func (sets *disjointSet) union(first, second int) {
	firstRoot, secondRoot := sets.find(first), sets.find(second)
	if firstRoot < secondRoot {
		sets.parents[secondRoot] = firstRoot
	} else if secondRoot < firstRoot {
		sets.parents[firstRoot] = secondRoot
	}
}
