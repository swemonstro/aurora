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
