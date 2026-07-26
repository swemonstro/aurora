//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

func TestRunRequiresExplicitHostID(t *testing.T) {
	var output strings.Builder
	if _, err := composeBroker(nil, &output); err == nil || !strings.Contains(err.Error(), "host-id") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidMessageAcceptedPerSocket(t *testing.T) {
	for _, tool := range []producerprotocol.Tool{producerprotocol.ToolClaude, producerprotocol.ToolCodex, producerprotocol.ToolGrok} {
		t.Run(string(tool), func(t *testing.T) {
			args, sockets := testSocketArgs(t)
			stderr := &lockedBuffer{}
			broker, err := composeBroker(args, stderr)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { broker.Serve(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })

			conn := dialProducer(t, sockets.forTool(tool))
			msg := producerMessage(tool, "inst-1", "epoch-1", 1, "working")
			if err := conn.WriteMessage(msg); err != nil {
				t.Fatal(err)
			}
			waitForInstanceState(t, broker.Registry, "inst-1", instancepresence.StateWorking, 1)
			inst, getErr := broker.Registry.Get("inst-1")
			if getErr != nil {
				t.Fatal(getErr)
			}
			if inst.Tool != instancepresence.ToolKind(tool) {
				t.Fatalf("instance = %#v", inst)
			}
		})
	}
}

func TestWrongToolRejectedPerSocketBeforeMutation(t *testing.T) {
	sockets := []struct {
		bound producerprotocol.Tool
		wrong producerprotocol.Tool
	}{
		{producerprotocol.ToolClaude, producerprotocol.ToolCodex},
		{producerprotocol.ToolCodex, producerprotocol.ToolGrok},
		{producerprotocol.ToolGrok, producerprotocol.ToolClaude},
	}
	for _, socket := range sockets {
		t.Run(string(socket.bound), func(t *testing.T) {
			args, testSockets := testSocketArgs(t)
			stderr := &lockedBuffer{}
			broker, err := composeBroker(args, stderr)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { broker.Serve(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })

			conn := dialProducer(t, testSockets.forTool(socket.bound))
			msg := producerMessage(socket.wrong, "inst-1", "epoch-1", 1, "working")
			// The client's own Conn is unbound (a fresh Dial), so the write
			// itself succeeds — the rejection happens on the server's side
			// of this same connection, which is bound to socket.bound: its
			// ReadMessage sees a tool that doesn't match the bound socket
			// and rejects the message before Ingestor.Apply is ever
			// called, then closes the connection.
			if err := conn.WriteMessage(msg); err != nil {
				t.Fatal(err)
			}
			waitFor(t, time.Second, func() bool {
				return strings.Contains(stderr.String(), "tool_mismatch")
			})
			// The message never reached the registry: nothing was ever mutated.
			if len(broker.Registry.ActiveInstances()) != 0 {
				t.Fatalf("wrong-tool message mutated the registry: %#v", broker.Registry.ActiveInstances())
			}
			// The server must have closed this connection: a further read
			// attempt observes disconnection, not a hung/half-open socket.
			if _, err := conn.ReadMessage(); !errors.Is(err, producerprotocol.ErrPeerDisconnected) {
				t.Fatalf("connection not closed after tool mismatch: %v", err)
			}
		})
	}
}

func TestWrongUIDRejected(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	// Force rejection deterministically: nobody in this test process can
	// possibly have this UID.
	broker.authenticators[producerprotocol.ToolClaude] = producerprotocol.SameUIDAuthenticator{
		ServerUID: uint32(os.Geteuid()) + 12345,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	msg := producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 1, "working")
	// The server closes the connection right after rejecting the peer; the
	// write may itself fail (disconnected) or the message may appear to
	// send but never take effect. Either way nothing may be registered.
	_ = conn.WriteMessage(msg)
	waitFor(t, time.Second, func() bool {
		return strings.Contains(stderr.String(), "authenticate")
	})
	if len(broker.Registry.ActiveInstances()) != 0 {
		t.Fatal("unauthorized peer's message mutated the registry")
	}
}

func TestUnknownPeerNotInAllowlistRejected(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	// A ServerUID we don't have, plus an allowlist that also excludes us:
	// exercises the "checked and still not found" branch specifically.
	broker.authenticators[producerprotocol.ToolCodex] = producerprotocol.SameUIDAuthenticator{
		ServerUID:   uint32(os.Geteuid()) + 12345,
		AllowedUIDs: map[uint32]struct{}{uint32(os.Geteuid()) + 6789: {}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.codex)
	_ = conn.WriteMessage(producerMessage(producerprotocol.ToolCodex, "inst-1", "epoch-1", 1, "working"))
	waitFor(t, time.Second, func() bool {
		return strings.Contains(stderr.String(), "authenticate")
	})
	if len(broker.Registry.ActiveInstances()) != 0 {
		t.Fatal("unknown peer's message mutated the registry")
	}
}

// TestAllowSelfUIDDefaultsToFalse proves the CLI's own default (not test
// helper convenience) is deny-by-default: composeBroker invoked without
// -allow-self-uid at all, and without any per-tool *-uid, must fail
// startup exactly like -allow-self-uid=false explicitly would. This
// bypasses testSocketArgs (which injects -allow-self-uid=true purely for
// test convenience) so it observes the flag package's actual default.
func TestAllowSelfUIDDefaultsToFalse(t *testing.T) {
	directory := filepath.Join(secureTempDir(t), "sockets")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-host-id", "host-fixture",
		"-claude-socket", filepath.Join(directory, "claude.sock"),
		"-codex-socket", filepath.Join(directory, "codex.sock"),
		"-grok-socket", filepath.Join(directory, "grok.sock"),
		// Deliberately no -allow-self-uid and no *-uid: this exercises the
		// flag's own zero-value default, not any test convenience.
	}
	_, err := composeBroker(args, &strings.Builder{})
	if err == nil {
		t.Fatal("expected composeBroker to fail closed with no UID policy configured and -allow-self-uid omitted (default must be false)")
	}
}

func TestAllowedUIDFlagAcceptsOwnUID(t *testing.T) {
	// The *-uid flags are meant for a future dedicated systemd account;
	// passing this process's own UID through them (alongside this test
	// suite's own -allow-self-uid=true convenience — see testSocketArgs)
	// must not break the ordinary same-UID path.
	args, sockets := testSocketArgs(t, "-claude-uid", strconv.Itoa(os.Getuid()))
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(broker.Registry.ActiveInstances()) == 1 })
}

