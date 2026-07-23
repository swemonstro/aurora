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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/relay"
	"github.com/swemonstro/aurora/internal/status"
)

func TestRunPublishesConfiguredCodexSource(t *testing.T) {
	snapshots := make(chan presence.Snapshot, 1)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/presence" {
			t.Errorf(
				"request = %s %s",
				request.Method,
				request.URL.Path,
			)
		}

		var snapshot presence.Snapshot
		if err := json.NewDecoder(request.Body).Decode(&snapshot); err != nil {
			t.Errorf("decode snapshot: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		snapshots <- snapshot
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "codex-api.json")
	values := map[string]string{
		codexhook.RelayURLEnv:  server.URL,
		codexhook.SourceEnv:    "codex-api",
		codexhook.StateFileEnv: statePath,
	}

	run(
		context.Background(),
		strings.NewReader(
			`{"hook_event_name":"UserPromptSubmit","session_id":"session-a","turn_id":"turn-a"}`,
		),
		func(key string) string { return values[key] },
	)

	select {
	case snapshot := <-snapshots:
		if snapshot.Version != presence.ProtocolVersion {
			t.Errorf(
				"version = %d, want %d",
				snapshot.Version,
				presence.ProtocolVersion,
			)
		}
		if snapshot.Source != "codex-api" {
			t.Errorf(
				"source = %q, want %q",
				snapshot.Source,
				"codex-api",
			)
		}
		if snapshot.State != status.Working {
			t.Errorf(
				"state = %q, want %q",
				snapshot.State,
				status.Working,
			)
		}
		if snapshot.Timestamp.IsZero() {
			t.Error("timestamp is zero")
		}
	case <-time.After(time.Second):
		t.Fatal("no snapshot was published")
	}
}

