package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/codexproducer"
	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/localhooktransport"
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

// withTestHookBeforeRecoveryAttempt substitutes the package-level
// testHookBeforeRecoveryAttempt var with fn and restores the original on
// cleanup.
func withTestHookBeforeRecoveryAttempt(t *testing.T, fn func()) {
	t.Helper()
	original := testHookBeforeRecoveryAttempt
	testHookBeforeRecoveryAttempt = fn
	t.Cleanup(func() { testHookBeforeRecoveryAttempt = original })
}

// TestWatchPermission_LostRecoveryRaceNeverShadowForwards is the direct
// regression test for the bug found in the G.4 ultrareview: watchPermission
// used to unconditionally shadow-forward a synthetic "Stop" after
// sourcelifecycle.WithLock returned, even when store.RecoverCancelled lost
// the race (recovered=false) because a genuine, concurrent Stop hook
// invocation (a separate OS process in production, e.g. run()'s own
// sourcelifecycle.WithLock call) cleared this exact permission watch first.
// That genuine Stop is shadow-forwarded by that other invocation; this
// watcher process must not also emit a second, phantom Stop for the same
// underlying transition.
//
// permissionMatches (store.go) is the same check both PermissionPending and
// RecoverCancelled use against the same session state, so the two only
// disagree when a mutation lands in the narrow window between them within
// one loop iteration — exactly the production race between a concurrent
// Stop hook process and this watcher. testHookBeforeRecoveryAttempt is a
// production no-op call site sitting exactly at that boundary (immediately
// after the transcript match is confirmed, immediately before
// RecoverCancelled/WithLock); substituting it lets this test apply the
// competing Stop synchronously, in-process, with an exact, guaranteed
// ordering — no goroutine, no lock race, no sleep, and therefore no way for
// this test to pass by accident via a less interesting code path.
func TestWatchPermission_LostRecoveryRaceNeverShadowForwards(t *testing.T) {
	shadowCalls := withStubbedDeliverShadow(t)

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptPath := filepath.Join(directory, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := codexhook.NewSessionStore(statePath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(codexhook.Event{
		HookEventName:  "PermissionRequest",
		SessionID:      "session-race",
		TurnID:         "turn-race",
		TranscriptPath: transcriptPath,
	})
	if err != nil || update.Watch == nil {
		t.Fatalf("permission update = %#v, err=%v", update, err)
	}
	// Write the turn_aborted marker after capturing watch.TranscriptOffset,
	// exactly like TestWatchPermissionPublishesIdleForMatchingAbort.
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-race"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hookCalls := 0
	withTestHookBeforeRecoveryAttempt(t, func() {
		hookCalls++
		// Runs synchronously, in-process, exactly once, at the precise
		// boundary between the transcript-match confirmation and
		// watchPermission's own RecoverCancelled attempt: apply the
		// competing Stop right here so RecoverCancelled is guaranteed —
		// not merely likely — to observe the post-Stop state.
		if _, _, err := store.UpdateLifecycle(codexhook.Event{
			HookEventName: "Stop",
			SessionID:     "session-race",
		}); err != nil {
			t.Fatal(err)
		}
	})

	values := map[string]string{
		codexhook.RelayURLEnv:         "http://unused.invalid",
		codexhook.SourceEnv:           "codex-api",
		codexhook.StateFileEnv:        statePath,
		codexhook.SessionTTLEnv:       "1h",
		codexhook.WatcherFileEnv:      filepath.Join(directory, "watchers"),
		codexproducer.EnvShadowSocket: filepath.Join(directory, "shadow.sock"),
	}
	if err := watchPermission(
		context.Background(),
		*update.Watch,
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("watch permission: %v", err)
	}

	if hookCalls != 1 {
		t.Fatalf("expected the synchronization hook to fire exactly once, got %d", hookCalls)
	}
	if calls := shadowCalls.value(); calls != 0 {
		t.Fatalf("expected zero shadow-forward calls when recovery lost the race, got %d", calls)
	}
}

// TestWatchPermission_WonRecoveryDoesShadowForward is the positive
// counterpart: when recovery genuinely wins, exactly one shadow-forward
// call happens.
func TestWatchPermission_WonRecoveryDoesShadowForward(t *testing.T) {
	shadowCalls := withStubbedDeliverShadow(t)

	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	transcriptPath := filepath.Join(directory, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := codexhook.NewSessionStore(statePath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	update, _, err := store.UpdateLifecycle(codexhook.Event{
		HookEventName:  "PermissionRequest",
		SessionID:      "session-win",
		TurnID:         "turn-win",
		TranscriptPath: transcriptPath,
	})
	if err != nil || update.Watch == nil {
		t.Fatalf("permission update = %#v, err=%v", update, err)
	}
	// The abort marker must be written after capturing watch.TranscriptOffset
	// (above), exactly like TestWatchPermissionPublishesIdleForMatchingAbort:
	// otherwise ScanTranscript's starting offset is already past it and
	// "matched" never becomes true, hanging the loop until config.TTL.
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-win"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	values := map[string]string{
		codexhook.RelayURLEnv:                  "http://unused.invalid",
		codexhook.SourceEnv:                    "codex-api",
		codexhook.StateFileEnv:                 statePath,
		codexhook.SessionTTLEnv:                "1h",
		codexhook.WatcherFileEnv:               filepath.Join(directory, "watchers"),
		codexproducer.EnvShadowSocket:          filepath.Join(directory, "shadow.sock"),
		localhooktransport.EnvLocalHookEnabled: "1",
	}
	if err := watchPermission(
		context.Background(),
		*update.Watch,
		func(key string) string { return values[key] },
	); err != nil {
		t.Fatalf("watch permission: %v", err)
	}

	if calls := shadowCalls.value(); calls != 1 {
		t.Fatalf("expected exactly one shadow-forward call when recovery wins, got %d", calls)
	}
}
