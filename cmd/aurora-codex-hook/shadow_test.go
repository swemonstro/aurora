package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/codexproducer"
	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/presence"
)

// These tests are the fail-open/timeout regression suite for shadow
// forwarding (AURORA_CODEX_SHADOW_SOCKET). The contract under test: an
// absent, missing, hanging, or misconfigured shadow target must never
// change the ordinary hook flow's exit code, must never prevent ordinary
// delivery, and must never block for more than a short, bounded, and
// configurable time budget.

// newRelayHarness starts a real HTTP relay (the ordinary delivery path) and
// returns env values plus a channel that receives one decoded snapshot per
// published event — the same fixture shape TestRunPublishesConfiguredCodexSource
// already uses elsewhere in this package.
func newRelayHarness(t *testing.T) (map[string]string, <-chan presence.Snapshot) {
	t.Helper()
	snapshots := make(chan presence.Snapshot, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var snapshot presence.Snapshot
		if err := json.NewDecoder(request.Body).Decode(&snapshot); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		snapshots <- snapshot
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	values := map[string]string{
		codexhook.RelayURLEnv:  server.URL,
		codexhook.SourceEnv:    "codex-api",
		codexhook.StateFileEnv: filepath.Join(t.TempDir(), "codex-api.json"),
	}
	return values, snapshots
}

func mustReceiveSnapshot(t *testing.T, snapshots <-chan presence.Snapshot) presence.Snapshot {
	t.Helper()
	select {
	case snapshot := <-snapshots:
		return snapshot
	case <-time.After(2 * time.Second):
		t.Fatal("expected ordinary relay delivery to succeed, but no snapshot was published")
		return presence.Snapshot{}
	}
}

func TestShadow_DisabledIsExactPriorBehavior(t *testing.T) {
	values, snapshots := newRelayHarness(t)
	// AURORA_CODEX_SHADOW_SOCKET intentionally left unset.
	err := run(context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-a"}`),
		func(key string) string { return values[key] },
	)
	if err != nil {
		t.Fatalf("run returned an error with shadow disabled: %v", err)
	}
	snapshot := mustReceiveSnapshot(t, snapshots)
	if snapshot.State != "working" {
		t.Fatalf("ordinary delivery state = %q, want working", snapshot.State)
	}
}

func TestShadow_MissingSocketOrdinaryDeliverySucceeds(t *testing.T) {
	values, snapshots := newRelayHarness(t)
	values[codexproducer.EnvShadowSocket] = filepath.Join(t.TempDir(), "no-such-shadow.sock")
	values[EnvShadowConnectTimeoutMS] = "20"
	values[EnvShadowWriteTimeoutMS] = "20"

	start := time.Now()
	err := run(context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-b"}`),
		func(key string) string { return values[key] },
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run returned an error with a missing shadow socket: %v", err)
	}
	mustReceiveSnapshot(t, snapshots)
	// A missing socket fails immediately (connection refused / no such
	// file), so this should be fast regardless of the configured timeout;
	// a generous bound still catches any accidental blocking regression.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("run took %v with a missing shadow socket, expected near-instant failure", elapsed)
	}
}

func TestShadow_HangingSocketReturnsWithinBoundedBudget(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "hanging-shadow.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			// Accept the connection but never read from or close it, so any
			// write attempt against it depends entirely on the write
			// deadline, not on the peer.
			select {
			case accepted <- struct{}{}:
			default:
			}
			_ = connection
		}
	}()

	values, snapshots := newRelayHarness(t)
	values[codexproducer.EnvShadowSocket] = socketPath
	const configuredTimeout = 20 * time.Millisecond
	values[EnvShadowConnectTimeoutMS] = "20"
	values[EnvShadowWriteTimeoutMS] = "20"

	start := time.Now()
	err = run(context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-c"}`),
		func(key string) string { return values[key] },
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run returned an error with a hanging shadow socket: %v", err)
	}
	mustReceiveSnapshot(t, snapshots)
	// Generous (25x) bound relative to the configured per-stage timeouts so
	// this never flakes under CI scheduling jitter, while still proving the
	// hook did not hang anywhere close to indefinitely.
	if elapsed > 25*configuredTimeout {
		t.Fatalf("run took %v with a hanging shadow socket, expected close to the configured %v budget", elapsed, configuredTimeout)
	}
}

