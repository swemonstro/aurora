package runtimerecognition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

type Role string

const (
	RoleDirect  Role = "direct"
	RoleWrapper Role = "wrapper"
	RoleNode    Role = "node_launcher"
	RoleNative  Role = "native_child"
)

const (
	PriorityLaunch     uint8 = 1
	PriorityExecutable uint8 = 2
)

type Recognition struct {
	Tool     instancepresence.ToolKind
	Role     Role
	Priority uint8
}

func (recognition Recognition) Validate() error {
	if err := recognition.Tool.Validate(); err != nil {
		return fmt.Errorf("recognizer tool: %w", err)
	}
	switch recognition.Role {
	case RoleDirect, RoleWrapper, RoleNode, RoleNative:
	default:
		return fmt.Errorf("unsupported recognition role %q", recognition.Role)
	}
	if recognition.Priority != PriorityLaunch && recognition.Priority != PriorityExecutable {
		return fmt.Errorf("unsupported recognition priority %d", recognition.Priority)
	}
	return nil
}

// AgentRuntimeRecognizer is owned by an agent adapter. It receives only an
// internal, sanitized, OS-neutral recognition process observation.
type AgentRuntimeRecognizer interface {
	Recognize(ProcessObservation) (Recognition, bool)
}

// RuntimeSnapshotSource must return process observations and BootID from one
// atomic observation. No split Snapshot/BootIdentity fallback exists.
type RuntimeSnapshotSource interface {
	RuntimeSnapshot(context.Context) (Snapshot, error)
}

type Source struct {
	snapshots   RuntimeSnapshotSource
	hostID      string
	recognizers []AgentRuntimeRecognizer
}

func NewSource(snapshots RuntimeSnapshotSource, hostID string, recognizers ...AgentRuntimeRecognizer) (*Source, error) {
	if snapshots == nil || isNilValue(snapshots) {
		return nil, errors.New("runtime snapshot source must not be nil")
	}
	if strings.TrimSpace(hostID) == "" {
		return nil, errors.New("host ID must not be empty")
	}
	if err := validateRecognizers(recognizers); err != nil {
		return nil, err
	}
	return &Source{snapshots: snapshots, hostID: hostID, recognizers: append([]AgentRuntimeRecognizer{}, recognizers...)}, nil
}

func (source *Source) Observe(ctx context.Context) ([]instancecorrelation.RuntimeObservation, error) {
	snapshot, err := source.snapshots.RuntimeSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("observe runtime snapshot: %w", err)
	}
	result, err := Recognize(snapshot, source.hostID, source.recognizers...)
	if err != nil {
		return nil, err
	}
	return result.Observations, nil
}

type ReasonCode string

const (
	ReasonIdentifiedAgentFamily ReasonCode = "identified_agent_family"
	ReasonUnknownProcess        ReasonCode = "unknown_process"
	ReasonAmbiguousRoot         ReasonCode = "ambiguous_root"
	ReasonRootMissingChildAlive ReasonCode = "root_missing_child_alive"
	ReasonMultipleRoots         ReasonCode = "multiple_possible_roots"
)

type Family struct {
	Candidate   instancepresence.RuntimeCandidate
	Shape       string
	ReasonCodes []ReasonCode
}

type UncertainFamily struct {
	Tool          instancepresence.ToolKind
	PossibleRoots []instancepresence.ProcessIdentity
	Members       []instancepresence.ProcessIdentity
	ReasonCodes   []ReasonCode
}

type Result struct {
	Observations      []instancecorrelation.RuntimeObservation
	Families          []Family
	UncertainFamilies []UncertainFamily
	UnknownProcesses  uint64
}