// TestNoUIDConfiguredFailsStartupClosed proves the UID policy fails
// closed: with self-UID explicitly disabled and no per-tool UID set for
// any socket, composeBroker must refuse to start rather than silently
// binding a socket nobody can ever authenticate against.
func TestNoUIDConfiguredFailsStartupClosed(t *testing.T) {
	args, _ := testSocketArgs(t, "-allow-self-uid=false")
	_, err := composeBroker(args, &strings.Builder{})
	if err == nil {
		t.Fatal("expected composeBroker to fail with no UID configured for any socket")
	}
}

// TestAllowSelfUIDFalseRejectsOwnUIDOnUnconfiguredSocket proves
// -allow-self-uid is a real, explicit gate: disabling it while only
// configuring an explicit UID for one tool (claude) must still let this
// process's own UID reach claude's socket (matching the configured UID,
// this test's own process) while a DIFFERENT socket with no configured UID
// at all fails startup — self-UID is never an implicit fallback once
// disabled, even partially.
func TestAllowSelfUIDFalseRejectsOwnUIDOnUnconfiguredSocket(t *testing.T) {
	args, _ := testSocketArgs(t, "-allow-self-uid=false", "-claude-uid", strconv.Itoa(os.Getuid()))
	_, err := composeBroker(args, &strings.Builder{})
	if err == nil {
		t.Fatal("expected composeBroker to fail: codex and grok sockets have no UID configured")
	}
	if !strings.Contains(err.Error(), "codex") && !strings.Contains(err.Error(), "grok") {
		t.Fatalf("error = %v, want it to name the unconfigured socket", err)
	}
}

// TestAllowSelfUIDFalseWithAllUIDsConfiguredStillAcceptsOwnUID exercises
// the fully-explicit, self-UID-disabled configuration a systemd deployment
// would use, with every tool's *-uid pointed at this test process's own
// UID (standing in for that tool's dedicated account) — proving disabling
// -allow-self-uid does not break authentication when every socket has its
// own explicit UID configured.
func TestAllowSelfUIDFalseWithAllUIDsConfiguredStillAcceptsOwnUID(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	args, sockets := testSocketArgs(t, "-allow-self-uid=false", "-claude-uid", uid, "-codex-uid", uid, "-grok-uid", uid)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(broker.Registry.ActiveInstances()) == 1 })
}

