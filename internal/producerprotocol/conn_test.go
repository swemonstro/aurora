package producerprotocol

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// scriptedConn is a net.Conn test double whose Read results are predetermined.
// It lets Conn.ReadMessage tests exercise idle vs partial-frame timeout
// classification without wall-clock deadlines or sleeps. SetReadDeadline is
// a no-op: the script supplies timeout errors directly.
type scriptedConn struct {
	mu         sync.Mutex
	steps      []scriptedRead
	pending    []byte
	pendingErr error
	closed     bool
}

type scriptedRead struct {
	data []byte
	err  error
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		if len(c.pending) > 0 {
			return n, nil
		}
		err := c.pendingErr
		c.pendingErr = nil
		return n, err
	}
	if len(c.steps) == 0 {
		return 0, io.EOF
	}
	step := c.steps[0]
	c.steps = c.steps[1:]
	if len(step.data) == 0 {
		return 0, step.err
	}
	n := copy(p, step.data)
	if n < len(step.data) {
		c.pending = append([]byte(nil), step.data[n:]...)
		c.pendingErr = step.err
		return n, nil
	}
	return n, step.err
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return len(p), nil
}

func (c *scriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *scriptedConn) LocalAddr() net.Addr              { return scriptedAddr{} }
func (c *scriptedConn) RemoteAddr() net.Addr             { return scriptedAddr{} }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

type scriptedAddr struct{}

func (scriptedAddr) Network() string { return "scripted" }
func (scriptedAddr) String() string  { return "scripted" }

// timeoutNetErrorForConn mirrors the framing-test timeout double so Conn
// tests share the same net.Error shape without importing across files.
type timeoutNetErrorForConn struct{}

func (timeoutNetErrorForConn) Error() string   { return "i/o timeout" }
func (timeoutNetErrorForConn) Timeout() bool   { return true }
func (timeoutNetErrorForConn) Temporary() bool { return true }

func encodeFrameForTest(t *testing.T, msg Message, maximum uint32) []byte {
	t.Helper()
	data, err := EncodeMessageJSON(CanonicalMessage(msg), maximum)
	if err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	return append(header[:], data...)
}

func pipeConns(t *testing.T, clock Clock) (*Conn, *Conn) {
	t.Helper()
	left, right := net.Pipe()
	config := DefaultConfig(clock)
	config.ReadTimeout = 200 * time.Millisecond
	config.WriteTimeout = 200 * time.Millisecond
	return newConn(left, config), newConn(right, config)
}

func TestConnReadWriteRoundTrip(t *testing.T) {
	clock := wallClock{}
	client, server := pipeConns(t, clock)
	defer client.Close()
	defer server.Close()

	msg := validMessage(ToolClaude)
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.WriteMessage(msg) }()

	got, err := server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if got.Tool != msg.Tool || got.InstanceID != msg.InstanceID || got.Revision != msg.Revision {
		t.Fatalf("got %#v, want %#v", got, msg)
	}
}

func TestConnMultipleMessagesInSequence(t *testing.T) {
	clock := wallClock{}
	client, server := pipeConns(t, clock)
	defer client.Close()
	defer server.Close()

	states := []State{StateWorking, StateIdle, StateAttention}
	go func() {
		for index, state := range states {
			msg := validMessage(ToolClaude)
			msg.State = state
			msg.Revision = Revision(index + 1)
			if err := client.WriteMessage(msg); err != nil {
				return
			}
		}
	}()

	for index, want := range states {
		got, err := server.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", index, err)
		}
		if got.State != want || got.Revision != Revision(index+1) {
			t.Fatalf("message %d = %#v, want state=%s revision=%d", index, got, want, index+1)
		}
	}
}