func TestShadow_PeerClosesImmediatelyAfterAcceptOrdinaryDeliveryUnaffected(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "closing-shadow.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connection.Close() // simulate a producer that immediately drops the connection.
		}
	}()

	values, snapshots := newRelayHarness(t)
	values[codexproducer.EnvShadowSocket] = socketPath
	values[EnvShadowConnectTimeoutMS] = "50"
	values[EnvShadowWriteTimeoutMS] = "50"

	err = run(context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-d"}`),
		func(key string) string { return values[key] },
	)
	if err != nil {
		t.Fatalf("run returned an error when the shadow peer closes immediately: %v", err)
	}
	mustReceiveSnapshot(t, snapshots)
}

func TestShadow_InvalidTimeoutOverridesFailOpenOntoDefaults(t *testing.T) {
	values, snapshots := newRelayHarness(t)
	values[codexproducer.EnvShadowSocket] = filepath.Join(t.TempDir(), "no-such-shadow.sock")
	// Garbage overrides must fall back to safe defaults, never panic or error.
	for _, garbage := range []string{"not-a-number", "-5", "0", "3.14", ""} {
		values[EnvShadowConnectTimeoutMS] = garbage
		values[EnvShadowWriteTimeoutMS] = garbage
		err := run(context.Background(),
			strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-e"}`),
			func(key string) string { return values[key] },
		)
		if err != nil {
			t.Fatalf("run returned an error for invalid timeout override %q: %v", garbage, err)
		}
		mustReceiveSnapshot(t, snapshots)
	}
}

