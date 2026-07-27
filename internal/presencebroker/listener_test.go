//go:build linux

package presencebroker

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

type listenerWallClock struct{}

func (listenerWallClock) Now() time.Time { return time.Now().UTC() }

// concurrentBuffer is a mutex-protected io.Writer used as stderr in listener
// tests so the test goroutine can snapshot logs while handleProducerConnection
// still writes without tripping the race detector.
type concurrentBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *concurrentBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *concurrentBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// handlerEnv is the shared broker-side state for one listener test: a real
// registry/ingestor pair plus a connection handler. Tests assert success by
// polling the registry, not by inferring from log silence.
type handlerEnv struct {
	registry *instanceregistry.Registry
	ingestor *Ingestor
	stderr   *concurrentBuffer
	clock    *fakeClock
}

func newHandlerEnv(t *testing.T) *handlerEnv {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock:                        clock,
		SlotNamespace:                "default",
		LeaseDuration:                time.Minute,
		GracePeriod:                  10 * time.Second,
		MaximumProducerLeaseDuration: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ingestor, err := NewIngestor(registry, "host-fixture", "aurora-presence-broker-listener-test")
	if err != nil {
		t.Fatal(err)
	}
	return &handlerEnv{
		registry: registry,
		ingestor: ingestor,
		stderr:   &concurrentBuffer{},
		clock:    clock,
	}
}

func listenerTestConfig(t *testing.T) producerprotocol.Config {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-presencebroker-listener-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	config := producerprotocol.DefaultConfig(listenerWallClock{})
	config.SocketPath = filepath.Join(directory, "producer.sock")
	// Short but not hair-trigger under -race: real deadline integration only.
	config.ReadTimeout = 100 * time.Millisecond
	config.WriteTimeout = time.Second
	config.DialTimeout = time.Second
	return config
}

// acceptResult is delivered by beginAccept. The background Accept goroutine
// only ever sends on a channel; it never calls testing.T methods.
type acceptResult struct {
	conn *producerprotocol.Conn
	err  error
}

// beginAccept starts exactly one listener.Accept in the background and returns
// a channel that receives its outcome. Call waitAccept from the test's main
// goroutine so all t.Fatal* calls stay on that goroutine. There is no nested
// accept helper — callers must not wrap beginAccept in another go-routine that
// itself fatals.
func beginAccept(listener *producerprotocol.Listener) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		conn, _, err := listener.Accept(context.Background())
		ch <- acceptResult{conn: conn, err: err}
	}()
	return ch
}

// waitAccept blocks until beginAccept delivers a result (or the bound timeout
// elapses) and reports failures only from the calling (test) goroutine.
func waitAccept(t *testing.T, pending <-chan acceptResult) *producerprotocol.Conn {
	t.Helper()
	select {
	case res := <-pending:
		if res.err != nil {
			t.Fatalf("accept: %v", res.err)
		}
		if res.conn == nil {
			t.Fatal("accept returned nil connection")
		}
		return res.conn
	case <-time.After(2 * time.Second):
		t.Fatal("accept timed out")
		return nil
	}
}

// startHandler runs handleProducerConnection for one accepted Conn against
// env's shared registry/ingestor. Returns a done channel closed when the
// handler returns.
func (env *handlerEnv) startHandler(t *testing.T, conn *producerprotocol.Conn) (done <-chan struct{}, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	session := env.ingestor.NewSession()
	go func() {
		defer close(finished)
		handleProducerConnection(ctx, conn, session, env.stderr)
	}()
	t.Cleanup(cancel)
	return finished, cancel
}

// startAcceptLoop accepts connections until ctx is done and runs each through
// handleProducerConnection against the shared env (reconnect-style tests).
func (env *handlerEnv) startAcceptLoop(t *testing.T, listener *producerprotocol.Listener) (cancel context.CancelFunc, loopDone <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var connections sync.WaitGroup
		defer connections.Wait()
		for {
			conn, _, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			session := env.ingestor.NewSession()
			connections.Add(1)
			go func(c *producerprotocol.Conn, s *ProducerSession) {
				defer connections.Done()
				handleProducerConnection(ctx, c, s, env.stderr)
			}(conn, session)
		}
	}()
	t.Cleanup(cancel)
	return cancel, finished
}

