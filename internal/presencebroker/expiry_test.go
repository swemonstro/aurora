package presencebroker

import (
	"context"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestRunExpiryLoopEndsOnlyTheExpiredInstance(t *testing.T) {
	registry, clock := newIngestTestRegistry(t) // GracePeriod: 10s (lease span itself now comes from each message).
	ingestor := newTestIngestor(t, registry)
	now := clock.Now()
	expiresSession, renewsSession := ingestor.NewSession(), ingestor.NewSession()

	if _, err := expiresSession.Apply(ingestMessage("claude", "expires", "epoch-1", 1, "idle", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := renewsSession.Apply(ingestMessage("codex", "renews", "epoch-1", 1, "idle", now)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunExpiryLoop(ctx, registry, 5*time.Millisecond, nil)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// "renews" keeps sending; "expires" never does, so only it should end.
	// Each message's own lease span is 1 minute (see ingestMessage); 70s
	// plus the registry's GracePeriod (10s) is enough to fully end an
	// un-renewed instance.
	clock.Advance(70 * time.Second)
	if _, err := renewsSession.Apply(ingestMessage("codex", "renews", "epoch-1", 2, "idle", clock.Now())); err != nil {
		t.Fatal(err)
	}

	// Ended instances remain in the registry (Get still succeeds) but drop
	// out of Status/ActiveInstances; poll for that transition instead of
	// Get() returning an error, which it never does for an ended record.
	waitForCondition(t, time.Second, func() bool {
		inst, err := registry.Get("expires")
		return err == nil && inst.Status == instancepresence.RuntimeEnded
	})
	renewed, err := registry.Get("renews")
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Status == instancepresence.RuntimeEnded {
		t.Fatal("renewed instance was ended by the expiry loop")
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not met before timeout")
	}
}
