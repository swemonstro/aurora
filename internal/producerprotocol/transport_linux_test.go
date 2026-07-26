//go:build linux

package producerprotocol

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testListenerConfig(t *testing.T) Config {
	t.Helper()
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(wallClock{})
	config.SocketPath = filepath.Join(private, "producer.sock")
	config.ReadTimeout = 500 * time.Millisecond
	config.WriteTimeout = 500 * time.Millisecond
	config.DialTimeout = 500 * time.Millisecond
	return config
}

func TestPeerCredentialsUsesKernelIdentity(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(private, "peer.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan PeerIdentity, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		identity, identityErr := peerCredentials(connection)
		if identityErr == nil {
			accepted <- identity
		}
	}()
	client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case identity := <-accepted:
		if identity.UID != uint32(os.Geteuid()) || identity.PID != int32(os.Getpid()) {
			t.Fatalf("peer identity = %#v", identity)
		}
	case <-time.After(time.Second):
		t.Fatal("peer credential check did not complete")
	}
}

func TestListenAcceptDialRoundTrip(t *testing.T) {
	config := testListenerConfig(t)
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan struct {
		conn *Conn
		peer PeerIdentity
	}, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, peer, err := listener.Accept(context.Background())
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- struct {
			conn *Conn
			peer PeerIdentity
		}{conn, peer}
	}()

	client, err := Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var server *Conn
	select {
	case result := <-accepted:
		server = result.conn
		if result.peer.UID != uint32(os.Geteuid()) {
			t.Fatalf("accepted peer UID = %d", result.peer.UID)
		}
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("accept did not complete")
	}
	defer server.Close()

	msg := validMessage(ToolCodex)
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.WriteMessage(msg) }()

	got, err := server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if got.Tool != ToolCodex || got.InstanceID != msg.InstanceID {
		t.Fatalf("got %#v", got)
	}
}

func TestAcceptBindsSocketToConfiguredTool(t *testing.T) {
	config := testListenerConfig(t)
	config.BoundTool = ToolGrok
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	acceptResult := make(chan *Conn, 1)
	go func() {
		conn, _, err := listener.Accept(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		acceptResult <- conn
	}()

	client, err := Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var server *Conn
	select {
	case server = <-acceptResult:
	case <-time.After(time.Second):
		t.Fatal("accept did not complete")
	}
	defer server.Close()

	if bound, ok := server.BoundTool(); !ok || bound != ToolGrok {
		t.Fatalf("server bound tool = %v ok=%t, want grok", bound, ok)
	}

	// A producer misreporting a different tool on a socket bound to grok
	// must be rejected, regardless of what the message itself claims.
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.WriteMessage(validMessage(ToolClaude)) }()
	if err := <-writeDone; !errors.Is(err, ErrToolMismatch) {
		t.Fatalf("client write on grok-bound socket with claude tool = %v, want ErrToolMismatch", err)
	}
}

func TestListenerReadTimeoutOnIdleConnection(t *testing.T) {
	config := testListenerConfig(t)
	config.ReadTimeout = 100 * time.Millisecond
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	acceptResult := make(chan *Conn, 1)
	go func() {
		conn, _, err := listener.Accept(context.Background())
		if err == nil {
			acceptResult <- conn
		}
	}()

	client, err := Dial(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var server *Conn
	select {
	case server = <-acceptResult:
	case <-time.After(time.Second):
		t.Fatal("accept did not complete")
	}
	defer server.Close()

	_, err = server.ReadMessage()
	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("error = %v, want ErrReadTimeout", err)
	}
}

func TestDialFailsWhenListenerAbsent(t *testing.T) {
	config := testListenerConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Dial(ctx, config); err == nil {
		t.Fatal("expected dial error against a nonexistent socket")
	}
}