func TestRevisionOrderingOverSocket(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 1, "idle")); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 2, "working")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		inst, getErr := broker.Registry.Get("inst-1")
		return getErr == nil && inst.State == instancepresence.StateWorking
	})

	// Revision 2 -> 1 is a protocol-level rejection: the connection stays
	// open (one bad message must not force a reconnect), but the state
	// must not regress.
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 1, "idle")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	inst, getErr := broker.Registry.Get("inst-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if inst.State != instancepresence.StateWorking || inst.Revisions.HookRevision != 2 {
		t.Fatalf("stale revision changed state: %#v", inst)
	}

	// The connection must still be usable after the rejected message.
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 3, "attention")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		inst, getErr := broker.Registry.Get("inst-1")
		return getErr == nil && inst.State == instancepresence.StateAttention
	})
}

func TestProducerRestartWithNewInstanceIDOverSocket(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	first := dialProducer(t, sockets.claude)
	if err := first.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-1", "epoch-1", 9, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-1", instancepresence.StateWorking, 9)
	_ = first.Close()

	// Simulates the producer crashing and restarting: a new connection, a
	// new instance id, revision starting over at 1 — must not be blocked
	// by the still-active old generation.
	second := dialProducer(t, sockets.claude)
	if err := second.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-2", "epoch-2", 1, "idle")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-2", instancepresence.StateIdle, 1)

	old, err := broker.Registry.Get("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if old.State != instancepresence.StateWorking || old.Revisions.HookRevision != 9 {
		t.Fatalf("old generation instance was affected by the restart: %#v", old)
	}
}

