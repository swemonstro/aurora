package producerprotocol

import (
	"errors"
	"strings"
	"time"
)

// Clock is the sole time source used by this package, so tests never depend
// on wall-clock time.
type Clock interface {
	Now() time.Time
}

// Config bounds the wire protocol and its transport. All limits are hard
// caps: values outside the supported range are rejected by Validate rather
// than silently clamped.
type Config struct {
	// SocketPath is the local Unix domain socket path. Required for
	// transport use; irrelevant to message validation alone.
	SocketPath string

	// MaximumMessageBytes hard-bounds one encoded message (and therefore one
	// frame body). Messages larger than this are rejected before decode.
	MaximumMessageBytes uint32

	// MaximumInstanceIDLength bounds InstanceID.
	MaximumInstanceIDLength int

	// MaximumRevision bounds Revision. Zero disables the upper bound (the
	// lower bound of "greater than zero" always applies).
	MaximumRevision uint64

	// ReadTimeout and WriteTimeout bound one frame read or write on a Conn.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// DialTimeout bounds establishing a new client connection.
	DialTimeout time.Duration

	// BoundTool, when non-empty, is the single tool a Listener binds every
	// accepted Conn to before returning it (the "one socket, one tool"
	// primitive). Leave empty for a socket shared across tools, where the
	// caller binds each Conn itself (e.g. via an IdentityBinder).
	BoundTool Tool

	Clock Clock
}

// DefaultConfig returns conservative, production-safe defaults. The message
// schema is small, so the byte ceiling is far below the framing protocol's
// absolute maximum.
func DefaultConfig(clock Clock) Config {
	return Config{
		MaximumMessageBytes:     4096,
		MaximumInstanceIDLength: 128,
		MaximumRevision:         1<<53 - 1, // JSON-number-safe upper bound for cross-language consumers.
		ReadTimeout:             2 * time.Second,
		WriteTimeout:            2 * time.Second,
		DialTimeout:             2 * time.Second,
		Clock:                   clock,
	}
}

// Validate checks the configuration. requireSocket should be true for any
// transport use (Listen/Dial) and false when Config is only used to bound
// message validation.
func (config Config) Validate(requireSocket bool) error {
	if requireSocket && strings.TrimSpace(config.SocketPath) == "" {
		return errors.New("socket path must not be empty")
	}
	if config.MaximumMessageBytes == 0 || config.MaximumMessageBytes > 65536 {
		return errors.New("maximum message bytes must be between 1 and 65536")
	}
	if config.MaximumInstanceIDLength < 8 || config.MaximumInstanceIDLength > 256 {
		return errors.New("maximum instance ID length is outside supported bounds")
	}
	if requireSocket {
		if config.ReadTimeout <= 0 || config.WriteTimeout <= 0 || config.DialTimeout <= 0 {
			return errors.New("transport timeouts must be positive")
		}
	}
	if config.BoundTool != "" {
		if err := config.BoundTool.Validate(); err != nil {
			return err
		}
	}
	if config.Clock == nil {
		return errors.New("clock must not be nil")
	}
	return nil
}
