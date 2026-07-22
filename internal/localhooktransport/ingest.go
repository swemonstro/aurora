package localhooktransport

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

const (
	// IngestProtocolVersion is the Package 6 ingress wire version.
	IngestProtocolVersion ProtocolVersion = 2

	// OperationIngestHookEvent is the Package 6 observe-only ingress operation.
	OperationIngestHookEvent Operation = "ingest_hook_event"

	DefaultIngestTotalBudget          = 100 * time.Millisecond
	DefaultIngestConnectDeadline      = 20 * time.Millisecond
	DefaultIngestWriteDeadline        = 20 * time.Millisecond
	DefaultIngestReadDeadline         = 60 * time.Millisecond
	DefaultIngestMaximumRequestBytes  = 8 * 1024
	DefaultIngestMaximumResponseBytes = 4 * 1024

	EnvLocalHookEnabled = "AURORA_LOCAL_HOOK_ENABLED"
	EnvLocalHookSocket  = "AURORA_LOCAL_HOOK_SOCKET"
	EnvXDGRuntimeDir    = "XDG_RUNTIME_DIR"

	defaultLocalHookSocketName = "presence-hook.sock"
	defaultLocalHookSocketDir  = "aurora"

	requestIDEntropyBytes = 16
)

const (
	CodeUnsupportedOperation       ErrorCode = "unsupported_operation"
	CodeInvalidIngress             ErrorCode = "invalid_ingress"
	CodeSequencingCapacityExceeded ErrorCode = "sequencing_capacity_exceeded"
	CodeRevisionOverflow           ErrorCode = "revision_overflow"
	CodeRequestInProgress          ErrorCode = "request_in_progress"
	CodeHandlerTimeout             ErrorCode = "handler_timeout"
)

// IngressPayload is the allowlisted Package 6 wire payload.
type IngressPayload struct {
	Tool           instancepresence.ToolKind       `json:"tool"`
	HookSessionRef instancepresence.OpaqueIdentity `json:"hook_session_ref"`
	Lifecycle      instancecorrelation.Lifecycle   `json:"lifecycle"`
}

// IngestRequest is the protocol version 2 local ingress request envelope.
type IngestRequest struct {
	ProtocolVersion ProtocolVersion `json:"protocol_version"`
	Operation       Operation       `json:"operation"`
	RequestID       string          `json:"request_id"`
	Payload         IngressPayload  `json:"payload"`
}

// IngestResponse is the content-free Package 6 response envelope.
type IngestResponse struct {
	ProtocolVersion    ProtocolVersion `json:"protocol_version"`
	RequestID          string          `json:"request_id,omitempty"`
	Status             ResponseStatus  `json:"status"`
	ErrorCodes         []ErrorCode     `json:"error_codes"`
	NoBindingPerformed bool            `json:"no_binding_performed"`
}

// IngestClientConfig holds bounded client limits for Package 6 delivery.
type IngestClientConfig struct {
	SocketPath             string
	TotalBudget            time.Duration
	ConnectDeadline        time.Duration
	WriteDeadline          time.Duration
	ReadDeadline           time.Duration
	MaximumRequestBytes    uint32
	MaximumResponseBytes   uint32
	MaximumRequestIDLength int
	MaximumOpaqueLength    int
	ExpectedUID            uint32
}

func DefaultIngestClientConfig() IngestClientConfig {
	return IngestClientConfig{
		TotalBudget:            DefaultIngestTotalBudget,
		ConnectDeadline:        DefaultIngestConnectDeadline,
		WriteDeadline:          DefaultIngestWriteDeadline,
		ReadDeadline:           DefaultIngestReadDeadline,
		MaximumRequestBytes:    DefaultIngestMaximumRequestBytes,
		MaximumResponseBytes:   DefaultIngestMaximumResponseBytes,
		MaximumRequestIDLength: 64,
		MaximumOpaqueLength:    128,
	}
}

