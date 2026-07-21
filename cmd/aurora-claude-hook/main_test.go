package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

func TestRunPublishesWithEnvironmentOverrides(t *testing.T) {
	snapshots := make(chan presence.Snapshot, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/presence" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var snapshot presence.Snapshot
		if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
			t.Errorf("decode snapshot: %v", err)
		}
		snapshots <- snapshot
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:  server.URL,
		claudehook.SourceEnv:    "overridden-source",
		claudehook.StateFileEnv: filepath.Join(t.TempDir(), "sessions.json"),
	}
	run(
		context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`),
		func(key string) string { return values[key] },
	)

	select {
	case snapshot := <-snapshots:
		if snapshot.State != status.Working {
			t.Errorf("state = %q, want %q", snapshot.State, status.Working)
		}
		if snapshot.Source != "overridden-source" {
			t.Errorf("source = %q, want overridden-source", snapshot.Source)
		}
		if snapshot.Version != presence.ProtocolVersion {
			t.Errorf("version = %d, want %d", snapshot.Version, presence.ProtocolVersion)
		}
		if snapshot.Timestamp.IsZero() {
			t.Error("timestamp is zero")
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not receive a snapshot")
	}
}

func TestRunIgnoresUnavailableRelay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	relayURL := server.URL
	server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:  relayURL,
		claudehook.StateFileEnv: filepath.Join(t.TempDir(), "sessions.json"),
	}
	done := make(chan struct{})
	go func() {
		run(
			context.Background(),
			strings.NewReader(`{"hook_event_name":"Stop"}`),
			func(key string) string { return values[key] },
		)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run blocked on unavailable relay")
	}
}

func TestCaptureFailureDoesNotPreventPublication(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	blockingFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		claudehook.RelayURLEnv:     server.URL,
		claudehook.CaptureHooksEnv: filepath.Join(blockingFile, "captures"),
		claudehook.StateFileEnv:    filepath.Join(t.TempDir(), "sessions.json"),
	}
	run(context.Background(), strings.NewReader(`{"hook_event_name":"Stop"}`), func(key string) string {
		return values[key]
	})

	select {
	case <-received:
	default:
		t.Fatal("relay did not receive publication after capture failure")
	}
}

func TestPublicationFailureDoesNotPreventCapture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	relayURL := server.URL
	server.Close()

	directory := t.TempDir()
	values := map[string]string{
		claudehook.RelayURLEnv:     relayURL,
		claudehook.CaptureHooksEnv: directory,
		claudehook.StateFileEnv:    filepath.Join(t.TempDir(), "sessions.json"),
	}
	run(context.Background(), strings.NewReader(`{"hook_event_name":"Stop"}`), func(key string) string {
		return values[key]
	})

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("capture file count = %d, want 1", len(entries))
	}
}

func TestStateFailureDoesNotPreventCapture(t *testing.T) {
	blockingFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureDirectory := t.TempDir()
	values := map[string]string{
		claudehook.StateFileEnv:    filepath.Join(blockingFile, "sessions.json"),
		claudehook.CaptureHooksEnv: captureDirectory,
	}
	run(context.Background(), strings.NewReader(
		`{"hook_event_name":"Stop","session_id":"session-a"}`,
	), func(key string) string { return values[key] })

	entries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("capture file count = %d, want 1", len(entries))
	}
}

func TestPublicationTimeoutRemainsOneSecond(t *testing.T) {
	if hookTimeout != time.Second {
		t.Fatalf("hookTimeout = %s, want 1s", hookTimeout)
	}
}

func TestPublicationSeesPersistedState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "sessions.json")
	checked := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content, err := os.ReadFile(stateFile)
		if err == nil && !strings.Contains(string(content), `"session-a"`) {
			err = fmt.Errorf("persisted state does not contain session-a")
		}
		checked <- err
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:  server.URL,
		claudehook.StateFileEnv: stateFile,
	}
	run(context.Background(), strings.NewReader(
		`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`,
	), func(key string) string { return values[key] })
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
}

func TestPublicationFailureLeavesPersistedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	relayURL := server.URL
	server.Close()
	stateFile := filepath.Join(t.TempDir(), "sessions.json")
	values := map[string]string{
		claudehook.RelayURLEnv:  relayURL,
		claudehook.StateFileEnv: stateFile,
	}
	run(context.Background(), strings.NewReader(
		`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`,
	), func(key string) string { return values[key] })

	content, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read state after publication failure: %v", err)
	}
	if !strings.Contains(string(content), `"session-a"`) {
		t.Fatalf("persisted state = %s", content)
	}
}

func TestConcurrentSessionFlow(t *testing.T) {
	published := make(chan status.State, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var snapshot presence.Snapshot
		if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
			t.Errorf("decode snapshot: %v", err)
		}
		published <- snapshot.State
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	captureDirectory := t.TempDir()
	values := map[string]string{
		claudehook.RelayURLEnv:     server.URL,
		claudehook.StateFileEnv:    filepath.Join(t.TempDir(), "sessions.json"),
		claudehook.CaptureHooksEnv: captureDirectory,
	}
	events := []struct {
		payload string
		want    status.State
	}{
		{payload: `{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`, want: status.Working},
		{payload: `{"hook_event_name":"UserPromptSubmit","session_id":"session-b"}`, want: status.Working},
		{payload: `{"hook_event_name":"Stop","session_id":"session-b"}`, want: status.Working},
		{payload: `{"hook_event_name":"SessionEnd","session_id":"session-b"}`, want: status.Working},
		{payload: `{"hook_event_name":"SessionEnd","session_id":"session-a"}`, want: status.Idle},
	}
	for index, event := range events {
		run(context.Background(), strings.NewReader(event.payload), func(key string) string { return values[key] })
		select {
		case got := <-published:
			if got != event.want {
				t.Fatalf("step %d aggregate = %q, want %q", index+1, got, event.want)
			}
			if index >= 3 {
				t.Logf("step %d aggregate = %s", index+1, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("step %d was not published", index+1)
		}
	}

	entries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(events) {
		t.Fatalf("capture count = %d, want %d", len(entries), len(events))
	}
}

func TestRunWritesNoStdout(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestHookHelperProcess")
	command.Env = append(os.Environ(), "AURORA_HOOK_HELPER=1")
	command.Stdin = strings.NewReader(`{"hook_event_name":"PreToolUse"}`)
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		t.Fatalf("helper process returned error: %v", err)
	}
	if len(output) != 0 {
		t.Fatalf("stdout = %q, want empty", output)
	}
}

func TestUnsupportedEventDoesNotPublish(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:  server.URL,
		claudehook.StateFileEnv: filepath.Join(t.TempDir(), "sessions.json"),
	}
	run(context.Background(), strings.NewReader(
		`{"hook_event_name":"PreToolUse","session_id":"session-a","tool_name":"Bash"}`,
	), func(key string) string { return values[key] })

	select {
	case <-requests:
		t.Fatal("unsupported event was published")
	default:
	}
}

func TestHookHelperProcess(t *testing.T) {
	if os.Getenv("AURORA_HOOK_HELPER") != "1" {
		return
	}
	run(context.Background(), os.Stdin, os.Getenv)
	os.Exit(0)
}
