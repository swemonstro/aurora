package transport

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/status"
)

func TestJSONSender(t *testing.T) {
	var output bytes.Buffer

	sender, err := NewJSONSender(&output)
	if err != nil {
		t.Fatalf("NewJSONSender returned error: %v", err)
	}

	message := status.Message{
		Version:   1,
		Source:    "test",
		State:     status.Working,
		Timestamp: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	}

	if err := sender.Send(context.Background(), message); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}

	want := `{"version":1,"source":"test","state":"working","timestamp":"2026-07-20T08:00:00Z"}`
	if strings.TrimSpace(output.String()) != want {
		t.Fatalf("output = %q, want %q", strings.TrimSpace(output.String()), want)
	}
}

func TestNewJSONSenderRejectsNilWriter(t *testing.T) {
	if _, err := NewJSONSender(nil); err == nil {
		t.Fatal("NewJSONSender(nil) returned no error")
	}
}
