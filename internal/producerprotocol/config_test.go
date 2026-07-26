package producerprotocol

import "testing"

func TestDefaultConfigIsValidForMessagesOnly(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	if err := config.Validate(false); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRequiresSocketPathForTransport(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	if err := config.Validate(true); err == nil {
		t.Fatal("expected error for missing socket path")
	}
	config.SocketPath = "/tmp/does-not-need-to-exist.sock"
	if err := config.Validate(true); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsOutOfBoundMessageSize(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	config.MaximumMessageBytes = 0
	if err := config.Validate(false); err == nil {
		t.Fatal("expected error for zero maximum message bytes")
	}
	config.MaximumMessageBytes = 1 << 20
	if err := config.Validate(false); err == nil {
		t.Fatal("expected error for maximum message bytes above ceiling")
	}
}

func TestConfigRejectsMissingClock(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	config.Clock = nil
	if err := config.Validate(false); err == nil {
		t.Fatal("expected error for nil clock")
	}
}

func TestConfigRejectsInvalidBoundTool(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	config.BoundTool = Tool("not-a-tool")
	if err := config.Validate(false); err == nil {
		t.Fatal("expected error for invalid bound tool")
	}
}

func TestConfigRejectsNonPositiveTimeoutsForTransport(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = "/tmp/producerprotocol-test.sock"
	config.ReadTimeout = 0
	if err := config.Validate(true); err == nil {
		t.Fatal("expected error for zero read timeout")
	}
}