func TestRunAggregatesConcurrentSessions(t *testing.T) {
	snapshots := make(chan presence.Snapshot, 3)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		var snapshot presence.Snapshot
		if err := json.NewDecoder(request.Body).Decode(&snapshot); err != nil {
			t.Errorf("decode snapshot: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		snapshots <- snapshot
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "codex-business.json")
	values := map[string]string{
		codexhook.RelayURLEnv:  server.URL,
		codexhook.SourceEnv:    "codex-business",
		codexhook.StateFileEnv: statePath,
	}
	getenv := func(key string) string { return values[key] }

	events := []string{
		`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`,
		`{"hook_event_name":"PermissionRequest","session_id":"session-b","tool_name":"Bash"}`,
		`{"hook_event_name":"Stop","session_id":"session-a"}`,
	}

	for _, event := range events {
		run(
			context.Background(),
			strings.NewReader(event),
			getenv,
		)
	}

	wantStates := []status.State{
		status.Working,
		status.Attention,
		status.Attention,
	}

	for index, want := range wantStates {
		select {
		case snapshot := <-snapshots:
			if snapshot.Source != "codex-business" {
				t.Fatalf(
					"step %d source = %q",
					index+1,
					snapshot.Source,
				)
			}
			if snapshot.State != want {
				t.Fatalf(
					"step %d state = %q, want %q",
					index+1,
					snapshot.State,
					want,
				)
			}
		case <-time.After(time.Second):
			t.Fatalf(
				"step %d published no snapshot",
				index+1,
			)
		}
	}
}

func TestRunIgnoresMalformedAndUnsupportedInput(t *testing.T) {
	requests := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		requests <- struct{}{}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	values := map[string]string{
		codexhook.RelayURLEnv:  server.URL,
		codexhook.StateFileEnv: filepath.Join(t.TempDir(), "state.json"),
	}
	getenv := func(key string) string { return values[key] }

	for _, input := range []string{
		`{`,
		`{"hook_event_name":"FutureEvent","session_id":"a"}`,
	} {
		run(
			context.Background(),
			strings.NewReader(input),
			getenv,
		)
	}

	select {
	case <-requests:
		t.Fatal("unexpected snapshot was published")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishLifecycleReturnsConstructionAndDeliveryErrors(t *testing.T) {
	failingRelay := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failingRelay.Close()

	tests := []struct {
		name      string
		config    codexhook.Config
		update    codexhook.LifecycleUpdate
		wantError string
	}{
		{
			name:      "publisher construction",
			config:    codexhook.Config{RelayURL: "://invalid", Source: "codex-api"},
			update:    codexhook.LifecycleUpdate{State: status.Working, Active: true},
			wantError: "create lifecycle publisher",
		},
		{
			name:      "agent construction",
			config:    codexhook.Config{RelayURL: failingRelay.URL, Source: " "},
			update:    codexhook.LifecycleUpdate{State: status.Working, Active: true},
			wantError: "create lifecycle agent",
		},
		{
			name:      "publication",
			config:    codexhook.Config{RelayURL: failingRelay.URL, Source: "codex-api"},
			update:    codexhook.LifecycleUpdate{State: status.Working, Active: true},
			wantError: "publish presence snapshot",
		},
		{
			name:      "removal",
			config:    codexhook.Config{RelayURL: failingRelay.URL, Source: "codex-api"},
			update:    codexhook.LifecycleUpdate{Active: false},
			wantError: "remove presence source",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := publishLifecycle(context.Background(), test.config, test.update)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want containing %q", err, test.wantError)
			}
		})
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
			codexhook.RelayURLEnv:  server.URL,
			codexhook.SourceEnv:    "codex-api",
			codexhook.StateFileEnv: filepath.Join(t.TempDir(), "state.json"),
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
			codexhook.RelayURLEnv:  server.URL,
			codexhook.SourceEnv:    "codex-api",
			codexhook.StateFileEnv: filepath.Join(t.TempDir(), "state.json"),
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

func TestRunSessionEndFileRemovesSession(t *testing.T) {
	type publication struct {
		method   string
		snapshot presence.Snapshot
	}
	publications := make(chan publication, 2)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		var snapshot presence.Snapshot
		if request.Method == http.MethodPost {
			if err := json.NewDecoder(request.Body).Decode(&snapshot); err != nil {
				t.Errorf("decode snapshot: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
		}

		publications <- publication{method: request.Method, snapshot: snapshot}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	sessionIDPath := filepath.Join(directory, "session-id")

	values := map[string]string{
		codexhook.RelayURLEnv:  server.URL,
		codexhook.SourceEnv:    "codex-api",
		codexhook.StateFileEnv: statePath,
	}
	getenv := func(key string) string { return values[key] }

	run(
		context.Background(),
		strings.NewReader(
			`{"hook_event_name":"PermissionRequest","session_id":"session-a"}`,
		),
		getenv,
	)

	if err := os.WriteFile(
		sessionIDPath,
		[]byte("session-a\n"),
		0o600,
	); err != nil {
		t.Fatalf("write session ID: %v", err)
	}

	runSessionEndFile(
		context.Background(),
		sessionIDPath,
		getenv,
	)

	wantMethods := []string{
		http.MethodPost,
		http.MethodDelete,
	}

	for index, want := range wantMethods {
		select {
		case publication := <-publications:
			if publication.method != want {
				t.Fatalf(
					"step %d method = %q, want %q",
					index+1,
					publication.method,
					want,
				)
			}
			if index == 0 && publication.snapshot.State != status.Attention {
				t.Fatalf("initial state = %q, want attention", publication.snapshot.State)
			}
		case <-time.After(time.Second):
			t.Fatalf(
				"step %d published no snapshot",
				index+1,
			)
		}
	}
}

func TestRunSessionEndFileIgnoresMissingAndEmptyFile(t *testing.T) {
	requests := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		requests <- struct{}{}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	directory := t.TempDir()
	values := map[string]string{
		codexhook.RelayURLEnv:  server.URL,
		codexhook.StateFileEnv: filepath.Join(directory, "state.json"),
	}
	getenv := func(key string) string { return values[key] }

	runSessionEndFile(
		context.Background(),
		filepath.Join(directory, "missing"),
		getenv,
	)

	emptyPath := filepath.Join(directory, "empty")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	runSessionEndFile(
		context.Background(),
		emptyPath,
		getenv,
	)

	select {
	case <-requests:
		t.Fatal("unexpected snapshot was published")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCodexAPIAndBusinessLifecycleIsolation(t *testing.T) {
	store := &relay.Store{}
	handler, err := relay.NewHandler(store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler.Routes())
	defer server.Close()

	directory := t.TempDir()
	runSource := func(source, stateFile, event string) {
		values := map[string]string{
			codexhook.RelayURLEnv:  server.URL,
			codexhook.SourceEnv:    source,
			codexhook.StateFileEnv: stateFile,
		}
		run(context.Background(), strings.NewReader(event), func(key string) string { return values[key] })
	}
	apiState := filepath.Join(directory, "api.json")
	businessState := filepath.Join(directory, "business.json")
	runSource("codex-api", apiState, `{"hook_event_name":"UserPromptSubmit","session_id":"api"}`)
	runSource("codex-business", businessState, `{"hook_event_name":"PermissionRequest","session_id":"business"}`)
	runSource("codex-api", apiState, `{"hook_event_name":"SessionEnd","session_id":"api"}`)

	got, ok := store.Latest()
	if !ok || got.Source != "codex-business" || got.State != status.Attention {
		t.Fatalf("remaining presence = %#v, %t", got, ok)
	}
	runSource("codex-business", businessState, `{"hook_event_name":"SessionEnd","session_id":"business"}`)
	if _, ok := store.Latest(); ok {
		t.Fatal("relay remained online after both Codex sources ended")
	}
}

func TestWatchPermissionPublishesIdleForMatchingAbort(t *testing.T) {
	states := make(chan status.State, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var snapshot presence.Snapshot
		if err := json.NewDecoder(request.Body).Decode(&snapshot); err != nil {
			t.Errorf("decode snapshot: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		states <- snapshot.State
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptPath := filepath.Join(directory, "transcript.jsonl")
	oldAbort := `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-a"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(oldAbort), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := codexhook.NewSessionStore(statePath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(codexhook.Event{
		HookEventName:  "PermissionRequest",
		SessionID:      "session-a",
		TurnID:         "turn-a",
		TranscriptPath: transcriptPath,
	})
	if err != nil || update.Watch == nil {
		t.Fatalf("permission update = %#v, err=%v", update, err)
	}
	file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-a"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		codexhook.RelayURLEnv:    server.URL,
		codexhook.SourceEnv:      "codex-api",
		codexhook.StateFileEnv:   statePath,
		codexhook.SessionTTLEnv:  "1h",
		codexhook.WatcherFileEnv: filepath.Join(directory, "watchers"),
	}
	if err := watchPermission(
		context.Background(),
		*update.Watch,
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("watch permission: %v", err)
	}

	select {
	case got := <-states:
		if got != status.Idle {
			t.Fatalf("state = %q, want idle", got)
		}
	case <-time.After(time.Second):
		t.Fatal("abort recovery was not published")
	}
	entries, err := os.ReadDir(values[codexhook.WatcherFileEnv])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("watcher registration leaked: %#v", entries)
	}
}

func TestWatchPermissionReturnsCancellationPublicationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptPath := filepath.Join(directory, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := codexhook.NewSessionStore(statePath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(codexhook.Event{
		HookEventName:  "PermissionRequest",
		SessionID:      "session-a",
		TurnID:         "turn-a",
		TranscriptPath: transcriptPath,
	})
	if err != nil || update.Watch == nil {
		t.Fatalf("permission update = %#v, err=%v", update, err)
	}
	if err := os.WriteFile(
		transcriptPath,
		[]byte(`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-a"}}`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	watcherDirectory := filepath.Join(directory, "watchers")
	values := map[string]string{
		codexhook.RelayURLEnv:    server.URL,
		codexhook.SourceEnv:      "codex-api",
		codexhook.StateFileEnv:   statePath,
		codexhook.SessionTTLEnv:  "1h",
		codexhook.WatcherFileEnv: watcherDirectory,
	}
	err = watchPermission(
		context.Background(),
		*update.Watch,
		func(key string) string { return values[key] },
	)
	if err == nil || !strings.Contains(err.Error(), "publish presence snapshot") {
		t.Fatalf("error = %v, want cancellation publication failure", err)
	}
	entries, readErr := os.ReadDir(watcherDirectory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("watcher registration leaked: %#v", entries)
	}
}

func TestRunLocalIngressEnabledSkipsLegacyStateAndRelay(t *testing.T) {
	relayHits := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		relayHits <- struct{}{}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	stateFile := filepath.Join(t.TempDir(), "codex-sessions.json")
	values := map[string]string{
		codexhook.RelayURLEnv:                  server.URL,
		codexhook.StateFileEnv:                 stateFile,
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket: filepath.Join(
			t.TempDir(),
			"missing.sock",
		),
	}

	err := run(
		context.Background(),
		strings.NewReader(
			`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`,
		),
		func(key string) string { return values[key] },
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	select {
	case <-relayHits:
		t.Fatal("local mode published to the legacy relay")
	case <-time.After(150 * time.Millisecond):
	}
	// Local mode may maintain session state for permission-watcher
	// coordination, but must never publish the aggregate to the relay.
}

func TestLocalPermissionCancelDeliversIdleToSocket(t *testing.T) {
	defer stubPermissionWatcher(t)()

	requests := make(chan localhooktransport.IngestRequest, 4)
	socketPath := startCodexLocalIngestSocketN(t, requests, 4)

	relayHits := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relayHits <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptA := filepath.Join(directory, "a.jsonl")
	transcriptB := filepath.Join(directory, "b.jsonl")
	if err := os.WriteFile(transcriptA, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	watcherDir := filepath.Join(directory, "watchers")
	values := map[string]string{
		codexhook.RelayURLEnv:                  server.URL,
		codexhook.SourceEnv:                    "codex-api",
		codexhook.StateFileEnv:                 statePath,
		codexhook.SessionTTLEnv:                "1h",
		codexhook.WatcherFileEnv:               watcherDir,
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	getenv := func(key string) string { return values[key] }

	// Two concurrent Codex sessions both wait for permission.
	if err := run(context.Background(), strings.NewReader(fmt.Sprintf(
		`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-a","transcript_path":%q}`,
		transcriptA,
	)), getenv); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), strings.NewReader(fmt.Sprintf(
		`{"hook_event_name":"PermissionRequest","session_id":"session-b","turn_id":"turn-b","transcript_path":%q}`,
		transcriptB,
	)), getenv); err != nil {
		t.Fatal(err)
	}

	// Drain permission attention deliveries.
	for i := 0; i < 2; i++ {
		select {
		case req := <-requests:
			if req.Payload.State != instancepresence.StateAttention {
				t.Fatalf("permission delivery %d = %#v", i+1, req.Payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing permission attention %d", i+1)
		}
	}

	store, err := codexhook.NewSessionStore(statePath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	watchA := mustPermissionWatch(t, statePath, "session-a", "turn-a", transcriptA)

	// Esc / turn_aborted only for session-a.
	if err := os.WriteFile(transcriptA, []byte(
		`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-a"}}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := watchPermission(context.Background(), watchA, getenv); err != nil {
		t.Fatalf("watchPermission A: %v", err)
	}

	select {
	case req := <-requests:
		if req.Payload.HookSessionRef != "session-a" {
			t.Fatalf("cancel session = %q, want session-a", req.Payload.HookSessionRef)
		}
		if req.Payload.State != instancepresence.StateIdle {
			t.Fatalf("cancel state = %q, want idle", req.Payload.State)
		}
		if req.Payload.Tool != instancepresence.ToolCodex {
			t.Fatalf("tool = %q", req.Payload.Tool)
		}
	case <-time.After(time.Second):
		t.Fatal("local idle not delivered after transcript cancel")
	}

	// Session B remains pending (attention) — no spurious idle for B.
	select {
	case extra := <-requests:
		t.Fatalf("unexpected extra delivery (session-b must stay untouched): %#v", extra)
	case <-time.After(150 * time.Millisecond):
	}

	pendingB, err := store.PermissionPending(mustPermissionWatch(t, statePath, "session-b", "turn-b", transcriptB))
	if err != nil {
		t.Fatal(err)
	}
	if !pendingB {
		t.Fatal("session-b permission must still be pending after A cancel")
	}

	select {
	case <-relayHits:
		t.Fatal("local mode must not publish cancel recovery to legacy relay")
	case <-time.After(50 * time.Millisecond):
	}

	entries, err := os.ReadDir(watcherDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("watcher registration leaked: %#v", entries)
	}
}

func TestLocalPermissionStopBeforeRecoverySkipsDuplicateIdle(t *testing.T) {
	defer stubPermissionWatcher(t)()

	requests := make(chan localhooktransport.IngestRequest, 4)
	socketPath := startCodexLocalIngestSocketN(t, requests, 4)

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptPath := filepath.Join(directory, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		codexhook.RelayURLEnv:                  "http://127.0.0.1:1",
		codexhook.StateFileEnv:                 statePath,
		codexhook.SessionTTLEnv:                "1h",
		codexhook.WatcherFileEnv:               filepath.Join(directory, "watchers"),
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	getenv := func(key string) string { return values[key] }

	if err := run(context.Background(), strings.NewReader(fmt.Sprintf(
		`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-a","transcript_path":%q}`,
		transcriptPath,
	)), getenv); err != nil {
		t.Fatal(err)
	}
	watch := mustPermissionWatch(t, statePath, "session-a", "turn-a", transcriptPath)

	// Normal Stop restores the session before transcript recovery runs.
	if err := run(context.Background(), strings.NewReader(
		`{"hook_event_name":"Stop","session_id":"session-a"}`,
	), getenv); err != nil {
		t.Fatal(err)
	}

	// Drain attention + stop idle from the socket.
	for i := 0; i < 2; i++ {
		select {
		case <-requests:
		case <-time.After(time.Second):
			t.Fatalf("missing delivery %d", i+1)
		}
	}

	if err := os.WriteFile(transcriptPath, []byte(
		`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-a"}}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := watchPermission(context.Background(), watch, getenv); err != nil {
		t.Fatalf("watchPermission after Stop: %v", err)
	}
	select {
	case extra := <-requests:
		t.Fatalf("recovery must not deliver duplicate idle after Stop: %#v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestLocalPermissionApprovedFollowsWorkingIdle(t *testing.T) {
	defer stubPermissionWatcher(t)()

	requests := make(chan localhooktransport.IngestRequest, 4)
	socketPath := startCodexLocalIngestSocketN(t, requests, 4)

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptPath := filepath.Join(directory, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		codexhook.StateFileEnv:                 statePath,
		codexhook.SessionTTLEnv:                "1h",
		codexhook.WatcherFileEnv:               filepath.Join(directory, "watchers"),
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	getenv := func(key string) string { return values[key] }

	steps := []struct {
		payload string
		want    instancepresence.EffectiveState
	}{
		{
			payload: fmt.Sprintf(
				`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-a","transcript_path":%q}`,
				transcriptPath,
			),
			want: instancepresence.StateAttention,
		},
		{
			payload: `{"hook_event_name":"PreToolUse","session_id":"session-a","tool_name":"Bash"}`,
			want:    instancepresence.StateWorking,
		},
		{
			payload: `{"hook_event_name":"Stop","session_id":"session-a"}`,
			want:    instancepresence.StateIdle,
		},
	}
	for _, step := range steps {
		if err := run(context.Background(), strings.NewReader(step.payload), getenv); err != nil {
			t.Fatal(err)
		}
		select {
		case req := <-requests:
			if req.Payload.State != step.want || req.Payload.HookSessionRef != "session-a" {
				t.Fatalf("got %#v, want state=%q session-a", req.Payload, step.want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing delivery for %s", step.want)
		}
	}
}

func stubPermissionWatcher(t *testing.T) func() {
	t.Helper()
	previous := startPermissionWatcher
	startPermissionWatcher = func(codexhook.PermissionWatch, func(string) string) error {
		return nil
	}
	return func() { startPermissionWatcher = previous }
}

func mustPermissionWatch(
	t *testing.T,
	statePath, sessionID, turnID, transcriptPath string,
) codexhook.PermissionWatch {
	t.Helper()
	// Rebuild watch identity from the live session store entry.
	store, err := codexhook.NewSessionStore(statePath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Probe revisions 1..32 until PermissionPending matches (stable in tests).
	for revision := uint64(1); revision <= 32; revision++ {
		watch := codexhook.PermissionWatch{
			SessionID:        sessionID,
			TurnID:           turnID,
			TranscriptPath:   transcriptPath,
			TranscriptOffset: 0,
			Revision:         revision,
		}
		pending, err := store.PermissionPending(watch)
		if err != nil {
			t.Fatal(err)
		}
		if pending {
			return watch
		}
	}
	t.Fatalf("no pending permission for session %s", sessionID)
	return codexhook.PermissionWatch{}
}

func startCodexLocalIngestSocketN(t *testing.T, requests chan<- localhooktransport.IngestRequest, accepts int) string {
	t.Helper()
	// Package 6 rejects sockets under world-writable /tmp; use $HOME.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, "aurora-codex-hook-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
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
		for i := 0; i < accepts; i++ {
			connection, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return
			}
			func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
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
				encoded, err := localhooktransport.EncodeIngestResponseJSON(
					response, localhooktransport.DefaultIngestMaximumResponseBytes,
				)
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
		}
	}()
	return socketPath
}