func TestProducerDisconnectDoesNotAffectOtherProducers(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	victim := dialProducer(t, sockets.claude)
	survivor := dialProducer(t, sockets.codex)
	if err := victim.WriteMessage(producerMessage(producerprotocol.ToolClaude, "victim", "epoch-1", 1, "working")); err != nil {
		t.Fatal(err)
	}
	if err := survivor.WriteMessage(producerMessage(producerprotocol.ToolCodex, "survivor", "epoch-1", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "victim", instancepresence.StateWorking, 1)
	waitForInstanceState(t, broker.Registry, "survivor", instancepresence.StateWorking, 1)

	if err := victim.Close(); err != nil {
		t.Fatal(err)
	}

	// The survivor's connection and the broker as a whole must be unaffected.
	if err := survivor.WriteMessage(producerMessage(producerprotocol.ToolCodex, "survivor", "epoch-1", 2, "attention")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "survivor", instancepresence.StateAttention, 2)
	victimInst, err := broker.Registry.Get("victim")
	if err != nil {
		t.Fatal(err)
	}
	if victimInst.State != instancepresence.StateWorking {
		t.Fatalf("victim's last known state changed on disconnect: %#v", victimInst)
	}
}

func TestOneListenerClosingDoesNotStopOthers(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// Simulate the Claude listener crashing/closing independently, without
	// cancelling the shared context.
	if err := broker.ClaudeListener.Close(); err != nil {
		t.Fatal(err)
	}

	// Codex and Grok must still work.
	codexConn := dialProducer(t, sockets.codex)
	if err := codexConn.WriteMessage(producerMessage(producerprotocol.ToolCodex, "codex-1", "epoch-1", 1, "working")); err != nil {
		t.Fatal(err)
	}
	grokConn := dialProducer(t, sockets.grok)
	if err := grokConn.WriteMessage(producerMessage(producerprotocol.ToolGrok, "grok-1", "epoch-1", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(broker.Registry.ActiveInstances()) == 2 })
}

func TestConcurrentProducersAreRaceSafe(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	tools := []producerprotocol.Tool{producerprotocol.ToolClaude, producerprotocol.ToolCodex, producerprotocol.ToolGrok}
	const perTool = 5
	var wait sync.WaitGroup
	for _, tool := range tools {
		for index := 0; index < perTool; index++ {
			wait.Add(1)
			go func(tool producerprotocol.Tool, index int) {
				defer wait.Done()
				conn := dialProducer(t, sockets.forTool(tool))
				id := producerprotocol.InstanceID(string(tool) + "-" + strconv.Itoa(index))
				for revision := producerprotocol.Revision(1); revision <= 3; revision++ {
					if err := conn.WriteMessage(producerMessage(tool, id, "epoch-1", revision, "working")); err != nil {
						t.Errorf("%s/%d rev %d: %v", tool, index, revision, err)
						return
					}
				}
			}(tool, index)
		}
	}
	wait.Wait()

	// All 15 writes succeeding only means the OS accepted them into the
	// socket buffer, not that the broker has read and applied every one:
	// wait for every instance to have actually reached its final revision,
	// not merely for the instance count to reach 15 (which the first
	// message alone already satisfies).
	allAtRevisionThree := func() bool {
		instances := broker.Registry.ActiveInstances()
		if len(instances) != len(tools)*perTool {
			return false
		}
		for _, instance := range instances {
			if instance.Revisions.HookRevision != 3 {
				return false
			}
		}
		return true
	}
	waitFor(t, 2*time.Second, allAtRevisionThree)
	for _, instance := range broker.Registry.ActiveInstances() {
		if instance.Revisions.HookRevision != 3 {
			t.Fatalf("instance %q settled at revision %d, want 3", instance.ID, instance.Revisions.HookRevision)
		}
	}
}

// TestConcurrentFirstMessagesForSameNewInstanceIDAreRaceSafe covers the
// narrower race TestConcurrentProducersAreRaceSafe does not: two separate
// connections racing to register the very same, previously-unknown
// instance id at once. instanceregistry.Registry.Register only allows
// exactly one winner for a given id; the loser must be rejected cleanly
// (not silently dropped, not corrupting the winner's record), and the
// registry must end up with exactly one consistent instance.
func TestConcurrentFirstMessagesForSameNewInstanceIDAreRaceSafe(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	const attempts = 8
	var wait sync.WaitGroup
	wait.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func(index int) {
			defer wait.Done()
			conn := dialProducer(t, sockets.claude)
			// Deliberately identical instance id and revision from every
			// goroutine: exactly one Register call can win.
			_ = conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "contested", "epoch-1", 1, "working"))
		}(index)
	}
	wait.Wait()

	// The eventual single winner still applies its message in two registry
	// calls (see ingest.go); wait for that to fully settle, not merely for
	// the instance to have been registered.
	waitForInstanceState(t, broker.Registry, "contested", instancepresence.StateWorking, 1)
	inst, err := broker.Registry.Get("contested")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Tool != instancepresence.ToolClaude {
		t.Fatalf("winning instance = %#v", inst)
	}
	if len(broker.Registry.ActiveInstances()) != 1 {
		t.Fatalf("active instances = %#v, want exactly 1", broker.Registry.ActiveInstances())
	}
}

func TestSnapshotAndPresentationMatchContract(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "inst-1", instancepresence.StateWorking, 1)

	snapshot, err := broker.Registry.CanonicalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("canonical snapshot fails its own contract: %v", err)
	}
	presentation, err := broker.Registry.Presentation(5)
	if err != nil {
		t.Fatal(err)
	}
	if err := presentation.Validate(); err != nil {
		t.Fatalf("presentation fails its own contract: %v", err)
	}
	if presentation.ActiveCount != 1 || presentation.Pixels[0].State != instancepresence.StateWorking {
		t.Fatalf("presentation = %#v", presentation)
	}
}

func TestShutdownRemovesSockets(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()

	waitFor(t, time.Second, func() bool {
		_, err := os.Lstat(sockets.claude)
		return err == nil
	})
	cancel()
	<-done

	for _, path := range []string{sockets.claude, sockets.codex, sockets.grok} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("socket remained after shutdown: %s (%v)", path, err)
		}
	}
}

func TestDoubleBrokerInstanceFailsCleanly(t *testing.T) {
	args, _ := testSocketArgs(t)
	stderr := &lockedBuffer{}
	first, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { first.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, time.Second, func() bool { return first.ClaudeListener != nil })

	var secondStderr strings.Builder
	_, err = composeBroker(args, &secondStderr)
	if !errors.Is(err, producerprotocol.ErrSocketAlreadyExists) {
		t.Fatalf("second broker error = %v, want ErrSocketAlreadyExists", err)
	}
}

