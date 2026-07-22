//go:build linux

package localhooktransport

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestValidateClientSocketPathReturnsNonNotExistLstatError(t *testing.T) {
	base := secureTempDir(t)
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Intermediate component is a regular file, so Lstat of the nested directory fails
	// with a non-not-exist error (typically ENOTDIR) rather than os.ErrNotExist.
	socketPath := filepath.Join(blocker, "nested", "hook.sock")
	err := validateClientSocketPath(socketPath)
	if err == nil {
		t.Fatal("non-not-exist directory inspection error was silently accepted")
	}
	if os.IsNotExist(err) {
		t.Fatalf("error = %v, want a non-not-exist inspection failure", err)
	}
}

func TestIngestClientUnavailableSocketIsBounded(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "missing.sock")
	config := DefaultIngestClientConfig()
	config.SocketPath = socketPath
	client, err := NewIngestClient(config)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = client.Send(context.Background(), testIngress())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("missing socket succeeded")
	}
	if elapsed > DefaultIngestTotalBudget+50*time.Millisecond {
		t.Fatalf("unavailable socket exceeded budget: %s err=%v", elapsed, err)
	}
}

func TestIngestClientReadTimeoutHonorsBudget(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "hang.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		accepted <- struct{}{}
		// Read the request frame, then never write a response.
		_, _ = readFrame(connection, DefaultIngestMaximumRequestBytes)
		time.Sleep(300 * time.Millisecond)
	}()

	config := DefaultIngestClientConfig()
	config.SocketPath = socketPath
	client, err := NewIngestClient(config)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = client.Send(context.Background(), testIngress())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("hanging server succeeded")
	}
	if elapsed > DefaultIngestTotalBudget+50*time.Millisecond {
		t.Fatalf("timeout exceeded total budget: %s err=%v", elapsed, err)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("timeout returned too quickly: %s err=%v", elapsed, err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("server never accepted")
	}
}

func TestIngestClientSuccessfulRoundTrip(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "ok.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			errCh <- acceptErr
			return
		}
		defer connection.Close()
		data, readErr := readFrame(connection, DefaultIngestMaximumRequestBytes)
		if readErr != nil {
			errCh <- readErr
			return
		}
		request, decodeErr := DecodeIngestRequestJSON(data)
		if decodeErr != nil {
			errCh <- decodeErr
			return
		}
		response := emptyIngestResponse(StatusOK, request.RequestID)
		encoded, encodeErr := EncodeIngestResponseJSON(response, DefaultIngestMaximumResponseBytes)
		if encodeErr != nil {
			errCh <- encodeErr
			return
		}
		errCh <- writeFrame(connection, encoded, DefaultIngestMaximumResponseBytes)
	}()

	config := DefaultIngestClientConfig()
	config.SocketPath = socketPath
	client, err := NewIngestClient(config)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Send(context.Background(), testIngress())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != StatusOK || !response.NoBindingPerformed || response.ProtocolVersion != IngestProtocolVersion {
		t.Fatalf("response = %#v", response)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestDeliverIngressDisabledSkipsSocket(t *testing.T) {
	called := false
	err := DeliverIngress(context.Background(), func(key string) string {
		called = true
		if key == EnvLocalHookEnabled {
			return "0"
		}
		t.Fatalf("socket config read while disabled: %s", key)
		return ""
	}, testIngress())
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("enabled flag was not inspected")
	}
}

func TestDeliverIngressUnavailableSocketFailsOpen(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "absent.sock")
	err := DeliverIngress(context.Background(), func(key string) string {
		switch key {
		case EnvLocalHookEnabled:
			return "1"
		case EnvLocalHookSocket:
			return socketPath
		default:
			return ""
		}
	}, testIngress())
	if err == nil {
		t.Fatal("expected delivery error for diagnostics")
	}
	TryDeliverIngress(context.Background(), func(key string) string {
		switch key {
		case EnvLocalHookEnabled:
			return "1"
		case EnvLocalHookSocket:
			return socketPath
		default:
			return ""
		}
	}, testIngress())
}

func TestIngestClientRejectsMalformedResponse(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "bad-response.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = readFrame(connection, DefaultIngestMaximumRequestBytes)
		// Oversized / malformed: valid frame length but unknown fields.
		payload, _ := json.Marshal(map[string]any{
			"protocol_version":     2,
			"request_id":           "ignored",
			"status":               "ok",
			"error_codes":          []string{},
			"no_binding_performed": true,
			"proposals":            []any{},
		})
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
		_, _ = connection.Write(header[:])
		_, _ = connection.Write(payload)
	}()

	config := DefaultIngestClientConfig()
	config.SocketPath = socketPath
	client, err := NewIngestClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(context.Background(), testIngress()); err == nil {
		t.Fatal("malformed response accepted")
	}
}

func testIngress() hookadapter.IngressObservation {
	return hookadapter.IngressObservation{
		Tool:           instancepresence.ToolClaude,
		HookSessionRef: "session-fixture",
		Lifecycle:      instancecorrelation.LifecycleActive,
	}
}

func TestIngestClientContextCancelClosesConnection(t *testing.T) {
	directory := secureTempDir(t)
	socketPath := filepath.Join(directory, "cancel.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = readFrame(connection, DefaultIngestMaximumRequestBytes)
		time.Sleep(300 * time.Millisecond)
	}()

	config := DefaultIngestClientConfig()
	config.SocketPath = socketPath
	client, err := NewIngestClient(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = client.Send(ctx, testIngress())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("canceled send succeeded")
	}
	if elapsed > 80*time.Millisecond {
		t.Fatalf("cancel did not bound wait: %s err=%v", elapsed, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrReadTimeout) && !errors.Is(err, ErrPeerDisconnected) && !errors.Is(err, net.ErrClosed) {
		// Any bounded transport failure is acceptable for cancel; just ensure it failed quickly.
		t.Logf("cancel error class = %v", err)
	}
}