func TestConnRejectsToolSwitchAfterBinding(t *testing.T) {
	clock := wallClock{}
	client, server := pipeConns(t, clock)
	defer client.Close()
	defer server.Close()

	go func() {
		_ = client.WriteMessage(validMessage(ToolClaude))
	}()
	first, err := server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if bound, ok := server.BoundTool(); !ok || bound != ToolClaude {
		t.Fatalf("server not bound to claude after first message: %v ok=%t", bound, ok)
	}

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- client.WriteMessage(validMessage(ToolCodex))
	}()

	// The client's own connection is bound to claude by its first write, so
	// its second write (a different tool) must be rejected before any bytes
	// reach the pipe.
	if err := <-writeErr; !errors.Is(err, ErrToolMismatch) {
		t.Fatalf("client write with switched tool = %v, want ErrToolMismatch", err)
	}
	_ = first
}

func TestConnServerRejectsIncomingToolSwitch(t *testing.T) {
	// Exercise the server-side enforcement directly: bind the server
	// connection first, then feed it a frame whose tool differs, bypassing
	// the client's own symmetric check.
	clock := wallClock{}
	left, right := net.Pipe()
	config := DefaultConfig(clock)
	config.ReadTimeout = 200 * time.Millisecond
	config.WriteTimeout = 200 * time.Millisecond
	server := newConn(right, config)
	defer server.Close()
	defer left.Close()

	if err := server.Bind(ToolClaude); err != nil {
		t.Fatal(err)
	}

	msg := validMessage(ToolCodex)
	data, err := EncodeMessageJSON(CanonicalMessage(msg), config.MaximumMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = writeFrame(left, data, config.MaximumMessageBytes) }()

	_, readErr := server.ReadMessage()
	if !errors.Is(readErr, ErrToolMismatch) {
		t.Fatalf("read after bind mismatch = %v, want ErrToolMismatch", readErr)
	}
}

func TestConnBindIsIdempotentForSameTool(t *testing.T) {
	clock := wallClock{}
	client, server := pipeConns(t, clock)
	defer client.Close()
	defer server.Close()
	if err := server.Bind(ToolGrok); err != nil {
		t.Fatal(err)
	}
	if err := server.Bind(ToolGrok); err != nil {
		t.Fatalf("second bind with same tool should be a no-op: %v", err)
	}
	if err := server.Bind(ToolClaude); !errors.Is(err, ErrToolMismatch) {
		t.Fatalf("bind with different tool = %v, want ErrToolMismatch", err)
	}
}

// TestConnReadTimeout is the wall-clock deadline integration test for Conn:
// a real net.Pipe with no peer data must surface ErrReadTimeout via
// SetReadDeadline. Classification-heavy cases below use scriptedConn so they
// do not depend on sleep racing a deadline.
func TestConnReadTimeout(t *testing.T) {
	clock := wallClock{}
	_, server := pipeConns(t, clock)
	defer server.Close()
	_, err := server.ReadMessage()
	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("error = %v, want ErrReadTimeout", err)
	}
	if !IsIdleReadTimeout(err) {
		t.Fatalf("zero-byte deadline must be idle, code=%v", ErrorCodeOf(err))
	}
}

// TestConnIdleReadTimeoutThenValidFrameOnSameConnection covers requirement A
// with a scripted peer: a zero-byte read timeout leaves framing intact, so a
// subsequent complete frame on the same Conn is accepted — no wall-clock wait.
func TestConnIdleReadTimeoutThenValidFrameOnSameConnection(t *testing.T) {
	config := DefaultConfig(wallClock{})
	msg := validMessage(ToolClaude)
	frame := encodeFrameForTest(t, msg, config.MaximumMessageBytes)
	raw := &scriptedConn{steps: []scriptedRead{
		{err: timeoutNetErrorForConn{}}, // idle: zero bytes
		{data: frame},                   // full valid frame
	}}
	server := newConn(raw, config)
	defer server.Close()

	_, err := server.ReadMessage()
	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("idle error = %v, want ErrReadTimeout", err)
	}
	if !IsIdleReadTimeout(err) {
		t.Fatalf("idle timeout must be recoverable, code=%v", ErrorCodeOf(err))
	}

	got, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("read after idle timeout: %v", err)
	}
	if got.Tool != msg.Tool || got.InstanceID != msg.InstanceID || got.Revision != msg.Revision {
		t.Fatalf("got %#v, want %#v", got, msg)
	}
}

