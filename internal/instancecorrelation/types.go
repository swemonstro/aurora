// Package instancecorrelation provides deterministic, observe-only correlation
// between sanitized hook observations and runtime process families. It never
// mutates presence state or performs transport, persistence, or OS discovery.
package instancecorrelation

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

type Lifecycle string

const (
	LifecycleActive Lifecycle = "active"
	LifecycleIdle   Lifecycle = "idle"
	LifecycleEnded  Lifecycle = "ended"
)

func (lifecycle Lifecycle) Validate() error {
	switch lifecycle {
	case LifecycleActive, LifecycleIdle, LifecycleEnded:
		return nil
	default:
		return fmt.Errorf("unsupported observation lifecycle %q", lifecycle)
	}
}

type HookObservation struct {
	Tool                instancepresence.ToolKind          `json:"tool"`
	Source              *instancepresence.SourceDescriptor `json:"source,omitempty"`
	HookSessionRef      instancepresence.OpaqueIdentity    `json:"hook_session_ref"`
	ProducerEpoch       instancepresence.ProducerEpoch     `json:"producer_epoch"`
	Revision            uint64                             `json:"revision"`
	IdempotencyKey      string                             `json:"idempotency_key,omitempty"`
	ObservedAt          time.Time                          `json:"observed_at"`
	Lifecycle           Lifecycle                          `json:"lifecycle"`
	ProcessHint         *instancepresence.ProcessIdentity  `json:"process_hint,omitempty"`
	RuntimeHint         *instancepresence.RuntimeIdentity  `json:"runtime_hint,omitempty"`
	ParentOrRootPIDHint *uint64                            `json:"parent_or_root_pid_hint,omitempty"`
	ProcessGroupOrJob   instancepresence.OpaqueIdentity    `json:"process_group_or_job,omitempty"`
	OSSession           instancepresence.OpaqueIdentity    `json:"os_session,omitempty"`
	TerminalFingerprint instancepresence.OpaqueIdentity    `json:"terminal_fingerprint,omitempty"`
	HostID              string                             `json:"host_id,omitempty"`
	BootID              instancepresence.BootIdentity      `json:"boot_id,omitempty"`
}

