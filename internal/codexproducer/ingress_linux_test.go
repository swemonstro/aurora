//go:build linux

package codexproducer

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

const (
	helperProcessEnv    = "AURORA_CODEXPRODUCER_TEST_HELPER"
	helperSocketEnv     = "AURORA_CODEXPRODUCER_TEST_SOCKET"
	helperSessionRefEnv = "AURORA_CODEXPRODUCER_TEST_SESSION"
)

// TestMain intercepts a special re-exec of this same test binary, used to
// deliver one hook observation from a genuinely separate OS process whose
// CODEX_HOME is set at exec time (see dialFromHelperProcess). Modifying the
// current test process's own environment after startup (e.g. t.Setenv)
// would not work here: /proc/<pid>/environ reflects the environment a
// process was actually exec'd with, not later in-process getenv/setenv
// calls, so exercising readPeerCorrelationHints realistically requires a
// real child process.
func TestMain(m *testing.M) {
	if os.Getenv(helperProcessEnv) == "1" {
		runIngressTestHelperProcess()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runIngressTestHelperProcess() {
	connection, err := net.Dial("unix", os.Getenv(helperSocketEnv))
	if err != nil {
		os.Exit(1)
	}
	defer connection.Close()
	observation := hookadapter.IngressObservation{
		Tool:           instancepresence.ToolCodex,
		HookSessionRef: instancepresence.OpaqueIdentity(os.Getenv(helperSessionRefEnv)),
		Lifecycle:      instancecorrelation.LifecycleActive,
		EffectiveState: instancepresence.StateWorking,
	}
	if err := json.NewEncoder(connection).Encode(observation); err != nil {
		os.Exit(1)
	}
	// Stay alive briefly so the server has a real window to capture this
	// process's /proc/<pid>/stat and /proc/<pid>/environ before it exits —
	// deterministic for this test only. In production, a hook invocation
	// exits almost immediately after writing, which is a genuine,
	// documented best-effort limitation of correlation hint capture (see
	// readPeerCorrelationHints's doc comment): a hint that arrives empty
	// because the process already exited degrades gracefully to "stays
	// pending" in Correlator, never a crash or a wrong bind.
	time.Sleep(150 * time.Millisecond)
}

// dialFromHelperProcess re-execs this test binary as a genuinely separate
// process with CODEX_HOME set in its exec environment, connects it to
// socketPath, and has it send one delivery for sessionRef.
func dialFromHelperProcess(t *testing.T, socketPath, sessionRef, codexHome string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^$")
	cmd.Env = append(os.Environ(),
		helperProcessEnv+"=1",
		helperSocketEnv+"="+socketPath,
		helperSessionRefEnv+"="+sessionRef,
		"CODEX_HOME="+codexHome,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper process failed: %v: %s", err, output)
	}
}

func ingressTestSocketPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-codexproducer-ingress-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "hook-ingress.sock")
}

type deliveryRecorder struct {
	mu         sync.Mutex
	deliveries []HookDelivery
}

func (recorder *deliveryRecorder) handle(delivery HookDelivery) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.deliveries = append(recorder.deliveries, delivery)
}

func (recorder *deliveryRecorder) snapshot() []HookDelivery {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]HookDelivery{}, recorder.deliveries...)
}

