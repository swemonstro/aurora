package runtimepresence

import (
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

func TestRegistrySync0To1To2To1To0(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default", LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sync, err := NewRegistrySync(registry, "host-a", "epoch-runtime", instancepresence.SourceDescriptor{
		Provider: "linux-runtime", Profile: "default", CollectorID: "runtime-presence",
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	boot := instancepresence.BootIdentity("boot-a")
	startA := clock.now.Add(-time.Hour)
	startB := clock.now.Add(-2 * time.Hour)
	idA := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 100, StartedAt: startA})
	idB := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 200, StartedAt: startB})

	family := func(id instancepresence.InstanceID, pid uint64, start time.Time) runtimerecognition.Family {
		return runtimerecognition.Family{Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolClaude,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
		}}
	}

	// 0 → 1
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(idA, 100, startA)}}, boot); err != nil {
		t.Fatal(err)
	}
	if sync.KnownCount() != 1 {
		t.Fatalf("known = %d", sync.KnownCount())
	}
	if inst, err := registry.Get(idA); err != nil || inst.Status != instancepresence.RuntimeAlive {
		t.Fatalf("A = %#v err=%v", inst, err)
	}

	// 1 → 2
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idA, 100, startA), family(idB, 200, startB),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	if sync.KnownCount() != 2 {
		t.Fatalf("known = %d", sync.KnownCount())
	}

	// 2 → 1 (B disappears)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(idA, 100, startA)}}, boot); err != nil {
		t.Fatal(err)
	}
	if sync.KnownCount() != 1 {
		t.Fatalf("known = %d", sync.KnownCount())
	}
	if inst, err := registry.Get(idB); err != nil || inst.Status != instancepresence.RuntimeEnded {
		t.Fatalf("B should be ended: %#v err=%v", inst, err)
	}
	if inst, err := registry.Get(idA); err != nil || !inst.Status.Active() {
		t.Fatalf("A should remain: %#v err=%v", inst, err)
	}

	// 1 → 0
	if err := sync.ApplyRecognition(runtimerecognition.Result{}, boot); err != nil {
		t.Fatal(err)
	}
	if sync.KnownCount() != 0 {
		t.Fatalf("known = %d", sync.KnownCount())
	}
	if inst, err := registry.Get(idA); err != nil || inst.Status != instancepresence.RuntimeEnded {
		t.Fatalf("A should be ended: %#v err=%v", inst, err)
	}
}

