//go:build linux

package codexproducer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

func validShadowObservation() hookadapter.IngressObservation {
	return hookadapter.IngressObservation{
		Tool: instancepresence.ToolCodex, HookSessionRef: "session-1",
		Lifecycle: instancecorrelation.LifecycleActive, EffectiveState: instancepresence.StateWorking,
	}
}

// TestTryDeliverShadow_ConnectTimeoutIsBoundedAndDeterministic uses a Dial
// that respects ctx.Done() instead of a real, potentially timing-flaky Unix
// socket: it blocks until the context is cancelled (i.e. ConnectTimeout
// elapses) and then returns an error, exactly like a real dial to an
// unreachable/slow peer would eventually do, but deterministically and
// without touching the filesystem or network stack.
func TestTryDeliverShadow_ConnectTimeoutIsBoundedAndDeterministic(t *testing.T) {
	const connectTimeout = 20 * time.Millisecond
	blockingDial := func(ctx context.Context, socketPath string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	config := ShadowDeliveryConfig{ConnectTimeout: connectTimeout, WriteTimeout: connectTimeout, Dial: blockingDial}

	start := time.Now()
	delivered := TryDeliverShadowWithConfig(context.Background(), config, "/irrelevant.sock", validShadowObservation())
	elapsed := time.Since(start)

	if delivered {
		t.Fatal("expected delivery to fail when connect never completes")
	}
	if elapsed < connectTimeout {
		t.Fatalf("returned before ConnectTimeout elapsed: %v < %v", elapsed, connectTimeout)
	}
	// Generous bound (10x) so this never flakes under CI scheduling jitter,
	// while still proving the call did not hang indefinitely.
	if elapsed > 10*connectTimeout {
		t.Fatalf("took %v, expected close to ConnectTimeout %v — connect timeout is not being enforced", elapsed, connectTimeout)
	}
}

// pipeConnAdapter wraps one side of a net.Pipe as the net.Conn TryDeliverShadowWithConfig
// dials to. net.Pipe is a synchronous, unbuffered in-memory connection: a
// Write on one end blocks until a matching Read happens on the other end (or
// the write deadline fires) — exactly the "peer accepted the connection but
// never reads" scenario, reproduced deterministically with no real socket,
// listener, or wall-clock-dependent OS buffering involved.
func TestTryDeliverShadow_WriteTimeoutWhenPeerNeverReads(t *testing.T) {
	const writeTimeout = 20 * time.Millisecond
	clientEnd, _ := net.Pipe() // the "server" end is intentionally never read from.
	dial := func(ctx context.Context, socketPath string) (net.Conn, error) {
		return clientEnd, nil
	}
	config := ShadowDeliveryConfig{ConnectTimeout: time.Second, WriteTimeout: writeTimeout, Dial: dial}

	start := time.Now()
	delivered := TryDeliverShadowWithConfig(context.Background(), config, "/irrelevant.sock", validShadowObservation())
	elapsed := time.Since(start)

	if delivered {
		t.Fatal("expected delivery to fail when the peer never reads")
	}
	if elapsed < writeTimeout {
		t.Fatalf("returned before WriteTimeout elapsed: %v < %v", elapsed, writeTimeout)
	}
	if elapsed > 10*writeTimeout {
		t.Fatalf("took %v, expected close to WriteTimeout %v — write timeout is not being enforced", elapsed, writeTimeout)
	}
}

// TestTryDeliverShadow_SucceedsWhenPeerReads is the baseline: a peer that
// actually reads receives exactly the encoded observation, using the same
// net.Pipe mechanism (deterministic, no real socket).
func TestTryDeliverShadow_SucceedsWhenPeerReads(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()
	decoded := make(chan hookadapter.IngressObservation, 1)
	go func() {
		var observation hookadapter.IngressObservation
		if err := json.NewDecoder(bufio.NewReader(serverEnd)).Decode(&observation); err == nil {
			decoded <- observation
		}
	}()
	dial := func(ctx context.Context, socketPath string) (net.Conn, error) { return clientEnd, nil }
	config := ShadowDeliveryConfig{ConnectTimeout: time.Second, WriteTimeout: time.Second, Dial: dial}

	want := validShadowObservation()
	if !TryDeliverShadowWithConfig(context.Background(), config, "/irrelevant.sock", want) {
		t.Fatal("expected delivery to succeed when the peer reads")
	}
	select {
	case got := <-decoded:
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("peer never observed the write")
	}
}

func TestTryDeliverShadow_InvalidObservationFailsOpenWithoutDialing(t *testing.T) {
	dialed := false
	dial := func(ctx context.Context, socketPath string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("must not be called")
	}
	config := ShadowDeliveryConfig{Dial: dial}
	invalid := hookadapter.IngressObservation{} // empty Tool/HookSessionRef/Lifecycle: fails Validate.
	if TryDeliverShadowWithConfig(context.Background(), config, "/irrelevant.sock", invalid) {
		t.Fatal("expected an invalid observation to fail delivery")
	}
	if dialed {
		t.Fatal("an invalid observation must fail before ever dialing")
	}
}

func TestTryDeliverShadow_EmptySocketPathNeverDials(t *testing.T) {
	dialed := false
	dial := func(ctx context.Context, socketPath string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("must not be called")
	}
	config := ShadowDeliveryConfig{Dial: dial}
	if TryDeliverShadowWithConfig(context.Background(), config, "", validShadowObservation()) {
		t.Fatal("expected an empty socket path to fail delivery")
	}
	if dialed {
		t.Fatal("an empty socket path must fail before ever dialing")
	}
}

func TestShadowDeliveryConfig_WithDefaultsClampsAbsurdlyLargeTimeouts(t *testing.T) {
	config := ShadowDeliveryConfig{
		ConnectTimeout: time.Hour,
		WriteTimeout:   24 * time.Hour,
	}.withDefaults()
	if config.ConnectTimeout != MaxShadowConnectTimeout {
		t.Fatalf("ConnectTimeout = %v, want the hard cap %v", config.ConnectTimeout, MaxShadowConnectTimeout)
	}
	if config.WriteTimeout != MaxShadowWriteTimeout {
		t.Fatalf("WriteTimeout = %v, want the hard cap %v", config.WriteTimeout, MaxShadowWriteTimeout)
	}
}

func TestShadowDeliveryConfig_WithDefaultsPassesThroughReasonableValues(t *testing.T) {
	config := ShadowDeliveryConfig{
		ConnectTimeout: 50 * time.Millisecond,
		WriteTimeout:   75 * time.Millisecond,
	}.withDefaults()
	if config.ConnectTimeout != 50*time.Millisecond || config.WriteTimeout != 75*time.Millisecond {
		t.Fatalf("got %+v, want unmodified reasonable values", config)
	}
}

// TestTryDeliverShadow_AbsurdlyLargeConfiguredTimeoutStillHasHardBoundedLatency
// is the direct regression test for the G.4 ultrareview finding: a
// syntactically valid but absurdly large positive timeout (as could result
// from an operator env-var misconfiguration, e.g. minutes typed where
// milliseconds were expected) must never turn into minutes of real
// blocking in the caller's critical path — MaxShadowConnectTimeout /
// MaxShadowWriteTimeout must clamp it. Uses a Dial that blocks until
// ctx.Done() (deterministic, no real socket) so the *actual* enforced
// timeout — not the requested one — is what determines how long this
// takes.
func TestTryDeliverShadow_AbsurdlyLargeConfiguredTimeoutStillHasHardBoundedLatency(t *testing.T) {
	blockingDial := func(ctx context.Context, socketPath string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	config := ShadowDeliveryConfig{
		ConnectTimeout: 10 * time.Minute, // absurd, syntactically valid
		WriteTimeout:   10 * time.Minute,
		Dial:           blockingDial,
	}

	start := time.Now()
	if TryDeliverShadowWithConfig(context.Background(), config, "/irrelevant.sock", validShadowObservation()) {
		t.Fatal("expected delivery to fail")
	}
	elapsed := time.Since(start)
	maxAllowed := MaxShadowConnectTimeout + MaxShadowWriteTimeout
	if elapsed > 3*maxAllowed {
		t.Fatalf("took %v, expected close to the hard-capped budget %v regardless of the requested 10m timeout", elapsed, maxAllowed)
	}
}

// TestTryDeliverShadow_AlreadyCancelledParentContextFailsFastWithoutHanging
// verifies that an already-cancelled parent context (e.g. a Codex hook
// invocation whose own deadline has already elapsed by the time shadow
// delivery is attempted) fails immediately rather than producing any
// unexpected fallback behavior. TryDeliverShadowWithConfig checks
// ctx.Err() up front and never even attempts to dial in this case — a
// stronger guarantee than merely "the dial returns quickly", and one that
// can never regress into "ignore the caller's own deadline".
func TestTryDeliverShadow_AlreadyCancelledParentContextFailsFastWithoutHanging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before TryDeliverShadowWithConfig is ever called.

	dialAttempts := 0
	dial := func(dialCtx context.Context, socketPath string) (net.Conn, error) {
		dialAttempts++
		return nil, dialCtx.Err()
	}
	config := ShadowDeliveryConfig{ConnectTimeout: time.Second, WriteTimeout: time.Second, Dial: dial}

	start := time.Now()
	if TryDeliverShadowWithConfig(ctx, config, "/irrelevant.sock", validShadowObservation()) {
		t.Fatal("expected delivery to fail with an already-cancelled parent context")
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("took %v with an already-cancelled parent context, expected an immediate failure", elapsed)
	}
	if dialAttempts != 0 {
		t.Fatalf("expected zero dial attempts (fail-fast on ctx.Err() before dialing), got %d", dialAttempts)
	}
}

// TestTryDeliverShadow_ParentDeadlineWinsOverWriteTimeout is the direct
// regression test for the parent-context-deadline requirement: when ctx's
// own deadline is earlier than config.WriteTimeout would otherwise allow,
// the write stage must respect the earlier one, not silently extend past
// it.
func TestTryDeliverShadow_ParentDeadlineWinsOverWriteTimeout(t *testing.T) {
	const parentBudget = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), parentBudget)
	defer cancel()

	clientEnd, _ := net.Pipe() // the "server" end is intentionally never read from.
	dial := func(dialCtx context.Context, socketPath string) (net.Conn, error) {
		return clientEnd, nil
	}
	// WriteTimeout is deliberately much larger than the parent's remaining
	// budget: if the parent deadline did not win, this would block for
	// close to a full second instead of close to parentBudget.
	config := ShadowDeliveryConfig{ConnectTimeout: time.Second, WriteTimeout: time.Second, Dial: dial}

	start := time.Now()
	if TryDeliverShadowWithConfig(ctx, config, "/irrelevant.sock", validShadowObservation()) {
		t.Fatal("expected delivery to fail when the peer never reads")
	}
	elapsed := time.Since(start)
	if elapsed > 10*parentBudget {
		t.Fatalf("took %v, expected close to the parent context's %v deadline, not WriteTimeout's 1s", elapsed, parentBudget)
	}
}
