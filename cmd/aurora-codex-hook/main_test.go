package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/presence"
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

func TestRunSessionEndFileRemovesSession(t *testing.T) {
	snapshots := make(chan presence.Snapshot, 2)

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

	wantStates := []status.State{
		status.Attention,
		status.Idle,
	}

	for index, want := range wantStates {
		select {
		case snapshot := <-snapshots:
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