func TestRegistrySyncSuspendedAndResume(t *testing.T) {
	// Agent-neutral path: both Claude and Codex share the same RegistrySync mapping.
	// Hook claims must be preserved identically through suspend/resume.
	for _, tool := range []instancepresence.ToolKind{instancepresence.ToolClaude, instancepresence.ToolCodex} {
		for _, claimCase := range []struct {
			name           string
			hookState      instancepresence.EffectiveState
			claim          instancepresence.HookClaim
			suspendedState instancepresence.EffectiveState
			resumedState   instancepresence.EffectiveState
		}{
			{
				name: "working", hookState: instancepresence.StateWorking, claim: instancepresence.ClaimWorking,
				suspendedState: instancepresence.StateAttention, resumedState: instancepresence.StateWorking,
			},
			{
				name: "attention", hookState: instancepresence.StateAttention, claim: instancepresence.ClaimAttention,
				suspendedState: instancepresence.StateAttention, resumedState: instancepresence.StateAttention,
			},
			{
				name: "error", hookState: instancepresence.StateError, claim: instancepresence.ClaimError,
				// Hook error outranks suspended attention.
				suspendedState: instancepresence.StateError, resumedState: instancepresence.StateError,
			},
		} {
			t.Run(string(tool)+"/"+claimCase.name, func(t *testing.T) {
				clock := &fakeClock{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
				registry, err := instanceregistry.New(instanceregistry.Config{
					Clock: clock, SlotNamespace: "default", LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
				})
				if err != nil {
					t.Fatal(err)
				}
				sync, err := NewRegistrySync(registry, "host-a", "epoch-runtime", instancepresence.SourceDescriptor{
					Provider: "linux-runtime", Profile: "default", CollectorID: "runtime-presence",
				}, clock.Now)
				if err != nil {
					t.Fatal(err)
				}
				boot := instancepresence.BootIdentity("boot-a")
				start := clock.now.Add(-time.Hour)
				pid := uint64(100)
				if tool == instancepresence.ToolCodex {
					pid = 200
				}
				id := runtimerecognition.StableInstanceID("host-a", boot, tool, instancepresence.ProcessIdentity{PID: pid, StartedAt: start})
				family := func(suspended bool) runtimerecognition.Family {
					return runtimerecognition.Family{
						Suspended: suspended,
						Candidate: instancepresence.RuntimeCandidate{
							InstanceID: id, Tool: tool,
							Runtime: instancepresence.RuntimeIdentity{
								HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
							},
							Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
						},
					}
				}

				if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(false)}}, boot); err != nil {
					t.Fatal(err)
				}
				if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", claimCase.hookState, clock.now, "hook-1"); err != nil {
					t.Fatal(err)
				}
				alive, err := registry.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				slot := alive.Slot
				if alive.HookClaim != claimCase.claim {
					t.Fatalf("pre-suspend claim = %q, want %q", alive.HookClaim, claimCase.claim)
				}

				clock.now = clock.now.Add(time.Second)
				if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(true)}}, boot); err != nil {
					t.Fatal(err)
				}
				suspended, err := registry.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				if suspended.Status != instancepresence.RuntimeSuspended || suspended.State != claimCase.suspendedState {
					t.Fatalf("suspended = %#v, want status suspended state %q", suspended, claimCase.suspendedState)
				}
				if suspended.HookClaim != claimCase.claim || suspended.Slot != slot {
					t.Fatalf("claim/slot lost under suspend: %#v", suspended)
				}

				clock.now = clock.now.Add(time.Second)
				if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(false)}}, boot); err != nil {
					t.Fatal(err)
				}
				resumed, err := registry.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				if resumed.Status != instancepresence.RuntimeAlive || resumed.State != claimCase.resumedState {
					t.Fatalf("resumed = %#v, want alive/%q", resumed, claimCase.resumedState)
				}
				if resumed.Slot != slot || resumed.HookClaim != claimCase.claim {
					t.Fatalf("slot/claim after resume: %#v", resumed)
				}

				clock.now = clock.now.Add(time.Second)
				if err := sync.ApplyRecognition(runtimerecognition.Result{}, boot); err != nil {
					t.Fatal(err)
				}
				ended, err := registry.Get(id)
				if err != nil || ended.Status != instancepresence.RuntimeEnded {
					t.Fatalf("ended = %#v err=%v", ended, err)
				}
			})
		}
	}
}

