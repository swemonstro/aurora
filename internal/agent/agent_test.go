package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/status"
)

type recordingPublisher struct {
	snapshot presence.Snapshot
	removed  string
	err      error
}

func (p *recordingPublisher) Publish(_ context.Context, snapshot presence.Snapshot) error {
	p.snapshot = snapshot
	return p.err
}

func (p *recordingPublisher) Remove(_ context.Context, source string) error {
	p.removed = source
	return p.err
}

func TestHandleNormalizesAndPublishesSnapshot(t *testing.T) {
	publisher := &recordingPublisher{}
	now := time.Date(2026, 7, 20, 10, 30, 0, 0, time.FixedZone("CEST", 2*60*60))

	instance, err := New("claude", publisher, func() time.Time {
		return now
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := instance.Handle(context.Background(), "completed"); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	wantTime := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)

	if publisher.snapshot.Version != presence.ProtocolVersion {
		t.Fatalf(
			"Version = %d, want %d",
			publisher.snapshot.Version,
			presence.ProtocolVersion,
		)
	}
	if publisher.snapshot.Source != "claude" {
		t.Fatalf("Source = %q, want %q", publisher.snapshot.Source, "claude")
	}
	if publisher.snapshot.State != status.Attention {
		t.Fatalf("State = %q, want %q", publisher.snapshot.State, status.Attention)
	}
	if !publisher.snapshot.Timestamp.Equal(wantTime) {
		t.Fatalf("Timestamp = %s, want %s", publisher.snapshot.Timestamp, wantTime)
	}
	if publisher.snapshot.Timestamp.Location() != time.UTC {
		t.Fatalf("Timestamp location = %s, want UTC", publisher.snapshot.Timestamp.Location())
	}
}

func TestHandleRejectsUnknownEvent(t *testing.T) {
	instance, err := New("test", &recordingPublisher{}, time.Now)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := instance.Handle(context.Background(), "unknown"); err == nil {
		t.Fatal("Handle returned no error")
	}
}

func TestHandleReturnsPublisherError(t *testing.T) {
	publishErr := errors.New("publisher unavailable")

	instance, err := New(
		"test",
		&recordingPublisher{err: publishErr},
		time.Now,
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	err = instance.Handle(context.Background(), "working")
	if !errors.Is(err, publishErr) {
		t.Fatalf("Handle error = %v, want wrapped publisher error", err)
	}
}

func TestHandleLifecycleRemovesInactiveSource(t *testing.T) {
	publisher := &recordingPublisher{}
	instance, err := New("codex-api", publisher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.HandleLifecycle(context.Background(), "idle", false); err != nil {
		t.Fatal(err)
	}
	if publisher.removed != "codex-api" {
		t.Fatalf("removed source = %q", publisher.removed)
	}
}

func TestNewValidatesDependencies(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		publisher publish.Publisher
		clock     Clock
	}{
		{
			name:      "empty source",
			source:    " ",
			publisher: &recordingPublisher{},
			clock:     time.Now,
		},
		{
			name:      "nil publisher",
			source:    "test",
			publisher: nil,
			clock:     time.Now,
		},
		{
			name:      "nil clock",
			source:    "test",
			publisher: &recordingPublisher{},
			clock:     nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.source, test.publisher, test.clock); err == nil {
				t.Fatal("New returned no error")
			}
		})
	}
}
