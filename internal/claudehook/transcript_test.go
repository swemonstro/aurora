package claudehook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/swemonstro/aurora/internal/status"
)

func TestScanQuestionTranscriptMatchesErrorToolResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	prefix := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu-old","name":"AskUserQuestion"}]}}` + "\n"
	// Content text must not influence matching — only tool_use_id + is_error.
	match := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu-a","is_error":true,"content":"arbitrary-text-must-not-matter"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(prefix+match), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, _, err := ScanQuestionTranscript(path, "toolu-a", int64(len(prefix)))
	if err != nil || !matched {
		t.Fatalf("matched=%t err=%v", matched, err)
	}
}

func TestScanQuestionTranscriptIgnoresOtherToolUseIDAndNonError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := "" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"other","is_error":true}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu-a","is_error":false,"content":"ok"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, offset, err := ScanQuestionTranscript(path, "toolu-a", 0)
	if err != nil || matched {
		t.Fatalf("matched=%t offset=%d err=%v", matched, offset, err)
	}
}

func TestScanQuestionTranscriptHandlesPartialAndTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	complete := "not-json\n"
	partial := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu-a","is_error":true}]}}`
	if err := os.WriteFile(path, []byte(complete+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, offset, err := ScanQuestionTranscript(path, "toolu-a", 0)
	if err != nil || matched {
		t.Fatalf("partial: matched=%t err=%v", matched, err)
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
	_ = file.Close()
	matched, _, err = ScanQuestionTranscript(path, "toolu-a", offset)
	if err != nil || !matched {
		t.Fatalf("completed partial: matched=%t err=%v", matched, err)
	}

	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, gotOffset, err := ScanQuestionTranscript(path, "toolu-a", 1000)
	if err != nil || matched || gotOffset != 3 {
		t.Fatalf("truncated: matched=%t offset=%d err=%v", matched, gotOffset, err)
	}
}

func TestQuestionWatchStartedAndRecovered(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	update, supported, err := store.UpdateLifecycle(Event{
		HookEventName:  "PreToolUse",
		SessionID:      "session-a",
		ToolName:       "AskUserQuestion",
		ToolUseID:      "toolu-a",
		TranscriptPath: transcript,
	})
	if err != nil || !supported || update.Watch == nil {
		t.Fatalf("update = %#v supported=%t err=%v", update, supported, err)
	}
	watch := *update.Watch
	if watch.SessionID != "session-a" || watch.ToolUseID != "toolu-a" ||
		watch.TranscriptPath != transcript || watch.Revision == 0 {
		t.Fatalf("watch = %#v", watch)
	}
	// permission_prompt preserves the pending question identity.
	mustUpdate(t, store, Event{
		HookEventName: "Notification", SessionID: "session-a", NotificationType: "permission_prompt",
	})
	pending, err := store.QuestionPending(watch)
	if err != nil || !pending {
		t.Fatalf("pending after permission_prompt = %t err=%v", pending, err)
	}

	recovered, recoveredOK, err := store.RecoverCancelledQuestion(watch)
	if err != nil || !recoveredOK {
		t.Fatalf("recover: ok=%t err=%v", recoveredOK, err)
	}
	if recovered.State != status.Idle {
		t.Fatalf("aggregate after recover = %q", recovered.State)
	}
	pending, err = store.QuestionPending(watch)
	if err != nil || pending {
		t.Fatalf("still pending after recover: %t err=%v", pending, err)
	}
}

func TestPermissionPromptPreservesAskUserQuestionWatch(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-a", TranscriptPath: transcript,
	})
	if err != nil || update.Watch == nil {
		t.Fatalf("start watch = %#v err=%v", update, err)
	}
	watch := *update.Watch
	mustUpdate(t, store, Event{
		HookEventName: "Notification", SessionID: "session-a", NotificationType: "permission_prompt",
	})
	session := readState(t, store.path).Sessions["session-a"]
	if session.State != status.Attention ||
		session.ToolUseID != "toolu-a" ||
		session.TranscriptPath != transcript ||
		session.Revision != watch.Revision {
		t.Fatalf("session after permission_prompt = %#v, want preserved watch %#v", session, watch)
	}
	pending, err := store.QuestionPending(watch)
	if err != nil || !pending {
		t.Fatalf("pending = %t err=%v", pending, err)
	}
}

