package producerprotocol

import (
	"fmt"
	"testing"
	"time"
)

// TestWireContractShape locks the exact JSON shape this package accepts:
// protocol_version, tool, instance_id, producer_epoch, state, revision,
// observed_at, and lease_expires_at, and nothing else. Any wire schema
// change must be a visible diff to this test, backed by a protocol_version
// bump.
func TestWireContractShape(t *testing.T) {
	for _, tool := range []Tool{"claude", "codex", "grok"} {
		t.Run(string(tool), func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{
				"protocol_version": 2,
				"tool": %q,
				"instance_id": "instance-01",
				"producer_epoch": "epoch-01",
				"state": "idle",
				"revision": 42,
				"observed_at": "2026-07-22T12:00:00Z",
				"lease_expires_at": "2026-07-22T12:05:00Z"
			}`, tool))
			msg, err := DecodeMessageJSON(raw, 4096)
			if err != nil {
				t.Fatal(err)
			}
			config := DefaultConfig(&testClock{now: testTime})
			if err := ValidateMessage(config, CanonicalMessage(msg)); err != nil {
				t.Fatalf("contract message rejected: %v", err)
			}
			if msg.ProtocolVersion != 2 || msg.Tool != tool || msg.InstanceID != "instance-01" ||
				msg.ProducerEpoch != "epoch-01" || msg.State != StateIdle || msg.Revision != 42 {
				t.Fatalf("decoded = %#v", msg)
			}
			wantObserved := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
			wantLease := time.Date(2026, 7, 22, 12, 5, 0, 0, time.UTC)
			if !msg.ObservedAt.Equal(wantObserved) || !msg.LeaseExpiresAt.Equal(wantLease) {
				t.Fatalf("timestamps = %v / %v", msg.ObservedAt, msg.LeaseExpiresAt)
			}
		})
	}
}

// TestWireContractRejectsEveryUnknownTopLevelField guards the "unknown
// fields are always rejected" invariant against future field additions
// elsewhere in the package accidentally loosening it.
func TestWireContractRejectsEveryUnknownTopLevelField(t *testing.T) {
	fields := []string{"metadata", "prompt", "cwd", "pid", "session_id", "extra"}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{
				"protocol_version": 2, "tool": "claude", "instance_id": "a",
				"producer_epoch": "epoch-01",
				"state": "idle", "revision": 1,
				"observed_at": "2026-07-22T12:00:00Z", "lease_expires_at": "2026-07-22T12:01:00Z",
				%q: "unexpected"
			}`, field))
			if _, err := DecodeMessageJSON(raw, 4096); ErrorCodeOf(err) != CodeUnknownField {
				t.Fatalf("field %q: code = %v, want unknown_field (err=%v)", field, ErrorCodeOf(err), err)
			}
		})
	}
}

// TestWireContractRejectsMissingProducerEpoch guards the "producer_epoch is
// required, not optional" invariant: a v2 message decoded without it must
// fail ValidateMessage, not silently default to empty.
func TestWireContractRejectsMissingProducerEpoch(t *testing.T) {
	raw := []byte(`{
		"protocol_version": 2, "tool": "claude", "instance_id": "a",
		"state": "idle", "revision": 1,
		"observed_at": "2026-07-22T12:00:00Z", "lease_expires_at": "2026-07-22T12:01:00Z"
	}`)
	msg, err := DecodeMessageJSON(raw, 4096)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(&testClock{now: testTime})
	if err := ValidateMessage(config, CanonicalMessage(msg)); ErrorCodeOf(err) != CodeInvalidProducerEpoch {
		t.Fatalf("code = %v, want invalid_producer_epoch (err=%v)", ErrorCodeOf(err), err)
	}
}
