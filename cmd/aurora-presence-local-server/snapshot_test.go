//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotFlagDefaultOff(t *testing.T) {
	socketPath := testSocketPath(t)
	directory := filepath.Dir(socketPath)
	// No snapshot-file flag: directory should not gain a snapshot artifact from compose.
	server, cleanup, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath},
		ioDiscard{},
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	defer server.Close()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "snapshot") {
			t.Fatalf("unexpected snapshot-related file with flag off: %s", entry.Name())
		}
	}
}

func TestSnapshotRelativePathRejected(t *testing.T) {
	socketPath := testSocketPath(t)
	var stderr bytes.Buffer
	_, _, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath, "-snapshot-file", "relative/snapshot.json"},
		&stderr,
		func(string) string { return "" },
	)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v", err)
	}
}

func TestSnapshotWriterErrorDoesNotStopServer(t *testing.T) {
	socketPath := testSocketPath(t)
	// Snapshot path under a non-directory component so writes fail.
	blocker := filepath.Join(filepath.Dir(socketPath), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badSnapshot := filepath.Join(blocker, "nested", "snapshot.json")

	stderr := &lockedBuffer{}
	server, cleanup, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath, "-snapshot-file", badSnapshot},
		stderr,
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	// Allow a couple of failing snapshot attempts.
	time.Sleep(40 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve stopped with error: %v", err)
	}
	// Stop snapshot loop before reading stderr.
	if cleanup != nil {
		cleanup()
	}
	if !strings.Contains(stderr.String(), "snapshot write:") {
		t.Fatalf("expected snapshot write errors on stderr, got %q", stderr.String())
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