func TestIdlePromptInvalidatesQuestionWatchAndClearsMetadata(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-a", TranscriptPath: transcript,
	})
	if err != nil || update.Watch == nil {
		t.Fatal(err)
	}
	watch := *update.Watch
	mustUpdate(t, store, Event{
		HookEventName: "Notification", SessionID: "session-a", NotificationType: "idle_prompt",
	})
	session := readState(t, store.path).Sessions["session-a"]
	if session.State != status.Idle {
		t.Fatalf("state = %q, want idle", session.State)
	}
	if session.ToolUseID != "" || session.TranscriptPath != "" || session.TranscriptOffset != 0 {
		t.Fatalf("question metadata not cleared: %#v", session)
	}
	if session.Revision == watch.Revision {
		t.Fatal("revision must advance when watch is invalidated")
	}
	pending, err := store.QuestionPending(watch)
	if err != nil || pending {
		t.Fatalf("old watch still pending: %t err=%v", pending, err)
	}
}

func TestLaterPermissionPromptCannotReviveStaleWatch(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-a", TranscriptPath: transcript,
	})
	if err != nil || update.Watch == nil {
		t.Fatal(err)
	}
	stale := *update.Watch
	// idle_prompt clears the question identity.
	mustUpdate(t, store, Event{
		HookEventName: "Notification", SessionID: "session-a", NotificationType: "idle_prompt",
	})
	// A later permission_prompt must not resurrect tool_use_id / path / revision.
	mustUpdate(t, store, Event{
		HookEventName: "Notification", SessionID: "session-a", NotificationType: "permission_prompt",
	})
	session := readState(t, store.path).Sessions["session-a"]
	if session.State != status.Attention {
		t.Fatalf("state = %q, want attention", session.State)
	}
	if session.ToolUseID != "" || session.TranscriptPath != "" || session.TranscriptOffset != 0 {
		t.Fatalf("stale question metadata revived: %#v", session)
	}
	if session.Revision == stale.Revision {
		t.Fatal("stale revision must not be reused after idle_prompt")
	}
	pending, err := store.QuestionPending(stale)
	if err != nil || pending {
		t.Fatalf("stale watch pending after revive attempt: %t err=%v", pending, err)
	}
	_, recovered, err := store.RecoverCancelledQuestion(stale)
	if err != nil || recovered {
		t.Fatalf("stale recover: recovered=%t err=%v", recovered, err)
	}
}

func TestUnknownNotificationInvalidatesQuestionWatch(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-a", TranscriptPath: transcript,
	})
	if err != nil || update.Watch == nil {
		t.Fatal(err)
	}
	watch := *update.Watch
	for _, notificationType := range []string{"", "future_type", "other"} {
		// Re-seed a fresh watch for each type.
		seed, _, err := store.UpdateLifecycle(Event{
			HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
			ToolUseID: "toolu-a", TranscriptPath: transcript,
		})
		if err != nil || seed.Watch == nil {
			t.Fatal(err)
		}
		watch = *seed.Watch
		mustUpdate(t, store, Event{
			HookEventName: "Notification", SessionID: "session-a", NotificationType: notificationType,
		})
		session := readState(t, store.path).Sessions["session-a"]
		if session.ToolUseID != "" || session.TranscriptPath != "" {
			t.Fatalf("notification_type %q kept metadata: %#v", notificationType, session)
		}
		pending, err := store.QuestionPending(watch)
		if err != nil || pending {
			t.Fatalf("notification_type %q: pending=%t err=%v", notificationType, pending, err)
		}
	}
}

func TestPostToolUseAndUserPromptSubmitInvalidateQuestionWatch(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, event := range []Event{
		{HookEventName: "PostToolUse", SessionID: "session-a", ToolName: "AskUserQuestion"},
		{HookEventName: "UserPromptSubmit", SessionID: "session-a"},
	} {
		t.Run(event.HookEventName, func(t *testing.T) {
			store := newTestStore(t)
			update, _, err := store.UpdateLifecycle(Event{
				HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
				ToolUseID: "toolu-a", TranscriptPath: transcript,
			})
			if err != nil || update.Watch == nil {
				t.Fatal(err)
			}
			watch := *update.Watch
			mustUpdate(t, store, event)
			session := readState(t, store.path).Sessions["session-a"]
			if session.ToolUseID != "" || session.TranscriptPath != "" {
				t.Fatalf("metadata retained after %s: %#v", event.HookEventName, session)
			}
			pending, err := store.QuestionPending(watch)
			if err != nil || pending {
				t.Fatalf("pending after %s: %t err=%v", event.HookEventName, pending, err)
			}
			_, recovered, err := store.RecoverCancelledQuestion(watch)
			if err != nil || recovered {
				t.Fatalf("recover after %s: recovered=%t err=%v", event.HookEventName, recovered, err)
			}
		})
	}
}