// Recognize conservatively derives runtime families from one atomic snapshot.
func Recognize(snapshot Snapshot, hostID string, recognizers ...AgentRuntimeRecognizer) (Result, error) {
	if err := snapshot.Validate(); err != nil {
		return Result{}, fmt.Errorf("runtime snapshot: %w", err)
	}
	if strings.TrimSpace(hostID) == "" {
		return Result{}, errors.New("host ID must not be empty")
	}
	if err := validateRecognizers(recognizers); err != nil {
		return Result{}, err
	}
	records := make([]processRecord, 0, len(snapshot.Processes))
	unknown := uint64(0)
	for _, process := range snapshot.Processes {
		recognition, recognized, err := recognize(process, recognizers)
		if err != nil {
			return Result{}, err
		}
		if !recognized {
			unknown++
		}
		records = append(records, processRecord{observation: process, recognition: recognition, recognized: recognized})
	}
	families, uncertain, err := buildFamilies(hostID, snapshot.BootID, records)
	if err != nil {
		return Result{}, err
	}
	observations := make([]instancecorrelation.RuntimeObservation, 0, len(families))
	for _, family := range families {
		root, exists := recordByIdentity(records, family.Candidate.Runtime.RootProcess)
		if !exists {
			return Result{}, errors.New("runtime family root disappeared during recognition")
		}
		observations = append(observations, instancecorrelation.RuntimeObservation{
			Candidate: family.Candidate, ObservedAt: snapshot.ObservedAt, Lifecycle: instancecorrelation.LifecycleActive,
			ProcessGroupOrJob: root.ProcessGroupOrJob, OSSession: root.OSSession, TerminalFingerprint: root.TerminalFingerprint,
		})
	}
	return Result{Observations: observations, Families: families, UncertainFamilies: uncertain, UnknownProcesses: unknown}, nil
}

func validateRecognizers(recognizers []AgentRuntimeRecognizer) error {
	if len(recognizers) == 0 {
		return errors.New("at least one runtime recognizer is required")
	}
	for index, recognizer := range recognizers {
		if recognizer == nil || isNilRecognizer(recognizer) {
			return fmt.Errorf("runtime recognizer %d must not be nil", index)
		}
	}
	return nil
}

func isNilRecognizer(recognizer AgentRuntimeRecognizer) bool {
	return isNilValue(recognizer)
}

