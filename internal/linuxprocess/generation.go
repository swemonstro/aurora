package linuxprocess

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

// GenerationCapture is a single-process generation read using the same
// double-read /proc rules as full snapshot observation.
type GenerationCapture struct {
	Identity    instancepresence.ProcessIdentity
	ReasonCodes []ReasonCode
	OK          bool
}

// AncestryCapture is a bounded peer + verified parent chain captured while the
// dialer is still expected to be alive (immediately after SO_PEERCRED).
type AncestryCapture struct {
	// Hops[0] is the peer generation; subsequent hops are verified parents.
	Hops        []instancepresence.ProcessIdentity
	ReasonCodes []ReasonCode
	// OK is true when the peer generation was captured. Parent walk may still
	// be partial; inspect ReasonCodes for ancestry_unresolved / depth.
	OK bool
}

// CaptureGeneration double-reads /proc/<pid> generation fields and returns a
// generation-safe ProcessIdentity when stable. It reuses readProcess and never
// trusts client-supplied start times.
func (adapter *Adapter) CaptureGeneration(ctx context.Context, pid uint64) GenerationCapture {
	if adapter == nil {
		return GenerationCapture{ReasonCodes: []ReasonCode{ReasonInvalidProcData}}
	}
	if err := ctx.Err(); err != nil {
		return GenerationCapture{ReasonCodes: []ReasonCode{ReasonProcessDisappeared}}
	}
	if pid == 0 {
		return GenerationCapture{ReasonCodes: []ReasonCode{ReasonInvalidProcData}}
	}
	reader, err := adapter.open()
	if err != nil {
		return GenerationCapture{ReasonCodes: []ReasonCode{ReasonPermissionDenied}}
	}
	defer reader.Close()

	bootTime, codes, ok := readBootTime(reader)
	if !ok {
		return GenerationCapture{ReasonCodes: codes}
	}
	record, outcome := readProcess(reader, pid, bootTime, adapter.config.ClockTicks, nil)
	codes = reasonCodesFromCounts(outcome.counts)
	if outcome.uncertain || !outcome.accepted {
		if len(codes) == 0 {
			codes = []ReasonCode{ReasonProcessDisappeared}
		}
		return GenerationCapture{ReasonCodes: codes, OK: false}
	}
	if err := record.identity.Validate(); err != nil {
		return GenerationCapture{ReasonCodes: []ReasonCode{ReasonInvalidProcData}}
	}
	if len(codes) == 0 {
		codes = nil
	}
	return GenerationCapture{Identity: record.identity, ReasonCodes: codes, OK: true}
}

// CaptureAncestryChain captures the peer generation and a bounded verified
// parent chain using the same double-read and parent re-check rules as
// verifiedParent. maxDepth is the maximum number of parent hops after the peer.
func (adapter *Adapter) CaptureAncestryChain(ctx context.Context, pid uint64, maxDepth int) AncestryCapture {
	if adapter == nil {
		return AncestryCapture{ReasonCodes: []ReasonCode{ReasonInvalidProcData}}
	}
	if err := ctx.Err(); err != nil {
		return AncestryCapture{ReasonCodes: []ReasonCode{ReasonProcessDisappeared}}
	}
	if pid == 0 {
		return AncestryCapture{ReasonCodes: []ReasonCode{ReasonInvalidProcData}}
	}
	if maxDepth < 0 {
		maxDepth = 0
	}
	reader, err := adapter.open()
	if err != nil {
		return AncestryCapture{ReasonCodes: []ReasonCode{ReasonPermissionDenied}}
	}
	defer reader.Close()

	bootTime, codes, ok := readBootTime(reader)
	if !ok {
		return AncestryCapture{ReasonCodes: codes}
	}

	child, outcome := readProcess(reader, pid, bootTime, adapter.config.ClockTicks, nil)
	codes = reasonCodesFromCounts(outcome.counts)
	if outcome.uncertain || !outcome.accepted {
		if len(codes) == 0 {
			codes = []ReasonCode{ReasonProcessDisappeared}
		}
		return AncestryCapture{ReasonCodes: codes, OK: false}
	}
	if err := child.identity.Validate(); err != nil {
		return AncestryCapture{ReasonCodes: []ReasonCode{ReasonInvalidProcData}}
	}

	hops := []instancepresence.ProcessIdentity{child.identity}
	resultCodes := append([]ReasonCode{}, codes...)

	current := child
	for depth := 1; depth <= maxDepth; depth++ {
		if err := ctx.Err(); err != nil {
			resultCodes = append(resultCodes, ReasonProcessDisappeared)
			break
		}
		if current.stat.ParentPID == 0 {
			break
		}
		parent, parentOutcome := readProcess(reader, current.stat.ParentPID, bootTime, adapter.config.ClockTicks, nil)
		if parentOutcome.uncertain || !parentOutcome.accepted {
			resultCodes = append(resultCodes, reasonCodesFromCounts(parentOutcome.counts)...)
			if !hasReason(resultCodes, ReasonProcessDisappeared) && !hasReason(resultCodes, ReasonPIDReused) {
				resultCodes = append(resultCodes, ReasonProcessDisappeared)
			}
			break
		}
		if !verifyParentEdge(reader, current, parent, bootTime, adapter.config.ClockTicks) {
			resultCodes = append(resultCodes, ReasonPIDReused)
			break
		}
		hops = append(hops, parent.identity)
		current = parent
	}

	if len(hops) > 1 && !hasReason(resultCodes, ReasonPIDReused) && !hasReason(resultCodes, ReasonProcessDisappeared) {
		// Content-free success marker is applied by the diagnostic package.
	}
	// Report only when the walk filled maxDepth and the last hop still claims a parent.
	// A chain that ends exactly at maxDepth (ParentPID == 0) is not exceeded.
	if maxDepth > 0 && len(hops)-1 >= maxDepth && current.stat.ParentPID != 0 {
		resultCodes = append(resultCodes, ReasonAncestryDepthExceeded)
	}
	return AncestryCapture{Hops: hops, ReasonCodes: uniqueReasonCodes(resultCodes), OK: true}
}

