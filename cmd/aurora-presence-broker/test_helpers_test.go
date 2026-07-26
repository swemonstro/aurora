//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// lockedBuffer is a race-safe stderr sink for tests that run background
// listener/connection goroutines concurrently with assertions on stderr.
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

// secureTempDir returns a private (0700) directory under the user's home
// directory. It deliberately avoids /tmp: on some systems /tmp itself or an
// ancestor may be world-writable or a symlink, which producerprotocol's
// secure-socket path validation must reject, so tests need a directory
// chain they control end to end.
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
	directory, err := os.MkdirTemp(home, ".aurora-presence-broker-test-")
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

// testSockets bundles the three per-tool socket paths a test composed, so
// helpers can both pass them as CLI arguments and dial them later without
// needing to read the bound path back off a *producerprotocol.Listener.
type testSockets struct {
	claude, codex, grok string
}

func (sockets testSockets) forTool(tool producerprotocol.Tool) string {
	switch tool {
	case producerprotocol.ToolClaude:
		return sockets.claude
	case producerprotocol.ToolCodex:
		return sockets.codex
	case producerprotocol.ToolGrok:
		return sockets.grok
	default:
		return ""
	}
}

// testSocketArgs returns composeBroker CLI arguments pointing every socket
// at a fresh, private temporary directory — never the real /run/aurora
// default paths — along with the paths themselves for dialing.
func testSocketArgs(t *testing.T, extra ...string) ([]string, testSockets) {
	t.Helper()
	directory := filepath.Join(secureTempDir(t), "sockets")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	sockets := testSockets{
		claude: filepath.Join(directory, "claude.sock"),
		codex:  filepath.Join(directory, "codex.sock"),
		grok:   filepath.Join(directory, "grok.sock"),
	}
	args := []string{
		"-host-id", "host-fixture",
		"-claude-socket", sockets.claude,
		"-codex-socket", sockets.codex,
		"-grok-socket", sockets.grok,
		// -allow-self-uid now defaults to false (deny-by-default; see
		// main.go). Tests dial as their own process's UID, so they opt in
		// to the shadow-mode/test convenience here; a test that wants to
		// exercise the deny-by-default behavior itself passes its own
		// "-allow-self-uid=false" as an extra arg, which — since flag
		// parsing applies flags in order and the extras are appended after
		// this base list — correctly overrides this default-on value.
		"-allow-self-uid=true",
	}
	return append(args, extra...), sockets
}

// dialProducer connects to path and returns a Conn ready to send messages
// for tool. It never sets Config.BoundTool on the dial side, mirroring a
// real producer that just sends messages of its own known tool.
func dialProducer(t *testing.T, path string) *producerprotocol.Conn {
	t.Helper()
	config := producerprotocol.DefaultConfig(wallClock{})
	config.SocketPath = path
	config.ReadTimeout = 2 * time.Second
	config.WriteTimeout = 2 * time.Second
	config.DialTimeout = 2 * time.Second
	conn, err := producerprotocol.Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

var testTime = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func producerMessage(tool producerprotocol.Tool, instanceID producerprotocol.InstanceID, epoch producerprotocol.ProducerEpoch, revision producerprotocol.Revision, state producerprotocol.State) producerprotocol.Message {
	now := time.Now().UTC()
	return producerprotocol.Message{
		ProtocolVersion: producerprotocol.CurrentProtocolVersion,
		Tool:            tool,
		InstanceID:      instanceID,
		ProducerEpoch:   epoch,
		State:           state,
		Revision:        revision,
		ObservedAt:      now,
		LeaseExpiresAt:  now.Add(time.Minute),
	}
}

// waitFor polls condition until it returns true or the deadline elapses,
// failing the test on timeout. It exists so tests never depend on a fixed
// sleep to observe an asynchronous effect (a goroutine's registry mutation,
// a listener shutting down, ...).
func waitFor(t *testing.T, deadline time.Duration, condition func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not met before deadline")
	}
}

// waitForInstanceState polls until id has reached exactly wantState at
// exactly wantRevision, or fails the test.
//
// A successful WriteMessage on the client's own Conn only proves the OS
// accepted the bytes into the socket buffer — it says nothing about
// whether the broker's connection-handling goroutine has actually read and
// applied it yet (async relative to the writer). Waiting on instance
// presence or count alone is not sufficient either: this helper waits for
// the real end state instead, so tests can't flake by sampling too early.
func waitForInstanceState(t *testing.T, registry *instanceregistry.Registry, id producerprotocol.InstanceID, wantState instancepresence.EffectiveState, wantRevision producerprotocol.Revision) {
	t.Helper()
	waitFor(t, time.Second, func() bool {
		inst, err := registry.Get(instancepresence.InstanceID(id))
		return err == nil && inst.State == wantState && uint64(inst.Revisions.HookRevision) == uint64(wantRevision)
	})
}

// retryUntilInstanceState repeatedly sends msg on conn until registry
// reflects wantState/wantRevision for id, or fails the test.
//
// This is distinct from waitForInstanceState: a single WriteMessage success
// does not mean the broker's ingest layer accepted the report — a rejected
// report (e.g. a takeover attempted while the previous generation's
// connection has not yet been noticed as disconnected server-side) is only
// logged, never surfaced to the writer, so a single write cannot be used as
// a "retry until accepted" signal. This resends the same message until the
// registry actually converges, absorbing exactly that kind of transient,
// expected-to-eventually-succeed race (e.g. a takeover attempted just
// after closing the previous connection, before the server side has
// necessarily noticed the disconnect yet).
func retryUntilInstanceState(t *testing.T, conn *producerprotocol.Conn, registry *instanceregistry.Registry, msg producerprotocol.Message, wantState instancepresence.EffectiveState, wantRevision producerprotocol.Revision) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.WriteMessage(msg); err != nil {
			t.Fatal(err)
		}
		inst, err := registry.Get(instancepresence.InstanceID(msg.InstanceID))
		if err == nil && inst.State == wantState && uint64(inst.Revisions.HookRevision) == uint64(wantRevision) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("instance %q did not converge to state=%s revision=%d before deadline", msg.InstanceID, wantState, wantRevision)
}