func (config IngestClientConfig) Validate() error {
	if strings.TrimSpace(config.SocketPath) == "" {
		return errors.New("socket path must not be empty")
	}
	if err := validateSocketPathSyntax(config.SocketPath); err != nil {
		return err
	}
	if config.TotalBudget <= 0 || config.TotalBudget > DefaultIngestTotalBudget {
		return errors.New("total budget must be positive and at most 100ms")
	}
	if config.ConnectDeadline <= 0 || config.ConnectDeadline > DefaultIngestConnectDeadline {
		return errors.New("connect deadline must be positive and at most 20ms")
	}
	if config.WriteDeadline <= 0 || config.WriteDeadline > DefaultIngestWriteDeadline {
		return errors.New("write deadline must be positive and at most 20ms")
	}
	if config.ReadDeadline <= 0 || config.ReadDeadline > DefaultIngestReadDeadline {
		return errors.New("read deadline must be positive and at most 60ms")
	}
	if config.MaximumRequestBytes == 0 || config.MaximumRequestBytes > DefaultIngestMaximumRequestBytes {
		return errors.New("maximum request bytes must be between 1 and 8192")
	}
	if config.MaximumResponseBytes == 0 || config.MaximumResponseBytes > DefaultIngestMaximumResponseBytes {
		return errors.New("maximum response bytes must be between 1 and 4096")
	}
	if config.MaximumRequestIDLength < 8 || config.MaximumRequestIDLength > 128 {
		return errors.New("request ID limit is outside supported bounds")
	}
	if config.MaximumOpaqueLength < 8 || config.MaximumOpaqueLength > 256 {
		return errors.New("opaque identity limit is outside supported bounds")
	}
	return nil
}

// LocalHookEnabled reports whether Package 6 local delivery is enabled.
// Only "1" or ASCII case-insensitive "true" after surrounding whitespace trim
// enable the feature. All other values, including empty, disable it.
func LocalHookEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// ResolveLocalHookSocket selects the absolute socket path for one invocation.
// Explicit AURORA_LOCAL_HOOK_SOCKET wins when absolute and normalized.
// Otherwise only $XDG_RUNTIME_DIR/aurora/presence-hook.sock is allowed when
// XDG_RUNTIME_DIR is absolute and cleaned. Relative paths and /tmp defaults
// are rejected.
func ResolveLocalHookSocket(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", ErrInsecureSocketPath
	}
	if explicit := strings.TrimSpace(getenv(EnvLocalHookSocket)); explicit != "" {
		if !filepath.IsAbs(explicit) || filepath.Clean(explicit) != explicit {
			return "", ErrInsecureSocketPath
		}
		if strings.HasPrefix(explicit+string(filepath.Separator), "/tmp"+string(filepath.Separator)) || explicit == "/tmp" {
			return "", ErrInsecureSocketPath
		}
		if err := validateSocketPathSyntax(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	runtimeDir := strings.TrimSpace(getenv(EnvXDGRuntimeDir))
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) || filepath.Clean(runtimeDir) != runtimeDir {
		return "", ErrInsecureSocketPath
	}
	if strings.HasPrefix(runtimeDir+string(filepath.Separator), "/tmp"+string(filepath.Separator)) || runtimeDir == "/tmp" {
		return "", ErrInsecureSocketPath
	}
	socketPath := filepath.Join(runtimeDir, defaultLocalHookSocketDir, defaultLocalHookSocketName)
	if err := validateSocketPathSyntax(socketPath); err != nil {
		return "", err
	}
	return socketPath, nil
}

// NewRequestID creates a cryptographically random opaque transport ID with at
// least 128 bits of entropy. Weak or deterministic fallbacks are forbidden.
func NewRequestID() (string, error) {
	var entropy [requestIDEntropyBytes]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}
	return hex.EncodeToString(entropy[:]), nil
}

// IngressPayloadFromObservation projects the transport-neutral ingress into
// the wire payload without attaching epoch, revision, or time fields.
func IngressPayloadFromObservation(ingress hookadapter.IngressObservation) (IngressPayload, error) {
	if err := ingress.Validate(); err != nil {
		return IngressPayload{}, err
	}
	return IngressPayload{
		Tool:           ingress.Tool,
		HookSessionRef: ingress.HookSessionRef,
		Lifecycle:      ingress.Lifecycle,
	}, nil
}

// NewIngestRequest builds a protocol version 2 request for one delivery attempt.
func NewIngestRequest(ingress hookadapter.IngressObservation) (IngestRequest, error) {
	payload, err := IngressPayloadFromObservation(ingress)
	if err != nil {
		return IngestRequest{}, err
	}
	requestID, err := NewRequestID()
	if err != nil {
		return IngestRequest{}, err
	}
	return IngestRequest{
		ProtocolVersion: IngestProtocolVersion,
		Operation:       OperationIngestHookEvent,
		RequestID:       requestID,
		Payload:         payload,
	}, nil
}

