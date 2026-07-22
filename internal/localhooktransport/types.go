// Package localhooktransport implements a bounded, observe-only local hook
// protocol. It never mutates presence state, registry entries, or slots.
package localhooktransport

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

type ProtocolVersion uint16

const CurrentProtocolVersion ProtocolVersion = 1

type Operation string

const OperationCorrelateObservation Operation = "correlate_observation"

type ResponseStatus string

const (
	StatusOK        ResponseStatus = "ok"
	StatusDuplicate ResponseStatus = "duplicate"
	StatusRejected  ResponseStatus = "rejected"
	StatusError     ResponseStatus = "error"
)

type ErrorCode string

const (
	CodeUnsupportedProtocolVersion ErrorCode = "unsupported_protocol_version"
	CodeMalformedRequest           ErrorCode = "malformed_request"
	CodeRequestTooLarge            ErrorCode = "request_too_large"
	CodeResponseTooLarge           ErrorCode = "response_too_large"
	CodeUnknownField               ErrorCode = "unknown_field"
	CodeInvalidRequestID           ErrorCode = "invalid_request_id"
	CodeUnauthorizedPeer           ErrorCode = "unauthorized_peer"
	CodeInsecureSocketPath         ErrorCode = "insecure_socket_path"
	CodeSocketAlreadyExists        ErrorCode = "socket_already_exists"
	CodePeerDisconnected           ErrorCode = "peer_disconnected"
	CodeReadTimeout                ErrorCode = "read_timeout"
	CodeWriteTimeout               ErrorCode = "write_timeout"
	CodeConcurrencyLimit           ErrorCode = "concurrency_limit"
	CodeInvalidHookObservation     ErrorCode = "invalid_hook_observation"
	CodeStaleObservation           ErrorCode = "stale_observation"
	CodeCorrelationFailed          ErrorCode = "correlation_failed"
	CodeAmbiguousResult            ErrorCode = "ambiguous_result"
	CodeInsufficientEvidence       ErrorCode = "insufficient_evidence"
	CodeInternalError              ErrorCode = "internal_error"
	CodeDuplicateRequest           ErrorCode = "duplicate_request"
	CodeRequestIDConflict          ErrorCode = "request_id_conflict"
	CodeReplayCacheFull            ErrorCode = "replay_cache_full"
)

var (
	ErrUnauthorizedPeer    = errors.New("unauthorized local peer")
	ErrInsecureSocketPath  = errors.New("insecure socket path")
	ErrSocketAlreadyExists = errors.New("socket path already exists")
	ErrRequestTooLarge     = errors.New("request frame too large")
	ErrResponseTooLarge    = errors.New("response frame too large")
	ErrPeerDisconnected    = errors.New("peer disconnected")
	ErrReadTimeout         = errors.New("local transport read timeout")
	ErrWriteTimeout        = errors.New("local transport write timeout")
	ErrReplayConflict      = errors.New("request ID replay conflict")
	ErrReplayCacheFull     = errors.New("replay cache full")
	ErrRuntimeLimit        = errors.New("runtime observation limit exceeded")
)