func TestSessionEndRemovesSessionAndBlocksStaleRecovery(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-a", TranscriptPath: transcript,
	})
	if err != nil || update.Watch == nil {
		t.Fatal(err)
	}
	watch := *update.Watch
	mustUpdate(t, store, Event{HookEventName: "SessionEnd", SessionID: "session-a"})
	if _, exists := readState(t, store.path).Sessions["session-a"]; exists {
		t.Fatal("session-a still present after SessionEnd")
	}
	pending, err := store.QuestionPending(watch)
	if err != nil || pending {
		t.Fatalf("pending after SessionEnd: %t err=%v", pending, err)
	}
	_, recovered, err := store.RecoverCancelledQuestion(watch)
	if err != nil || recovered {
		t.Fatalf("recover after SessionEnd: recovered=%t err=%v", recovered, err)
	}
}

func TestStaleQuestionWatcherCannotRecover(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first, _, _ := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-old", TranscriptPath: transcript,
	})
	second, _, _ := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-new", TranscriptPath: transcript,
	})
	if first.Watch == nil || second.Watch == nil || first.Watch.Revision == second.Watch.Revision {
		t.Fatal("expected distinct revisions")
	}
	_, recovered, err := store.RecoverCancelledQuestion(*first.Watch)
	if err != nil {
		t.Fatal(err)
	}
	if recovered {
		t.Fatal("stale watcher recovered over newer AskUserQuestion")
	}
	if got := readState(t, store.path).Sessions["session-a"].ToolUseID; got != "toolu-new" {
		t.Fatalf("tool_use_id = %q", got)
	}
}

func TestStopInvalidatesQuestionWatcher(t *testing.T) {
	store := newTestStore(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	update, _, _ := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-a", TranscriptPath: transcript,
	})
	mustUpdate(t, store, Event{HookEventName: "Stop", SessionID: "session-a"})
	pending, err := store.QuestionPending(*update.Watch)
	if err != nil || pending {
		t.Fatalf("pending after Stop = %t err=%v", pending, err)
	}
	_, recovered, err := store.RecoverCancelledQuestion(*update.Watch)
	if err != nil || recovered {
		t.Fatalf("recover after Stop: recovered=%t err=%v", recovered, err)
	}
}

func TestParallelSessionsQuestionRecoveryIndependent(t *testing.T) {
	store := newTestStore(t)
	transcriptA := filepath.Join(t.TempDir(), "a.jsonl")
	transcriptB := filepath.Join(t.TempDir(), "b.jsonl")
	_ = os.WriteFile(transcriptA, nil, 0o600)
	_ = os.WriteFile(transcriptB, nil, 0o600)
	watchA, _, _ := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-a", TranscriptPath: transcriptA,
	})
	watchB, _, _ := store.UpdateLifecycle(Event{
		HookEventName: "PreToolUse", SessionID: "session-b", ToolName: "AskUserQuestion",
		ToolUseID: "toolu-b", TranscriptPath: transcriptB,
	})
	if _, recovered, err := store.RecoverCancelledQuestion(*watchA.Watch); err != nil || !recovered {
		t.Fatalf("recover A: %v recovered=%t", err, recovered)
	}
	pendingB, err := store.QuestionPending(*watchB.Watch)
	if err != nil || !pendingB {
		t.Fatalf("B pending = %t err=%v", pendingB, err)
	}
	state := readState(t, store.path)
	if state.Sessions["session-a"].State != status.Idle {
		t.Fatalf("A = %#v", state.Sessions["session-a"])
	}
	if state.Sessions["session-b"].State != status.Attention {
		t.Fatalf("B = %#v", state.Sessions["session-b"])
	}
}
