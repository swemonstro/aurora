package producerprotocol

import (
	"fmt"
	"time"
)

// ProtocolVersion identifies the wire schema. Changing the meaning of an
// existing field, or adding a field, requires a new version; this package
// never accepts an unsupported version.
type ProtocolVersion uint16

// CurrentProtocolVersion is the only version this package accepts on decode.
const CurrentProtocolVersion ProtocolVersion = 1

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

// Message is the wire form of one producer presence report.
type Message struct {
	ProtocolVersion ProtocolVersion `json:"protocol_version"`
	Tool            Tool            `json:"tool"`
	InstanceID      InstanceID      `json:"instance_id"`
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