func (observation HookObservation) Validate() error {
	if err := observation.Tool.Validate(); err != nil {
		return fmt.Errorf("hook tool: %w", err)
	}
	if observation.Source != nil {
		if err := observation.Source.Validate(); err != nil {
			return fmt.Errorf("hook source: %w", err)
		}
	}
	if strings.TrimSpace(string(observation.HookSessionRef)) == "" {
		return errors.New("hook session reference must not be empty")
	}
	if err := observation.ProducerEpoch.Validate(); err != nil {
		return fmt.Errorf("hook producer epoch: %w", err)
	}
	if observation.Revision == 0 {
		return errors.New("hook revision must be positive")
	}
	if observation.ObservedAt.IsZero() {
		return errors.New("hook observation time must not be zero")
	}
	if err := observation.Lifecycle.Validate(); err != nil {
		return err
	}
	if observation.ProcessHint != nil {
		if err := observation.ProcessHint.Validate(); err != nil {
			return fmt.Errorf("hook process hint: %w", err)
		}
	}
	if observation.RuntimeHint != nil {
		if err := observation.RuntimeHint.Validate(); err != nil {
			return fmt.Errorf("hook runtime hint: %w", err)
		}
	}
	if observation.ParentOrRootPIDHint != nil && *observation.ParentOrRootPIDHint == 0 {
		return errors.New("parent or root PID hint must be positive")
	}
	if observation.HostID != "" && strings.TrimSpace(observation.HostID) == "" {
		return errors.New("hook host ID must not be whitespace")
	}
	if observation.BootID != "" {
		if err := observation.BootID.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (observation HookObservation) Ref() string {
	return string(observation.Tool) + ":" + string(observation.HookSessionRef)
}

type RuntimeObservation struct {
	Candidate           instancepresence.RuntimeCandidate  `json:"candidate"`
	Source              *instancepresence.SourceDescriptor `json:"source,omitempty"`
	ObservedAt          time.Time                          `json:"observed_at"`
	Lifecycle           Lifecycle                          `json:"lifecycle"`
	EndedAt             *time.Time                         `json:"ended_at,omitempty"`
	ProcessGroupOrJob   instancepresence.OpaqueIdentity    `json:"process_group_or_job,omitempty"`
	OSSession           instancepresence.OpaqueIdentity    `json:"os_session,omitempty"`
	TerminalFingerprint instancepresence.OpaqueIdentity    `json:"terminal_fingerprint,omitempty"`
}

func (observation RuntimeObservation) Validate() error {
	if err := observation.Candidate.Validate(); err != nil {
		return fmt.Errorf("runtime candidate: %w", err)
	}
	if observation.Source != nil {
		if err := observation.Source.Validate(); err != nil {
			return fmt.Errorf("runtime source: %w", err)
		}
	}
	if observation.ObservedAt.IsZero() {
		return errors.New("runtime observation time must not be zero")
	}
	if err := observation.Lifecycle.Validate(); err != nil {
		return err
	}
	if observation.Lifecycle == LifecycleEnded {
		if observation.EndedAt == nil || observation.EndedAt.IsZero() {
			return errors.New("ended runtime observation requires ended time")
		}
		if observation.EndedAt.Before(observation.Candidate.Runtime.RootProcess.StartedAt) {
			return errors.New("runtime ended time precedes process start")
		}
	} else if observation.EndedAt != nil {
		return errors.New("live runtime observation must not have ended time")
	}
	return nil
}

func (observation RuntimeObservation) Ref() string {
	return string(observation.Candidate.InstanceID)
}

type CorrelationInput struct {
	EvaluatedAt     time.Time            `json:"evaluated_at"`
	Runtimes        []RuntimeObservation `json:"runtimes"`
	Hooks           []HookObservation    `json:"hooks"`
	ExpectedMatches []ExpectedMatch      `json:"expected_matches,omitempty"`
}

func (input CorrelationInput) Validate() error {
	if input.EvaluatedAt.IsZero() {
		return errors.New("correlation evaluation time must not be zero")
	}
	seenRuntimes := make(map[string]struct{}, len(input.Runtimes))
	for index, runtime := range input.Runtimes {
		if err := runtime.Validate(); err != nil {
			return fmt.Errorf("runtime observation %d: %w", index, err)
		}
		if _, exists := seenRuntimes[runtime.Ref()]; exists {
			return fmt.Errorf("duplicate runtime reference %q", runtime.Ref())
		}
		seenRuntimes[runtime.Ref()] = struct{}{}
	}
	seenHooks := make(map[string]struct{}, len(input.Hooks))
	for index, hook := range input.Hooks {
		if err := hook.Validate(); err != nil {
			return fmt.Errorf("hook observation %d: %w", index, err)
		}
		seenHooks[hook.Ref()] = struct{}{}
	}
	seenLabels := make(map[string]struct{}, len(input.ExpectedMatches))
	for index, expected := range input.ExpectedMatches {
		if strings.TrimSpace(expected.HookRef) == "" {
			return fmt.Errorf("expected match %d has empty hook reference", index)
		}
		if expected.RuntimeRef != "" {
			if _, exists := seenRuntimes[expected.RuntimeRef]; !exists {
				return fmt.Errorf("expected match %d references unknown runtime %q", index, expected.RuntimeRef)
			}
		}
		if _, exists := seenHooks[expected.HookRef]; !exists {
			return fmt.Errorf("expected match %d references unknown hook %q", index, expected.HookRef)
		}
		if _, exists := seenLabels[expected.HookRef]; exists {
			return fmt.Errorf("duplicate expected match for hook %q", expected.HookRef)
		}
		seenLabels[expected.HookRef] = struct{}{}
	}
	return nil
}

// ExpectedMatch is optional opaque ground truth for fixture and local risk
// measurement. Empty RuntimeRef means that no proposal is expected.
type ExpectedMatch struct {
	HookRef    string `json:"hook_ref"`
	RuntimeRef string `json:"runtime_ref,omitempty"`
}

type Confidence string

const (
	ConfidenceExact     Confidence = "exact"
	ConfidenceStrong    Confidence = "strong"
	ConfidenceWeak      Confidence = "weak"
	ConfidenceAmbiguous Confidence = "ambiguous"
	ConfidenceRejected  Confidence = "rejected"
)

type ReasonCode string

const (
	ReasonExactProcessIdentity      ReasonCode = "exact_process_identity"
	ReasonExactRuntimeIdentity      ReasonCode = "exact_runtime_identity"
	ReasonRootProcessMatch          ReasonCode = "root_process_match"
	ReasonMemberProcessMatch        ReasonCode = "member_process_match"
	ReasonHostMatch                 ReasonCode = "host_match"
	ReasonBootMatch                 ReasonCode = "boot_match"
	ReasonToolMatch                 ReasonCode = "tool_match"
	ReasonProcessGroupMatch         ReasonCode = "process_group_match"
	ReasonOSSessionMatch            ReasonCode = "os_session_match"
	ReasonTerminalMatch             ReasonCode = "terminal_match"
	ReasonStartTimeClose            ReasonCode = "start_time_close"
	ReasonObservationTimeClose      ReasonCode = "observation_time_close"
	ReasonMissingProcessStartTime   ReasonCode = "missing_process_start_time"
	ReasonPIDOnlyHint               ReasonCode = "pid_only_hint"
	ReasonHostConflict              ReasonCode = "host_conflict"
	ReasonBootConflict              ReasonCode = "boot_conflict"
	ReasonProcessGenerationConflict ReasonCode = "process_generation_conflict"
	ReasonExplicitProcessConflict   ReasonCode = "explicit_process_conflict"
	ReasonToolConflict              ReasonCode = "tool_conflict"
	ReasonLifecycleConflict         ReasonCode = "lifecycle_conflict"
	ReasonStaleHook                 ReasonCode = "stale_hook"
	ReasonStaleRuntime              ReasonCode = "stale_runtime"
	ReasonOutOfOrderRevision        ReasonCode = "out_of_order_revision"
	ReasonSupersededRevision        ReasonCode = "superseded_revision"
	ReasonSameRevisionConflict      ReasonCode = "same_revision_conflict"
	ReasonIdempotencyConflict       ReasonCode = "idempotency_conflict"
	ReasonProducerEpochConflict     ReasonCode = "producer_epoch_conflict"
	ReasonInsufficientEvidence      ReasonCode = "insufficient_evidence"
	ReasonAmbiguousTopScore         ReasonCode = "ambiguous_top_score"
	ReasonCompetingHook             ReasonCode = "competing_hook"
	ReasonCompetingRuntime          ReasonCode = "competing_runtime"
	ReasonCandidateLimitExceeded    ReasonCode = "candidate_limit_exceeded"
)

type Reason struct {
	Code   ReasonCode `json:"code"`
	Points int        `json:"points"`
}

type MatchProposal struct {
	HookRef                        string     `json:"hook_ref"`
	RuntimeRef                     string     `json:"runtime_ref"`
	Score                          int        `json:"score"`
	Confidence                     Confidence `json:"confidence"`
	Reasons                        []Reason   `json:"reasons"`
	WouldBindUnderCurrentThreshold bool       `json:"would_bind_under_current_threshold"`
	RequiresReview                 bool       `json:"requires_review"`
}

type UnmatchedRuntime struct {
	RuntimeRef string       `json:"runtime_ref"`
	Reasons    []ReasonCode `json:"reasons"`
}

type UnmatchedHook struct {
	HookRef string       `json:"hook_ref"`
	Reasons []ReasonCode `json:"reasons"`
}

type AmbiguousMatch struct {
	HookRefs    []string     `json:"hook_refs"`
	RuntimeRefs []string     `json:"runtime_refs"`
	BestScore   int          `json:"best_score"`
	Confidence  Confidence   `json:"confidence"`
	Reasons     []ReasonCode `json:"reasons"`
}

type RejectedMatch struct {
	HookRef    string       `json:"hook_ref"`
	RuntimeRef string       `json:"runtime_ref,omitempty"`
	Reasons    []ReasonCode `json:"reasons"`
}

type SupersededHook struct {
	HookRef     string       `json:"hook_ref"`
	Epoch       string       `json:"epoch"`
	Revision    uint64       `json:"revision"`
	ReasonCodes []ReasonCode `json:"reason_codes"`
}

type Summary struct {
	Runtimes  int `json:"runtimes"`
	Hooks     int `json:"hooks"`
	Exact     int `json:"exact"`
	Strong    int `json:"strong"`
	Weak      int `json:"weak"`
	Ambiguous int `json:"ambiguous"`
	Rejected  int `json:"rejected"`
}

type RiskSummary struct {
	Labeled       int `json:"labeled"`
	TruePositive  int `json:"true_positive"`
	TrueNegative  int `json:"true_negative"`
	FalsePositive int `json:"false_positive"`
	FalseNegative int `json:"false_negative"`
}

type CorrelationResult struct {
	Summary           Summary            `json:"summary"`
	Risk              RiskSummary        `json:"risk"`
	Proposals         []MatchProposal    `json:"proposals"`
	Ambiguous         []AmbiguousMatch   `json:"ambiguous"`
	Rejected          []RejectedMatch    `json:"rejected"`
	UnmatchedHooks    []UnmatchedHook    `json:"unmatched_hooks"`
	UnmatchedRuntimes []UnmatchedRuntime `json:"unmatched_runtimes"`
	SupersededHooks   []SupersededHook   `json:"superseded_hooks"`
	Diagnostics       []ReasonCode       `json:"diagnostics"`
}

type Correlator interface {
	Correlate(CorrelationInput) (CorrelationResult, error)
}

func sortReasonCodes(values []ReasonCode) {
	sort.Slice(values, func(first, second int) bool { return values[first] < values[second] })
}

func uniqueReasonCodes(values []ReasonCode) []ReasonCode {
	seen := make(map[ReasonCode]struct{}, len(values))
	result := make([]ReasonCode, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sortReasonCodes(result)
	return result
}
