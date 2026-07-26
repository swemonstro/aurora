//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencebroker"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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
	directory, err := os.MkdirTemp(home, ".aurora-codex-presence-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func TestRun_RequiresAtLeastOneSource(t *testing.T) {
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	err := run(context.Background(), []string{"-socket", "/tmp/unused.sock"}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "-source") {
		t.Fatalf("expected a -source-required error, got %v", err)
	}
}

// testCodexHomeDir creates and returns a fresh, real, hermetic directory to
// stand in for one configured CODEX_HOME source: codexproducer.NewSourceSet
// requires every configured path to actually exist, so tests can no longer
// use arbitrary path strings that happen to exist (or not) on whichever
// machine runs them.
func testCodexHomeDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun_RejectsRelativeSocketPath(t *testing.T) {
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	business := testCodexHomeDir(t, "business")
	err := run(context.Background(), []string{"-source", "business=" + business, "-socket", "relative.sock"}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected an absolute-path error, got %v", err)
	}
}

func TestRun_RejectsLeaseDurationNotExceedingPollInterval(t *testing.T) {
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	business := testCodexHomeDir(t, "business")
	err := run(context.Background(), []string{
		"-source", "business=" + business,
		"-socket", "/tmp/unused.sock",
		"-poll-interval", "10s",
		"-lease-duration", "5s",
	}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "lease-duration") {
		t.Fatalf("expected a lease-duration error, got %v", err)
	}
}

func TestRun_RejectsLeaseDurationAtOrAboveBrokerMaximum(t *testing.T) {
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	business := testCodexHomeDir(t, "business")
	err := run(context.Background(), []string{
		"-source", "business=" + business,
		"-socket", "/tmp/unused.sock",
		"-poll-interval", "1s",
		"-lease-duration", "3m",
	}, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "2 minute") {
		t.Fatalf("expected a maximum-lease error, got %v", err)
	}
}

func TestRun_RejectsBadSourceFlag(t *testing.T) {
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	err := run(context.Background(), []string{"-source", "not-a-valid-source-flag"}, stdout, stderr)
	if err == nil {
		t.Fatalf("expected an error for a malformed -source flag")
	}
}

func TestRun_RejectsSharedSourcePath(t *testing.T) {
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	shared := testCodexHomeDir(t, "shared")
	err := run(context.Background(), []string{
		"-source", "business=" + shared,
		"-source", "api=" + shared,
	}, stdout, stderr)
	if err == nil {
		t.Fatalf("expected an error: business and api must never share a path")
	}
}

// TestRun_ShadowModeConnectsAndShutsDownCleanly is a smoke test: run() must
// announce shadow mode, connect to a real (temporary, private) broker
// socket, and return promptly and without hanging once its context is
// cancelled — never touching any real /run/aurora path.
func TestRun_ShadowModeConnectsAndShutsDownCleanly(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "codex-broker.sock")

	clock := wallClockForTest{}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default",
		LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
		MaximumProducerLeaseDuration: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ingestor, err := presencebroker.NewIngestor(registry, "host-fixture", "test-broker")
	if err != nil {
		t.Fatal(err)
	}
	config := producerprotocol.DefaultConfig(clock)
	config.SocketPath = socketPath
	config.BoundTool = producerprotocol.ToolCodex
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	authenticator := producerprotocol.SameUIDAuthenticator{ServerUID: uint32(os.Geteuid())}
	brokerCtx, brokerCancel := context.WithCancel(context.Background())
	brokerDone := make(chan struct{})
	go func() {
		presencebroker.RunProducerListener(brokerCtx, listener, authenticator, ingestor, io.Discard)
		close(brokerDone)
	}()
	t.Cleanup(func() {
		brokerCancel()
		<-brokerDone
		_ = listener.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	business := testCodexHomeDir(t, "business")
	runDone := make(chan error, 1)
	go func() {
		runDone <- run(ctx, []string{
			"-source", "business=" + business,
			"-socket", socketPath,
			"-poll-interval", "50ms",
			"-lease-duration", "500ms",
		}, stdout, stderr)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stdout.String(), "shadow mode") {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "shadow mode") {
		t.Fatalf("expected shadow-mode banner, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run returned an error after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return promptly after context cancellation")
	}
}

type wallClockForTest struct{}

func (wallClockForTest) Now() time.Time { return time.Now().UTC() }