func isNilValue(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type processRecord struct {
	observation ProcessObservation
	recognition Recognition
	recognized  bool
}

func recognize(process ProcessObservation, recognizers []AgentRuntimeRecognizer) (Recognition, bool, error) {
	var selected Recognition
	found := false
	for _, recognizer := range recognizers {
		candidate, ok := recognizer.Recognize(process)
		if !ok {
			continue
		}
		if err := candidate.Validate(); err != nil {
			return Recognition{}, false, err
		}
		if !found || candidate.Priority > selected.Priority {
			selected, found = candidate, true
			continue
		}
		if candidate.Priority == selected.Priority && (candidate.Tool != selected.Tool || candidate.Role != selected.Role) {
			return Recognition{}, false, nil
		}
	}
	return selected, found, nil
}

func buildFamilies(hostID string, bootID instancepresence.BootIdentity, records []processRecord) ([]Family, []UncertainFamily, error) {
	byIdentity := make(map[string]int, len(records))
	byPID := make(map[uint64][]int, len(records))
	classified := make([]int, 0, len(records))
	for index := range records {
		identityKey := processIdentityKey(records[index].observation.Process)
		byIdentity[identityKey] = index
		pid := records[index].observation.Process.PID
		byPID[pid] = append(byPID[pid], index)
		if records[index].recognized {
			classified = append(classified, index)
		}
	}
	sets := newDisjointSet(len(records))
	bridges := make(map[int][]int)
	for _, childIndex := range classified {
		child := records[childIndex]
		current, exact := knownParent(child.observation, byIdentity, records)
		path := make([]int, 0, 3)
		for depth := 0; exact && depth <= 3; depth++ {
			ancestor := records[current]
			if !compatibleExecutionContext(child.observation, ancestor.observation) {
				break
			}
			if ancestor.recognized {
				if ancestor.recognition.Tool == child.recognition.Tool {
					sets.union(childIndex, current)
					bridges[childIndex] = append(bridges[childIndex], path...)
				}
				break
			}
			path = append(path, current)
			current, exact = knownParent(ancestor.observation, byIdentity, records)
		}
	}

	type component struct {
		tool        instancepresence.ToolKind
		indices     []int
		rootIndices []int
	}
	componentsByRoot := make(map[int]map[int]struct{})
	for _, index := range classified {
		root := sets.find(index)
		if componentsByRoot[root] == nil {
			componentsByRoot[root] = make(map[int]struct{})
		}
		componentsByRoot[root][index] = struct{}{}
	}
	for child, path := range bridges {
		root := sets.find(child)
		for _, index := range path {
			componentsByRoot[root][index] = struct{}{}
		}
	}
	componentList := make([]component, 0, len(componentsByRoot))
	for _, memberSet := range componentsByRoot {
		indices := make([]int, 0, len(memberSet))
		memberIdentities := make(map[string]struct{}, len(memberSet))
		var tool instancepresence.ToolKind
		for index := range memberSet {
			indices = append(indices, index)
			memberIdentities[processIdentityKey(records[index].observation.Process)] = struct{}{}
			if records[index].recognized {
				tool = records[index].recognition.Tool
			}
		}
		sort.Slice(indices, func(first, second int) bool {
			return processIdentityKey(records[indices[first]].observation.Process) < processIdentityKey(records[indices[second]].observation.Process)
		})
		roots := make([]int, 0, 2)
		for _, index := range indices {
			parent, exact := knownParent(records[index].observation, byIdentity, records)
			if !exact {
				roots = append(roots, index)
				continue
			}
			if _, inside := memberIdentities[processIdentityKey(records[parent].observation.Process)]; !inside {
				roots = append(roots, index)
			}
		}
		componentList = append(componentList, component{tool: tool, indices: indices, rootIndices: roots})
	}

	ambiguous := make(map[int]struct{})
	ambiguousGroups := make(map[int][]int)
	missingGroups := make(map[string][]int)
	for index, value := range componentList {
		if len(value.rootIndices) != 1 {
			ambiguous[index] = struct{}{}
			continue
		}
		root := records[value.rootIndices[0]].observation
		parentPID, unresolved := unresolvedParent(root, byIdentity, records)
		if !unresolved {
			continue
		}
		if len(byPID[parentPID]) > 0 {
			// A live process with the same numerical PID cannot establish the
			// missing generation. Treat it as explicit PID-reuse ambiguity.
			ambiguous[index] = struct{}{}
			ambiguousGroups[index] = []int{index}
		}
		key := fmt.Sprintf("%s/%d/%s/%s", value.tool, parentPID, root.ProcessGroupOrJob, root.OSSession)
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
		root := records[value.rootIndices[0]].observation
		key := fmt.Sprintf("%s/%s/%s", value.tool, root.ProcessGroupOrJob, root.OSSession)
		contextGroups[key] = append(contextGroups[key], index)
		_, unresolved := unresolvedParent(root, byIdentity, records)
		contextHasMissing[key] = contextHasMissing[key] || unresolved
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
	consumed := make(map[int]struct{})
	for index, value := range componentList {
		if _, isAmbiguous := ambiguous[index]; isAmbiguous {
			if _, done := consumed[index]; done {
				continue
			}
			group := ambiguousGroups[index]
			if len(group) == 0 {
				group = []int{index}
			}
			possibleRoots, members := []instancepresence.ProcessIdentity{}, []instancepresence.ProcessIdentity{}
			rootMissing := false
			for _, componentIndex := range group {
				consumed[componentIndex] = struct{}{}
				for _, rootIndex := range componentList[componentIndex].rootIndices {
					_, unresolved := unresolvedParent(records[rootIndex].observation, byIdentity, records)
					rootMissing = rootMissing || unresolved
					possibleRoots = append(possibleRoots, records[rootIndex].observation.Process)
				}
				for _, memberIndex := range componentList[componentIndex].indices {
					members = append(members, records[memberIndex].observation.Process)
				}
			}
			sortProcesses(possibleRoots)
			sortProcesses(members)
			if len(possibleRoots) == 0 {
				possibleRoots = append(possibleRoots, members...)
			}
			if len(possibleRoots) == 0 {
				return nil, nil, errors.New("uncertain runtime family has no possible roots")
			}
			reasons := []ReasonCode{ReasonAmbiguousRoot}
			if rootMissing {
				reasons = append(reasons, ReasonRootMissingChildAlive)
			}
			if len(possibleRoots) > 1 {
				reasons = append(reasons, ReasonMultipleRoots)
			}
			sort.Slice(reasons, func(first, second int) bool { return reasons[first] < reasons[second] })
			uncertain = append(uncertain, UncertainFamily{Tool: value.tool, PossibleRoots: possibleRoots, Members: members, ReasonCodes: reasons})
			continue
		}
		if len(value.rootIndices) != 1 {
			return nil, nil, errors.New("runtime family has no unique root")
		}
		root := records[value.rootIndices[0]].observation
		members := make([]instancepresence.ProcessIdentity, 0, len(value.indices))
		roles := make(map[Role]struct{})
		for _, memberIndex := range value.indices {
			members = append(members, records[memberIndex].observation.Process)
			if records[memberIndex].recognized {
				roles[records[memberIndex].recognition.Role] = struct{}{}
			}
		}
		sortProcesses(members)
		reasons := []ReasonCode{ReasonIdentifiedAgentFamily}
		if _, unresolved := unresolvedParent(root, byIdentity, records); unresolved {
			reasons = append(reasons, ReasonRootMissingChildAlive)
		}
		sort.Slice(reasons, func(first, second int) bool { return reasons[first] < reasons[second] })
		candidate := instancepresence.RuntimeCandidate{InstanceID: observedCandidateID(hostID, bootID, value.tool, root.Process), Tool: value.tool, Runtime: instancepresence.RuntimeIdentity{HostID: hostID, BootID: bootID, RootProcess: root.Process}, Members: members}
		if err := candidate.Validate(); err != nil {
			return nil, nil, fmt.Errorf("validate runtime candidate: %w", err)
		}
		families = append(families, Family{Candidate: candidate, Shape: familyShape(roles), ReasonCodes: reasons})
	}
	sort.Slice(families, func(first, second int) bool {
		if families[first].Candidate.Tool != families[second].Candidate.Tool {
			return families[first].Candidate.Tool < families[second].Candidate.Tool
		}
		return processIdentityKey(families[first].Candidate.Runtime.RootProcess) < processIdentityKey(families[second].Candidate.Runtime.RootProcess)
	})
	sort.Slice(uncertain, func(first, second int) bool {
		if uncertain[first].Tool != uncertain[second].Tool {
			return uncertain[first].Tool < uncertain[second].Tool
		}
		return processIdentityKey(uncertain[first].PossibleRoots[0]) < processIdentityKey(uncertain[second].PossibleRoots[0])
	})
	return families, uncertain, nil
}

func knownParent(process ProcessObservation, byIdentity map[string]int, records []processRecord) (int, bool) {
	if process.Parent == nil {
		return 0, false
	}
	index, exists := byIdentity[processIdentityKey(*process.Parent)]
	if !exists || records[index].observation.Process.PID != process.Parent.PID || !records[index].observation.Process.StartedAt.Equal(process.Parent.StartedAt) {
		return 0, false
	}
	return index, true
}

// unresolvedParent reports a numerical parent hint only when no exact parent
// generation is present in this snapshot. A PID match alone is never evidence
// of parent presence because the PID may have been reused.
func unresolvedParent(process ProcessObservation, byIdentity map[string]int, records []processRecord) (uint64, bool) {
	if _, exact := knownParent(process, byIdentity, records); exact {
		return 0, false
	}
	if process.Parent != nil {
		return process.Parent.PID, true
	}
	if process.ParentPIDHint != 0 {
		return process.ParentPIDHint, true
	}
	return 0, false
}

func compatibleExecutionContext(first, second ProcessObservation) bool {
	return (first.ProcessGroupOrJob == "" || second.ProcessGroupOrJob == "" || first.ProcessGroupOrJob == second.ProcessGroupOrJob) && (first.OSSession == "" || second.OSSession == "" || first.OSSession == second.OSSession)
}

func recordByIdentity(records []processRecord, identity instancepresence.ProcessIdentity) (ProcessObservation, bool) {
	for _, record := range records {
		if record.observation.Process.PID == identity.PID && record.observation.Process.StartedAt.Equal(identity.StartedAt) {
			return record.observation, true
		}
	}
	return ProcessObservation{}, false
}

func processIdentityKey(identity instancepresence.ProcessIdentity) string {
	return fmt.Sprintf("%020d/%020d", identity.PID, identity.StartedAt.UnixNano())
}

func sortProcesses(processes []instancepresence.ProcessIdentity) {
	sort.Slice(processes, func(first, second int) bool {
		return processIdentityKey(processes[first]) < processIdentityKey(processes[second])
	})
}

// StableInstanceID returns the deterministic runtime instance ID from host,
// boot, tool, and generation-safe root process identity (PID + start time).
func StableInstanceID(hostID string, bootID instancepresence.BootIdentity, tool instancepresence.ToolKind, root instancepresence.ProcessIdentity) instancepresence.InstanceID {
	return observedCandidateID(hostID, bootID, tool, root)
}

func observedCandidateID(hostID string, bootID instancepresence.BootIdentity, tool instancepresence.ToolKind, root instancepresence.ProcessIdentity) instancepresence.InstanceID {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", hostID, bootID, tool, root.PID, root.StartedAt.UnixNano())
	digest := sha256.Sum256([]byte(value))
	return instancepresence.InstanceID("observe-" + hex.EncodeToString(digest[:12]))
}

func familyShape(roles map[Role]struct{}) string {
	parts := make([]string, 0, 4)
	for _, role := range []Role{RoleWrapper, RoleNode, RoleNative, RoleDirect} {
		if _, exists := roles[role]; exists {
			parts = append(parts, string(role))
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "+")
}

type disjointSet struct{ parents []int }

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
