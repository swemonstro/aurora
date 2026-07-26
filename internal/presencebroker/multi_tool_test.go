package presencebroker

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
)

// registerTool registers one instance under the given tool.
func registerTool(t *testing.T, registry *instanceregistry.Registry, id string, tool instancepresence.ToolKind, pid uint64, observed time.Time) {
	t.Helper()
	_, err := registry.Register(instanceregistry.Registration{
		InstanceID: instancepresence.InstanceID(id),
		Tool:       tool,
		Source:     instancepresence.SourceDescriptor{Provider: "linux-runtime", Profile: "default", CollectorID: "local-server"},
		Runtime: instancepresence.RuntimeIdentity{
			HostID: "host-a", BootID: "boot-a",
			RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: observed.Add(-time.Duration(pid) * time.Second)},
		},
		ProducerEpoch: "epoch-a", RuntimeRevision: 1, ObservedAt: observed,
		IdempotencyKey: "reg-" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMultiToolInstancesAreIndependent is the broker-core characterization
// test for G.2: the broker core (registry projection into canonical
// snapshot and v2 presentation) must treat Claude, Codex, and Grok
// instances as fully independent normalized instances. It never branches on
// Tool for behavior — Tool is passed through as data — so a mutation
// targeted at one instance must never change another instance's state,
// tool identity, or revision, and slot order must stay deterministic.
func TestMultiToolInstancesAreIndependent(t *testing.T) {
	registry, clock := testRegistry(t)
	now := clock.Now()
	registerTool(t, registry, "claude-1", instancepresence.ToolClaude, 100, now)
	registerTool(t, registry, "codex-1", instancepresence.ToolCodex, 200, now)
	registerTool(t, registry, "grok-1", instancepresence.ToolGrok, 300, now)

	// Mutate only the Codex instance.
	if _, err := registry.ApplyHookMutation("codex-1", presencev2.HookStateMutation{
		ProducerEpoch: "epoch-a", HookRevision: 1, State: instancepresence.StateWorking,
		ObservedAt: now.Add(time.Second), IdempotencyKey: "hook-codex-1",
	}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := registry.CanonicalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Instances) != 3 {
		t.Fatalf("instances = %d, want 3", len(snapshot.Instances))
	}

	byID := map[string]presencev2.Instance{}
	for _, inst := range snapshot.Instances {
		byID[string(inst.InstanceID)] = inst
	}
	claude, codex, grok := byID["claude-1"], byID["codex-1"], byID["grok-1"]

	if claude.Tool != instancepresence.ToolClaude || claude.State != instancepresence.StateIdle {
		t.Fatalf("claude instance mutated unexpectedly: %#v", claude)
	}
	if grok.Tool != instancepresence.ToolGrok || grok.State != instancepresence.StateIdle {
		t.Fatalf("grok instance mutated unexpectedly: %#v", grok)
	}
	if codex.Tool != instancepresence.ToolCodex || codex.State != instancepresence.StateWorking {
		t.Fatalf("codex instance did not receive its own mutation or lost tool identity: %#v", codex)
	}

	// Revisions are independent per instance: only codex's hook revision advanced.
	if claude.Revisions.HookRevision != 0 || grok.Revisions.HookRevision != 0 {
		t.Fatalf("unmutated instances gained a hook revision: claude=%d grok=%d",
			claude.Revisions.HookRevision, grok.Revisions.HookRevision)
	}
	if codex.Revisions.HookRevision != 1 {
		t.Fatalf("codex hook revision = %d, want 1", codex.Revisions.HookRevision)
	}
	if claude.Revisions.RuntimeRevision != 1 || codex.Revisions.RuntimeRevision != 1 || grok.Revisions.RuntimeRevision != 1 {
		t.Fatalf("runtime revisions changed unexpectedly: claude=%d codex=%d grok=%d",
			claude.Revisions.RuntimeRevision, codex.Revisions.RuntimeRevision, grok.Revisions.RuntimeRevision)
	}

	// Slot assignment is deterministic: registration order drives slot order.
	if claude.Slot.Index != 0 || codex.Slot.Index != 1 || grok.Slot.Index != 2 {
		t.Fatalf("slot assignment not deterministic: claude=%d codex=%d grok=%d",
			claude.Slot.Index, codex.Slot.Index, grok.Slot.Index)
	}

	// The presentation projection — what PresentationBridge actually
	// publishes — must reflect the same independent per-tool state.
	presentation, err := registry.Presentation(5)
	if err != nil {
		t.Fatal(err)
	}
	if presentation.ActiveCount != 3 || len(presentation.Pixels) != 3 || presentation.OverflowCount != 0 {
		t.Fatalf("presentation = %#v", presentation)
	}
	pixelByID := map[instancepresence.InstanceID]presencev2.Pixel{}
	for _, pixel := range presentation.Pixels {
		pixelByID[pixel.InstanceID] = pixel
	}
	if pixelByID["claude-1"].State != instancepresence.StateIdle ||
		pixelByID["grok-1"].State != instancepresence.StateIdle ||
		pixelByID["codex-1"].State != instancepresence.StateWorking {
		t.Fatalf("presentation pixels = %#v", presentation.Pixels)
	}
}