// TestConnPartialHeaderTimeoutIsFatal covers requirement B without wall-clock
// deadlines: two length-prefix bytes then a timeout.
func TestConnPartialHeaderTimeoutIsFatal(t *testing.T) {
	config := DefaultConfig(wallClock{})
	raw := &scriptedConn{steps: []scriptedRead{
		{data: []byte{0x00, 0x10}},
		{err: timeoutNetErrorForConn{}},
	}}
	server := newConn(raw, config)
	defer server.Close()

	_, err := server.ReadMessage()
	if !errors.Is(err, ErrIncompleteFrame) {
		t.Fatalf("error = %v, want ErrIncompleteFrame", err)
	}
	if ErrorCodeOf(err) != CodeIncompleteFrame {
		t.Fatalf("code = %v, want incomplete_frame", ErrorCodeOf(err))
	}
	if IsIdleReadTimeout(err) {
		t.Fatal("partial header timeout must not be idle")
	}
}

// TestConnPartialBodyTimeoutIsFatal covers requirement C without wall-clock
// deadlines: complete length prefix + partial body, then timeout.
func TestConnPartialBodyTimeoutIsFatal(t *testing.T) {
	config := DefaultConfig(wallClock{})
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 64)
	payload := append(header[:], []byte("ab")...)
	raw := &scriptedConn{steps: []scriptedRead{
		{data: payload},
		{err: timeoutNetErrorForConn{}},
	}}
	server := newConn(raw, config)
	defer server.Close()

	_, err := server.ReadMessage()
	if !errors.Is(err, ErrIncompleteFrame) {
		t.Fatalf("error = %v, want ErrIncompleteFrame", err)
	}
	if ErrorCodeOf(err) != CodeIncompleteFrame {
		t.Fatalf("code = %v, want incomplete_frame", ErrorCodeOf(err))
	}
	if IsIdleReadTimeout(err) {
		t.Fatal("partial body timeout must not be idle")
	}
}

// TestConnPartialHeaderTimeoutDoesNotSurfaceAsMessageTooLarge covers
// requirement D at the Conn layer with a scripted partial length prefix:
// the first ReadMessage must fail as incomplete_frame, never as the
// desynchronized message_too_large class.
func TestConnPartialHeaderTimeoutDoesNotSurfaceAsMessageTooLarge(t *testing.T) {
	config := DefaultConfig(wallClock{})
	config.MaximumMessageBytes = 64
	raw := &scriptedConn{steps: []scriptedRead{
		{data: []byte{0x00, 0x00}},
		{err: timeoutNetErrorForConn{}},
	}}
	server := newConn(raw, config)
	defer server.Close()

	_, err := server.ReadMessage()
	if ErrorCodeOf(err) == CodeMessageTooLarge {
		t.Fatalf("partial header timed out as message_too_large; stream was desynchronized")
	}
	if ErrorCodeOf(err) != CodeIncompleteFrame {
		t.Fatalf("code = %v, want incomplete_frame (err=%v)", ErrorCodeOf(err), err)
	}
}

func TestConnPeerDisconnectDuringRead(t *testing.T) {
	clock := wallClock{}
	client, server := pipeConns(t, clock)
	defer server.Close()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := server.ReadMessage()
	if !errors.Is(err, ErrPeerDisconnected) {
		t.Fatalf("error = %v, want ErrPeerDisconnected", err)
	}
}

func TestConnWriteRejectsInvalidMessage(t *testing.T) {
	clock := wallClock{}
	client, server := pipeConns(t, clock)
	defer client.Close()
	defer server.Close()
	msg := validMessage(ToolClaude)
	msg.Revision = 0
	if err := client.WriteMessage(msg); ErrorCodeOf(err) != CodeInvalidRevision {
		t.Fatalf("code = %v, want invalid_revision (err=%v)", ErrorCodeOf(err), err)
	}
}
