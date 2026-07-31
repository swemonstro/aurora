package codexhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestPermissionRequestDoesNotPersistTranscriptPath(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	update, supported, err := store.UpdateLifecycle(Event{
		HookEventName:  "PermissionRequest",
		SessionID:      "session-a",
		TurnID:         "turn-a",
		TranscriptPath: transcript,
	})
	if err != nil || !supported {
		t.Fatalf("permission update = %#v, supported=%t err=%v", update, supported, err)
	}
	pending := readState(t, store.path).Sessions["session-a"]
	if pending.TurnID != "turn-a" {
		t.Fatalf("stored turn identity = %q, want turn-a", pending.TurnID)
	}
	stateFile, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateFile), transcript) || strings.Contains(string(stateFile), "transcript_path") {
		t.Fatalf("state file contains transcript metadata: %s", stateFile)
	}
}

func TestPromptSubmitDoesNotPersistSensitiveUnknownFields(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	event, err := ParseEvent([]byte(`{
		"hook_event_name": "UserPromptSubmit",
		"session_id": "session-a",
		"turn_id": "turn-a",
		"transcript_path": "` + transcript + `",
		"cwd": "/secret/project",
		"prompt": "do not store this"
	}`))
	if err != nil {
		t.Fatal(err)
	}

	update, supported, err := store.UpdateLifecycle(event)
	if err != nil || !supported {
		t.Fatalf("prompt update = %#v, supported=%t err=%v", update, supported, err)
	}
	session := readState(t, store.path).Sessions["session-a"]
	if session.TurnID != "turn-a" {
		t.Fatalf("stored turn identity = %q, want turn-a", session.TurnID)
	}
	stateFile, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{transcript, "/secret/project", "do not store this", "transcript_path", "cwd", "prompt"} {
		if strings.Contains(string(stateFile), forbidden) {
			t.Fatalf("state file contains sensitive payload content %q in %s", forbidden, stateFile)
		}
	}
}
