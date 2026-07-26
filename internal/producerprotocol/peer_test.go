package producerprotocol

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestSameUIDAuthenticator(t *testing.T) {
	auth := SameUIDAuthenticator{ServerUID: 1000}
	if err := auth.Authenticate(PeerIdentity{UID: 1000}); err != nil {
		t.Fatalf("same UID rejected: %v", err)
	}
	if err := auth.Authenticate(PeerIdentity{UID: 1001}); !errors.Is(err, ErrUnauthorizedPeer) {
		t.Fatalf("other UID error = %v", err)
	}
	auth.AllowedUIDs = map[uint32]struct{}{1001: {}}
	if err := auth.Authenticate(PeerIdentity{UID: 1001}); err != nil {
		t.Fatalf("allowlisted UID rejected: %v", err)
	}
}

type staticBinder struct {
	tool Tool
	err  error
}

func (binder staticBinder) BindTool(PeerIdentity) (Tool, error) { return binder.tool, binder.err }

func TestBindPeerToolAppliesBinderResult(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	config := DefaultConfig(wallClock{})
	config.ReadTimeout, config.WriteTimeout = time.Second, time.Second
	conn := newConn(right, config)

	if err := BindPeerTool(conn, PeerIdentity{UID: 1000}, staticBinder{tool: ToolCodex}); err != nil {
		t.Fatal(err)
	}
	if bound, ok := conn.BoundTool(); !ok || bound != ToolCodex {
		t.Fatalf("bound = %v ok=%t, want codex", bound, ok)
	}
}

func TestBindPeerToolPropagatesBinderError(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	config := DefaultConfig(wallClock{})
	config.ReadTimeout, config.WriteTimeout = time.Second, time.Second
	conn := newConn(right, config)

	sentinel := errors.New("no policy for this peer")
	if err := BindPeerTool(conn, PeerIdentity{UID: 1000}, staticBinder{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if _, ok := conn.BoundTool(); ok {
		t.Fatal("connection must not be bound after a binder error")
	}
}