// waitForInstance polls the registry until id is present with the expected
// hook revision and state, or until the bound deadline elapses.
func waitForInstance(
	t *testing.T,
	registry *instanceregistry.Registry,
	id producerprotocol.InstanceID,
	wantRevision producerprotocol.Revision,
	wantState producerprotocol.State,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		inst, err := registry.Get(instancepresence.InstanceID(id))
		if err == nil &&
			inst.Revisions.HookRevision == instancepresence.HookRevision(wantRevision) &&
			inst.State == instancepresence.EffectiveState(wantState) {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("revision=%d state=%s", inst.Revisions.HookRevision, inst.State)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("registry never reached id=%s revision=%d state=%s (last=%s)", id, wantRevision, wantState, last)
}

func waitForLogContains(t *testing.T, stderr *concurrentBuffer, fragment string, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stderr.String(), fragment) {
			return
		}
		select {
		case <-done:
			if strings.Contains(stderr.String(), fragment) {
				return
			}
			t.Fatalf("handler exited before log %q; stderr=%q", fragment, stderr.String())
		case <-time.After(2 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for log %q; stderr=%q", fragment, stderr.String())
}

// TestHandleProducerConnection_IdleTimeoutThenAcceptsFrame is the broker-side
// real-deadline integration test for idle recovery: after at least one
// ReadTimeout interval with no bytes, a complete frame on the same
// connection is applied and visible in the registry.
func TestHandleProducerConnection_IdleTimeoutThenAcceptsFrame(t *testing.T) {
	config := listenerTestConfig(t)
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	pending := beginAccept(listener)

	client, err := producerprotocol.Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conn := waitAccept(t, pending)
	env := newHandlerEnv(t)
	done, cancel := env.startHandler(t, conn)
	defer cancel()

	// Real wall-clock deadline: the handler must observe one idle
	// ReadMessage timeout before the producer speaks. Margin is generous
	// under -race; success is still defined by registry contents, not by
	// the sleep alone.
	time.Sleep(config.ReadTimeout + config.ReadTimeout/2)

	msg := ingestMessage(producerprotocol.ToolClaude, "inst-idle-1", "epoch-1", 1, producerprotocol.StateWorking, env.clock.Now())
	if err := client.WriteMessage(msg); err != nil {
		t.Fatalf("write after idle: %v", err)
	}

	waitForInstance(t, env.registry, "inst-idle-1", 1, producerprotocol.StateWorking)

	select {
	case <-done:
		t.Fatalf("handler exited after successful ingest; stderr=%q", env.stderr.String())
	default:
	}
	if strings.Contains(env.stderr.String(), "producer connection read:") {
		t.Fatalf("unexpected transport error log: %q", env.stderr.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after cancel")
	}
}

// TestHandleProducerConnection_PartialHeaderTimeoutCloses covers requirement B.
func TestHandleProducerConnection_PartialHeaderTimeoutCloses(t *testing.T) {
	config := listenerTestConfig(t)
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	pending := beginAccept(listener)

	rawClient, err := dialRaw(t, config.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawClient.Close()

	conn := waitAccept(t, pending)
	env := newHandlerEnv(t)
	done, _ := env.startHandler(t, conn)

	if _, err := rawClient.Write([]byte{0x00, 0x10}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after partial header timeout")
	}
	log := env.stderr.String()
	if !strings.Contains(log, string(producerprotocol.CodeIncompleteFrame)) {
		t.Fatalf("stderr = %q, want incomplete_frame", log)
	}
	if strings.Contains(log, string(producerprotocol.CodeMessageTooLarge)) {
		t.Fatalf("partial header must not desync to message_too_large: %q", log)
	}
}

// TestHandleProducerConnection_PartialBodyTimeoutCloses covers requirement C.
func TestHandleProducerConnection_PartialBodyTimeoutCloses(t *testing.T) {
	config := listenerTestConfig(t)
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	pending := beginAccept(listener)

	rawClient, err := dialRaw(t, config.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawClient.Close()

	conn := waitAccept(t, pending)
	env := newHandlerEnv(t)
	done, _ := env.startHandler(t, conn)

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 64)
	if _, err := rawClient.Write(append(header[:], []byte("ab")...)); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after partial body timeout")
	}
	log := env.stderr.String()
	if !strings.Contains(log, string(producerprotocol.CodeIncompleteFrame)) {
		t.Fatalf("stderr = %q, want incomplete_frame", log)
	}
}

// encodeFrame builds one length-prefixed wire frame for a canonical message
// using the public producerprotocol codec (same format Conn.WriteMessage uses).
func encodeFrame(t *testing.T, msg producerprotocol.Message, maximum uint32) []byte {
	t.Helper()
	body, err := producerprotocol.EncodeMessageJSON(producerprotocol.CanonicalMessage(msg), maximum)
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	return append(header[:], body...)
}

// TestHandleProducerConnection_ReconnectAfterPartialFrameAppliesHigherRevision
// models the producer transport contract after a partial-frame fatal
// timeout: a real revision-1 report is interrupted mid-frame on connection 1,
// the stream closes as incomplete_frame (never message_too_large), the
// producer reconnects on connection 2, and a fresh revision-2 report for the
// same instance/epoch is applied in the shared registry.
func TestHandleProducerConnection_ReconnectAfterPartialFrameAppliesHigherRevision(t *testing.T) {
	config := listenerTestConfig(t)
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	env := newHandlerEnv(t)
	cancelLoop, _ := env.startAcceptLoop(t, listener)
	defer cancelLoop()

	const (
		instanceID = producerprotocol.InstanceID("inst-reconnect-1")
		epoch      = producerprotocol.ProducerEpoch("epoch-1")
	)
	now := env.clock.Now()

	// Connection 1: encode a genuine revision-1 report, then send only a
	// strict prefix (complete 4-byte length header + some body bytes) so the
	// server's ReadMessage times out mid-frame as incomplete_frame.
	rev1 := ingestMessage(producerprotocol.ToolCodex, instanceID, epoch, 1, producerprotocol.StateIdle, now)
	fullFrame := encodeFrame(t, rev1, config.MaximumMessageBytes)
	if len(fullFrame) < 4+8 {
		t.Fatalf("encoded frame too small to partial-body truncate: %d bytes", len(fullFrame))
	}
	// Complete header + first 8 body bytes — never the whole frame.
	partial := fullFrame[:4+8]
	if len(partial) >= len(fullFrame) {
		t.Fatal("partial prefix must be strictly shorter than the full frame")
	}

	rawClient, err := dialRaw(t, config.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawClient.Write(partial); err != nil {
		t.Fatal(err)
	}

	waitForLogContains(t, env.stderr, string(producerprotocol.CodeIncompleteFrame), nil)
	_ = rawClient.Close()

	if strings.Contains(env.stderr.String(), string(producerprotocol.CodeMessageTooLarge)) {
		t.Fatalf("partial frame path logged message_too_large: %q", env.stderr.String())
	}
	// Interrupted rev-1 must never have been applied.
	if _, err := env.registry.Get(instancepresence.InstanceID(instanceID)); err == nil {
		t.Fatal("registry must not contain the instance after a mid-frame abort of revision 1")
	}

	// Connection 2: producer reconnects with a complete, fresher revision-2
	// report for the same instance_id and producer_epoch (forward progress,
	// not a byte-for-byte replay of the interrupted frame).
	client, err := producerprotocol.Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	rev2 := ingestMessage(producerprotocol.ToolCodex, instanceID, epoch, 2, producerprotocol.StateWorking, now.Add(time.Second))
	if err := client.WriteMessage(rev2); err != nil {
		t.Fatalf("reconnect write: %v", err)
	}

	waitForInstance(t, env.registry, instanceID, 2, producerprotocol.StateWorking)

	log := env.stderr.String()
	if strings.Contains(log, string(producerprotocol.CodeMessageTooLarge)) {
		t.Fatalf("message_too_large appeared after reconnect path: %q", log)
	}
	if !strings.Contains(log, string(producerprotocol.CodeIncompleteFrame)) {
		t.Fatalf("expected incomplete_frame from connection 1; stderr=%q", log)
	}
}

// TestHandleProducerConnection_RejectedIngestKeepsConnectionOpen covers the
// preserved property that application-level rejection does not close the
// connection, and that a later good revision on the same connection is
// actually applied in the registry.
func TestHandleProducerConnection_RejectedIngestKeepsConnectionOpen(t *testing.T) {
	config := listenerTestConfig(t)
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	pending := beginAccept(listener)

	client, err := producerprotocol.Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conn := waitAccept(t, pending)
	env := newHandlerEnv(t)
	done, cancel := env.startHandler(t, conn)
	defer cancel()

	now := env.clock.Now()
	const instanceID producerprotocol.InstanceID = "inst-reject-1"

	first := ingestMessage(producerprotocol.ToolClaude, instanceID, "epoch-1", 2, producerprotocol.StateWorking, now)
	if err := client.WriteMessage(first); err != nil {
		t.Fatal(err)
	}
	waitForInstance(t, env.registry, instanceID, 2, producerprotocol.StateWorking)

	// Stale revision relative to first — must be rejected, not applied.
	stale := ingestMessage(producerprotocol.ToolClaude, instanceID, "epoch-1", 1, producerprotocol.StateIdle, now.Add(time.Second))
	if err := client.WriteMessage(stale); err != nil {
		t.Fatal(err)
	}
	waitForLogContains(t, env.stderr, "producer connection ingest:", done)

	// Still revision 2 / working after the rejection.
	inst, err := env.registry.Get(instancepresence.InstanceID(instanceID))
	if err != nil {
		t.Fatal(err)
	}
	if inst.Revisions.HookRevision != 2 || inst.State != instancepresence.StateWorking {
		t.Fatalf("stale report mutated registry: revision=%d state=%s", inst.Revisions.HookRevision, inst.State)
	}

	// A good follow-up on the same connection must apply.
	good := ingestMessage(producerprotocol.ToolClaude, instanceID, "epoch-1", 3, producerprotocol.StateIdle, now.Add(2*time.Second))
	if err := client.WriteMessage(good); err != nil {
		t.Fatal(err)
	}
	waitForInstance(t, env.registry, instanceID, 3, producerprotocol.StateIdle)

	select {
	case <-done:
		t.Fatalf("handler closed after application rejection path; stderr=%q", env.stderr.String())
	default:
	}
	if strings.Contains(env.stderr.String(), "producer connection read:") {
		t.Fatalf("unexpected transport close: %q", env.stderr.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after cancel")
	}
}

func dialRaw(t *testing.T, socketPath string) (net.Conn, error) {
	t.Helper()
	// Raw net.Dial so tests can emit incomplete length prefixes without
	// producerprotocol.Conn.WriteMessage's full-frame validation.
	return net.Dial("unix", socketPath)
}

// Silence unused import if io is only needed for interface satisfaction in
// some toolchains; concurrentBuffer implements io.Writer via Write.
var _ io.Writer = (*concurrentBuffer)(nil)
