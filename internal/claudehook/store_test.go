package claudehook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/status"
)

func TestParseEventFields(t *testing.T) {
	event, err := ParseEvent([]byte(
		`{"hook_event_name":"Notification","session_id":" session-a ","notification_type":"permission_prompt","tool_name":"AskUserQuestion","prompt":"ignored"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if event.HookEventName != "Notification" || event.SessionID != "session-a" ||
		event.NotificationType != "permission_prompt" || event.ToolName != "AskUserQuestion" {
		t.Fatalf("event = %#v", event)
	}
}

func TestSessionEventSemantics(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  status.State
	}{
		{name: "prompt", event: Event{HookEventName: "UserPromptSubmit", SessionID: "a"}, want: status.Working},
		{name: "stop for unknown session", event: Event{HookEventName: "Stop", SessionID: "a"}, want: status.Idle},
		{name: "permission", event: Event{HookEventName: "Notification", SessionID: "a", NotificationType: "permission_prompt"}, want: status.Attention},
		{name: "idle prompt", event: Event{HookEventName: "Notification", SessionID: "a", NotificationType: "idle_prompt"}, want: status.Idle},
		{name: "other notification", event: Event{HookEventName: "Notification", SessionID: "a", NotificationType: "other"}, want: status.Attention},
		{name: "question begins", event: Event{HookEventName: "PreToolUse", SessionID: "a", ToolName: "AskUserQuestion"}, want: status.Attention},
		{name: "question answered", event: Event{HookEventName: "PostToolUse", SessionID: "a", ToolName: "AskUserQuestion"}, want: status.Working},
		{name: "question declined", event: Event{HookEventName: "PostToolUseFailure", SessionID: "a", ToolName: "AskUserQuestion"}, want: status.Idle},
		{name: "failure", event: Event{HookEventName: "StopFailure", SessionID: "a"}, want: status.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			got, supported, err := store.Update(test.event)
			if err != nil {
				t.Fatal(err)
			}
			if !supported {
				t.Fatal("Update reported supported event as unsupported")
			}
			if got != test.want {
				t.Fatalf("aggregate = %q, want %q", got, test.want)
			}
		})
	}
}

func TestToolActivityClearsAttentionUntilNextQuestion(t *testing.T) {
	store := newTestStore(t)

	if got := mustUpdate(t, store, Event{
		HookEventName:    "Notification",
		SessionID:        "a",
		NotificationType: "permission_prompt",
	}); got != status.Attention {
		t.Fatalf("initial aggregate = %q, want attention", got)
	}

	if got := mustUpdate(t, store, Event{
		HookEventName: "PreToolUse",
		SessionID:     "a",
		ToolName:      "Bash",
	}); got != status.Working {
		t.Fatalf("aggregate during tool work = %q, want working", got)
	}

	if got := mustUpdate(t, store, Event{
		HookEventName: "PreToolUse",
		SessionID:     "a",
		ToolName:      "AskUserQuestion",
	}); got != status.Attention {
		t.Fatalf("aggregate at next question = %q, want attention", got)
	}

	if got := mustUpdate(t, store, Event{
		HookEventName: "PostToolUseFailure",
		SessionID:     "a",
		ToolName:      "AskUserQuestion",
	}); got != status.Idle {
		t.Fatalf("aggregate after decline = %q, want idle", got)
	}
}

func TestAskUserQuestionDeclineClearsOnlyThatSession(t *testing.T) {
	store := newTestStore(t)
	mustUpdate(t, store, Event{
		HookEventName: "PreToolUse", SessionID: "session-a", ToolName: "AskUserQuestion",
	})
	mustUpdate(t, store, Event{
		HookEventName: "PreToolUse", SessionID: "session-b", ToolName: "AskUserQuestion",
	})
	if got := mustUpdate(t, store, Event{
		HookEventName: "PostToolUseFailure", SessionID: "session-a", ToolName: "AskUserQuestion",
	}); got != status.Attention {
		t.Fatalf("aggregate after A decline = %q, want attention (B still asking)", got)
	}
	if got := mustUpdate(t, store, Event{
		HookEventName: "PostToolUseFailure", SessionID: "session-b", ToolName: "AskUserQuestion",
	}); got != status.Idle {
		t.Fatalf("aggregate after both declines = %q, want idle", got)
	}
}

func TestStopChangesOnlyItsSessionToIdle(t *testing.T) {
	store := newTestStore(t)
	mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: "a"})
	mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: "b"})
	if got := mustUpdate(t, store, Event{HookEventName: "Stop", SessionID: "b"}); got != status.Working {
		t.Fatalf("aggregate = %q, want working", got)
	}
	state := readState(t, store.path)
	if state.Sessions["a"].State != status.Working || state.Sessions["b"].State != status.Idle {
		t.Fatalf("sessions = %#v", state.Sessions)
	}
}

func TestSessionEndRemovesOnlyMatchingSession(t *testing.T) {
	tests := []struct {
		name       string
		otherEvent string
		want       status.State
	}{
		{name: "working remains", otherEvent: "UserPromptSubmit", want: status.Working},
		{name: "idle remains", otherEvent: "Stop", want: status.Idle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			mustUpdate(t, store, Event{HookEventName: test.otherEvent, SessionID: "a"})
			mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: "b"})
			got := mustUpdate(t, store, Event{HookEventName: "SessionEnd", SessionID: "b"})
			if got != test.want {
				t.Fatalf("aggregate = %q, want %q", got, test.want)
			}
			state := readState(t, store.path)
			if _, exists := state.Sessions["b"]; exists || len(state.Sessions) != 1 {
				t.Fatalf("sessions = %#v", state.Sessions)
			}
		})
	}
}

func TestFinalSessionEndReportsInactiveSource(t *testing.T) {
	store := newTestStore(t)
	mustUpdate(t, store, Event{HookEventName: "Stop", SessionID: "a"})
	update, supported, err := store.UpdateLifecycle(Event{HookEventName: "SessionEnd", SessionID: "a"})
	if err != nil || !supported {
		t.Fatalf("UpdateLifecycle: supported=%t err=%v", supported, err)
	}
	if update.Active {
		t.Fatalf("update = %#v, want inactive", update)
	}
}

func TestUnknownSessionEndAggregatesRemainingSessions(t *testing.T) {
	store := newTestStore(t)
	mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: "a"})
	if got := mustUpdate(t, store, Event{HookEventName: "SessionEnd", SessionID: "unknown"}); got != status.Working {
		t.Fatalf("aggregate = %q, want working", got)
	}
}

func TestBlankSessionIDDoesNotCreatePersistentSession(t *testing.T) {
	store := newTestStore(t)
	if got := mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: " "}); got != status.Idle {
		t.Fatalf("aggregate = %q, want idle", got)
	}
	if sessions := readState(t, store.path).Sessions; len(sessions) != 0 {
		t.Fatalf("sessions = %#v, want empty", sessions)
	}
}

func TestAggregatePriority(t *testing.T) {
	if got := Aggregate(nil); got != status.Idle {
		t.Fatalf("empty aggregate = %q", got)
	}
	sessions := map[string]Session{"working": {State: status.Working}}
	if got := Aggregate(sessions); got != status.Working {
		t.Fatal(got)
	}
	sessions["attention"] = Session{State: status.Attention}
	if got := Aggregate(sessions); got != status.Attention {
		t.Fatal(got)
	}
	sessions["error"] = Session{State: status.Error}
	if got := Aggregate(sessions); got != status.Error {
		t.Fatal(got)
	}
}

func TestStaleSessionsArePruned(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	store.ttl = time.Hour
	store.now = func() time.Time { return now }
	writeState(t, store.path, sessionState{Sessions: map[string]Session{
		"stale": {State: status.Error, UpdatedAt: now.Add(-2 * time.Hour)},
		"fresh": {State: status.Working, UpdatedAt: now.Add(-30 * time.Minute)},
	}})
	if got := mustUpdate(t, store, Event{HookEventName: "SessionEnd", SessionID: "unknown"}); got != status.Working {
		t.Fatalf("aggregate = %q, want working", got)
	}
	if _, exists := readState(t, store.path).Sessions["stale"]; exists {
		t.Fatal("stale session was not removed")
	}
}

func TestTTLConfiguration(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{value: "30m", want: 30 * time.Minute},
		{value: "invalid", want: DefaultSessionTTL},
		{value: "0", want: DefaultSessionTTL},
		{value: "-2h", want: DefaultSessionTTL},
	}
	for _, test := range tests {
		config, err := StateConfigFromEnv(func(key string) string {
			if key == SessionTTLEnv {
				return test.value
			}
			return ""
		}, func() (string, error) { return "/home/test", nil })
		if err != nil || config.TTL != test.want {
			t.Fatalf("value %q: config=%#v err=%v", test.value, config, err)
		}
	}
}

func TestStatePathExpandsCurrentUserHome(t *testing.T) {
	config, err := StateConfigFromEnv(func(key string) string {
		if key == StateFileEnv {
			return "~/.state/claude.json"
		}
		return ""
	}, func() (string, error) { return "/home/current", nil })
	if err != nil {
		t.Fatal(err)
	}
	if config.Path != "/home/current/.state/claude.json" {
		t.Fatalf("path = %q", config.Path)
	}
}

func TestStatePermissionsAndAtomicPersistence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "state", "sessions.json")
	store, err := NewSessionStore(path, DefaultSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: "a"})
	if runtime.GOOS != "windows" {
		directoryInfo, _ := os.Stat(filepath.Dir(path))
		fileInfo, _ := os.Stat(path)
		if directoryInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
			t.Fatalf("permissions directory=%o file=%o", directoryInfo.Mode().Perm(), fileInfo.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatalf("temporary state file remains: %s", entry.Name())
		}
	}
	_ = readState(t, path)
}

func TestMalformedStateFailsWithoutOverwrite(t *testing.T) {
	store := newTestStore(t)
	original := []byte(`not-json`)
	if err := os.WriteFile(store.path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(Event{HookEventName: "UserPromptSubmit", SessionID: "a"}); err == nil {
		t.Fatal("Update returned no error")
	}
	got, _ := os.ReadFile(store.path)
	if !bytes.Equal(got, original) {
		t.Fatalf("malformed state was overwritten: %q", got)
	}
}

func TestUnsupportedEventDoesNotRewriteState(t *testing.T) {
	store := newTestStore(t)
	mustUpdate(t, store, Event{HookEventName: "UserPromptSubmit", SessionID: "a"})
	original, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}

	_, supported, err := store.Update(Event{HookEventName: "FutureHookEvent", SessionID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if supported {
		t.Fatal("Update reported unsupported event as supported")
	}
	got, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("state changed for unsupported event:\n%s\nwant:\n%s", got, original)
	}
}

func TestConcurrentUpdatesDoNotLoseSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	const count = 8
	commands := make([]*exec.Cmd, 0, count)
	for index := 0; index < count; index++ {
		command := exec.Command(os.Args[0], "-test.run=^TestSessionStoreUpdateHelper$")
		command.Env = append(os.Environ(),
			"AURORA_STORE_HELPER=1",
			"AURORA_STORE_HELPER_PATH="+path,
			fmt.Sprintf("AURORA_STORE_HELPER_SESSION=session-%d", index),
		)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper process failed: %v", err)
		}
	}
	if got := len(readState(t, path).Sessions); got != count {
		t.Fatalf("session count = %d, want %d", got, count)
	}
}

func TestSessionStoreUpdateHelper(t *testing.T) {
	if os.Getenv("AURORA_STORE_HELPER") != "1" {
		return
	}
	store, err := NewSessionStore(os.Getenv("AURORA_STORE_HELPER_PATH"), DefaultSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(Event{
		HookEventName: "UserPromptSubmit",
		SessionID:     os.Getenv("AURORA_STORE_HELPER_SESSION"),
	}); err != nil {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T) *SessionStore {
	t.Helper()
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"), DefaultSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) }
	return store
}

func mustUpdate(t *testing.T, store *SessionStore, event Event) status.State {
	t.Helper()
	state, supported, err := store.Update(event)
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		t.Fatal("Update reported supported event as unsupported")
	}
	return state
}

func readState(t *testing.T, path string) sessionState {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state sessionState
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func writeState(t *testing.T, path string, state sessionState) {
	t.Helper()
	content, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}
