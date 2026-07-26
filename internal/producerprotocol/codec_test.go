package producerprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	msg := validMessage(ToolCodex)
	data, err := EncodeMessageJSON(msg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMessageJSON(data, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Tool != msg.Tool || decoded.InstanceID != msg.InstanceID || decoded.Revision != msg.Revision {
		t.Fatalf("round trip mismatch: %#v vs %#v", decoded, msg)
	}
	if !decoded.ObservedAt.Equal(msg.ObservedAt) || !decoded.LeaseExpiresAt.Equal(msg.LeaseExpiresAt) {
		t.Fatalf("timestamp round trip mismatch: %#v vs %#v", decoded, msg)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	raw := []byte(`{
		"protocol_version": 1, "tool": "claude", "instance_id": "a",
		"state": "idle", "revision": 1,
		"observed_at": "2026-07-22T12:00:00Z", "lease_expires_at": "2026-07-22T12:01:00Z",
		"extra_field": "unexpected"
	}`)
	_, err := DecodeMessageJSON(raw, 4096)
	if ErrorCodeOf(err) != CodeUnknownField {
		t.Fatalf("code = %v, want unknown_field (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	raw := []byte(`{"protocol_version":1,"tool":"claude","instance_id":"a","state":"idle","revision":1,"observed_at":"2026-07-22T12:00:00Z","lease_expires_at":"2026-07-22T12:01:00Z"}{}`)
	_, err := DecodeMessageJSON(raw, 4096)
	if ErrorCodeOf(err) != CodeMalformedMessage {
		t.Fatalf("code = %v, want malformed_message (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestDecodeRejectsEmptyMessage(t *testing.T) {
	_, err := DecodeMessageJSON(nil, 4096)
	if ErrorCodeOf(err) != CodeMalformedMessage {
		t.Fatalf("code = %v, want malformed_message (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestDecodeRejectsOversizedPayload(t *testing.T) {
	raw := []byte(`{"protocol_version":1,"tool":"claude","instance_id":"a","state":"idle","revision":1,"observed_at":"2026-07-22T12:00:00Z","lease_expires_at":"2026-07-22T12:01:00Z"}`)
	_, err := DecodeMessageJSON(raw, uint32(len(raw)-1))
	if ErrorCodeOf(err) != CodeMessageTooLarge {
		t.Fatalf("code = %v, want message_too_large (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestDecodeRejectsMalformedTimestamp(t *testing.T) {
	raw := []byte(`{"protocol_version":1,"tool":"claude","instance_id":"a","state":"idle","revision":1,"observed_at":"not-a-timestamp","lease_expires_at":"2026-07-22T12:01:00Z"}`)
	_, err := DecodeMessageJSON(raw, 4096)
	if ErrorCodeOf(err) != CodeMalformedMessage {
		t.Fatalf("code = %v, want malformed_message for bad timestamp (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	_, err := DecodeMessageJSON([]byte(`{not json`), 4096)
	if ErrorCodeOf(err) != CodeMalformedMessage {
		t.Fatalf("code = %v, want malformed_message (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestEncodeRejectsOversizedMessage(t *testing.T) {
	msg := validMessage(ToolClaude)
	msg.InstanceID = InstanceID(strings.Repeat("a", 200))
	if _, err := EncodeMessageJSON(msg, 8); err == nil {
		t.Fatal("expected message_too_large error")
	}
}

func TestEncodeProducesValidJSON(t *testing.T) {
	msg := validMessage(ToolGrok)
	data, err := EncodeMessageJSON(msg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"protocol_version", "tool", "instance_id", "state", "revision", "observed_at", "lease_expires_at"} {
		if _, ok := generic[key]; !ok {
			t.Fatalf("missing wire field %q in %s", key, data)
		}
	}
}
