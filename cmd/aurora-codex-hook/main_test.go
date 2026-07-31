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

	t.Run("matching stop", func(t *testing.T) {
		fail := false
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			if fail {
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
		if err := run(context.Background(), strings.NewReader(
			`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-a"}`,
		), getenv); err != nil {
			t.Fatalf("seed permission: %v", err)
		}
		fail = true
		err := run(context.Background(), strings.NewReader(
			`{"hook_event_name":"Stop","session_id":"session-a","turn_id":"turn-a"}`,
		), getenv)
		if err == nil || !strings.Contains(err.Error(), "publish presence snapshot") {
			t.Fatalf("error = %v, want matching Stop publication failure", err)
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
	// Local mode maintains only lifecycle ordering state and never publishes
	// the aggregate to the relay.
}

func TestLocalPermissionRequestDoesNotPersistTranscriptPathOrStartWatcher(t *testing.T) {
	requests := make(chan localhooktransport.IngestRequest, 2)
	socketPath := startCodexLocalIngestSocketN(t, requests, 2)

	relayHits := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relayHits <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptPath := filepath.Join(directory, "session.jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o600); err != nil {
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

	if err := run(context.Background(), strings.NewReader(fmt.Sprintf(
		`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-a","transcript_path":%q,"cwd":"/secret/project","prompt":"do not store"}`,
		transcriptPath,
	)), getenv); err != nil {
		t.Fatal(err)
	}

	select {
	case req := <-requests:
		if req.Payload.HookSessionRef != "session-a" {
			t.Fatalf("permission session = %q, want session-a", req.Payload.HookSessionRef)
		}
		if req.Payload.State != instancepresence.StateAttention {
			t.Fatalf("permission state = %q, want attention", req.Payload.State)
		}
		if req.Payload.Tool != instancepresence.ToolCodex {
			t.Fatalf("tool = %q", req.Payload.Tool)
		}
	case <-time.After(time.Second):
		t.Fatal("local attention not delivered for permission request")
	}

	select {
	case <-relayHits:
		t.Fatal("local mode must not publish permission event to legacy relay")
	case <-time.After(50 * time.Millisecond):
	}

	entries, err := os.ReadDir(watcherDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("watcher registration leaked: %#v", entries)
	}

	stateFile, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{transcriptPath, "/secret/project", "do not store"} {
		if strings.Contains(string(stateFile), forbidden) {
			t.Fatalf("state file contains sensitive payload content %q in %s", forbidden, stateFile)
		}
	}
}

func TestLocalMatchingStopDeliversIdleWithoutAffectingParallelSession(t *testing.T) {
	requests := make(chan localhooktransport.IngestRequest, 4)
	socketPath := startCodexLocalIngestSocketN(t, requests, 3)
	statePath := filepath.Join(t.TempDir(), "state.json")
	values := map[string]string{
		codexhook.StateFileEnv:                 statePath,
		codexhook.SessionTTLEnv:                "1h",
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	getenv := func(key string) string { return values[key] }

	for _, payload := range []string{
		`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-a"}`,
		`{"hook_event_name":"PermissionRequest","session_id":"session-b","turn_id":"turn-b"}`,
		`{"hook_event_name":"Stop","session_id":"session-a","turn_id":"turn-a"}`,
	} {
		if err := run(context.Background(), strings.NewReader(payload), getenv); err != nil {
			t.Fatal(err)
		}
	}

	wants := []struct {
		session instancepresence.OpaqueIdentity
		state   instancepresence.EffectiveState
	}{
		{"session-a", instancepresence.StateAttention},
		{"session-b", instancepresence.StateAttention},
		{"session-a", instancepresence.StateIdle},
	}
	for index, want := range wants {
		select {
		case request := <-requests:
			if request.Payload.HookSessionRef != want.session || request.Payload.State != want.state {
				t.Fatalf("delivery %d = %#v, want session=%q state=%q", index+1, request.Payload, want.session, want.state)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing delivery %d", index+1)
		}
	}

	state := readCodexStateForTest(t, statePath)
	if got := state["session-a"].State; got != status.Idle {
		t.Fatalf("session-a state = %q, want idle", got)
	}
	if got := state["session-b"].State; got != status.Attention {
		t.Fatalf("session-b state = %q, want attention", got)
	}
}

func TestLocalStaleStopCannotClearOrDeliverOverNewerAttention(t *testing.T) {
	requests := make(chan localhooktransport.IngestRequest, 4)
	socketPath := startCodexLocalIngestSocketN(t, requests, 2)
	statePath := filepath.Join(t.TempDir(), "state.json")
	values := map[string]string{
		codexhook.StateFileEnv:                 statePath,
		codexhook.SessionTTLEnv:                "1h",
		localhooktransport.EnvLocalHookEnabled: "1",
		localhooktransport.EnvLocalHookSocket:  socketPath,
	}
	getenv := func(key string) string { return values[key] }

	for _, payload := range []string{
		`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-old"}`,
		`{"hook_event_name":"PermissionRequest","session_id":"session-a","turn_id":"turn-new"}`,
		`{"hook_event_name":"Stop","session_id":"session-a","turn_id":"turn-old"}`,
	} {
		if err := run(context.Background(), strings.NewReader(payload), getenv); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case request := <-requests:
			if request.Payload.State != instancepresence.StateAttention {
				t.Fatalf("delivery %d = %#v, want attention", i+1, request.Payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing attention delivery %d", i+1)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("stale Stop was delivered: %#v", request.Payload)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestTranscriptAbortCannotCreateLifecycleDelivery(t *testing.T) {
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

	// Normal Stop restores the session. Transcript recovery is deliberately not
	// started for Codex because transcript_path must not leave parse scope.
	if err := run(context.Background(), strings.NewReader(
		`{"hook_event_name":"Stop","session_id":"session-a","turn_id":"turn-a"}`,
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
	select {
	case extra := <-requests:
		t.Fatalf("transcript recovery must not deliver duplicate idle after Stop: %#v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestLocalPermissionApprovedFollowsWorkingIdle(t *testing.T) {
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
			payload: `{"hook_event_name":"PreToolUse","session_id":"session-a","turn_id":"turn-a","tool_name":"Bash"}`,
			want:    instancepresence.StateWorking,
		},
		{
			payload: `{"hook_event_name":"Stop","session_id":"session-a","turn_id":"turn-a"}`,
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

type codexStateForTest struct {
	State status.State `json:"state"`
}

func readCodexStateForTest(t *testing.T, path string) map[string]codexStateForTest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Sessions map[string]codexStateForTest `json:"sessions"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document.Sessions
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
