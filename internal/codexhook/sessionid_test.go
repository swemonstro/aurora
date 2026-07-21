package codexhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordSessionIDForSessionStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "session-id")

	err := RecordSessionID(path, Event{
		HookEventName: "SessionStart",
		SessionID:     " session-a ",
	})
	if err != nil {
		t.Fatalf("RecordSessionID returned error: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session ID file: %v", err)
	}
	if got := strings.TrimSpace(string(content)); got != "session-a" {
		t.Fatalf("session ID = %q, want %q", got, "session-a")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat session ID file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o, want 600", got)
	}
}

func TestRecordSessionIDIgnoresOtherEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-id")

	if err := RecordSessionID(path, Event{
		HookEventName: "UserPromptSubmit",
		SessionID:     "session-a",
	}); err != nil {
		t.Fatalf("RecordSessionID returned error: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("session ID file unexpectedly exists: %v", err)
	}
}

func TestRecordSessionIDIgnoresEmptyPathAndID(t *testing.T) {
	if err := RecordSessionID("", Event{
		HookEventName: "SessionStart",
		SessionID:     "session-a",
	}); err != nil {
		t.Fatalf("empty path returned error: %v", err)
	}

	path := filepath.Join(t.TempDir(), "session-id")
	if err := RecordSessionID(path, Event{
		HookEventName: "SessionStart",
		SessionID:     " ",
	}); err != nil {
		t.Fatalf("empty session ID returned error: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("session ID file unexpectedly exists: %v", err)
	}
}
