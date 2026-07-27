package producerprotocol

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestClassifyIOErrorNeverLeaksAddress guards the property that this
// package's transport errors are always safe to log: a *net.OpError for a
// Unix domain socket formats its Addr (the socket path) into Error(), so
// classifyIOError must never return such an error unwrapped, for any
// classification branch, while still preserving errors.Is/Unwrap so callers
// can classify and callers of ErrorCodeOf keep working.
func TestClassifyIOErrorNeverLeaksAddress(t *testing.T) {
	const secretPath = "/run/aurora/instance-secret-abc123.sock"
	addr := &net.UnixAddr{Name: secretPath, Net: "unix"}

	timeoutErr := &net.OpError{Op: "read", Net: "unix", Addr: addr, Err: os.ErrDeadlineExceeded}
	classifiedTimeout := classifyIOError(timeoutErr, true)
	if strings.Contains(classifiedTimeout.Error(), secretPath) {
		t.Fatalf("timeout classification leaked socket path: %v", classifiedTimeout)
	}
	if !errors.Is(classifiedTimeout, ErrReadTimeout) {
		t.Fatalf("timeout classification = %v, want ErrReadTimeout", classifiedTimeout)
	}
	if !errors.Is(classifiedTimeout, os.ErrDeadlineExceeded) {
		t.Fatalf("timeout classification lost its cause: %v", classifiedTimeout)
	}

	closedErr := &net.OpError{Op: "read", Net: "unix", Addr: addr, Err: net.ErrClosed}
	classifiedClosed := classifyIOError(closedErr, true)
	if strings.Contains(classifiedClosed.Error(), secretPath) {
		t.Fatalf("disconnect classification leaked socket path: %v", classifiedClosed)
	}
	if !errors.Is(classifiedClosed, ErrPeerDisconnected) {
		t.Fatalf("disconnect classification = %v, want ErrPeerDisconnected", classifiedClosed)
	}

	unclassifiedErr := &net.OpError{Op: "dial", Net: "unix", Addr: addr, Err: syscall.EACCES}
	classifiedDefault := classifyIOError(unclassifiedErr, true)
	if strings.Contains(classifiedDefault.Error(), secretPath) {
		t.Fatalf("default classification leaked socket path: %v", classifiedDefault)
	}
	if !errors.Is(classifiedDefault, syscall.EACCES) {
		t.Fatalf("default classification lost its cause: %v", classifiedDefault)
	}
}

func TestIsIdleReadTimeout(t *testing.T) {
	if IsIdleReadTimeout(nil) {
		t.Fatal("nil must not be idle read timeout")
	}
	idle := classifyIOError(&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, true)
	if !IsIdleReadTimeout(idle) {
		t.Fatalf("classified timeout should be idle: %v", idle)
	}
	partial := protocolError(CodeIncompleteFrame, &wrappedError{
		sentinel: ErrIncompleteFrame,
		cause:    idle,
	})
	if IsIdleReadTimeout(partial) {
		t.Fatalf("incomplete frame must not be idle: %v", partial)
	}
	if ErrorCodeOf(partial) != CodeIncompleteFrame {
		t.Fatalf("code = %v, want incomplete_frame", ErrorCodeOf(partial))
	}
	if !errors.Is(partial, ErrIncompleteFrame) {
		t.Fatal("errors.Is IncompleteFrame failed")
	}
	if !errors.Is(partial, ErrReadTimeout) {
		t.Fatal("incomplete frame should still match ErrReadTimeout via cause chain")
	}
}

func TestWrapOpaqueHidesCauseTextButPreservesUnwrap(t *testing.T) {
	cause := &os.PathError{Op: "lstat", Path: "/run/aurora/instance-secret.sock", Err: os.ErrPermission}
	wrapped := wrapOpaque("inspect socket path", cause)
	if strings.Contains(wrapped.Error(), "instance-secret") {
		t.Fatalf("wrapOpaque leaked path: %v", wrapped)
	}
	if wrapped.Error() != "inspect socket path" {
		t.Fatalf("wrapOpaque text = %q, want %q", wrapped.Error(), "inspect socket path")
	}
	if !errors.Is(wrapped, os.ErrPermission) {
		t.Fatal("wrapOpaque must preserve the cause chain for errors.Is")
	}
	if wrapOpaque("anything", nil) != nil {
		t.Fatal("wrapOpaque(_, nil) must return nil")
	}
}