func readBootTime(reader procReader) (time.Time, []ReasonCode, bool) {
	bootData, err := reader.ReadFile("stat", statReadLimit)
	if err != nil {
		return time.Time{}, []ReasonCode{ReasonInvalidProcData}, false
	}
	bootTime, err := parseBootTime(bootData)
	if err != nil {
		return time.Time{}, []ReasonCode{ReasonInvalidProcData}, false
	}
	return bootTime, nil, true
}

// verifyParentEdge re-checks child and parent generations and the PPID edge,
// matching verifiedParent's fail-closed spirit without requiring a full snapshot.
func verifyParentEdge(reader procReader, child, parent rawProcess, bootTime time.Time, clockTicks uint64) bool {
	parentData, err := reader.ReadFile(path.Join(strconv.FormatUint(parent.identity.PID, 10), "stat"), statReadLimit)
	if err != nil {
		return false
	}
	currentParent, err := parseProcStat(parentData)
	if err != nil || currentParent.PID != parent.identity.PID ||
		!startedAt(bootTime, currentParent.StartTicks, clockTicks).Equal(parent.identity.StartedAt) {
		return false
	}
	childData, err := reader.ReadFile(path.Join(strconv.FormatUint(child.identity.PID, 10), "stat"), statReadLimit)
	if err != nil {
		return false
	}
	currentChild, err := parseProcStat(childData)
	if err != nil || currentChild.PID != child.identity.PID ||
		currentChild.ParentPID != parent.identity.PID ||
		!startedAt(bootTime, currentChild.StartTicks, clockTicks).Equal(child.identity.StartedAt) {
		return false
	}
	return true
}

func reasonCodesFromCounts(counts map[ReasonCode]uint64) []ReasonCode {
	if len(counts) == 0 {
		return nil
	}
	codes := make([]ReasonCode, 0, len(counts))
	for code, count := range counts {
		if count > 0 {
			codes = append(codes, code)
		}
	}
	sortReasonCodes(codes)
	return codes
}

func uniqueReasonCodes(codes []ReasonCode) []ReasonCode {
	if len(codes) == 0 {
		return nil
	}
	seen := make(map[ReasonCode]struct{}, len(codes))
	out := make([]ReasonCode, 0, len(codes))
	for _, code := range codes {
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	sortReasonCodes(out)
	return out
}

func hasReason(codes []ReasonCode, want ReasonCode) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

// FormatProcessIdentity keeps diagnostics content-free and deterministic.
func FormatProcessIdentity(identity instancepresence.ProcessIdentity) string {
	return fmt.Sprintf("%d@%s", identity.PID, identity.StartedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"))
}
