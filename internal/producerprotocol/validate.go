package producerprotocol

import (
	"fmt"
	"strings"
)

// ValidateMessage strictly validates msg against config's bounds. Callers
// must pass a canonicalized message (see CanonicalMessage) so timestamp
// comparisons are UTC-normalized.
func ValidateMessage(config Config, msg Message) error {
	if msg.ProtocolVersion != CurrentProtocolVersion {
		return protocolError(CodeUnsupportedProtocolVersion, ErrUnsupportedProtocolVersion)
	}
	if err := msg.Tool.Validate(); err != nil {
		return protocolError(CodeInvalidTool, err)
	}
	if err := msg.State.Validate(); err != nil {
		return protocolError(CodeInvalidState, err)
	}
	if err := validateInstanceID(msg.InstanceID, config.MaximumInstanceIDLength); err != nil {
		return protocolError(CodeInvalidInstanceID, err)
	}
	if msg.Revision == 0 {
		return protocolError(CodeInvalidRevision, fmt.Errorf("revision must be positive"))
	}
	if config.MaximumRevision != 0 && uint64(msg.Revision) > config.MaximumRevision {
		return protocolError(CodeInvalidRevision, fmt.Errorf("revision exceeds configured maximum"))
	}
	if msg.ObservedAt.IsZero() {
		return protocolError(CodeInvalidTimestamp, fmt.Errorf("observed_at must not be zero"))
	}
	if msg.LeaseExpiresAt.IsZero() {
		return protocolError(CodeInvalidTimestamp, fmt.Errorf("lease_expires_at must not be zero"))
	}
	if !msg.LeaseExpiresAt.After(msg.ObservedAt) {
		return protocolError(CodeInvalidTimestamp, fmt.Errorf("lease_expires_at must be strictly after observed_at"))
	}
	return nil
}

// validateInstanceID enforces a non-empty, bounded, printable-ASCII
// identifier. The restricted charset keeps the identifier safe to embed in
// logs, metrics labels, and file paths without further escaping.
func validateInstanceID(id InstanceID, maximum int) error {
	value := string(id)
	if value == "" {
		return fmt.Errorf("instance ID must not be empty")
	}
	if len(value) > maximum {
		return fmt.Errorf("instance ID exceeds maximum length")
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("instance ID must not contain surrounding whitespace")
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case strings.ContainsRune("._:@-", character):
		default:
			return fmt.Errorf("instance ID contains unsupported characters")
		}
	}
	return nil
}