// TestPartialBindFailureRollsBackEarlierSockets covers a composeBroker
// failure partway through binding the three sockets (claude and codex
// succeed, grok's target path is pre-occupied by something else): the
// already-bound claude and codex socket files must not be left behind.
func TestPartialBindFailureRollsBackEarlierSockets(t *testing.T) {
	directory := filepath.Join(secureTempDir(t), "sockets")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(directory, "claude.sock")
	codexPath := filepath.Join(directory, "codex.sock")
	grokPath := filepath.Join(directory, "grok.sock")
	// Occupy the grok path with an unrelated regular file, which Listen
	// rejects as ErrSocketAlreadyExists without ever touching it.
	if err := os.WriteFile(grokPath, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}

	stderr := &lockedBuffer{}
	_, err := composeBroker([]string{
		"-host-id", "host-fixture",
		"-claude-socket", claudePath,
		"-codex-socket", codexPath,
		"-grok-socket", grokPath,
		"-allow-self-uid=true",
	}, stderr)
	if !errors.Is(err, producerprotocol.ErrSocketAlreadyExists) {
		t.Fatalf("error = %v, want ErrSocketAlreadyExists", err)
	}
	for _, path := range []string{claudePath, codexPath} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("socket left behind after partial bind failure: %s (%v)", path, statErr)
		}
	}
	content, err := os.ReadFile(grokPath)
	if err != nil || string(content) != "occupied" {
		t.Fatalf("unrelated file at grok path was disturbed: %q, %v", content, err)
	}
}

func TestNoErrorLeaksSocketPathOrInstanceID(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	const marker = "leak-canary-instance-9f2b1c"
	conn := dialProducer(t, sockets.claude)
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, producerprotocol.InstanceID(marker), "epoch-1", 5, "working")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(broker.Registry.ActiveInstances()) == 1 })
	// Trigger a stale-revision rejection and a cross-tool rejection so
	// their logged classifications exist in stderr.
	_ = conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, producerprotocol.InstanceID(marker), "epoch-1", 1, "idle"))
	codexConn := dialProducer(t, sockets.codex)
	_ = codexConn.WriteMessage(producerMessage(producerprotocol.ToolCodex, producerprotocol.InstanceID(marker), "epoch-1", 6, "idle"))
	time.Sleep(50 * time.Millisecond)

	log := stderr.String()
	if strings.Contains(log, marker) {
		t.Fatalf("log leaked instance id: %s", log)
	}
	if strings.Contains(log, sockets.claude) || strings.Contains(log, sockets.codex) || strings.Contains(log, sockets.grok) {
		t.Fatalf("log leaked a socket path: %s", log)
	}
	if strings.Contains(log, filepath.Dir(sockets.claude)) {
		t.Fatalf("log leaked the socket directory: %s", log)
	}
}

// TestEpochTakeoverAfterDisconnectOverSocket is BLOCKING POINT 2's primary
// scenario end to end: a producer process restarts (new connection, new
// producer_epoch) while the underlying AI session is the same instance_id.
// The old connection disconnecting must be enough to let the new one take
// over, resuming at revision 1 — a fresh instance_id must not be required.
func TestEpochTakeoverAfterDisconnectOverSocket(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	oldConn := dialProducer(t, sockets.claude)
	if err := oldConn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-A", 9, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-x", instancepresence.StateWorking, 9)
	if err := oldConn.Close(); err != nil {
		t.Fatal(err)
	}

	newConn := dialProducer(t, sockets.claude)
	retryUntilInstanceState(t, newConn, broker.Registry,
		producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-B", 1, "idle"),
		instancepresence.StateIdle, 1)

	inst, err := broker.Registry.Get("session-x")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-B" {
		t.Fatalf("epoch = %q, want epoch-B", inst.Revisions.ProducerEpoch)
	}
}

