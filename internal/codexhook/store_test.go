package codexhook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/status"
)

func TestSessionEventSemantics(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  status.State
	}{
		{
			name: "session start",
			event: Event{
				HookEventName: "SessionStart",
				SessionID:     "a",
			},
			want: status.Idle,
		},
		{
			name: "prompt",
			event: Event{
				HookEventName: "UserPromptSubmit",
				SessionID:     "a",
			},
			want: status.Working,
		},
		{
			name: "approval",
			event: Event{
				HookEventName: "PermissionRequest",
				SessionID:     "a",
			},
			want: status.Attention,
		},
		{
			name: "tool completed",
			event: Event{
				HookEventName: "PostToolUse",
				SessionID:     "a",
			},
			want: status.Working,
		},
		{
			name: "turn stopped",
			event: Event{
				HookEventName: "Stop",
				SessionID:     "a",
			},
			want: status.Idle,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)

			got := mustUpdate(t, store, test.event)
			if got != test.want {
				t.Fatalf("aggregate = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStopChangesOnlyMatchingSession(t *testing.T) {
	store := newTestStore(t)

	mustUpdate(t, store, Event{
		HookEventName: "UserPromptSubmit",
		SessionID:     "a",
	})
	mustUpdate(t, store, Event{
		HookEventName: "UserPromptSubmit",
		SessionID:     "b",
	})

	got := mustUpdate(t, store, Event{
		HookEventName: "Stop",
		SessionID:     "b",
	})
	if got != status.Working {
		t.Fatalf("aggregate = %q, want %q", got, status.Working)
	}
}

func TestSessionEndRemovesOnlyMatchingSession(t *testing.T) {
	store := newTestStore(t)

	mustUpdate(t, store, Event{
		HookEventName: "PermissionRequest",
		SessionID:     "a",
	})
	mustUpdate(t, store, Event{
		HookEventName: "UserPromptSubmit",
		SessionID:     "b",
	})

	got := mustUpdate(t, store, Event{
		HookEventName: "SessionEnd",
		SessionID:     "a",
	})
	if got != status.Working {
		t.Fatalf("aggregate = %q, want %q", got, status.Working)
	}

	state := readState(t, store.path)
	if _, exists := state.Sessions["a"]; exists {
		t.Fatal("ended session still exists")
	}
	if _, exists := state.Sessions["b"]; !exists {
		t.Fatal("unrelated session was removed")
	}
}

func TestBlankSessionIDDoesNotCreateSession(t *testing.T) {
	store := newTestStore(t)

	got := mustUpdate(t, store, Event{
		HookEventName: "UserPromptSubmit",
		SessionID:     " ",
	})
	if got != status.Idle {
		t.Fatalf("aggregate = %q, want %q", got, status.Idle)
	}

	if sessions := readState(t, store.path).Sessions; len(sessions) != 0 {
		t.Fatalf("sessions = %#v, want empty", sessions)
	}
}

func TestAggregatePriority(t *testing.T) {
	if got := Aggregate(nil); got != status.Idle {
		t.Fatalf("empty aggregate = %q", got)
	}

	sessions := map[string]Session{
		"working": {State: status.Working},
	}
	if got := Aggregate(sessions); got != status.Working {
		t.Fatalf("working aggregate = %q", got)
	}

	sessions["attention"] = Session{State: status.Attention}
	if got := Aggregate(sessions); got != status.Attention {
		t.Fatalf("attention aggregate = %q", got)
	}

	sessions["error"] = Session{State: status.Error}
	if got := Aggregate(sessions); got != status.Error {
		t.Fatalf("error aggregate = %q", got)
	}
}

func TestStaleSessionsArePruned(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	store.ttl = time.Hour

	writeState(t, store.path, sessionState{
		Sessions: map[string]Session{
			"stale": {
				State:     status.Attention,
				UpdatedAt: now.Add(-2 * time.Hour),
			},
			"fresh": {
				State:     status.Working,
				UpdatedAt: now.Add(-30 * time.Minute),
			},
		},
	})

	got := mustUpdate(t, store, Event{
		HookEventName: "SessionEnd",
		SessionID:     "unknown",
	})
	if got != status.Working {
		t.Fatalf("aggregate = %q, want %q", got, status.Working)
	}

	if _, exists := readState(t, store.path).Sessions["stale"]; exists {
		t.Fatal("stale session was not removed")
	}
}

func TestStatePermissions(t *testing.T) {
	store := newTestStore(t)

	mustUpdate(t, store, Event{
		HookEventName: "UserPromptSubmit",
		SessionID:     "a",
	})

	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state permissions = %o, want 600", got)
	}
}

func TestUnsupportedEventDoesNotCreateState(t *testing.T) {
	store := newTestStore(t)

	_, supported, err := store.Update(Event{
		HookEventName: "FutureEvent",
		SessionID:     "a",
	})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if supported {
		t.Fatal("unsupported event reported as supported")
	}
	if _, err := os.Stat(store.path); !os.IsNotExist(err) {
		t.Fatalf("state file unexpectedly exists: %v", err)
	}
}

func TestNewSessionStoreValidatesArguments(t *testing.T) {
	if _, err := NewSessionStore(" ", time.Hour); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := NewSessionStore("state.json", 0); err == nil {
		t.Fatal("zero TTL accepted")
	}
}

func newTestStore(t *testing.T) *SessionStore {
	t.Helper()

	store, err := NewSessionStore(
		filepath.Join(t.TempDir(), "sessions.json"),
		DefaultSessionTTL,
	)
	if err != nil {
		t.Fatalf("NewSessionStore returned error: %v", err)
	}

	return store
}

func mustUpdate(
	t *testing.T,
	store *SessionStore,
	event Event,
) status.State {
	t.Helper()

	state, supported, err := store.Update(event)
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if !supported {
		t.Fatal("event was not supported")
	}

	return state
}

func readState(t *testing.T, path string) sessionState {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer file.Close()

	var state sessionState
	if err := json.NewDecoder(file).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}

	return state
}

func writeState(t *testing.T, path string, state sessionState) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create state directory: %v", err)
	}

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create state: %v", err)
	}

	if err := json.NewEncoder(file).Encode(state); err != nil {
		file.Close()
		t.Fatalf("encode state: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close state: %v", err)
	}
}
