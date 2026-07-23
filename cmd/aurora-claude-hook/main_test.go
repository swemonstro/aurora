package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/localhooktransport"
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

func TestRunReturnsLifecyclePublicationAndRemovalFailures(t *testing.T) {
	t.Run("publication", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			_ *http.Request,
		) {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		values := map[string]string{
			claudehook.RelayURLEnv:  server.URL,
			claudehook.StateFileEnv: filepath.Join(t.TempDir(), "sessions.json"),
		}
		err := run(
			context.Background(),
			strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`),
			func(key string) string { return values[key] },
		)
		if err == nil || !strings.Contains(err.Error(), "publish presence snapshot") {
			t.Fatalf("error = %v, want publication failure", err)
		}
	})

	t.Run("removal", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			request *http.Request,
		) {
			if request.Method == http.MethodDelete {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()
		values := map[string]string{
			claudehook.RelayURLEnv:  server.URL,
			claudehook.StateFileEnv: filepath.Join(t.TempDir(), "sessions.json"),
		}
		getenv := func(key string) string { return values[key] }
		if err := run(
			context.Background(),
			strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`),
			getenv,
		); err != nil {
			t.Fatalf("seed lifecycle: %v", err)
		}
		err := run(
			context.Background(),
			strings.NewReader(`{"hook_event_name":"SessionEnd","session_id":"session-a"}`),
			getenv,
		)
		if err == nil || !strings.Contains(err.Error(), "remove presence source") {
			t.Fatalf("error = %v, want removal failure", err)
		}
	})
}

func TestConcurrentSessionFlow(t *testing.T) {
	published := make(chan status.State, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			published <- status.State("deleted")
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
		{payload: `{"hook_event_name":"SessionEnd","session_id":"session-a"}`, want: status.State("deleted")},
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

func TestLocalIngressEnabledReachesSocketWithZeroRelay(t *testing.T) {
	requests := make(chan localhooktransport.IngestRequest, 1)
	socketPath := startLocalIngestSocket(t, requests)

	relayHits := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relayHits <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	stateFile := filepath.Join(t.TempDir(), "sessions.json")
	values := map[string]string{
		claudehook.RelayURLEnv:                 server.URL,
		claudehook.StateFileEnv:                stateFile,
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	if err := run(
		context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`),
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("run: %v", err)
	}

	select {
	case request := <-requests:
		if request.ProtocolVersion != localhooktransport.IngestProtocolVersion {
			t.Fatalf("protocol_version = %d, want %d", request.ProtocolVersion, localhooktransport.IngestProtocolVersion)
		}
		if request.Operation != localhooktransport.OperationIngestHookEvent {
			t.Fatalf("operation = %q, want %q", request.Operation, localhooktransport.OperationIngestHookEvent)
		}
		if request.Payload.Tool != instancepresence.ToolClaude {
			t.Fatalf("tool = %q, want %q", request.Payload.Tool, instancepresence.ToolClaude)
		}
		if request.Payload.HookSessionRef != "session-a" {
			t.Fatalf("hook_session_ref = %q, want session-a", request.Payload.HookSessionRef)
		}
		if request.Payload.Lifecycle != instancecorrelation.LifecycleActive {
			t.Fatalf("lifecycle = %q, want %q", request.Payload.Lifecycle, instancecorrelation.LifecycleActive)
		}
		if request.Payload.State != instancepresence.StateWorking {
			t.Fatalf("state = %q, want %q", request.Payload.State, instancepresence.StateWorking)
		}
	case <-time.After(time.Second):
		t.Fatal("enabled local delivery did not reach socket")
	}

	select {
	case <-relayHits:
		t.Fatal("enabled local mode must not call relay")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(stateFile); err == nil {
		t.Fatal("enabled local mode must not create legacy session store")
	}
}

func TestLocalIngressDisabledUsesLegacyRelay(t *testing.T) {
	requests := make(chan localhooktransport.IngestRequest, 1)
	socketPath := startLocalIngestSocket(t, requests)

	relayHits := make(chan presence.Snapshot, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var snapshot presence.Snapshot
		if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
			t.Errorf("decode: %v", err)
		}
		relayHits <- snapshot
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:                 server.URL,
		claudehook.SourceEnv:                   "overridden-source",
		claudehook.StateFileEnv:                filepath.Join(t.TempDir(), "sessions.json"),
		localhooktransport.EnvLocalHookEnabled: "0",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	if err := run(
		context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`),
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("run: %v", err)
	}

	select {
	case request := <-requests:
		t.Fatalf("disabled local delivery reached socket: %#v", request)
	case <-time.After(150 * time.Millisecond):
	}

	select {
	case snapshot := <-relayHits:
		if snapshot.Source != "overridden-source" {
			t.Fatalf("source = %q, want overridden-source", snapshot.Source)
		}
		if snapshot.State != status.Working {
			t.Fatalf("state = %q, want working", snapshot.State)
		}
	case <-time.After(time.Second):
		t.Fatal("disabled local mode did not use legacy relay")
	}
}

func TestLocalIngressEnabledMissingSocketNoRelayNoError(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "missing.sock")

	relayHits := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relayHits <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:                 server.URL,
		claudehook.StateFileEnv:                filepath.Join(t.TempDir(), "sessions.json"),
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	if err := run(
		context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`),
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("run: %v", err)
	}

	select {
	case <-relayHits:
		t.Fatal("enabled local mode with missing socket must not call relay")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestLocalIngressSessionEndDoesNotRemoveClaudeCode(t *testing.T) {
	requests := make(chan localhooktransport.IngestRequest, 1)
	socketPath := startLocalIngestSocket(t, requests)

	relayMethods := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayMethods <- r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:                 server.URL,
		claudehook.StateFileEnv:                filepath.Join(t.TempDir(), "sessions.json"),
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	if err := run(
		context.Background(),
		strings.NewReader(`{"hook_event_name":"SessionEnd","session_id":"session-a"}`),
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("run: %v", err)
	}

	select {
	case request := <-requests:
		if request.Payload.Lifecycle != instancecorrelation.LifecycleEnded {
			t.Fatalf("lifecycle = %q, want ended", request.Payload.Lifecycle)
		}
		if request.Payload.HookSessionRef != "session-a" {
			t.Fatalf("session = %q", request.Payload.HookSessionRef)
		}
	case <-time.After(time.Second):
		t.Fatal("SessionEnd was not delivered locally")
	}

	select {
	case method := <-relayMethods:
		t.Fatalf("local SessionEnd must not touch relay (got %s); claude-code must not be deleted", method)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLocalIngressUnsupportedEventIsNotDelivered(t *testing.T) {
	requests := make(chan localhooktransport.IngestRequest, 1)
	socketPath := startLocalIngestSocket(t, requests)

	relayHits := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relayHits <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		claudehook.RelayURLEnv:                 server.URL,
		claudehook.StateFileEnv:                filepath.Join(t.TempDir(), "sessions.json"),
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	if err := run(
		context.Background(),
		strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"session-a","tool_name":"Bash"}`),
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("run: %v", err)
	}

	select {
	case request := <-requests:
		t.Fatalf("unsupported event was delivered locally: %#v", request)
	case <-time.After(150 * time.Millisecond):
	}

	select {
	case <-relayHits:
		t.Fatal("unsupported event was published to relay")
	default:
	}
}

