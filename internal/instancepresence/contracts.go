package instancepresence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BootIdentity is an opaque, per-boot value. It must not expose a raw platform
// identifier outside the local adapter boundary.
type BootIdentity string

func (identity BootIdentity) Validate() error {
	if strings.TrimSpace(string(identity)) == "" {
		return errors.New("boot identity must not be empty")
	}
	return nil
}

type OpaqueIdentity string

type ProcessObservation struct {
	Process             ProcessIdentity  `json:"process"`
	Parent              *ProcessIdentity `json:"parent,omitempty"`
	ExecutableIdentity  OpaqueIdentity   `json:"executable_identity"`
	ProcessGroupOrJob   OpaqueIdentity   `json:"process_group_or_job,omitempty"`
	OSSession           OpaqueIdentity   `json:"os_session,omitempty"`
	TerminalFingerprint OpaqueIdentity   `json:"terminal_fingerprint,omitempty"`
	CWDFingerprint      OpaqueIdentity   `json:"cwd_fingerprint,omitempty"`
	OwnerIdentity       OpaqueIdentity   `json:"owner_identity"`
}

func (observation ProcessObservation) Validate() error {
	if err := observation.Process.Validate(); err != nil {
		return err
	}
	if observation.Parent != nil {
		if err := observation.Parent.Validate(); err != nil {
			return fmt.Errorf("parent process: %w", err)
		}
	}
	if observation.ExecutableIdentity == "" {
		return errors.New("executable identity must not be empty")
	}
	if observation.OwnerIdentity == "" {
		return errors.New("owner identity must not be empty")
	}
	return nil
}

type ProcessSnapshot struct {
	ObservedAt time.Time            `json:"observed_at"`
	Processes  []ProcessObservation `json:"processes"`
}

func (snapshot ProcessSnapshot) Validate() error {
	if snapshot.ObservedAt.IsZero() {
		return errors.New("process snapshot observation time must not be zero")
	}
	for index, process := range snapshot.Processes {
		if err := process.Validate(); err != nil {
			return fmt.Errorf("process snapshot observation %d: %w", index, err)
		}
		for previous := 0; previous < index; previous++ {
			if sameProcessIdentity(snapshot.Processes[previous].Process, process.Process) {
				return fmt.Errorf("duplicate process identity at observations %d and %d", previous, index)
			}
		}
	}
	return nil
}

type ProcessExit struct {
	Process    ProcessIdentity
	ObservedAt time.Time
}

func (exit ProcessExit) Validate() error {
	if err := exit.Process.Validate(); err != nil {
		return fmt.Errorf("exited process: %w", err)
	}
	if exit.ObservedAt.IsZero() {
		return errors.New("process exit observation time must not be zero")
	}
	return nil
}

type ExitSubscription interface {
	Next(context.Context) (ProcessExit, error)
	Close() error
}

// ProcessAdapter is implemented per OS and exposes only allowlisted metadata.
type ProcessAdapter interface {
	BootIdentity(context.Context) (BootIdentity, error)
	Snapshot(context.Context) (ProcessSnapshot, error)
}

// ProcessExitObserver is an optional adapter capability. Adapters without it
// rely on successive snapshots; callers discover it with a type assertion.
type ProcessExitObserver interface {
	WatchExits(context.Context) (ExitSubscription, error)
}

type RuntimeCandidate struct {
	InstanceID InstanceID
	Tool       ToolKind
	Runtime    RuntimeIdentity
	Members    []ProcessIdentity
}

func (candidate RuntimeCandidate) Validate() error {
	if err := candidate.InstanceID.Validate(); err != nil {
		return err
	}
	if err := candidate.Tool.Validate(); err != nil {
		return err
	}
	if err := candidate.Runtime.Validate(); err != nil {
		return err
	}
	if len(candidate.Members) == 0 {
		return errors.New("runtime candidate must contain process members")
	}
	rootCount := 0
	for index, member := range candidate.Members {
		if err := member.Validate(); err != nil {
			return fmt.Errorf("runtime candidate member %d: %w", index, err)
		}
		if sameProcessIdentity(member, candidate.Runtime.RootProcess) {
			rootCount++
		}
		for previous := 0; previous < index; previous++ {
			if sameProcessIdentity(candidate.Members[previous], member) {
				return fmt.Errorf("duplicate runtime candidate members %d and %d", previous, index)
			}
		}
	}
	if rootCount != 1 {
		return fmt.Errorf("runtime root process must occur exactly once in members; got %d", rootCount)
	}
	return nil
}

func sameProcessIdentity(first, second ProcessIdentity) bool {
	return first.PID == second.PID && first.StartedAt.Equal(second.StartedAt)
}

