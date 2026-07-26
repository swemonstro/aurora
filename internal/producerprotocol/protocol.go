package producerprotocol

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProtocolVersion identifies the wire schema. Changing the meaning of an
// existing field, or adding a field, requires a new version; this package
// never accepts an unsupported version.
type ProtocolVersion uint16

// CurrentProtocolVersion is the only version this package accepts on decode.
//
// Version 2 added ProducerEpoch (see below) and is not wire-compatible with
// version 1: decoding rejects unknown fields and requires every field
// DisallowUnknownFields does not default, so a v1 message (no
// producer_epoch) fails ProducerEpoch.Validate, and a v2 message decoded by
// a v1-only reader would have been rejected as an unknown field. This is
// the intentional "adding a field is always a protocol_version change"
// contract from doc.go, not a bug.
const CurrentProtocolVersion ProtocolVersion = 2

// Tool is a closed enum naming the presence producer. This package does not
// special-case any value here: adding a tool is a data change, not a code
// change to validation, framing, or transport.
type Tool string

const (
	ToolClaude Tool = "claude"
	ToolCodex  Tool = "codex"
	ToolGrok   Tool = "grok"
)

// Validate reports whether the tool is one of the closed set of known
// values. Unknown tools are rejected rather than passed through.
func (tool Tool) Validate() error {
	switch tool {
	case ToolClaude, ToolCodex, ToolGrok:
		return nil
	default:
		return fmt.Errorf("unsupported tool %q", tool)
	}
}

// State is a closed enum for the producer-reported effective state.
type State string

const (
	StateIdle      State = "idle"
	StateWorking   State = "working"
	StateAttention State = "attention"
	StateError     State = "error"
)

// Validate reports whether the state is one of the closed set of known
// values.
func (state State) Validate() error {
	switch state {
	case StateIdle, StateWorking, StateAttention, StateError:
		return nil
	default:
		return fmt.Errorf("unsupported state %q", state)
	}
}

// InstanceID is an opaque producer-assigned identifier. It carries no
// semantics beyond identity and must never be treated as a session, path, or
// credential value.
type InstanceID string

// Revision is a producer-owned counter. This package only requires it to be
// positive; ordering and monotonicity across messages is enforced by the
// consumer (a future broker), not by this transport-neutral package.
type Revision uint64

// ProducerEpoch is an opaque value a producer assigns once per process
// generation and never changes for the lifetime of that generation. It
// exists because Revision alone cannot distinguish two situations a
// consumer must tell apart:
//
//   - stale or out-of-order delivery of an already-seen generation, where a
//     lower Revision must never be allowed to overwrite newer state, and
//   - a legitimate producer restart, where Revision resets (typically to 1)
//     and must NOT be permanently rejected as "older than what we already
//     have" just because the raw number went down.
//
// A consumer compares Revision only within a matching ProducerEpoch; a
// changed epoch is a signal that this is a new generation, handled
// separately from ordinary revision comparison (see the broker package for
// how that signal is used — this package only carries and validates the
// value, it does not interpret restarts).
type ProducerEpoch string

// Validate reports whether the producer epoch is non-empty. Beyond that,
// this package treats it as fully opaque.
func (epoch ProducerEpoch) Validate() error {
	if strings.TrimSpace(string(epoch)) == "" {
		return errors.New("producer epoch must not be empty")
	}
	return nil
}

// Message is the wire form of one producer presence report.
type Message struct {
	ProtocolVersion ProtocolVersion `json:"protocol_version"`
	Tool            Tool            `json:"tool"`
	InstanceID      InstanceID      `json:"instance_id"`
	ProducerEpoch   ProducerEpoch   `json:"producer_epoch"`
	State           State           `json:"state"`
	Revision        Revision        `json:"revision"`
	ObservedAt      time.Time       `json:"observed_at"`
	LeaseExpiresAt  time.Time       `json:"lease_expires_at"`
}

// canonicalTime normalizes a timestamp to a monotonic-free UTC value so that
// equal instants compare equal regardless of wall-clock representation or
// original time zone.
func canonicalTime(value time.Time) time.Time { return value.Round(0).UTC() }

// CanonicalMessage returns msg with its timestamps normalized to UTC. It does
// not validate the message.
func CanonicalMessage(msg Message) Message {
	msg.ObservedAt = canonicalTime(msg.ObservedAt)
	msg.LeaseExpiresAt = canonicalTime(msg.LeaseExpiresAt)
	return msg
}