// TestShadow_AbsurdlyLargeTimeoutOverrideStaysHardBounded is the end-to-end
// regression test (real socket, real env parsing, real
// codexproducer.ShadowDeliveryConfig clamp) for the G.4 ultrareview finding:
// a syntactically valid but absurdly large positive override (as if an
// operator typed minutes where milliseconds were expected) must not turn
// into minutes of real blocking — codexproducer.MaxShadowConnectTimeout /
// MaxShadowWriteTimeout hard-cap it regardless of what the env var says.
func TestShadow_AbsurdlyLargeTimeoutOverrideStaysHardBounded(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "hanging-shadow.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection // accept but never read: only the write deadline can end this.
		}
	}()

	values, snapshots := newRelayHarness(t)
	values[codexproducer.EnvShadowSocket] = socketPath
	// A huge, syntactically valid override: 10 minutes in milliseconds.
	values[EnvShadowConnectTimeoutMS] = "600000"
	values[EnvShadowWriteTimeoutMS] = "600000"

	start := time.Now()
	err = run(context.Background(),
		strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"session-f"}`),
		func(key string) string { return values[key] },
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	mustReceiveSnapshot(t, snapshots)
	if elapsed > 5*time.Second {
		t.Fatalf("run took %v with a requested 10-minute timeout override; expected the hard cap (at most codexproducer.MaxShadowConnectTimeout+MaxShadowWriteTimeout) to apply", elapsed)
	}
}

// TestShadow_DeliverShadowNeverReturnsAnErrorOrLoggableValue is a structural
// canary: deliverShadow's signature must have no return value at all (not
// even an error), so there is no channel through which observation content,
// CODEX_HOME, cwd, session id, or the shadow socket path could ever be
// surfaced to a caller or logged. If this signature ever changes, this test
// forces a conscious decision about what a new return value may safely
// expose.
func TestShadow_DeliverShadowNeverReturnsAnErrorOrLoggableValue(t *testing.T) {
	signature := reflect.TypeOf(deliverShadow)
	if signature.NumOut() != 0 {
		t.Fatalf("deliverShadow must not return any value (found %d); a return value is a new leak surface that needs explicit review", signature.NumOut())
	}
}

func TestShadowTimeoutFromEnv_NeverPanicsAndFallsBackSafely(t *testing.T) {
	const fallback = 123 * time.Millisecond
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"", fallback},
		{"not-a-number", fallback},
		{"-5", fallback},
		{"0", fallback},
		{"3.14", fallback},
		{"50", 50 * time.Millisecond},
	}
	for _, testCase := range cases {
		got := shadowTimeoutFromEnv(func(string) string { return testCase.value }, "TEST_ENV", fallback)
		if got != testCase.want {
			t.Fatalf("shadowTimeoutFromEnv(%q) = %v, want %v", testCase.value, got, testCase.want)
		}
	}
}

func TestShadow_StaleStopIsNotForwardedOverNewerAttention(t *testing.T) {
	shadowCalls := withStubbedDeliverShadow(t)
	values, snapshots := newRelayHarness(t)
	getenv := func(key string) string { return values[key] }

	for _, payload := range []string{
		`{"hook_event_name":"PermissionRequest","session_id":"session-race","turn_id":"turn-old"}`,
		`{"hook_event_name":"PermissionRequest","session_id":"session-race","turn_id":"turn-new"}`,
	} {
		if err := run(context.Background(), strings.NewReader(payload), getenv); err != nil {
			t.Fatal(err)
		}
		if got := mustReceiveSnapshot(t, snapshots).State; got != "attention" {
			t.Fatalf("permission state = %q, want attention", got)
		}
	}
	if err := run(context.Background(), strings.NewReader(
		`{"hook_event_name":"Stop","session_id":"session-race","turn_id":"turn-old"}`,
	), getenv); err != nil {
		t.Fatal(err)
	}
	if calls := shadowCalls.value(); calls != 2 {
		t.Fatalf("stale Stop added a shadow-forward call: got %d calls, want 2", calls)
	}
	select {
	case snapshot := <-snapshots:
		t.Fatalf("stale Stop was published to ordinary relay: %#v", snapshot)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestShadow_MatchingStopForwardsExactlyOnce(t *testing.T) {
	shadowCalls := withStubbedDeliverShadow(t)
	values, snapshots := newRelayHarness(t)
	getenv := func(key string) string { return values[key] }

	if err := run(context.Background(), strings.NewReader(
		`{"hook_event_name":"PermissionRequest","session_id":"session-win","turn_id":"turn-win"}`,
	), getenv); err != nil {
		t.Fatal(err)
	}
	mustReceiveSnapshot(t, snapshots)
	before := shadowCalls.value()
	if err := run(context.Background(), strings.NewReader(
		`{"hook_event_name":"Stop","session_id":"session-win","turn_id":"turn-win"}`,
	), getenv); err != nil {
		t.Fatal(err)
	}
	if got := mustReceiveSnapshot(t, snapshots).State; got != "idle" {
		t.Fatalf("matching Stop state = %q, want idle", got)
	}
	if calls := shadowCalls.value(); calls != before+1 {
		t.Fatalf("matching Stop shadow calls = %d, want %d", calls, before+1)
	}
}

// withStubbedDeliverShadow substitutes the package-level deliverShadow var
// with fake, recording every call, and restores the original on cleanup.
func withStubbedDeliverShadow(t *testing.T) *int32Counter {
	t.Helper()
	original := deliverShadow
	counter := &int32Counter{}
	deliverShadow = func(ctx context.Context, getenv func(string) string, ingress hookadapter.IngressObservation) {
		counter.increment()
	}
	t.Cleanup(func() { deliverShadow = original })
	return counter
}

type int32Counter struct {
	mu    sync.Mutex
	count int
}

func (c *int32Counter) increment() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
}

func (c *int32Counter) value() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}