// TestDialErrorDoesNotLeakSocketPath guards against a real regression: Go's
// net package formats the Unix socket path into a dial failure's Error()
// text, and a producer dialing before the broker is up is an ordinary,
// expected condition, not a rare edge case. That error must stay safe to
// log without redaction by the caller.
func TestDialErrorDoesNotLeakSocketPath(t *testing.T) {
	config := testListenerConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Dial(ctx, config)
	if err == nil {
		t.Fatal("expected dial error against a nonexistent socket")
	}
	if strings.Contains(err.Error(), config.SocketPath) {
		t.Fatalf("dial error leaked socket path: %v", err)
	}
}

// TestAcceptErrorAfterCloseDoesNotLeakSocketPath exercises the same
// AcceptUnix failure path a "listener close races with a blocked Accept"
// hits: closing unblocks (or precedes) AcceptUnix with a *net.OpError that
// would otherwise format the socket's path into Error().
//
// Close happens fully before Accept is called, rather than being raced
// against a goroutine blocked in Accept: Go guarantees AcceptUnix on an
// already-closed net.UnixListener returns immediately with the identical
// closed-network error a blocked AcceptUnix gets when Close interrupts it
// (verified directly: a bare net.ListenUnix + Close + AcceptUnix returns
// within microseconds, never blocks), so this reaches the exact same
// classifyIOError call site in Accept() without any dependency on
// scheduler timing or machine load.
//
// The socket file uses a distinctive, unlikely-to-collide name so the leak
// check below cannot pass by coincidence. filepath.Base(config.SocketPath)
// is deliberately what gets checked, not config.SocketPath itself: Listen
// binds through /proc/self/fd/N/<basename> (see boundPath in Listen), so
// the listener's own Addr — and therefore any raw AcceptUnix error — only
// ever contains the socket's base filename, never its directory. A check
// against the full config.SocketPath would silently never trigger, which
// is exactly the mistake the previous version of this test made.
func TestAcceptErrorAfterCloseDoesNotLeakSocketPath(t *testing.T) {
	config := testListenerConfig(t)
	config.SocketPath = filepath.Join(filepath.Dir(config.SocketPath), "instance-leak-canary-9f2b1c9e.sock")
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err = listener.Accept(context.Background())
	if err == nil {
		t.Fatal("expected an error from Accept on an already-closed listener")
	}
	if !errors.Is(err, ErrPeerDisconnected) {
		t.Fatalf("accept-after-close error = %v, want ErrPeerDisconnected", err)
	}
	base := filepath.Base(config.SocketPath)
	if strings.Contains(err.Error(), base) {
		t.Fatalf("accept error leaked the socket filename %q: %v", base, err)
	}
	if strings.Contains(err.Error(), config.SocketPath) {
		t.Fatalf("accept error leaked the socket path: %v", err)
	}
}

// TestConcurrentConnectionsAreRaceFree drives many concurrent client
// connections through one shared Listener under -race: each connection uses
// its own Conn (independent binding state protected by its own mutex), and
// the Listener's Accept path is exercised concurrently with itself.
func TestConcurrentConnectionsAreRaceFree(t *testing.T) {
	config := testListenerConfig(t)
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	const connections = 16
	var serverWait sync.WaitGroup
	serverWait.Add(connections)
	go func() {
		for i := 0; i < connections; i++ {
			conn, _, err := listener.Accept(context.Background())
			if err != nil {
				serverWait.Done()
				continue
			}
			go func(conn *Conn) {
				defer serverWait.Done()
				defer conn.Close()
				if _, err := conn.ReadMessage(); err != nil {
					t.Errorf("server read: %v", err)
				}
			}(conn)
		}
	}()

	var clientWait sync.WaitGroup
	clientWait.Add(connections)
	tools := []Tool{ToolClaude, ToolCodex, ToolGrok}
	for i := 0; i < connections; i++ {
		go func(index int) {
			defer clientWait.Done()
			client, err := Dial(context.Background(), config)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer client.Close()
			msg := validMessage(tools[index%len(tools)])
			msg.Revision = Revision(index + 1)
			if err := client.WriteMessage(msg); err != nil {
				t.Errorf("client write: %v", err)
			}
		}(i)
	}
	clientWait.Wait()
	serverWait.Wait()
}
