package producerprotocol

import (
	"errors"
	"net"
	"testing"
	"time"
)

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

func TestConnReadTimeout(t *testing.T) {
	clock := wallClock{}
	_, server := pipeConns(t, clock)
	defer server.Close()
	_, err := server.ReadMessage()
	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("error = %v, want ErrReadTimeout", err)
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
