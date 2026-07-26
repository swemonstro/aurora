package producerprotocol

import (
	"errors"
	"testing"
	"time"
)

func TestValidMessagePerTool(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	for _, tool := range []Tool{ToolClaude, ToolCodex, ToolGrok} {
		t.Run(string(tool), func(t *testing.T) {
			msg := CanonicalMessage(validMessage(tool))
			if err := ValidateMessage(config, msg); err != nil {
				t.Fatalf("valid message for %s rejected: %v", tool, err)
			}
		})
	}
}

func TestUnknownProtocolVersionRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.ProtocolVersion = CurrentProtocolVersion + 1
	err := ValidateMessage(config, msg)
	if !errors.Is(err, ErrUnsupportedProtocolVersion) {
		t.Fatalf("error = %v, want ErrUnsupportedProtocolVersion", err)
	}
	if ErrorCodeOf(err) != CodeUnsupportedProtocolVersion {
		t.Fatalf("code = %v", ErrorCodeOf(err))
	}
}

func TestUnknownToolRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(Tool("gpt"))
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidTool {
		t.Fatalf("code = %v, want invalid_tool (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestUnknownStateRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.State = State("busy")
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidState {
		t.Fatalf("code = %v, want invalid_state (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestEmptyInstanceIDRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.InstanceID = ""
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidInstanceID {
		t.Fatalf("code = %v, want invalid_instance_id (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestInstanceIDTooLongRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	config.MaximumInstanceIDLength = 8
	msg := validMessage(ToolClaude)
	msg.InstanceID = "this-instance-id-is-far-too-long"
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidInstanceID {
		t.Fatalf("code = %v, want invalid_instance_id (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestInstanceIDUnsupportedCharactersRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.InstanceID = "bad id/with space"
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidInstanceID {
		t.Fatalf("code = %v, want invalid_instance_id (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestRevisionZeroRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.Revision = 0
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidRevision {
		t.Fatalf("code = %v, want invalid_revision (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestRevisionAboveConfiguredMaximumRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	config.MaximumRevision = 10
	msg := validMessage(ToolClaude)
	msg.Revision = 11
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidRevision {
		t.Fatalf("code = %v, want invalid_revision (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestZeroObservedAtRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.ObservedAt = time.Time{}
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidTimestamp {
		t.Fatalf("code = %v, want invalid_timestamp (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestZeroLeaseExpiresAtRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.LeaseExpiresAt = time.Time{}
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidTimestamp {
		t.Fatalf("code = %v, want invalid_timestamp (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestLeaseBeforeObservedRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.LeaseExpiresAt = msg.ObservedAt.Add(-time.Second)
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidTimestamp {
		t.Fatalf("code = %v, want invalid_timestamp (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestLeaseEqualToObservedRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := testConfig(clock)
	msg := validMessage(ToolClaude)
	msg.LeaseExpiresAt = msg.ObservedAt
	err := ValidateMessage(config, msg)
	if ErrorCodeOf(err) != CodeInvalidTimestamp {
		t.Fatalf("code = %v, want invalid_timestamp for lease == observed (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestCanonicalMessageNormalizesToUTC(t *testing.T) {
	location := time.FixedZone("test-zone", 3600)
	msg := Message{
		ObservedAt:     testTime.In(location),
		LeaseExpiresAt: testTime.Add(time.Minute).In(location),
	}
	canonical := CanonicalMessage(msg)
	if canonical.ObservedAt.Location() != time.UTC || canonical.LeaseExpiresAt.Location() != time.UTC {
		t.Fatalf("timestamps not normalized to UTC: observed=%v lease=%v", canonical.ObservedAt.Location(), canonical.LeaseExpiresAt.Location())
	}
	if !canonical.ObservedAt.Equal(testTime) {
		t.Fatalf("observed_at instant changed: %v", canonical.ObservedAt)
	}
}

func TestToolValidate(t *testing.T) {
	for _, tool := range []Tool{ToolClaude, ToolCodex, ToolGrok} {
		if err := tool.Validate(); err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
	}
	if err := Tool("unknown").Validate(); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestStateValidate(t *testing.T) {
	for _, state := range []State{StateIdle, StateWorking, StateAttention, StateError} {
		if err := state.Validate(); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
	if err := State("unknown").Validate(); err == nil {
		t.Fatal("expected error for unknown state")
	}
}
