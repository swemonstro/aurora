package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/runtimepresence"
	"github.com/swemonstro/aurora/internal/status"
)

func TestRunRequiresHostIDAndRelay(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(context.Background(), nil, ioDiscard{}, &stderr); err == nil || !strings.Contains(err.Error(), "host-id") {
		t.Fatalf("error = %v", err)
	}
	if err := run(context.Background(), []string{"-host-id", "host-a"}, ioDiscard{}, &stderr); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunPublishesOnSecureClaudeFamilyAndRemovesOnShutdown(t *testing.T) {
	published := make(chan presence.Snapshot, 1)
	removed := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			var snapshot presence.Snapshot
			if err := json.NewDecoder(request.Body).Decode(&snapshot); err != nil {
				t.Errorf("decode: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			published <- snapshot
			writer.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			removed <- request.URL.Query().Get("source")
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	root := newProcFixture(t)
	// Secure Claude direct process (executable basename "claude").
	writeProcessFixture(t, root, 4242, "claude", 1, 4242, 10, 0, 500, []string{"/usr/bin/claude"})

	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-host-id", "host-a",
			"-relay", server.URL,
			"-proc-root", root,
			"-interval", "50ms",
			"-clock-ticks", "100",
		}, ioDiscard{}, &stderr)
	}()

	select {
	case snapshot := <-published:
		if snapshot.Source != runtimepresence.SourceClaudeRuntime {
			t.Fatalf("source = %q", snapshot.Source)
		}
		if snapshot.State != status.Idle {
			t.Fatalf("state = %q", snapshot.State)
		}
		if snapshot.Version != presence.ProtocolVersion {
			t.Fatalf("version = %d", snapshot.Version)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatalf("no presence published for secure Claude family; stderr=%s", stderr.String())
	}

	// Allow the client-side publish to finish marking the source active.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v stderr=%s", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run did not exit after cancel; stderr=%s", stderr.String())
	}

	select {
	case source := <-removed:
		if source != runtimepresence.SourceClaudeRuntime {
			t.Fatalf("removed = %q", source)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown did not remove claude-runtime; stderr=%s", stderr.String())
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

// Minimal proc fixture helpers (mirrors linuxprocess tests).

func newProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, root, "stat", "cpu 1 2 3 4\nbtime 1784707200\n")
	writeFixtureFile(t, root, "sys/kernel/random/boot_id", "fixture-boot-id\n")
	return root
}

func writeProcessFixture(
	t *testing.T,
	root string,
	pid uint64,
	comm string,
	ppid, group, session uint64,
	tty int64,
	startTicks uint64,
	arguments []string,
) {
	t.Helper()
	directory := filepath.Join(root, stringPID(pid))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "stat"), statLine(pid, comm, ppid, group, session, tty, startTicks))
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "comm"), comm+"\n")
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "cmdline"), joinCmdline(arguments))
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "status"), "Name:\tfixture\nUid:\t1000\t1000\t1000\t1000\n")
}

func writeFixtureFile(t *testing.T, root, name, contents string) {
	t.Helper()
	filename := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func joinCmdline(arguments []string) string {
	value := ""
	for _, argument := range arguments {
		value += argument + "\x00"
	}
	return value
}

func stringPID(pid uint64) string {
	return strconv.FormatUint(pid, 10)
}

func statLine(pid uint64, comm string, ppid, group, session uint64, tty int64, start uint64) string {
	fields := make([]string, 23)
	for index := range fields {
		fields[index] = "0"
	}
	fields[0] = "R"
	fields[1] = strconv.FormatUint(ppid, 10)
	fields[2] = strconv.FormatUint(group, 10)
	fields[3] = strconv.FormatUint(session, 10)
	fields[4] = strconv.FormatInt(tty, 10)
	fields[19] = strconv.FormatUint(start, 10)
	return fmt.Sprintf("%d (%s) %s", pid, comm, strings.Join(fields, " "))
}