// TestOldEpochRejectedAfterTakeoverOverSocket covers BLOCKING POINT 2's
// second required scenario: once a new generation has been accepted, a
// stale attempt under the old epoch (e.g. a delayed reconnect from the
// crashed process) must never reactivate or overwrite it.
func TestOldEpochRejectedAfterTakeoverOverSocket(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	oldConn := dialProducer(t, sockets.claude)
	if err := oldConn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-A", 9, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-x", instancepresence.StateWorking, 9)
	if err := oldConn.Close(); err != nil {
		t.Fatal(err)
	}

	newConn := dialProducer(t, sockets.claude)
	retryUntilInstanceState(t, newConn, broker.Registry,
		producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-B", 1, "idle"),
		instancepresence.StateIdle, 1)

	// A stale, delayed message under the retired epoch-A arrives on yet
	// another connection: must be rejected, must not touch the new
	// generation's state. epoch-A is now permanently retired (not merely
	// "still active" under someone else), so the ingest layer classifies
	// this precisely as producer_epoch_retired.
	staleConn := dialProducer(t, sockets.claude)
	if err := staleConn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-A", 10, "error")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return strings.Contains(stderr.String(), "producer_epoch_retired")
	})
	inst, err := broker.Registry.Get("session-x")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-B" || inst.State != instancepresence.StateIdle {
		t.Fatalf("stale old-epoch message affected the new generation: %#v", inst)
	}
}

// TestEpochChangeOnLiveConnectionRejectedOverSocket covers BLOCKING POINT
// 2's third required scenario: a single connection must never be allowed
// to switch producer_epoch mid-stream, even for its own instance id.
func TestEpochChangeOnLiveConnectionRejectedOverSocket(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-A", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-x", instancepresence.StateWorking, 1)

	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-B", 1, "idle")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return strings.Contains(stderr.String(), "connection_epoch_changed")
	})
	inst, err := broker.Registry.Get("session-x")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Revisions.ProducerEpoch != "epoch-A" || inst.State != instancepresence.StateWorking {
		t.Fatalf("epoch-changing message on a live connection mutated the instance: %#v", inst)
	}

	// The connection must still be usable under its original epoch.
	if err := conn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-A", 2, "attention")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-x", instancepresence.StateAttention, 2)
}

// TestTwoConcurrentGenerationsCannotBothWinOverSocket covers BLOCKING POINT
// 2's fourth required scenario at the full socket/listener level.
func TestTwoConcurrentGenerationsCannotBothWinOverSocket(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	oldConn := dialProducer(t, sockets.claude)
	if err := oldConn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-A", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-x", instancepresence.StateWorking, 1)
	if err := oldConn.Close(); err != nil {
		t.Fatal(err)
	}

	// A rejected write is silent on the wire (message accepted by the
	// socket, rejected by ingest, only logged) — so a single round of
	// concurrent sends cannot be used as a "retry until one wins" signal by
	// itself: a round can legitimately have every candidate lose if the old
	// connection's disconnect has not yet been noticed server-side (the
	// same reason a single write is not a valid retry condition anywhere
	// else in this file; see retryUntilInstanceState). Resend concurrently
	// in rounds until the registry actually shows a winner.
	const attempts = 6
	conns := make([]*producerprotocol.Conn, attempts)
	epochs := make([]producerprotocol.ProducerEpoch, attempts)
	for index := range conns {
		conns[index] = dialProducer(t, sockets.claude)
		epochs[index] = producerprotocol.ProducerEpoch(fmt.Sprintf("epoch-candidate-%d", index))
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var round sync.WaitGroup
		round.Add(attempts)
		for index := range conns {
			go func(index int) {
				defer round.Done()
				_ = conns[index].WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", epochs[index], 1, "idle"))
			}(index)
		}
		round.Wait()
		if inst, err := broker.Registry.Get("session-x"); err == nil && inst.Revisions.ProducerEpoch != "epoch-A" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let any losing attempts finish being rejected.
	inst, err := broker.Registry.Get("session-x")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Revisions.ProducerEpoch == "epoch-A" {
		t.Fatal("no candidate generation ever won the takeover")
	}
	if inst.Revisions.HookRevision != 1 || inst.State != instancepresence.StateIdle {
		t.Fatalf("final state = %#v, want exactly one generation's revision 1", inst)
	}
}

// TestShutdownWaitsForConnectionGoroutines is BLOCKING POINT 4's core
// guarantee: Serve must not return until every connection goroutine it
// spawned has too. It is deterministic because each connection's deferred
// conn.Close() runs synchronously before its goroutine returns and signals
// the WaitGroup Serve waits on — so by the time Serve() (and therefore
// <-done below) has returned, every one of these connections must already
// be closed on the server side, observable here as a prompt disconnect on
// read, never a timeout or continued liveness.
func TestShutdownWaitsForConnectionGoroutines(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()

	const n = 10
	conns := make([]*producerprotocol.Conn, n)
	for i := range conns {
		conns[i] = dialProducer(t, sockets.claude)
		id := producerprotocol.InstanceID(fmt.Sprintf("conn-%d", i))
		if err := conns[i].WriteMessage(producerMessage(producerprotocol.ToolClaude, id, "epoch-1", 1, "working")); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Second, func() bool { return len(broker.Registry.ActiveInstances()) == n })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}

	for i, conn := range conns {
		if _, err := conn.ReadMessage(); !errors.Is(err, producerprotocol.ErrPeerDisconnected) {
			t.Fatalf("connection %d not closed by the time Serve() returned: %v", i, err)
		}
	}
}

