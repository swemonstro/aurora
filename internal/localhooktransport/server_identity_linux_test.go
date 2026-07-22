//go:build linux

package localhooktransport

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

type recordingIdentityObserver struct {
	captures  atomic.Int64
	completes atomic.Int64
	mu        sync.Mutex
	validated bool
	tool      instancepresence.ToolKind
}

func (observer *recordingIdentityObserver) CapturePeer(peer PeerIdentity) IdentityPeerCapture {
	observer.captures.Add(1)
	pid := uint64(0)
	if peer.PID > 0 {
		pid = uint64(peer.PID)
	}
	return IdentityPeerCapture{
		PeerUID: peer.UID, PeerGID: peer.GID, PeerPID: peer.PID,
		GenerationOK: true, GenerationPID: pid,
		GenerationStarted: testTime,
		Ancestry: []IdentityAncestryHop{
			{PID: pid, StartedAt: testTime, Depth: 0, IsPeer: true},
		},
		ReasonCodes: []string{"peer_generation_ok"},
		CapturedAt:  testTime,
	}
}

func (observer *recordingIdentityObserver) CompleteIngest(capture IdentityPeerCapture, tool instancepresence.ToolKind, lifecycle instancecorrelation.Lifecycle, validated bool) {
	observer.completes.Add(1)
	observer.mu.Lock()
	observer.validated = validated
	observer.tool = tool
	observer.mu.Unlock()
	_ = capture
	_ = lifecycle
}

type panickingIdentityObserver struct{}

func (panickingIdentityObserver) CapturePeer(PeerIdentity) IdentityPeerCapture {
	panic("capture boom")
}

func (panickingIdentityObserver) CompleteIngest(IdentityPeerCapture, instancepresence.ToolKind, instancecorrelation.Lifecycle, bool) {
	panic("complete boom")
}

func TestIdentityObserverDisabledByDefault(t *testing.T) {
	server, cancel, done := startIdentityNetworkServer(t, true, nil)
	defer cancel()
	defer func() { _ = server.Close(); <-done }()
	if server.IdentityObserverEnabled() {
		t.Fatal("observer should be disabled by default")
	}
}

func TestIdentityObserverDoesNotChangeIngestResponse(t *testing.T) {
	observer := &recordingIdentityObserver{}
	server, cancel, done := startIdentityNetworkServer(t, true, observer)
	defer cancel()
	defer func() { _ = server.Close(); <-done }()

	response, raw := sendIngestToServer(t, server.config.SocketPath, hookadapter.IngressObservation{
		Tool:           instancepresence.ToolClaude,
		HookSessionRef: "session-measure-a",
		Lifecycle:      instancecorrelation.LifecycleActive,
	})
	if response.Status != StatusOK || !response.NoBindingPerformed {
		t.Fatalf("response = %#v", response)
	}
	for _, needle := range []string{"peer_pid", "started_at", "ancestry", "process_hint", "trusted_hard"} {
		if strings.Contains(raw, needle) {
			t.Fatalf("response leaked identity field %q: %s", needle, raw)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (observer.captures.Load() < 1 || observer.completes.Load() < 1) {
		time.Sleep(5 * time.Millisecond)
	}
	if observer.captures.Load() < 1 || observer.completes.Load() < 1 {
		t.Fatalf("captures=%d completes=%d", observer.captures.Load(), observer.completes.Load())
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if !observer.validated || observer.tool != instancepresence.ToolClaude {
		t.Fatalf("validated=%v tool=%q", observer.validated, observer.tool)
	}
}

func TestIdentityObserverFailureDoesNotRejectValidIngest(t *testing.T) {
	server, cancel, done := startIdentityNetworkServer(t, true, panickingIdentityObserver{})
	defer cancel()
	defer func() { _ = server.Close(); <-done }()

	response, _ := sendIngestToServer(t, server.config.SocketPath, hookadapter.IngressObservation{
		Tool:           instancepresence.ToolCodex,
		HookSessionRef: "session-measure-b",
		Lifecycle:      instancecorrelation.LifecycleIdle,
	})
	if response.Status != StatusOK || !response.NoBindingPerformed {
		t.Fatalf("response = %#v", response)
	}
}

func TestIdentityObserverNotInvokedWhenDisabled(t *testing.T) {
	observer := &recordingIdentityObserver{}
	server, cancel, done := startIdentityNetworkServer(t, true, nil)
	defer cancel()
	defer func() { _ = server.Close(); <-done }()

	server.SetIdentityObserver(observer)
	server.SetIdentityObserver(nil)
	if server.IdentityObserverEnabled() {
		t.Fatal("observer should be disabled")
	}

	response, _ := sendIngestToServer(t, server.config.SocketPath, hookadapter.IngressObservation{
		Tool:           instancepresence.ToolClaude,
		HookSessionRef: "session-measure-c",
		Lifecycle:      instancecorrelation.LifecycleActive,
	})
	if response.Status != StatusOK {
		t.Fatalf("response = %#v", response)
	}
	if observer.captures.Load() != 0 || observer.completes.Load() != 0 {
		t.Fatalf("observer invoked while disabled: captures=%d completes=%d", observer.captures.Load(), observer.completes.Load())
	}
}

func startIdentityNetworkServer(t *testing.T, enableIngest bool, observer IngestIdentityObserver) (*Server, context.CancelFunc, <-chan error) {
	t.Helper()
	clock := wallClock{}
	config := DefaultConfig(clock)
	config.SocketPath = filepath.Join(secureTempDir(t), "identity.sock")
	if err := os.Chmod(filepath.Dir(config.SocketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	config.ReadDeadline = 250 * time.Millisecond
	config.WriteDeadline = 250 * time.Millisecond
	config.MaximumHandlingTime = time.Second

	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCorrelationService(&fakeSnapshots{samples: [][]instancecorrelation.RuntimeObservation{testSample()}}, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(config, service)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config, receiver, DefaultAuthenticator(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if enableIngest {
		if err := server.EnableIngest(DefaultIngestServerConfig(clock)); err != nil {
			t.Fatal(err)
		}
	}
	if observer != nil {
		server.SetIdentityObserver(observer)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	return server, cancel, done
}

func sendIngestToServer(t *testing.T, socketPath string, ingress hookadapter.IngressObservation) (IngestResponse, string) {
	t.Helper()
	request, err := NewIngestRequest(ingress)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeIngestRequestJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeFrame(conn, payload, DefaultIngestMaximumRequestBytes); err != nil {
		t.Fatal(err)
	}
	data, err := readFrame(conn, DefaultIngestMaximumResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	response, err := DecodeIngestResponseJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(data)
}
