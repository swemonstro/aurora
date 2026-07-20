package publish

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

func TestJSONPublisher(t *testing.T) {
	var output bytes.Buffer

	publisher, err := NewJSONPublisher(&output)
	if err != nil {
		t.Fatalf("NewJSONPublisher returned error: %v", err)
	}

	snapshot := presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "test",
		State:     status.Working,
		Timestamp: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	}

	if err := publisher.Publish(context.Background(), snapshot); err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}

	want := `{"version":1,"source":"test","state":"working","timestamp":"2026-07-20T08:00:00Z"}`
	if strings.TrimSpace(output.String()) != want {
		t.Fatalf("output = %q, want %q", strings.TrimSpace(output.String()), want)
	}
}

func TestNewJSONPublisherRejectsNilWriter(t *testing.T) {
	if _, err := NewJSONPublisher(nil); err == nil {
		t.Fatal("NewJSONPublisher(nil) returned no error")
	}
}