func EncodeIngestRequestJSON(request IngestRequest) ([]byte, error) {
	return json.Marshal(request)
}

func DecodeIngestRequestJSON(data []byte) (IngestRequest, error) {
	if len(data) == 0 {
		return IngestRequest{}, protocolError(CodeMalformedRequest, errors.New("empty request"))
	}
	var request IngestRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		code := CodeMalformedRequest
		if strings.Contains(err.Error(), "unknown field") {
			code = CodeUnknownField
		}
		return IngestRequest{}, protocolError(code, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return IngestRequest{}, protocolError(CodeMalformedRequest, err)
	}
	return request, nil
}

func EncodeIngestResponseJSON(response IngestResponse, maximum uint32) ([]byte, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) > uint64(maximum) {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func DecodeIngestResponseJSON(data []byte) (IngestResponse, error) {
	if len(data) == 0 {
		return IngestResponse{}, protocolError(CodeMalformedRequest, errors.New("empty response"))
	}
	var response IngestResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		code := CodeMalformedRequest
		if strings.Contains(err.Error(), "unknown field") {
			code = CodeUnknownField
		}
		return IngestResponse{}, protocolError(code, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return IngestResponse{}, protocolError(CodeMalformedRequest, err)
	}
	return response, nil
}

// ValidateIngestRequest checks the versioned envelope and minimal ingress.
func ValidateIngestRequest(config IngestClientConfig, request IngestRequest) error {
	if request.ProtocolVersion != IngestProtocolVersion {
		return protocolError(CodeUnsupportedProtocolVersion, fmt.Errorf("unsupported version"))
	}
	if request.Operation != OperationIngestHookEvent {
		return protocolError(CodeUnsupportedOperation, fmt.Errorf("unsupported operation"))
	}
	if err := validateOpaque("request ID", request.RequestID, config.MaximumRequestIDLength, true); err != nil {
		return protocolError(CodeInvalidRequestID, err)
	}
	if err := validateOpaque("hook session reference", string(request.Payload.HookSessionRef), config.MaximumOpaqueLength, true); err != nil {
		return protocolError(CodeInvalidIngress, err)
	}
	if err := request.Payload.Tool.Validate(); err != nil {
		return protocolError(CodeInvalidIngress, err)
	}
	if err := request.Payload.Lifecycle.Validate(); err != nil {
		return protocolError(CodeInvalidIngress, err)
	}
	return nil
}

func ValidateIngestResponse(response IngestResponse, requestID string) error {
	if response.ProtocolVersion != IngestProtocolVersion {
		return protocolError(CodeUnsupportedProtocolVersion, fmt.Errorf("unsupported response version"))
	}
	switch response.Status {
	case StatusOK, StatusDuplicate, StatusRejected, StatusError:
	default:
		return protocolError(CodeMalformedRequest, fmt.Errorf("unsupported response status"))
	}
	if !response.NoBindingPerformed {
		return protocolError(CodeMalformedRequest, fmt.Errorf("response must report no binding"))
	}
	if response.RequestID != "" && response.RequestID != requestID {
		return protocolError(CodeInvalidRequestID, fmt.Errorf("response request ID mismatch"))
	}
	if response.ErrorCodes == nil {
		return protocolError(CodeMalformedRequest, fmt.Errorf("response error codes missing"))
	}
	return nil
}

func clientLatencyBucket(duration time.Duration, timedOut bool) string {
	if timedOut {
		return "timeout"
	}
	switch {
	case duration < 10*time.Millisecond:
		return "lt_10ms"
	case duration < 50*time.Millisecond:
		return "lt_50ms"
	case duration < 100*time.Millisecond:
		return "lt_100ms"
	default:
		return "timeout"
	}
}

func emptyIngestResponse(status ResponseStatus, requestID string, codes ...ErrorCode) IngestResponse {
	return IngestResponse{
		ProtocolVersion:    IngestProtocolVersion,
		RequestID:          requestID,
		Status:             status,
		ErrorCodes:         append([]ErrorCode{}, codes...),
		NoBindingPerformed: true,
	}
}
