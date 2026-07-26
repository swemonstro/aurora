package presencebroker

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

// TestLeaseExpiryOnlyAffectsTheExpiredInstance is the broker-core
// characterization test for lease/lifecycle isolation: three instances
// registered at the same instant, sharing the same lease deadline, must
// still expire independently once one of them renews. Ending claude-1 and
// grok-1 must not touch codex-1's tool identity, state, or revision, and
// codex-1 must not appear as expired or missing from the projection.
func TestLeaseExpiryOnlyAffectsTheExpiredInstance(t *testing.T) {
	registry, clock := testRegistry(t) // LeaseDuration: time.Minute, GracePeriod: 10s.
	t0 := clock.Now()
	registerTool(t, registry, "claude-1", instancepresence.ToolClaude, 100, t0)
	registerTool(t, registry, "codex-1", instancepresence.ToolCodex, 200, t0)
	registerTool(t, registry, "grok-1", instancepresence.ToolGrok, 300, t0)

	// Advance to the shared lease boundary: all three become SuspectMissing.
	clock.Advance(time.Minute)
	result, err := registry.ExpireLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SuspectMissing) != 3 || len(result.Ended) != 0 {
		t.Fatalf("expiry result = %#v, want all 3 suspect and none ended", result)
	}

	// Only codex-1 renews: fresh runtime revision extends its lease from now.
	if _, err := registry.ApplyRuntimeMutation("codex-1", presencev2.RuntimeMutation{
		ProducerEpoch: "epoch-a", RuntimeRevision: 2, Status: instancepresence.RuntimeAlive,
		ObservedAt: clock.Now(), IdempotencyKey: "renew-codex-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Advance past claude/grok's grace period (still well inside codex's
	// freshly extended lease): only claude-1 and grok-1 must end.
	clock.Advance(11 * time.Second)
	result, err = registry.ExpireLeases()
	if err != nil {
		t.Fatal(err)
	}
	endedSet := map[string]bool{}
	for _, id := range result.Ended {
		endedSet[string(id)] = true
	}
	if !endedSet["claude-1"] || !endedSet["grok-1"] || endedSet["codex-1"] {
		t.Fatalf("expiry result = %#v, want claude-1 and grok-1 ended, codex-1 untouched", result)
	}

	snapshot, err := registry.CanonicalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Instances) != 1 || snapshot.Instances[0].InstanceID != "codex-1" {
		t.Fatalf("active instances = %#v, want only codex-1", snapshot.Instances)
	}
	codex := snapshot.Instances[0]
	if codex.Tool != instancepresence.ToolCodex {
		t.Fatalf("codex tool identity changed: %#v", codex)
	}
	if codex.State != instancepresence.StateIdle {
		t.Fatalf("codex state changed by neighboring expiry: %#v", codex)
	}
	if codex.Revisions.RuntimeRevision != 2 {
		t.Fatalf("codex runtime revision = %d, want 2 (from its own renewal only)", codex.Revisions.RuntimeRevision)
	}
}
