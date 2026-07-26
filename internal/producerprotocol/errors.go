package producerprotocol

import (
	"errors"
	"io"
	"net"
)

// ErrorCode is a stable, content-free classification of a protocol or
// transport failure, suitable for logging and metrics.
type ErrorCode string

const (
	CodeUnsupportedProtocolVersion ErrorCode = "unsupported_protocol_version"
	CodeMalformedMessage           ErrorCode = "malformed_message"
	CodeUnknownField               ErrorCode = "unknown_field"
	CodeInvalidTool                ErrorCode = "invalid_tool"
	CodeInvalidState               ErrorCode = "invalid_state"
	CodeInvalidInstanceID          ErrorCode = "invalid_instance_id"
	CodeInvalidProducerEpoch       ErrorCode = "invalid_producer_epoch"
	CodeInvalidRevision            ErrorCode = "invalid_revision"
	CodeInvalidTimestamp           ErrorCode = "invalid_timestamp"
	CodeMessageTooLarge            ErrorCode = "message_too_large"
	CodeToolMismatch               ErrorCode = "tool_mismatch"
	CodeUnauthorizedPeer           ErrorCode = "unauthorized_peer"
	CodeInsecureSocketPath         ErrorCode = "insecure_socket_path"
	CodeSocketAlreadyExists        ErrorCode = "socket_already_exists"
	CodePeerDisconnected           ErrorCode = "peer_disconnected"
	CodeReadTimeout                ErrorCode = "read_timeout"
	CodeWriteTimeout               ErrorCode = "write_timeout"
	CodeInternalError              ErrorCode = "internal_error"
)

var (
	ErrUnsupportedProtocolVersion = errors.New("unsupported protocol version")
	ErrMalformedMessage           = errors.New("malformed message")
	ErrUnknownField               = errors.New("unknown message field")
	ErrInvalidTool                = errors.New("invalid tool")
	ErrInvalidState               = errors.New("invalid state")
	ErrInvalidInstanceID          = errors.New("invalid instance ID")
	ErrInvalidRevision            = errors.New("invalid revision")
	ErrInvalidTimestamp           = errors.New("invalid timestamp")
	ErrMessageTooLarge            = errors.New("message exceeds maximum size")
	ErrToolMismatch               = errors.New("tool does not match the bound connection tool")
	ErrUnauthorizedPeer           = errors.New("unauthorized local peer")
	ErrInsecureSocketPath         = errors.New("insecure socket path")
	ErrSocketAlreadyExists        = errors.New("socket path already exists")
	ErrPeerDisconnected           = errors.New("peer disconnected")
	ErrReadTimeout                = errors.New("read timeout")
	ErrWriteTimeout               = errors.New("write timeout")
)

// ProtocolError pairs a stable ErrorCode with the underlying cause.
type ProtocolError struct {
	Code ErrorCode
	Err  error
}

func (err *ProtocolError) Error() string {
	if err == nil {
		return "protocol error"
	}
	return string(err.Code)
}

func (err *ProtocolError) Unwrap() error { return err.Err }

func protocolError(code ErrorCode, err error) error {
	return &ProtocolError{Code: code, Err: err}
}

// ErrorCodeOf classifies err into a stable ErrorCode for logging. It never
// returns the underlying error text.
func ErrorCodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var protocol *ProtocolError
	if errors.As(err, &protocol) {
		return protocol.Code
	}
	switch {
	case errors.Is(err, ErrUnauthorizedPeer):
		return CodeUnauthorizedPeer
	case errors.Is(err, ErrToolMismatch):
		return CodeToolMismatch
	case errors.Is(err, ErrMessageTooLarge):
		return CodeMessageTooLarge
	case errors.Is(err, ErrInsecureSocketPath):
		return CodeInsecureSocketPath
	case errors.Is(err, ErrSocketAlreadyExists):
		return CodeSocketAlreadyExists
	case errors.Is(err, ErrPeerDisconnected), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, io.ErrClosedPipe), errors.Is(err, net.ErrClosed):
		return CodePeerDisconnected
	case errors.Is(err, ErrReadTimeout):
		return CodeReadTimeout
	case errors.Is(err, ErrWriteTimeout):
		return CodeWriteTimeout
	default:
		return CodeInternalError
	}
}

// classifyIOError maps a raw net/io error into one of this package's
// sentinel errors so callers never need to inspect platform-specific error
// strings. The returned error's Error() text is always content-free (see
// wrappedError): a *net.OpError for a Unix domain socket formats its Addr,
// which is the socket path, so raw net/io errors must never be returned
// as-is from this package.
func classifyIOError(err error, read bool) error {
	if err == nil {
		return nil
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		if read {
			return &wrappedError{sentinel: ErrReadTimeout, cause: err}
		}
		return &wrappedError{sentinel: ErrWriteTimeout, cause: err}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return &wrappedError{sentinel: ErrPeerDisconnected, cause: err}
	}
	return wrapOpaque("io error", err)
}

// wrapOpaque returns an error whose Error() text is exactly message, with
// cause reachable only through Unwrap. Use it for any raw OS or net error
// that might embed a socket path, address, or other filesystem detail (see
// doc.go: this package must never leak a path through a log line).
func wrapOpaque(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &wrappedError{sentinel: errors.New(message), cause: cause}
}

// wrappedError lets classifyIOError and wrapOpaque attach a sentinel (via
// errors.Is) while preserving the original cause for Unwrap, without ever
// formatting the cause into the sentinel's own message: cause may be a
// *net.OpError or *fs.PathError carrying a socket path, and Error() must
// stay content-free regardless of what it wraps.
type wrappedError struct {
	sentinel error
	cause    error
}

func (err *wrappedError) Error() string        { return err.sentinel.Error() }
func (err *wrappedError) Is(target error) bool { return target == err.sentinel }
func (err *wrappedError) Unwrap() error        { return err.cause }
