package relay

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

func TestStoreStartsEmpty(t *testing.T) {
	var store Store

	if _, ok := store.Latest(); ok {
		t.Fatal("Latest reported a value for an empty store")
	}
}

func TestStoreReturnsLatestSnapshot(t *testing.T) {
	var store Store

	first := presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "claude",
		State:     status.Working,
		Timestamp: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	}
	second := presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "claude",
		State:     status.Attention,
		Timestamp: time.Date(2026, 7, 20, 8, 5, 0, 0, time.UTC),
	}

	store.Set(first)
	store.Set(second)

	got, ok := store.Latest()
	if !ok {
		t.Fatal("Latest reported no value")
	}
	if got != second {
		t.Fatalf("Latest = %#v, want %#v", got, second)
	}
}

func TestStoreAggregatesSnapshotsBySourcePriority(t *testing.T) {
	var store Store

	store.Set(presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "claude-code",
		State:     status.Attention,
		Timestamp: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	})
	store.Set(presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "codex-api",
		State:     status.Idle,
		Timestamp: time.Date(2026, 7, 20, 8, 5, 0, 0, time.UTC),
	})

	got, ok := store.Latest()
	if !ok {
		t.Fatal("Latest reported no value")
	}
	if got.Source != aggregateSource {
		t.Fatalf("Source = %q, want %q", got.Source, aggregateSource)
	}
	if got.State != status.Attention {
		t.Fatalf("State = %q, want %q", got.State, status.Attention)
	}

	wantTimestamp := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	if !got.Timestamp.Equal(wantTimestamp) {
		t.Fatalf("Timestamp = %s, want %s", got.Timestamp, wantTimestamp)
	}
}

func TestStoreUsesNewestTimestampAmongWinningSources(t *testing.T) {
	var store Store

	older := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 20, 8, 5, 0, 0, time.UTC)

	store.Set(presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "codex-api",
		State:     status.Working,
		Timestamp: older,
	})
	store.Set(presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "codex-business",
		State:     status.Working,
		Timestamp: newer,
	})

	got, ok := store.Latest()
	if !ok {
		t.Fatal("Latest reported no value")
	}
	if got.State != status.Working {
		t.Fatalf("State = %q, want %q", got.State, status.Working)
	}
	if !got.Timestamp.Equal(newer) {
		t.Fatalf("Timestamp = %s, want %s", got.Timestamp, newer)
	}
}

func TestStoreReplacesOnlyMatchingSource(t *testing.T) {
	var store Store

	store.Set(presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "claude-code",
		State:     status.Attention,
		Timestamp: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	})
	store.Set(presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "codex-api",
		State:     status.Working,
		Timestamp: time.Date(2026, 7, 20, 8, 1, 0, 0, time.UTC),
	})
	store.Set(presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "codex-api",
		State:     status.Idle,
		Timestamp: time.Date(2026, 7, 20, 8, 2, 0, 0, time.UTC),
	})

	got, ok := store.Latest()
	if !ok {
		t.Fatal("Latest reported no value")
	}
	if got.State != status.Attention {
		t.Fatalf("State = %q, want %q", got.State, status.Attention)
	}
}