func TestRegistrySyncIndependentSuspendClaudeAndCodex(t *testing.T) {
	// Manual acceptance scenario: both agents active; suspend one leaves the other green.
	clock := &fakeClock{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default", LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sync, err := NewRegistrySync(registry, "host-a", "epoch-runtime", instancepresence.SourceDescriptor{
		Provider: "linux-runtime", Profile: "default", CollectorID: "runtime-presence",
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	boot := instancepresence.BootIdentity("boot-a")
	startClaude := clock.now.Add(-time.Hour)
	startCodex := clock.now.Add(-2 * time.Hour)
	idClaude := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 100, StartedAt: startClaude})
	idCodex := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 200, StartedAt: startCodex})
	family := func(id instancepresence.InstanceID, tool instancepresence.ToolKind, pid uint64, start time.Time, suspended bool) runtimerecognition.Family {
		return runtimerecognition.Family{
			Suspended: suspended,
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: tool,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
			},
		}
	}

	bothAlive := []runtimerecognition.Family{
		family(idClaude, instancepresence.ToolClaude, 100, startClaude, false),
		family(idCodex, instancepresence.ToolCodex, 200, startCodex, false),
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: bothAlive}, boot); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ApplyNextHookMutation(idClaude, "epoch-runtime", instancepresence.StateWorking, clock.now, "c-hook"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ApplyNextHookMutation(idCodex, "epoch-runtime", instancepresence.StateWorking, clock.now, "x-hook"); err != nil {
		t.Fatal(err)
	}

	// Ctrl+Z on Claude only.
	clock.now = clock.now.Add(time.Second)
	claudeStopped := []runtimerecognition.Family{
		family(idClaude, instancepresence.ToolClaude, 100, startClaude, true),
		family(idCodex, instancepresence.ToolCodex, 200, startCodex, false),
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: claudeStopped}, boot); err != nil {
		t.Fatal(err)
	}
	claude, err := registry.Get(idClaude)
	if err != nil {
		t.Fatal(err)
	}
	codex, err := registry.Get(idCodex)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Status != instancepresence.RuntimeSuspended || claude.State != instancepresence.StateAttention {
		t.Fatalf("Claude suspended = %#v", claude)
	}
	if claude.HookClaim != instancepresence.ClaimWorking {
		t.Fatalf("Claude claim lost: %q", claude.HookClaim)
	}
	if codex.Status != instancepresence.RuntimeAlive || codex.State != instancepresence.StateWorking {
		t.Fatalf("Codex must stay working: %#v", codex)
	}

	// fg on Claude.
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: bothAlive}, boot); err != nil {
		t.Fatal(err)
	}
	claude, _ = registry.Get(idClaude)
	codex, _ = registry.Get(idCodex)
	if claude.Status != instancepresence.RuntimeAlive || claude.State != instancepresence.StateWorking {
		t.Fatalf("Claude after fg = %#v", claude)
	}
	if codex.State != instancepresence.StateWorking {
		t.Fatalf("Codex after Claude fg = %#v", codex)
	}

	// Ctrl+Z on Codex only.
	clock.now = clock.now.Add(time.Second)
	codexStopped := []runtimerecognition.Family{
		family(idClaude, instancepresence.ToolClaude, 100, startClaude, false),
		family(idCodex, instancepresence.ToolCodex, 200, startCodex, true),
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: codexStopped}, boot); err != nil {
		t.Fatal(err)
	}
	claude, _ = registry.Get(idClaude)
	codex, _ = registry.Get(idCodex)
	if codex.Status != instancepresence.RuntimeSuspended || codex.State != instancepresence.StateAttention {
		t.Fatalf("Codex suspended = %#v", codex)
	}
	if codex.HookClaim != instancepresence.ClaimWorking {
		t.Fatalf("Codex claim lost: %q", codex.HookClaim)
	}
	if claude.Status != instancepresence.RuntimeAlive || claude.State != instancepresence.StateWorking {
		t.Fatalf("Claude must stay working: %#v", claude)
	}

	// fg on Codex — both green again; claims preserved.
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: bothAlive}, boot); err != nil {
		t.Fatal(err)
	}
	claude, _ = registry.Get(idClaude)
	codex, _ = registry.Get(idCodex)
	if claude.Status != instancepresence.RuntimeAlive || claude.State != instancepresence.StateWorking {
		t.Fatalf("Claude after Codex fg = %#v", claude)
	}
	if codex.Status != instancepresence.RuntimeAlive || codex.State != instancepresence.StateWorking {
		t.Fatalf("Codex after fg = %#v", codex)
	}
	if claude.HookClaim != instancepresence.ClaimWorking || codex.HookClaim != instancepresence.ClaimWorking {
		t.Fatalf("claims after dual resume: claude=%q codex=%q", claude.HookClaim, codex.HookClaim)
	}

	// Normal end of Claude only — Codex slot/LED remain.
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idCodex, instancepresence.ToolCodex, 200, startCodex, false),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	claude, err = registry.Get(idClaude)
	if err != nil || claude.Status != instancepresence.RuntimeEnded {
		t.Fatalf("Claude end = %#v err=%v", claude, err)
	}
	codex, err = registry.Get(idCodex)
	if err != nil || codex.Status != instancepresence.RuntimeAlive || codex.State != instancepresence.StateWorking {
		t.Fatalf("Codex after Claude end = %#v err=%v", codex, err)
	}

	// Normal end of Codex — no other active instance left.
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{}, boot); err != nil {
		t.Fatal(err)
	}
	codex, err = registry.Get(idCodex)
	if err != nil || codex.Status != instancepresence.RuntimeEnded {
		t.Fatalf("Codex end = %#v err=%v", codex, err)
	}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