type ProcessIdentity struct {
	PID       uint64    `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func (identity ProcessIdentity) domain() instancepresence.ProcessIdentity {
	return instancepresence.ProcessIdentity{PID: identity.PID, StartedAt: canonicalTime(identity.StartedAt)}
}

type RuntimeIdentity struct {
	HostID      string                        `json:"host_id"`
	BootID      instancepresence.BootIdentity `json:"boot_id"`
	RootProcess ProcessIdentity               `json:"root_process"`
}

func (identity RuntimeIdentity) domain() instancepresence.RuntimeIdentity {
	return instancepresence.RuntimeIdentity{
		HostID: identity.HostID, BootID: identity.BootID, RootProcess: identity.RootProcess.domain(),
	}
}

// HookObservation is the allowlisted transport form. It deliberately has no
// arbitrary metadata map and no prompt, path, command, environment, or UID.
type HookObservation struct {
	Tool                instancepresence.ToolKind       `json:"tool"`
	HookSessionRef      instancepresence.OpaqueIdentity `json:"hook_session_ref"`
	ProducerEpoch       instancepresence.ProducerEpoch  `json:"producer_epoch"`
	Revision            uint64                          `json:"revision"`
	IdempotencyKey      string                          `json:"idempotency_key,omitempty"`
	ObservedAt          time.Time                       `json:"observed_at"`
	Lifecycle           instancecorrelation.Lifecycle   `json:"lifecycle"`
	ProcessHint         *ProcessIdentity                `json:"process_hint,omitempty"`
	RuntimeHint         *RuntimeIdentity                `json:"runtime_hint,omitempty"`
	ParentOrRootPIDHint *uint64                         `json:"parent_or_root_pid_hint,omitempty"`
	ProcessGroupOrJob   instancepresence.OpaqueIdentity `json:"process_group_or_job,omitempty"`
	OSSession           instancepresence.OpaqueIdentity `json:"os_session,omitempty"`
	TerminalFingerprint instancepresence.OpaqueIdentity `json:"terminal_fingerprint,omitempty"`
	HostID              string                          `json:"host_id,omitempty"`
	BootID              instancepresence.BootIdentity   `json:"boot_id,omitempty"`
}

func (observation HookObservation) domain() instancecorrelation.HookObservation {
	var processHint *instancepresence.ProcessIdentity
	if observation.ProcessHint != nil {
		value := observation.ProcessHint.domain()
		processHint = &value
	}
	var runtimeHint *instancepresence.RuntimeIdentity
	if observation.RuntimeHint != nil {
		value := observation.RuntimeHint.domain()
		runtimeHint = &value
	}
	return instancecorrelation.HookObservation{
		Tool: observation.Tool, HookSessionRef: observation.HookSessionRef,
		ProducerEpoch: observation.ProducerEpoch, Revision: observation.Revision,
		IdempotencyKey: observation.IdempotencyKey, ObservedAt: canonicalTime(observation.ObservedAt),
		Lifecycle: observation.Lifecycle, ProcessHint: processHint, RuntimeHint: runtimeHint,
		ParentOrRootPIDHint: observation.ParentOrRootPIDHint,
		ProcessGroupOrJob:   observation.ProcessGroupOrJob, OSSession: observation.OSSession,
		TerminalFingerprint: observation.TerminalFingerprint,
		HostID:              observation.HostID, BootID: observation.BootID,
	}
}

type Request struct {
	ProtocolVersion ProtocolVersion `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	Operation       Operation       `json:"operation"`
	Observation     HookObservation `json:"observation"`
}

type Proposal struct {
	HookRef    string                         `json:"hook_ref"`
	RuntimeRef string                         `json:"runtime_ref"`
	Score      int                            `json:"score"`
	Confidence instancecorrelation.Confidence `json:"confidence"`
	Reasons    []instancecorrelation.Reason   `json:"reasons"`
	WouldBind  bool                           `json:"would_bind_under_current_threshold"`
	Review     bool                           `json:"requires_review"`
}

type Ambiguous struct {
	HookRefs    []string                         `json:"hook_refs"`
	RuntimeRefs []string                         `json:"runtime_refs"`
	BestScore   int                              `json:"best_score"`
	Confidence  instancecorrelation.Confidence   `json:"confidence"`
	Reasons     []instancecorrelation.ReasonCode `json:"reasons"`
}

type Rejected struct {
	HookRef    string                           `json:"hook_ref"`
	RuntimeRef string                           `json:"runtime_ref,omitempty"`
	Reasons    []instancecorrelation.ReasonCode `json:"reasons"`
}

type Unmatched struct {
	Ref     string                           `json:"ref"`
	Reasons []instancecorrelation.ReasonCode `json:"reasons"`
}

type Response struct {
	ProtocolVersion    ProtocolVersion             `json:"protocol_version"`
	RequestID          string                      `json:"request_id,omitempty"`
	Status             ResponseStatus              `json:"status"`
	ErrorCodes         []ErrorCode                 `json:"error_codes"`
	Summary            instancecorrelation.Summary `json:"summary"`
	Proposals          []Proposal                  `json:"proposals"`
	Ambiguous          []Ambiguous                 `json:"ambiguous"`
	Rejected           []Rejected                  `json:"rejected"`
	UnmatchedHooks     []Unmatched                 `json:"unmatched_hooks"`
	UnmatchedRuntimes  []Unmatched                 `json:"unmatched_runtimes"`
	NoBindingPerformed bool                        `json:"no_binding_performed"`
}

func emptyResponse(status ResponseStatus, requestID string, codes ...ErrorCode) Response {
	return Response{
		ProtocolVersion: CurrentProtocolVersion, RequestID: requestID, Status: status,
		ErrorCodes: append([]ErrorCode{}, codes...), Proposals: []Proposal{}, Ambiguous: []Ambiguous{},
		Rejected: []Rejected{}, UnmatchedHooks: []Unmatched{}, UnmatchedRuntimes: []Unmatched{},
		NoBindingPerformed: true,
	}
}

type PeerIdentity struct {
	UID uint32
	GID uint32
	PID int32
}

type Authenticator interface {
	Authenticate(PeerIdentity) error
}

type Clock interface {
	Now() time.Time
}

func canonicalTime(value time.Time) time.Time { return value.Round(0).UTC() }

func validateOpaque(name, value string, maximum int, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s must not be empty", name)
		}
		return nil
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds maximum length", name)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not contain surrounding whitespace", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._:@-", character) {
			continue
		}
		return fmt.Errorf("%s contains unsupported characters", name)
	}
	return nil
}