// TestMaximumLeaseFlagRejectsExcessiveLease covers BLOCKING POINT 3's
// configurable maximum: a producer cannot grant itself permanent (or just
// very long-lived) state by declaring an excessive lease_expires_at.
func TestMaximumLeaseFlagRejectsExcessiveLease(t *testing.T) {
	args, sockets := testSocketArgs(t, "-maximum-lease", "10s")
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	msg := producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 1, "working")
	msg.LeaseExpiresAt = msg.ObservedAt.Add(time.Hour) // exceeds the 10s maximum.
	if err := conn.WriteMessage(msg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return strings.Contains(stderr.String(), "lease_exceeds_maximum")
	})
	if len(broker.Registry.ActiveInstances()) != 0 {
		t.Fatal("excessive-lease report created an instance")
	}
}

// TestDuplicateReportIsIdempotentOverSocket exercises the exact duplicate
// semantics from the hardening section end to end: an identical resend is
// a no-op, the same revision with different content is rejected, and a
// lower revision is rejected without mutation.
func TestDuplicateReportIsIdempotentOverSocket(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn := dialProducer(t, sockets.claude)
	msg := producerMessage(producerprotocol.ToolClaude, "inst-1", "epoch-1", 3, "working")
	if err := conn.WriteMessage(msg); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "inst-1", instancepresence.StateWorking, 3)

	// Exact retry: idempotent, no error surfaced, no state change.
	if err := conn.WriteMessage(msg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	inst, err := broker.Registry.Get("inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != instancepresence.StateWorking || inst.Revisions.HookRevision != 3 {
		t.Fatalf("exact retry changed state: %#v", inst)
	}
}

func TestTakeoverDoesNotAffectOtherToolsOrInstances(t *testing.T) {
	args, sockets := testSocketArgs(t)
	stderr := &lockedBuffer{}
	broker, err := composeBroker(args, stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { broker.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	codexConn := dialProducer(t, sockets.codex)
	if err := codexConn.WriteMessage(producerMessage(producerprotocol.ToolCodex, "codex-1", "epoch-1", 4, "attention")); err != nil {
		t.Fatal(err)
	}
	otherClaudeConn := dialProducer(t, sockets.claude)
	if err := otherClaudeConn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "claude-other", "epoch-1", 2, "error")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "codex-1", instancepresence.StateAttention, 4)
	waitForInstanceState(t, broker.Registry, "claude-other", instancepresence.StateError, 2)

	oldConn := dialProducer(t, sockets.claude)
	if err := oldConn.WriteMessage(producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-A", 1, "working")); err != nil {
		t.Fatal(err)
	}
	waitForInstanceState(t, broker.Registry, "session-x", instancepresence.StateWorking, 1)
	if err := oldConn.Close(); err != nil {
		t.Fatal(err)
	}
	newConn := dialProducer(t, sockets.claude)
	retryUntilInstanceState(t, newConn, broker.Registry,
		producerMessage(producerprotocol.ToolClaude, "session-x", "epoch-B", 1, "idle"),
		instancepresence.StateIdle, 1)

	codexInst, err := broker.Registry.Get("codex-1")
	if err != nil {
		t.Fatal(err)
	}
	claudeOtherInst, err := broker.Registry.Get("claude-other")
	if err != nil {
		t.Fatal(err)
	}
	if codexInst.State != instancepresence.StateAttention || codexInst.Revisions.HookRevision != 4 {
		t.Fatalf("unrelated codex instance affected by claude takeover: %#v", codexInst)
	}
	if claudeOtherInst.State != instancepresence.StateError || claudeOtherInst.Revisions.HookRevision != 2 {
		t.Fatalf("unrelated claude instance affected by another claude instance's takeover: %#v", claudeOtherInst)
	}
}