type HookObservation struct {
	Tool                  ToolKind          `json:"tool"`
	HookSessionRef        OpaqueIdentity    `json:"hook_session_ref"`
	ObservedAt            time.Time         `json:"observed_at"`
	VerifiedAncestors     []ProcessIdentity `json:"verified_ancestors,omitempty"`
	ProcessGroupOrJob     OpaqueIdentity    `json:"process_group_or_job,omitempty"`
	OSSession             OpaqueIdentity    `json:"os_session,omitempty"`
	TerminalFingerprint   OpaqueIdentity    `json:"terminal_fingerprint,omitempty"`
	CWDFingerprint        OpaqueIdentity    `json:"cwd_fingerprint,omitempty"`
	TranscriptFingerprint OpaqueIdentity    `json:"transcript_fingerprint,omitempty"`
}

func (observation HookObservation) Validate() error {
	if err := observation.Tool.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(string(observation.HookSessionRef)) == "" {
		return errors.New("hook session reference must not be empty")
	}
	if observation.ObservedAt.IsZero() {
		return errors.New("hook observation time must not be zero")
	}
	for index, ancestor := range observation.VerifiedAncestors {
		if err := ancestor.Validate(); err != nil {
			return fmt.Errorf("verified hook ancestor %d: %w", index, err)
		}
	}
	return nil
}

type CorrelationOutcome string

const (
	CorrelationUniquelyMatched CorrelationOutcome = "uniquely_matched"
	CorrelationAmbiguous       CorrelationOutcome = "ambiguous"
	CorrelationUnmatched       CorrelationOutcome = "unmatched"
)

func (evidence EvidenceKind) Validate() error {
	switch evidence {
	case EvidenceExistingBinding, EvidenceAncestor, EvidenceProcessGroup,
		EvidenceOSSession, EvidenceTerminal, EvidenceCWD, EvidenceTranscript:
		return nil
	default:
		return fmt.Errorf("unsupported correlation evidence %q", evidence)
	}
}

type EvidenceKind string

const (
	EvidenceExistingBinding EvidenceKind = "existing_binding"
	EvidenceAncestor        EvidenceKind = "verified_ancestor"
	EvidenceProcessGroup    EvidenceKind = "process_group_or_job"
	EvidenceOSSession       EvidenceKind = "os_session"
	EvidenceTerminal        EvidenceKind = "terminal_fingerprint"
	EvidenceCWD             EvidenceKind = "cwd_fingerprint"
	EvidenceTranscript      EvidenceKind = "transcript_fingerprint"
)

type CorrelationResult struct {
	Outcome    CorrelationOutcome `json:"outcome"`
	InstanceID InstanceID         `json:"instance_id,omitempty"`
	Candidates []InstanceID       `json:"candidates"`
	Evidence   []EvidenceKind     `json:"evidence,omitempty"`
}

func (result CorrelationResult) Validate() error {
	seen := make(map[InstanceID]struct{}, len(result.Candidates))
	for _, candidate := range result.Candidates {
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("correlation candidate: %w", err)
		}
		if _, exists := seen[candidate]; exists {
			return fmt.Errorf("duplicate correlation candidate %q", candidate)
		}
		seen[candidate] = struct{}{}
	}
	seenEvidence := make(map[EvidenceKind]struct{}, len(result.Evidence))
	for _, evidence := range result.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
		if _, exists := seenEvidence[evidence]; exists {
			return fmt.Errorf("duplicate correlation evidence %q", evidence)
		}
		seenEvidence[evidence] = struct{}{}
	}
	switch result.Outcome {
	case CorrelationUniquelyMatched:
		if err := result.InstanceID.Validate(); err != nil {
			return fmt.Errorf("unique correlation: %w", err)
		}
		if len(result.Candidates) != 1 || result.Candidates[0] != result.InstanceID {
			return errors.New("unique correlation must contain exactly its matched candidate")
		}
		if len(result.Evidence) == 0 {
			return errors.New("unique correlation requires evidence")
		}
	case CorrelationAmbiguous:
		if result.InstanceID != "" {
			return errors.New("ambiguous correlation must not select an instance")
		}
		if len(result.Candidates) < 2 {
			return errors.New("ambiguous correlation requires at least two candidates")
		}
	case CorrelationUnmatched:
		if result.InstanceID != "" || len(result.Candidates) != 0 {
			return errors.New("unmatched correlation must not select candidates")
		}
	default:
		return fmt.Errorf("unsupported correlation outcome %q", result.Outcome)
	}
	return nil
}

func (result CorrelationResult) Target() (InstanceID, bool) {
	if result.Validate() != nil || result.Outcome != CorrelationUniquelyMatched {
		return "", false
	}
	return result.InstanceID, true
}

type Correlator interface {
	Correlate(HookObservation, []RuntimeCandidate) CorrelationResult
}

type SlotCoordinator interface {
	Assign(context.Context, InstanceID, string) (Slot, error)
	Release(context.Context, InstanceID, time.Time) error
}

type Clock interface {
	Now() time.Time
}