func startLocalIngestSocket(t *testing.T, requests chan<- localhooktransport.IngestRequest) string {
	t.Helper()
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "hook.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(time.Second))

		var header [4]byte
		if _, err := io.ReadFull(connection, header[:]); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(header[:])
		if size == 0 || size > localhooktransport.DefaultIngestMaximumRequestBytes {
			return
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(connection, payload); err != nil {
			return
		}
		request, err := localhooktransport.DecodeIngestRequestJSON(payload)
		if err != nil {
			return
		}
		response := localhooktransport.IngestResponse{
			ProtocolVersion:    localhooktransport.IngestProtocolVersion,
			RequestID:          request.RequestID,
			Status:             localhooktransport.StatusOK,
			ErrorCodes:         []localhooktransport.ErrorCode{},
			NoBindingPerformed: true,
		}
		encoded, err := localhooktransport.EncodeIngestResponseJSON(response, localhooktransport.DefaultIngestMaximumResponseBytes)
		if err != nil {
			return
		}
		var responseHeader [4]byte
		binary.BigEndian.PutUint32(responseHeader[:], uint32(len(encoded)))
		if _, err := connection.Write(responseHeader[:]); err != nil {
			return
		}
		if _, err := connection.Write(encoded); err != nil {
			return
		}
		select {
		case requests <- request:
		default:
		}
	}()
	return socketPath
}

// secureTempDir creates a private directory under the home path. Package 6
// rejects sockets under /tmp, so t.TempDir() is not usable for local delivery.
func secureTempDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-claude-hook-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove secure test directory: %v", err)
		}
	})
	return directory
}