func TestIngress_AcceptsAndDeliversValidObservation(t *testing.T) {
	socketPath := ingressTestSocketPath(t)
	authenticator := producerprotocol.SameUIDAuthenticator{ServerUID: uint32(os.Geteuid())}
	listener, err := ListenIngress(socketPath, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &deliveryRecorder{}
	done := make(chan struct{})
	go func() {
		_ = listener.Serve(ctx, recorder.handle)
		close(done)
	}()

	const testCodexHome = "/home/carl/.codex-ingress-test"
	dialFromHelperProcess(t, socketPath, "session-1", testCodexHome)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	deliveries := recorder.snapshot()
	if len(deliveries) != 1 {
		t.Fatalf("expected exactly one delivery, got %d", len(deliveries))
	}
	got := deliveries[0]
	if got.Observation.HookSessionRef != "session-1" || got.Observation.EffectiveState != instancepresence.StateWorking {
		t.Fatalf("unexpected observation: %+v", got.Observation)
	}
	if got.EnvCodexHome != testCodexHome {
		t.Fatalf("expected captured EnvCodexHome %q, got %q", testCodexHome, got.EnvCodexHome)
	}
	if got.ProcessGroupOrJob == "" || !strings.HasPrefix(got.ProcessGroupOrJob, "pgrp:") {
		t.Fatalf("expected a captured process-group hint, got %q", got.ProcessGroupOrJob)
	}

	cancel()
	<-done
}

func TestIngress_MalformedMessageDropsOnlyThatConnection(t *testing.T) {
	socketPath := ingressTestSocketPath(t)
	authenticator := producerprotocol.SameUIDAuthenticator{ServerUID: uint32(os.Geteuid())}
	listener, err := ListenIngress(socketPath, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &deliveryRecorder{}
	done := make(chan struct{})
	go func() {
		_ = listener.Serve(ctx, recorder.handle)
		close(done)
	}()

	// First connection sends garbage.
	bad, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Write([]byte("not json at all")); err != nil {
		t.Fatal(err)
	}
	_ = bad.Close()

	// A second, independent connection with a valid message must still be
	// accepted: one bad peer must never affect another.
	good, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	observation := hookadapter.IngressObservation{
		Tool: instancepresence.ToolCodex, HookSessionRef: "session-2",
		Lifecycle: instancecorrelation.LifecycleIdle, EffectiveState: instancepresence.StateIdle,
	}
	if err := json.NewEncoder(good).Encode(observation); err != nil {
		t.Fatal(err)
	}
	_ = good.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	deliveries := recorder.snapshot()
	if len(deliveries) != 1 || deliveries[0].Observation.HookSessionRef != "session-2" {
		t.Fatalf("expected exactly the good connection's delivery, got %+v", deliveries)
	}
	cancel()
	<-done
}

func TestIngress_RejectsBindingOverExistingPath(t *testing.T) {
	socketPath := ingressTestSocketPath(t)
	authenticator := producerprotocol.SameUIDAuthenticator{ServerUID: uint32(os.Geteuid())}
	first, err := ListenIngress(socketPath, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if _, err := ListenIngress(socketPath, authenticator); err == nil {
		t.Fatal("expected an error binding a second listener over an existing socket path")
	}
}

func TestIngress_RequiresAuthenticator(t *testing.T) {
	socketPath := ingressTestSocketPath(t)
	if _, err := ListenIngress(socketPath, nil); err == nil {
		t.Fatal("expected an error for a nil authenticator")
	}
}

func TestTryDeliverShadow_DisabledWhenSocketEmpty(t *testing.T) {
	if TryDeliverShadow(context.Background(), "", hookadapter.IngressObservation{}) {
		t.Fatal("empty socket path must never attempt delivery")
	}
}

func TestTryDeliverShadow_FailsOpenWhenUnreachable(t *testing.T) {
	// No listener at this path: delivery must fail quietly (bool result
	// only), never panic or block beyond DefaultShadowTimeout.
	observation := hookadapter.IngressObservation{
		Tool: instancepresence.ToolCodex, HookSessionRef: "session-1",
		Lifecycle: instancecorrelation.LifecycleIdle, EffectiveState: instancepresence.StateIdle,
	}
	start := time.Now()
	delivered := TryDeliverShadow(context.Background(), "/nonexistent/aurora-codex-shadow-test.sock", observation)
	if delivered {
		t.Fatal("expected delivery to an unreachable socket to fail")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("TryDeliverShadow must not block substantially beyond its own timeout")
	}
}
