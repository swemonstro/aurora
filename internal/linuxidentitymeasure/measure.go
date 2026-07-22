//go:build linux

// Package linuxidentitymeasure provides a default-off, read-only Package 7.0
// diagnostic that records peer process trees for Package 6 hook ingress on
// Linux. It never mutates registry/slots, never binds, and never publishes.
//
// Output is diagnostic evidence only — not approved production attestation.
package linuxidentitymeasure

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

const (
	SchemaV1 = "aurora.package70.identity_measure.v1"

	DefaultMaxAncestryDepth     = 6
	DefaultMaxRuntimeCandidates = 12
	DefaultMeasureBudget        = 50 * time.Millisecond
	DefaultMaxOutputBytes       = 8 * 1024
)

// Config bounds the diagnostic measurement transaction.
type Config struct {
	HostID               string
	MaxAncestryDepth     int
	MaxRuntimeCandidates int
	MeasureBudget        time.Duration
	MaxOutputBytes       int
	Clock                func() time.Time
}

func (config Config) withDefaults() Config {
	if config.MaxAncestryDepth <= 0 {
		config.MaxAncestryDepth = DefaultMaxAncestryDepth
	}
	if config.MaxRuntimeCandidates <= 0 {
		config.MaxRuntimeCandidates = DefaultMaxRuntimeCandidates
	}
	if config.MeasureBudget <= 0 {
		config.MeasureBudget = DefaultMeasureBudget
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	return config
}

// Record is a bounded, content-free JSON Lines diagnostic line.
// DiagnosticUniqueLink is measurement-only and is not production attestation.
type Record struct {
	Schema                        string         `json:"schema"`
	MeasuredAt                    time.Time      `json:"measured_at"`
	Tool                          string         `json:"tool,omitempty"`
	Lifecycle                     string         `json:"lifecycle,omitempty"`
	ValidatedIngress              bool           `json:"validated_ingress"`
	PeerUID                       uint32         `json:"peer_uid"`
	PeerPID                       int32          `json:"peer_pid"`
	PeerGenerationOK              bool           `json:"peer_generation_ok"`
	PeerStartedAt                 *time.Time     `json:"peer_started_at,omitempty"`
	Ancestry                      []AncestryHop  `json:"ancestry"`
	PossibleLinks                 []PossibleLink `json:"possible_links"`
	MatchingRuntimeCount          int            `json:"matching_runtime_count"`
	LinkRules                     []string       `json:"link_rules"`
	ReasonCodes                   []string       `json:"reason_codes"`
	CaptureDurationMicros         int64          `json:"capture_duration_micros"`
	MeasureDurationMicros         int64          `json:"measure_duration_micros"`
	DurationBucket                string         `json:"duration_bucket"`
	DiagnosticUniqueLink          bool           `json:"diagnostic_unique_link"`
	Package6SequencingIndependent bool           `json:"package6_sequencing_independent"`
	NoMutationPerformed           bool           `json:"no_mutation_performed"`
}

// AncestryHop is one verified parent-chain entry (PID + start time only).
type AncestryHop struct {
	PID                  uint64    `json:"pid"`
	StartedAt            time.Time `json:"started_at"`
	IsPeer               bool      `json:"is_peer"`
	MatchesRuntimeRoot   bool      `json:"matches_runtime_root"`
	MatchesRuntimeMember bool      `json:"matches_runtime_member"`
	Depth                int       `json:"depth"`
}

// PossibleLink is a candidate L1/L2/L3 join without authorizing bind.
type PossibleLink struct {
	LinkRule    string    `json:"link_rule"`
	RuntimeRef  string    `json:"runtime_ref"`
	RootPID     uint64    `json:"root_pid"`
	RootStarted time.Time `json:"root_started_at"`
}

// ProcessAdapter is the read-only process backend surface used by the measurer.
type ProcessAdapter interface {
	CaptureGeneration(ctx context.Context, pid uint64) linuxprocess.GenerationCapture
	CaptureAncestryChain(ctx context.Context, pid uint64, maxDepth int) linuxprocess.AncestryCapture
	Observe(ctx context.Context) (linuxprocess.Sample, error)
}

// Observer implements localhooktransport.IngestIdentityObserver.
type Observer struct {
	adapter     ProcessAdapter
	hostID      string
	recognizers []runtimerecognition.AgentRuntimeRecognizer
	config      Config
	writer      io.Writer
	mutex       sync.Mutex
}

// NewObserver constructs a diagnostic observer. writer receives one JSON object
// per line. adapter and hostID are required.
func NewObserver(
	adapter ProcessAdapter,
	hostID string,
	writer io.Writer,
	config Config,
	recognizers ...runtimerecognition.AgentRuntimeRecognizer,
) (*Observer, error) {
	if adapter == nil {
		return nil, errors.New("process adapter is required")
	}
	if strings.TrimSpace(hostID) == "" {
		return nil, errors.New("host ID is required")
	}
	if writer == nil {
		return nil, errors.New("writer is required")
	}
	if len(recognizers) == 0 {
		return nil, errors.New("at least one runtime recognizer is required")
	}
	return &Observer{
		adapter:     adapter,
		hostID:      hostID,
		recognizers: append([]runtimerecognition.AgentRuntimeRecognizer{}, recognizers...),
		config:      config.withDefaults(),
		writer:      writer,
	}, nil
}

// OpenFileWriter opens path for append-only diagnostic JSON Lines output.
func OpenFileWriter(path string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("identity measure path must not be empty")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// CapturePeer implements localhooktransport.IngestIdentityObserver.
// It captures peer generation and a bounded verified parent chain immediately
// after auth, before the request frame is read.
func (observer *Observer) CapturePeer(peer localhooktransport.PeerIdentity) localhooktransport.IdentityPeerCapture {
	started := observer.config.Clock()
	capture := localhooktransport.IdentityPeerCapture{
		PeerUID:    peer.UID,
		PeerGID:    peer.GID,
		PeerPID:    peer.PID,
		CapturedAt: started.UTC(),
		Ancestry:   []localhooktransport.IdentityAncestryHop{},
	}
	if peer.PID <= 0 {
		capture.ReasonCodes = []string{"peer_pid_invalid"}
		capture.CaptureDuration = observer.config.Clock().Sub(started)
		return capture
	}
	ctx, cancel := context.WithTimeout(context.Background(), observer.config.MeasureBudget)
	defer cancel()

	chain := observer.adapter.CaptureAncestryChain(ctx, uint64(peer.PID), observer.config.MaxAncestryDepth)
	capture.CaptureDuration = observer.config.Clock().Sub(started)
	for _, code := range chain.ReasonCodes {
		capture.ReasonCodes = append(capture.ReasonCodes, string(code))
	}
	if !chain.OK || len(chain.Hops) == 0 {
		if len(capture.ReasonCodes) == 0 {
			capture.ReasonCodes = []string{"peer_process_unreadable"}
		} else {
			capture.ReasonCodes = append(capture.ReasonCodes, "peer_process_unreadable")
		}
		return capture
	}

	peerIdentity := chain.Hops[0]
	capture.GenerationOK = true
	capture.GenerationPID = peerIdentity.PID
	capture.GenerationStarted = peerIdentity.StartedAt.UTC()
	capture.ReasonCodes = append(capture.ReasonCodes, "peer_generation_ok")

	hops := make([]localhooktransport.IdentityAncestryHop, 0, len(chain.Hops))
	for index, identity := range chain.Hops {
		hops = append(hops, localhooktransport.IdentityAncestryHop{
			PID: identity.PID, StartedAt: identity.StartedAt.UTC(), Depth: index, IsPeer: index == 0,
		})
	}
	capture.Ancestry = hops
	if len(hops) > 1 {
		capture.ReasonCodes = append(capture.ReasonCodes, "ancestry_verified")
	}
	return capture
}

// CompleteIngest implements localhooktransport.IngestIdentityObserver.
// Joins the pre-captured ancestry against current runtime candidates using the
// validated tool as namespace only. Does not re-capture the peer process tree.
func (observer *Observer) CompleteIngest(
	capture localhooktransport.IdentityPeerCapture,
	tool instancepresence.ToolKind,
	lifecycle instancecorrelation.Lifecycle,
	validated bool,
) {
	// Never panic into the request path.
	defer func() { _ = recover() }()

	measureStarted := observer.config.Clock()
	ancestry := hopsFromCapture(capture)
	record := Record{
		Schema:                        SchemaV1,
		MeasuredAt:                    measureStarted.UTC(),
		ValidatedIngress:              validated,
		PeerUID:                       capture.PeerUID,
		PeerPID:                       capture.PeerPID,
		PeerGenerationOK:              capture.GenerationOK,
		Ancestry:                      ancestry,
		PossibleLinks:                 []PossibleLink{},
		LinkRules:                     []string{},
		ReasonCodes:                   append([]string{}, capture.ReasonCodes...),
		CaptureDurationMicros:         capture.CaptureDuration.Microseconds(),
		DiagnosticUniqueLink:          false,
		Package6SequencingIndependent: true,
		NoMutationPerformed:           true,
	}
	if capture.GenerationOK {
		started := capture.GenerationStarted.UTC()
		record.PeerStartedAt = &started
	}
	if validated {
		record.Tool = string(tool)
		record.Lifecycle = string(lifecycle)
		record.ReasonCodes = append(record.ReasonCodes, "request_validated", "tool_namespace_selected")
	} else {
		record.ReasonCodes = append(record.ReasonCodes, "request_not_validated")
	}

	if !capture.GenerationOK {
		record.ReasonCodes = append(record.ReasonCodes, "diagnostic_no_unique_link", "fail_closed")
		observer.finish(record, measureStarted)
		return
	}
	if !validated {
		record.ReasonCodes = append(record.ReasonCodes, "diagnostic_no_unique_link", "fail_closed")
		observer.finish(record, measureStarted)
		return
	}

	remaining := observer.config.MeasureBudget - capture.CaptureDuration
	if remaining <= 0 {
		record.ReasonCodes = append(record.ReasonCodes, "attestation_timeout", "diagnostic_no_unique_link", "fail_closed")
		observer.finish(record, measureStarted)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()

	// Runtime candidates are long-lived relative to the hook helper. Join only;
	// do not re-walk peer ancestry (already captured before request read).
	sample, err := observer.adapter.Observe(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			record.ReasonCodes = append(record.ReasonCodes, "attestation_timeout")
		} else {
			record.ReasonCodes = append(record.ReasonCodes, "attestation_internal_error")
		}
		record.ReasonCodes = append(record.ReasonCodes, "diagnostic_no_unique_link", "fail_closed")
		observer.finish(record, measureStarted)
		return
	}

	recognition, err := runtimerecognition.Recognize(sample.Recognition, observer.hostID, observer.recognizers...)
	if err != nil {
		record.ReasonCodes = append(record.ReasonCodes, "attestation_internal_error", "diagnostic_no_unique_link", "fail_closed")
		observer.finish(record, measureStarted)
		return
	}

	peer := instancepresence.ProcessIdentity{
		PID: capture.GenerationPID, StartedAt: capture.GenerationStarted.UTC(),
	}
	runtimes := filterToolRuntimes(recognition.Observations, tool, observer.config.MaxRuntimeCandidates)
	sameToolCount := 0
	for _, runtime := range recognition.Observations {
		if runtime.Candidate.Tool == tool {
			sameToolCount++
		}
	}
	if sameToolCount > observer.config.MaxRuntimeCandidates {
		record.ReasonCodes = append(record.ReasonCodes, "candidate_limit_exceeded")
	}

	links, linkCodes := evaluateLinks(peer, ancestry, runtimes)
	record.PossibleLinks = links
	record.LinkRules = uniqueSortedRules(links)
	record.MatchingRuntimeCount = countUniqueRuntimeRefs(links)
	record.Ancestry = annotateAncestry(ancestry, runtimes)
	record.ReasonCodes = append(record.ReasonCodes, linkCodes...)

	if record.MatchingRuntimeCount == 1 {
		// Diagnostic uniqueness only — not approved production hard identity.
		record.DiagnosticUniqueLink = true
		record.ReasonCodes = append(record.ReasonCodes, "unique_runtime_link", "diagnostic_unique_link")
	} else {
		record.DiagnosticUniqueLink = false
		if record.MatchingRuntimeCount == 0 {
			record.ReasonCodes = append(record.ReasonCodes, "no_unique_runtime_link", "diagnostic_no_unique_link")
		} else {
			record.ReasonCodes = append(record.ReasonCodes, "ambiguous_runtime_link", "diagnostic_ambiguous_link")
		}
		record.ReasonCodes = append(record.ReasonCodes, "fail_closed")
	}

	observer.finish(record, measureStarted)
}

func hopsFromCapture(capture localhooktransport.IdentityPeerCapture) []AncestryHop {
	if len(capture.Ancestry) == 0 {
		if !capture.GenerationOK {
			return []AncestryHop{}
		}
		return []AncestryHop{{
			PID: capture.GenerationPID, StartedAt: capture.GenerationStarted.UTC(), IsPeer: true, Depth: 0,
		}}
	}
	hops := make([]AncestryHop, 0, len(capture.Ancestry))
	for _, hop := range capture.Ancestry {
		hops = append(hops, AncestryHop{
			PID: hop.PID, StartedAt: hop.StartedAt.UTC(), IsPeer: hop.IsPeer, Depth: hop.Depth,
		})
	}
	return hops
}

func (observer *Observer) finish(record Record, measureStarted time.Time) {
	record.MeasureDurationMicros = observer.config.Clock().Sub(measureStarted).Microseconds()
	record.DurationBucket = durationBucket(time.Duration(record.CaptureDurationMicros+record.MeasureDurationMicros) * time.Microsecond)
	record.ReasonCodes = uniqueSorted(record.ReasonCodes)
	record.NoMutationPerformed = true
	record.Package6SequencingIndependent = true
	if record.Ancestry == nil {
		record.Ancestry = []AncestryHop{}
	}
	if record.PossibleLinks == nil {
		record.PossibleLinks = []PossibleLink{}
	}
	if record.LinkRules == nil {
		record.LinkRules = []string{}
	}
	observer.writeRecord(record)
}

func (observer *Observer) writeRecord(record Record) {
	payload, err := json.Marshal(record)
	if err != nil {
		return
	}
	if len(payload) > observer.config.MaxOutputBytes {
		minimal := Record{
			Schema:                        SchemaV1,
			MeasuredAt:                    record.MeasuredAt,
			ValidatedIngress:              record.ValidatedIngress,
			PeerUID:                       record.PeerUID,
			PeerPID:                       record.PeerPID,
			PeerGenerationOK:              record.PeerGenerationOK,
			Ancestry:                      []AncestryHop{},
			PossibleLinks:                 []PossibleLink{},
			LinkRules:                     []string{},
			ReasonCodes:                   uniqueSorted([]string{"output_too_large", "diagnostic_no_unique_link", "fail_closed", "no_mutation_performed"}),
			CaptureDurationMicros:         record.CaptureDurationMicros,
			MeasureDurationMicros:         record.MeasureDurationMicros,
			DurationBucket:                record.DurationBucket,
			DiagnosticUniqueLink:          false,
			Package6SequencingIndependent: true,
			NoMutationPerformed:           true,
		}
		payload, err = json.Marshal(minimal)
		if err != nil {
			return
		}
	}
	observer.mutex.Lock()
	defer observer.mutex.Unlock()
	_, _ = observer.writer.Write(append(payload, '\n'))
}

func filterToolRuntimes(
	observations []instancecorrelation.RuntimeObservation,
	tool instancepresence.ToolKind,
	limit int,
) []instancecorrelation.RuntimeObservation {
	selected := make([]instancecorrelation.RuntimeObservation, 0, limit)
	for _, observation := range observations {
		if observation.Candidate.Tool != tool {
			continue
		}
		selected = append(selected, observation)
		if len(selected) >= limit {
			break
		}
	}
	return selected
}

func evaluateLinks(
	peer instancepresence.ProcessIdentity,
	ancestry []AncestryHop,
	runtimes []instancecorrelation.RuntimeObservation,
) ([]PossibleLink, []string) {
	links := make([]PossibleLink, 0)
	codes := []string{}
	ancestorSet := make(map[string]struct{}, len(ancestry))
	for _, hop := range ancestry {
		if hop.IsPeer {
			continue
		}
		ancestorSet[processKey(instancepresence.ProcessIdentity{PID: hop.PID, StartedAt: hop.StartedAt})] = struct{}{}
	}

	for index := range runtimes {
		runtime := runtimes[index]
		root := runtime.Candidate.Runtime.RootProcess
		ref := string(runtime.Candidate.InstanceID)
		if sameProcess(peer, root) {
			links = append(links, PossibleLink{
				LinkRule: "L1_root", RuntimeRef: ref, RootPID: root.PID, RootStarted: root.StartedAt.UTC(),
			})
			continue
		}
		memberMatch := false
		for _, member := range runtime.Candidate.Members {
			if sameProcess(peer, member) {
				links = append(links, PossibleLink{
					LinkRule: "L2_member", RuntimeRef: ref, RootPID: root.PID, RootStarted: root.StartedAt.UTC(),
				})
				memberMatch = true
				break
			}
		}
		if memberMatch {
			continue
		}
		if _, ok := ancestorSet[processKey(root)]; ok {
			links = append(links, PossibleLink{
				LinkRule: "L3_ancestry", RuntimeRef: ref, RootPID: root.PID, RootStarted: root.StartedAt.UTC(),
			})
			continue
		}
		for _, member := range runtime.Candidate.Members {
			if _, ok := ancestorSet[processKey(member)]; ok {
				links = append(links, PossibleLink{
					LinkRule: "L3_ancestry", RuntimeRef: ref, RootPID: root.PID, RootStarted: root.StartedAt.UTC(),
				})
				break
			}
		}
	}

	if len(links) == 0 && len(runtimes) == 0 {
		codes = append(codes, "no_runtime")
	}
	return links, codes
}

func annotateAncestry(hops []AncestryHop, runtimes []instancecorrelation.RuntimeObservation) []AncestryHop {
	if len(hops) == 0 {
		return hops
	}
	roots := map[string]struct{}{}
	members := map[string]struct{}{}
	for _, runtime := range runtimes {
		roots[processKey(runtime.Candidate.Runtime.RootProcess)] = struct{}{}
		for _, member := range runtime.Candidate.Members {
			members[processKey(member)] = struct{}{}
		}
	}
	out := make([]AncestryHop, len(hops))
	copy(out, hops)
	for index := range out {
		key := processKey(instancepresence.ProcessIdentity{PID: out[index].PID, StartedAt: out[index].StartedAt})
		_, out[index].MatchesRuntimeRoot = roots[key]
		_, out[index].MatchesRuntimeMember = members[key]
	}
	return out
}

func processKey(identity instancepresence.ProcessIdentity) string {
	return linuxprocess.FormatProcessIdentity(identity)
}

func sameProcess(first, second instancepresence.ProcessIdentity) bool {
	return first.PID == second.PID && first.StartedAt.Equal(second.StartedAt)
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func uniqueSortedRules(links []PossibleLink) []string {
	rules := make([]string, 0, len(links))
	for _, link := range links {
		rules = append(rules, link.LinkRule)
	}
	return uniqueSorted(rules)
}

func countUniqueRuntimeRefs(links []PossibleLink) int {
	seen := make(map[string]struct{}, len(links))
	for _, link := range links {
		seen[link.RuntimeRef] = struct{}{}
	}
	return len(seen)
}

func durationBucket(duration time.Duration) string {
	switch {
	case duration < 5*time.Millisecond:
		return "lt_5ms"
	case duration < 20*time.Millisecond:
		return "lt_20ms"
	case duration < 50*time.Millisecond:
		return "lt_50ms"
	case duration < 100*time.Millisecond:
		return "lt_100ms"
	default:
		return "timeout"
	}
}
