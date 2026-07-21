package codexhook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/swemonstro/aurora/internal/status"
)

func TestScanTranscriptMatchesOnlyRequestedTurnAbort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	prefix := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"turn_aborted\",\"turn_id\":\"old\"}}\n"
	content := prefix +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"other\",\"turn_id\":\"turn-a\"}}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"turn_aborted\",\"turn_id\":\"turn-a\"}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	matched, _, err := ScanTranscript(path, "turn-a", int64(len(prefix)))
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("matching turn_aborted event was not detected")
	}
}

func TestScanTranscriptHandlesMalformedPartialMissingAndTruncatedJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	complete := "not-json\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"turn_aborted\",\"turn_id\":\"other\"}}\n"
	partial := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"turn_aborted\",\"turn_id\":\"turn-a\"}}"
	if err := os.WriteFile(path, []byte(complete+partial), 0o600); err != nil {
		t.Fatal(err)
	}

	matched, offset, err := ScanTranscript(path, "turn-a", 0)
	if err != nil || matched {
		t.Fatalf("partial scan: matched=%t offset=%d err=%v", matched, offset, err)
	}
	if offset != int64(len(complete)) {
		t.Fatalf("offset = %d, want %d", offset, len(complete))
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	matched, _, err = ScanTranscript(path, "turn-a", offset)
	if err != nil || !matched {
		t.Fatalf("completed partial scan: matched=%t err=%v", matched, err)
	}

	if _, _, err := ScanTranscript(filepath.Join(t.TempDir(), "missing"), "turn-a", 0); err == nil {
		t.Fatal("missing transcript returned no error")
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, gotOffset, err := ScanTranscript(path, "turn-a", 1000)
	if err != nil || matched || gotOffset != 3 {
		t.Fatalf("truncated scan: matched=%t offset=%d err=%v", matched, gotOffset, err)
	}
}

func TestPermissionRecoveryIsCorrelatedAndParallelSafe(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	first, supported, err := store.UpdateLifecycle(Event{
		HookEventName:  "PermissionRequest",
		SessionID:      "session-a",
		TurnID:         "turn-a",
		TranscriptPath: transcript,
	})
	if err != nil || !supported || first.Watch == nil {
		t.Fatalf("first permission update = %#v, supported=%t err=%v", first, supported, err)
	}
	pending := readState(t, store.path).Sessions["session-a"]
	if pending.TurnID != "turn-a" || pending.TranscriptPath != transcript || pending.Revision == 0 {
		t.Fatalf("stored permission metadata = %#v", pending)
	}
	mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: "session-b"})

	update, recovered, err := store.RecoverCancelled(*first.Watch)
	if err != nil || !recovered {
		t.Fatalf("RecoverCancelled: recovered=%t err=%v", recovered, err)
	}
	if update.State != status.Working || !update.Active {
		t.Fatalf("aggregate = %#v, want active working", update)
	}
	state := readState(t, store.path)
	if state.Sessions["session-a"].State != status.Idle || state.Sessions["session-b"].State != status.Working {
		t.Fatalf("sessions = %#v", state.Sessions)
	}
}

func TestStaleAbortCannotClearNewerAttentionRequest(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	event := Event{
		HookEventName:  "PermissionRequest",
		SessionID:      "session-a",
		TurnID:         "turn-a",
		TranscriptPath: transcript,
	}
	oldUpdate, _, _ := store.UpdateLifecycle(event)
	newUpdate, _, _ := store.UpdateLifecycle(event)
	if oldUpdate.Watch == nil || newUpdate.Watch == nil || oldUpdate.Watch.Revision == newUpdate.Watch.Revision {
		t.Fatal("permission requests did not receive distinct revisions")
	}

	_, recovered, err := store.RecoverCancelled(*oldUpdate.Watch)
	if err != nil {
		t.Fatal(err)
	}
	if recovered {
		t.Fatal("stale abort cleared a newer permission request")
	}
	if got := readState(t, store.path).Sessions["session-a"].State; got != status.Attention {
		t.Fatalf("state = %q, want attention", got)
	}
}
