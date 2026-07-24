package runtimepresence

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/codextrust"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
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

func TestClaudeStartupPendingAfterObserverBaseline(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
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
	baseline := sync.ObserverStartedAt()

	// Pre-existing Claude (started before observer): idle, not startup-pending.
	preStart := baseline.Add(-time.Hour)
	idPre := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 100, StartedAt: preStart})
	// New Claude born after baseline: startup-pending attention.
	newStart := baseline.Add(time.Second)
	idNew := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 101, StartedAt: newStart})
	// New Codex after baseline: unchanged idle (not Claude).
	codexStart := baseline.Add(2 * time.Second)
	idCodex := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 200, StartedAt: codexStart})

	family := func(id instancepresence.InstanceID, tool instancepresence.ToolKind, pid uint64, start time.Time) runtimerecognition.Family {
		return runtimerecognition.Family{Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: tool,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
		}}
	}

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idPre, instancepresence.ToolClaude, 100, preStart),
		family(idNew, instancepresence.ToolClaude, 101, newStart),
		family(idCodex, instancepresence.ToolCodex, 200, codexStart),
	}}, boot); err != nil {
		t.Fatal(err)
	}

	pre, err := registry.Get(idPre)
	if err != nil {
		t.Fatal(err)
	}
	if pre.StartupPending || pre.State != instancepresence.StateIdle || pre.Revisions.HookRevision != 0 {
		t.Fatalf("pre-existing Claude = %#v, want idle non-pending", pre)
	}

	neu, err := registry.Get(idNew)
	if err != nil {
		t.Fatal(err)
	}
	if !neu.StartupPending || neu.State != instancepresence.StateAttention || neu.Revisions.HookRevision != 0 {
		t.Fatalf("new Claude = %#v, want startup attention", neu)
	}
	slotNew := neu.Slot

	codex, err := registry.Get(idCodex)
	if err != nil {
		t.Fatal(err)
	}
	if codex.StartupPending || codex.State != instancepresence.StateIdle {
		t.Fatalf("Codex = %#v, want idle non-pending", codex)
	}

	// Service-restart style: multiple pre-existing Claudes stay green.
	// (Already covered by pre; re-check second pre-existing via separate PID.)
	pre2Start := baseline.Add(-30 * time.Minute)
	idPre2 := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 102, StartedAt: pre2Start})
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idPre, instancepresence.ToolClaude, 100, preStart),
		family(idNew, instancepresence.ToolClaude, 101, newStart),
		family(idPre2, instancepresence.ToolClaude, 102, pre2Start),
		family(idCodex, instancepresence.ToolCodex, 200, codexStart),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	pre2, err := registry.Get(idPre2)
	if err != nil || pre2.StartupPending || pre2.State != instancepresence.StateIdle {
		t.Fatalf("second pre-existing = %#v err=%v", pre2, err)
	}

	// SessionStart for new Claude: same ID/slot, idle, positive hook rev, pending cleared.
	clock.now = clock.now.Add(time.Second)
	afterStart, err := registry.ApplyNextHookMutation(idNew, "epoch-runtime", instancepresence.StateIdle, clock.now, "session-start-new")
	if err != nil {
		t.Fatal(err)
	}
	if afterStart.ID != idNew || afterStart.Slot != slotNew {
		t.Fatalf("identity/slot changed: %#v", afterStart)
	}
	if afterStart.StartupPending || afterStart.State != instancepresence.StateIdle || afterStart.Revisions.HookRevision != 1 {
		t.Fatalf("after SessionStart = %#v", afterStart)
	}

	// Independent B: still startup-pending after A SessionStart.
	bStart := baseline.Add(3 * time.Second)
	idB := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 103, StartedAt: bStart})
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idNew, instancepresence.ToolClaude, 101, newStart),
		family(idB, instancepresence.ToolClaude, 103, bStart),
		family(idCodex, instancepresence.ToolCodex, 200, codexStart),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	b, err := registry.Get(idB)
	if err != nil || !b.StartupPending || b.State != instancepresence.StateAttention {
		t.Fatalf("B after A SessionStart = %#v err=%v", b, err)
	}
	a, err := registry.Get(idNew)
	if err != nil || a.State != instancepresence.StateIdle || a.StartupPending {
		t.Fatalf("A must stay idle: %#v err=%v", a, err)
	}

	// End B before SessionStart: instance ends.
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idNew, instancepresence.ToolClaude, 101, newStart),
		family(idCodex, instancepresence.ToolCodex, 200, codexStart),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	if ended, err := registry.Get(idB); err != nil || ended.Status != instancepresence.RuntimeEnded {
		t.Fatalf("B end = %#v err=%v", ended, err)
	}
}

func TestClaudeStartupPendingSuspendAndFirstHookWins(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)}
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
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 50, StartedAt: start})
	family := func(suspended bool) runtimerecognition.Family {
		return runtimerecognition.Family{
			Suspended: suspended,
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolClaude,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 50, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: 50, StartedAt: start}},
			},
		}
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(false)}}, boot); err != nil {
		t.Fatal(err)
	}
	inst, _ := registry.Get(id)
	if !inst.StartupPending || inst.State != instancepresence.StateAttention {
		t.Fatalf("startup = %#v", inst)
	}

	// Suspend before SessionStart: still attention, pending preserved.
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(true)}}, boot); err != nil {
		t.Fatal(err)
	}
	suspended, _ := registry.Get(id)
	if !suspended.StartupPending || suspended.Status != instancepresence.RuntimeSuspended || suspended.State != instancepresence.StateAttention {
		t.Fatalf("suspended startup = %#v", suspended)
	}

	// Resume before hook: still startup attention.
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(false)}}, boot); err != nil {
		t.Fatal(err)
	}
	resumed, _ := registry.Get(id)
	if !resumed.StartupPending || resumed.State != instancepresence.StateAttention {
		t.Fatalf("resume before hook = %#v", resumed)
	}

	// UserPromptSubmit before SessionStart: working wins, pending cleared.
	clock.now = clock.now.Add(time.Second)
	working, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateWorking, clock.now, "prompt-1")
	if err != nil || working.State != instancepresence.StateWorking || working.StartupPending {
		t.Fatalf("first hook working = %#v err=%v", working, err)
	}

	// SessionStart then suspend/resume: idle -> attention -> idle.
	clock.now = clock.now.Add(time.Second)
	// Start a fresh startup-pending instance for post-SessionStart suspend path.
	start2 := sync.ObserverStartedAt().Add(10 * time.Second)
	id2 := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 51, StartedAt: start2})
	family2 := func(suspended bool) runtimerecognition.Family {
		return runtimerecognition.Family{
			Suspended: suspended,
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id2, Tool: instancepresence.ToolClaude,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 51, StartedAt: start2},
				},
				Members: []instancepresence.ProcessIdentity{{PID: 51, StartedAt: start2}},
			},
		}
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(false), family2(false),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id2, "epoch-runtime", instancepresence.StateIdle, clock.now, "session-start-2"); err != nil {
		t.Fatal(err)
	}
	idle, _ := registry.Get(id2)
	if idle.State != instancepresence.StateIdle || idle.StartupPending {
		t.Fatalf("after SessionStart = %#v", idle)
	}
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(false), family2(true),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	susp, _ := registry.Get(id2)
	if susp.State != instancepresence.StateAttention || susp.Status != instancepresence.RuntimeSuspended {
		t.Fatalf("suspend after SessionStart = %#v", susp)
	}
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(false), family2(false),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	back, _ := registry.Get(id2)
	if back.State != instancepresence.StateIdle || back.StartupPending {
		t.Fatalf("resume after SessionStart = %#v", back)
	}
}

func TestClaudePIDReuseDoesNotInheritStartupPending(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)}
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
	start1 := sync.ObserverStartedAt().Add(time.Second)
	id1 := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 77, StartedAt: start1})
	familyAt := func(id instancepresence.InstanceID, start time.Time) runtimerecognition.Family {
		return runtimerecognition.Family{Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolClaude,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 77, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 77, StartedAt: start}},
		}}
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{familyAt(id1, start1)}}, boot); err != nil {
		t.Fatal(err)
	}
	first, _ := registry.Get(id1)
	if !first.StartupPending {
		t.Fatal("first generation should be startup-pending")
	}
	// PID reuse: same PID, new StartedAt → new InstanceID; first ends.
	clock.now = clock.now.Add(time.Minute)
	start2 := sync.ObserverStartedAt().Add(2 * time.Minute)
	id2 := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 77, StartedAt: start2})
	if id1 == id2 {
		t.Fatal("PID reuse must produce a new instance ID")
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{familyAt(id2, start2)}}, boot); err != nil {
		t.Fatal(err)
	}
	ended, err := registry.Get(id1)
	if err != nil || ended.Status != instancepresence.RuntimeEnded || ended.StartupPending {
		t.Fatalf("old generation = %#v err=%v", ended, err)
	}
	second, err := registry.Get(id2)
	if err != nil || !second.StartupPending || second.State != instancepresence.StateAttention {
		t.Fatalf("new generation = %#v err=%v", second, err)
	}
}

func TestClaudeStartupPendingHelper(t *testing.T) {
	baseline := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	if claudeStartupPending(instancepresence.ToolCodex, baseline.Add(time.Second), baseline) {
		t.Fatal("claudeStartupPending helper is Claude-only")
	}
	if claudeStartupPending(instancepresence.ToolClaude, baseline.Add(-time.Second), baseline) {
		t.Fatal("pre-baseline Claude must not be pending")
	}
	if claudeStartupPending(instancepresence.ToolClaude, baseline, baseline) {
		t.Fatal("equal start time must not be pending (strict After)")
	}
	if !claudeStartupPending(instancepresence.ToolClaude, baseline.Add(time.Nanosecond), baseline) {
		t.Fatal("post-baseline Claude must be pending")
	}
}

func TestCodexTrustStartupPending(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 16, 0, 0, 0, time.UTC)}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default", LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	userHome := t.TempDir()
	trustHomeA := filepath.Join(t.TempDir(), "codex-a")
	trustHomeB := filepath.Join(t.TempDir(), "codex-b")
	_ = os.MkdirAll(trustHomeA, 0o700)
	_ = os.MkdirAll(trustHomeB, 0o700)
	projectA := filepath.Join(t.TempDir(), "proj-a")
	projectB := filepath.Join(t.TempDir(), "proj-b")
	_ = os.MkdirAll(projectA, 0o700)
	_ = os.MkdirAll(projectB, 0o700)

	sync, err := NewRegistrySync(registry, "host-a", "epoch-runtime", instancepresence.SourceDescriptor{
		Provider: "linux-runtime", Profile: "default", CollectorID: "runtime-presence",
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	sync.userHome = func() (string, error) { return userHome, nil }

	baseline := sync.ObserverStartedAt()
	boot := instancepresence.BootIdentity("boot-a")

	// Pre-baseline interactive Codex without project trust → idle (service restart).
	preStart := baseline.Add(-time.Hour)
	idPre := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 300, StartedAt: preStart})
	// New interactive, missing project → pending attention.
	newStart := baseline.Add(time.Second)
	idNew := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 301, StartedAt: newStart})
	// New interactive, already trusted → idle.
	trustedStart := baseline.Add(2 * time.Second)
	idTrusted := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 302, StartedAt: trustedStart})
	projectTrusted := filepath.Join(t.TempDir(), "already-trusted")
	_ = os.MkdirAll(projectTrusted, 0o700)
	writeCodexTrust(t, trustHomeA, projectTrusted, "trusted")
	// codex exec after baseline → never pending.
	execStart := baseline.Add(3 * time.Second)
	idExec := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 303, StartedAt: execStart})
	// Claude after baseline still pending (unchanged).
	claudeStart := baseline.Add(4 * time.Second)
	idClaude := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 304, StartedAt: claudeStart})

	codexFamily := func(id instancepresence.InstanceID, pid uint64, start time.Time, cwd, home string, argv []string, suspended bool) runtimerecognition.Family {
		return runtimerecognition.Family{
			Suspended: suspended,
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
			},
			WorkingDirectory: cwd,
			EnvCodexHome:     home,
			Argv:             argv,
		}
	}
	claudeFamily := func(id instancepresence.InstanceID, pid uint64, start time.Time) runtimerecognition.Family {
		return runtimerecognition.Family{Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolClaude,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
		}}
	}

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		codexFamily(idPre, 300, preStart, projectA, trustHomeA, []string{"codex"}, false),
		codexFamily(idNew, 301, newStart, projectA, trustHomeA, []string{"codex"}, false),
		codexFamily(idTrusted, 302, trustedStart, projectTrusted, trustHomeA, []string{"codex"}, false),
		codexFamily(idExec, 303, execStart, projectA, trustHomeA, []string{"codex", "exec", "ls"}, false),
		claudeFamily(idClaude, 304, claudeStart),
	}}, boot); err != nil {
		t.Fatal(err)
	}

	pre, _ := registry.Get(idPre)
	if pre.StartupPending || pre.State != instancepresence.StateIdle {
		t.Fatalf("pre-existing Codex = %#v", pre)
	}
	neu, _ := registry.Get(idNew)
	if !neu.StartupPending || neu.State != instancepresence.StateAttention || neu.Revisions.HookRevision != 0 {
		t.Fatalf("new untrusted Codex = %#v", neu)
	}
	slotNew := neu.Slot
	revNew := neu.Revisions.RuntimeRevision

	tr, _ := registry.Get(idTrusted)
	if tr.StartupPending || tr.State != instancepresence.StateIdle {
		t.Fatalf("already-trusted Codex = %#v", tr)
	}
	ex, _ := registry.Get(idExec)
	if ex.StartupPending || ex.State != instancepresence.StateIdle {
		t.Fatalf("codex exec = %#v", ex)
	}
	cl, _ := registry.Get(idClaude)
	if !cl.StartupPending || cl.State != instancepresence.StateAttention {
		t.Fatalf("Claude pending broken: %#v", cl)
	}

	// Trust becomes trusted for A: attention → idle, same ID/slot, hook_rev=0, runtime rev advances.
	writeCodexTrust(t, trustHomeA, projectA, "trusted")
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		codexFamily(idNew, 301, newStart, projectA, trustHomeA, []string{"codex"}, false),
		claudeFamily(idClaude, 304, claudeStart),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	after, err := registry.Get(idNew)
	if err != nil {
		t.Fatal(err)
	}
	if after.StartupPending || after.State != instancepresence.StateIdle || after.Revisions.HookRevision != 0 {
		t.Fatalf("after trust = %#v", after)
	}
	if after.Slot != slotNew || after.ID != idNew {
		t.Fatalf("slot/id changed: %#v", after)
	}
	if after.Revisions.RuntimeRevision <= revNew {
		t.Fatalf("runtime revision did not advance: %d -> %d", revNew, after.Revisions.RuntimeRevision)
	}

	// Removing project trust must not re-pending.
	if err := os.Remove(filepath.Join(trustHomeA, "config.toml")); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		codexFamily(idNew, 301, newStart, projectA, trustHomeA, []string{"codex"}, false),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	stable, _ := registry.Get(idNew)
	if stable.StartupPending || stable.State != instancepresence.StateIdle {
		t.Fatalf("pending re-activated: %#v", stable)
	}

	// Two profiles / cwds: B still pending while A is green.
	bStart := baseline.Add(5 * time.Second)
	idB := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 305, StartedAt: bStart})
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		codexFamily(idNew, 301, newStart, projectA, trustHomeA, []string{"codex"}, false),
		codexFamily(idB, 305, bStart, projectB, trustHomeB, []string{"codex"}, false),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	b, _ := registry.Get(idB)
	if !b.StartupPending || b.State != instancepresence.StateAttention {
		t.Fatalf("B pending = %#v", b)
	}
	a, _ := registry.Get(idNew)
	if a.StartupPending || a.State != instancepresence.StateIdle {
		t.Fatalf("A must stay idle: %#v", a)
	}
}

func TestCodexTrustUnknownAndParseFailures(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 17, 0, 0, 0, time.UTC)}
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
	// Force unknown at registration → no false attention.
	sync.projectTrust = func(string, string, string) codextrust.Status { return codextrust.Unknown }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 401, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 401, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 401, StartedAt: start}},
		},
		WorkingDirectory: filepath.Join(t.TempDir(), "p"),
		EnvCodexHome:     t.TempDir(),
		Argv:             []string{"codex"},
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	inst, _ := registry.Get(id)
	if inst.StartupPending || inst.State != instancepresence.StateIdle {
		t.Fatalf("unknown at register = %#v", inst)
	}

	// Pending preserved across temporary unknown during renew.
	// Register a pending instance via not_trusted first.
	sync2, err := NewRegistrySync(registry, "host-a", "epoch-runtime-2", instancepresence.SourceDescriptor{
		Provider: "linux-runtime", Profile: "default", CollectorID: "runtime-presence",
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	// Use separate registry for clean isolation.
	registry2, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default", LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sync2.registry = registry2
	sync2.projectTrust = func(string, string, string) codextrust.Status { return codextrust.NotTrusted }
	start2 := sync2.ObserverStartedAt().Add(time.Second)
	id2 := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 402, StartedAt: start2})
	family2 := family
	family2.Candidate.InstanceID = id2
	family2.Candidate.Runtime.RootProcess = instancepresence.ProcessIdentity{PID: 402, StartedAt: start2}
	family2.Candidate.Members = []instancepresence.ProcessIdentity{{PID: 402, StartedAt: start2}}
	if err := sync2.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family2}}, boot); err != nil {
		t.Fatal(err)
	}
	pending, _ := registry2.Get(id2)
	if !pending.StartupPending {
		t.Fatalf("expected pending: %#v", pending)
	}
	sync2.projectTrust = func(string, string, string) codextrust.Status { return codextrust.Unknown }
	clock.now = clock.now.Add(time.Second)
	if err := sync2.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family2}}, boot); err != nil {
		t.Fatal(err)
	}
	still, _ := registry2.Get(id2)
	if !still.StartupPending || still.State != instancepresence.StateAttention {
		t.Fatalf("unknown must preserve pending: %#v", still)
	}
}

func TestCodexTrustSuspendAndHookPriority(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC)}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default", LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "p")
	_ = os.MkdirAll(project, 0o700)
	sync, err := NewRegistrySync(registry, "host-a", "epoch-runtime", instancepresence.SourceDescriptor{
		Provider: "linux-runtime", Profile: "default", CollectorID: "runtime-presence",
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 501, StartedAt: start})
	family := func(suspended bool) runtimerecognition.Family {
		return runtimerecognition.Family{
			Suspended: suspended,
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 501, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: 501, StartedAt: start}},
			},
			WorkingDirectory: project,
			EnvCodexHome:     home,
			Argv:             []string{"codex"},
		}
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(false)}}, boot); err != nil {
		t.Fatal(err)
	}
	// Suspend / resume before trust: attention + pending.
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(true)}}, boot)
	susp, _ := registry.Get(id)
	if !susp.StartupPending || susp.State != instancepresence.StateAttention {
		t.Fatalf("suspend before trust = %#v", susp)
	}
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family(false)}}, boot)
	res, _ := registry.Get(id)
	if !res.StartupPending || res.State != instancepresence.StateAttention {
		t.Fatalf("resume before trust = %#v", res)
	}
	// First hook wins before trust metadata.
	clock.now = clock.now.Add(time.Second)
	working, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateWorking, clock.now, "hook-1")
	if err != nil || working.State != instancepresence.StateWorking || working.StartupPending {
		t.Fatalf("hook wins = %#v err=%v", working, err)
	}
	// End process before trust on a fresh pending instance.
	start2 := sync.ObserverStartedAt().Add(10 * time.Second)
	id2 := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 502, StartedAt: start2})
	f2 := family(false)
	f2.Candidate.InstanceID = id2
	f2.Candidate.Runtime.RootProcess = instancepresence.ProcessIdentity{PID: 502, StartedAt: start2}
	f2.Candidate.Members = []instancepresence.ProcessIdentity{{PID: 502, StartedAt: start2}}
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{f2}}, boot)
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{}, boot)
	ended, err := registry.Get(id2)
	if err != nil || ended.Status != instancepresence.RuntimeEnded {
		t.Fatalf("end before trust = %#v err=%v", ended, err)
	}
}

func TestCodexShellNodeNativeEndToEndRecognizeToRegistry(t *testing.T) {
	// Full chain: Recognize(real Codex recognizer) → ApplyRecognition.
	// Does not manually set LaunchProcess / WorkingDirectory / Argv on Family.
	//
	// Production-shaped family: long-lived shell is the component root (pre-
	// baseline InstanceID), while the post-baseline Node launcher supplies
	// LaunchProcess metadata. The shell carries a Codex launch identity so it
	// is the family root under current recognition rules, but remains a
	// terminal shell name so LaunchProcess selection never picks it.
	clock := &fakeClock{now: time.Date(2026, 7, 27, 14, 0, 0, 0, time.UTC)}
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
	sync.projectTrust = func(_, projectDir, _ string) codextrust.Status {
		if projectDir == "/tmp/untrusted-project" {
			return codextrust.NotTrusted
		}
		return codextrust.Unknown
	}

	baseline := sync.ObserverStartedAt()
	boot := instancepresence.BootIdentity("boot-a")
	shellStart := baseline.Add(-3 * time.Hour)
	nodeStart := baseline.Add(time.Second)
	nativeStart := baseline.Add(2 * time.Second)

	shell := runtimerecognition.ProcessObservation{
		Process:       instancepresence.ProcessIdentity{PID: 100, StartedAt: shellStart},
		ParentPIDHint: 1,
		CommIdentity:  "exe:bash", ExecutableIdentity: "exe:bash",
		// Outer family member / component root (pre-baseline). Terminal name
		// ensures LaunchProcess selection skips it in favour of Node.
		LaunchIdentities:  []instancepresence.OpaqueIdentity{"launch:openai-codex"},
		ProcessGroupOrJob: "pgrp:shared", OSSession: "session:shared",
		OwnerIdentity:    "owner:fixture",
		WorkingDirectory: "/tmp/old-shell-cwd",
		Argv:             []string{"-bash"},
	}
	shellID := shell.Process
	wantID := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, shell.Process)

	node := runtimerecognition.ProcessObservation{
		Process: instancepresence.ProcessIdentity{PID: 200, StartedAt: nodeStart},
		Parent:  &shellID, ParentPIDHint: 100,
		CommIdentity: "exe:node", ExecutableIdentity: "exe:node",
		LaunchIdentities:  []instancepresence.OpaqueIdentity{"launch:openai-codex"},
		ProcessGroupOrJob: "pgrp:shared", OSSession: "session:shared",
		OwnerIdentity:    "owner:fixture",
		WorkingDirectory: "/tmp/untrusted-project",
		EnvCodexHome:     "/tmp/codex-home",
		Argv:             []string{"codex"},
	}
	nodeID := node.Process
	native := runtimerecognition.ProcessObservation{
		Process: instancepresence.ProcessIdentity{PID: 201, StartedAt: nativeStart},
		Parent:  &nodeID, ParentPIDHint: 200,
		CommIdentity: "exe:codex-linux-x86_64", ExecutableIdentity: "exe:codex-linux-x86_64",
		ProcessGroupOrJob: "pgrp:shared", OSSession: "session:shared",
		OwnerIdentity:    "owner:fixture",
		WorkingDirectory: "/tmp/native-cwd",
		EnvCodexHome:     "/tmp/native-home",
		Argv:             []string{"codex-linux-x86_64"},
	}

	result, err := runtimerecognition.Recognize(
		runtimerecognition.Snapshot{
			ObservedAt: baseline.Add(10 * time.Second),
			BootID:     boot,
			Processes:  []runtimerecognition.ProcessObservation{shell, node, native},
		},
		"host-a",
		codexhook.RuntimeRecognizer(),
	)
	if err != nil || len(result.Families) != 1 {
		t.Fatalf("Recognize = %#v err=%v", result, err)
	}
	family := result.Families[0]
	if family.Candidate.Tool != instancepresence.ToolCodex {
		t.Fatalf("tool = %q", family.Candidate.Tool)
	}
	if family.Candidate.Runtime.RootProcess != shell.Process {
		t.Fatalf("RootProcess = %#v, want shell %#v (PID 200 is not allowed)", family.Candidate.Runtime.RootProcess, shell.Process)
	}
	if family.Candidate.InstanceID != wantID {
		t.Fatalf("InstanceID = %q, want shell-based %q", family.Candidate.InstanceID, wantID)
	}
	if family.LaunchProcess != node.Process {
		t.Fatalf("LaunchProcess = %#v, want Node %#v", family.LaunchProcess, node.Process)
	}
	if family.WorkingDirectory != "/tmp/untrusted-project" || family.EnvCodexHome != "/tmp/codex-home" {
		t.Fatalf("metadata must come from Node: cwd=%q home=%q", family.WorkingDirectory, family.EnvCodexHome)
	}
	if len(family.Argv) == 0 || family.Argv[0] != "codex" {
		t.Fatalf("Argv from Node = %#v", family.Argv)
	}
	for _, token := range family.Argv {
		if token == "-bash" || token == "bash" {
			t.Fatalf("shell argv leaked: %#v", family.Argv)
		}
	}
	if !family.LaunchProcess.StartedAt.After(baseline) {
		t.Fatal("LaunchProcess must be post-baseline")
	}
	if !family.Candidate.Runtime.RootProcess.StartedAt.Before(baseline) {
		t.Fatal("RootProcess (shell) must be pre-baseline")
	}

	if err := sync.ApplyRecognition(result, boot); err != nil {
		t.Fatal(err)
	}
	inst, err := registry.Get(wantID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != wantID {
		t.Fatalf("registry ID = %q, want %q", inst.ID, wantID)
	}
	if inst.Runtime.RootProcess != shell.Process {
		t.Fatalf("registry RootProcess = %#v, want shell", inst.Runtime.RootProcess)
	}
	if !inst.StartupPending || inst.State != instancepresence.StateAttention || inst.Revisions.HookRevision != 0 {
		t.Fatalf("want startup attention from LaunchProcess generation: %#v", inst)
	}
	slot := inst.Slot

	// Trust accepted on the same recognition result (no manual Family rewrite).
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return codextrust.Trusted }
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(result, boot); err != nil {
		t.Fatal(err)
	}
	after, err := registry.Get(wantID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StartupPending || after.State != instancepresence.StateIdle || after.Revisions.HookRevision != 0 {
		t.Fatalf("after trust = %#v", after)
	}
	if after.ID != wantID || after.Slot != slot {
		t.Fatalf("id/slot changed: %#v", after)
	}
	if after.Runtime.RootProcess != shell.Process {
		t.Fatalf("RootProcess after trust = %#v", after.Runtime.RootProcess)
	}
}

func TestCodexOldShellNewNodeLauncherIsStartupPending(t *testing.T) {
	// Production regression: long-lived bash (pre-baseline) + post-baseline
	// Node codex launcher + native child. Trust NotTrusted. Must be attention
	// even when family RootProcess is the old shell.
	clock := &fakeClock{now: time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)}
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
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return codextrust.NotTrusted }

	baseline := sync.ObserverStartedAt()
	boot := instancepresence.BootIdentity("boot-a")
	shellStart := baseline.Add(-3 * time.Hour)
	nodeStart := baseline.Add(time.Second)
	nativeStart := baseline.Add(2 * time.Second)

	// RootProcess identity follows the shell (pre-baseline) for InstanceID stability.
	root := instancepresence.ProcessIdentity{PID: 100, StartedAt: shellStart}
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, root)
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: root,
			},
			Members: []instancepresence.ProcessIdentity{
				root,
				{PID: 200, StartedAt: nodeStart},
				{PID: 201, StartedAt: nativeStart},
			},
		},
		// LaunchProcess is the post-baseline Node launcher.
		LaunchProcess:    instancepresence.ProcessIdentity{PID: 200, StartedAt: nodeStart},
		WorkingDirectory: "/tmp/untrusted-project",
		EnvCodexHome:     "/tmp/codex-home",
		Argv:             []string{"codex"}, // from node, not shell -bash
	}

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	inst, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !inst.StartupPending || inst.State != instancepresence.StateAttention || inst.Revisions.HookRevision != 0 {
		t.Fatalf("want startup attention, got %#v", inst)
	}
	// Instance identity remains shell-rooted.
	if inst.Runtime.RootProcess.PID != 100 || !inst.Runtime.RootProcess.StartedAt.Equal(shellStart) {
		t.Fatalf("RootProcess identity changed: %#v", inst.Runtime.RootProcess)
	}
	slot := inst.Slot

	// Trust accepted: attention -> idle, same ID/slot, hook_rev stays 0.
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return codextrust.Trusted }
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	after, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.StartupPending || after.State != instancepresence.StateIdle || after.Revisions.HookRevision != 0 {
		t.Fatalf("after trust = %#v", after)
	}
	if after.Slot != slot || after.ID != id {
		t.Fatalf("id/slot changed: %#v", after)
	}
}

func TestCodexOldShellNewExecIsNotPending(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC)}
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
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return codextrust.NotTrusted }
	baseline := sync.ObserverStartedAt()
	boot := instancepresence.BootIdentity("boot-a")
	shellStart := baseline.Add(-time.Hour)
	nodeStart := baseline.Add(time.Second)
	root := instancepresence.ProcessIdentity{PID: 50, StartedAt: shellStart}
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, root)
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{HostID: "host-a", BootID: boot, RootProcess: root},
			Members: []instancepresence.ProcessIdentity{root, {PID: 51, StartedAt: nodeStart}},
		},
		LaunchProcess:    instancepresence.ProcessIdentity{PID: 51, StartedAt: nodeStart},
		WorkingDirectory: "/tmp/p",
		Argv:             []string{"codex", "exec"},
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	inst, _ := registry.Get(id)
	if inst.StartupPending || inst.State != instancepresence.StateIdle {
		t.Fatalf("exec must not be pending: %#v", inst)
	}
	if _, ok := sync.codexTrustStateFor(id); ok {
		t.Fatal("exec must not install trust tracking")
	}
}

func TestCodexPreBaselineShellAndLauncherIdle(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}
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
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return codextrust.NotTrusted }
	baseline := sync.ObserverStartedAt()
	boot := instancepresence.BootIdentity("boot-a")
	shellStart := baseline.Add(-2 * time.Hour)
	nodeStart := baseline.Add(-time.Hour) // launcher also pre-baseline (service restart)
	root := instancepresence.ProcessIdentity{PID: 60, StartedAt: shellStart}
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, root)
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{HostID: "host-a", BootID: boot, RootProcess: root},
			Members: []instancepresence.ProcessIdentity{root, {PID: 61, StartedAt: nodeStart}},
		},
		LaunchProcess:    instancepresence.ProcessIdentity{PID: 61, StartedAt: nodeStart},
		WorkingDirectory: "/tmp/p",
		Argv:             []string{"codex"},
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	inst, _ := registry.Get(id)
	if inst.StartupPending || inst.State != instancepresence.StateIdle {
		t.Fatalf("pre-baseline launcher must be idle: %#v", inst)
	}
}

func TestCodexTwoOldShellsIndependentLaunchPending(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 27, 13, 0, 0, 0, time.UTC)}
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
	sync.projectTrust = func(_, projectDir, _ string) codextrust.Status {
		if projectDir == "/tmp/untrusted" {
			return codextrust.NotTrusted
		}
		return codextrust.Trusted
	}
	baseline := sync.ObserverStartedAt()
	boot := instancepresence.BootIdentity("boot-a")
	shellStart := baseline.Add(-time.Hour)
	nodeA := baseline.Add(time.Second)
	nodeB := baseline.Add(2 * time.Second)
	rootA := instancepresence.ProcessIdentity{PID: 70, StartedAt: shellStart}
	rootB := instancepresence.ProcessIdentity{PID: 71, StartedAt: shellStart.Add(time.Minute)}
	idA := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, rootA)
	idB := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, rootB)
	family := func(id instancepresence.InstanceID, root, launch instancepresence.ProcessIdentity, cwd string) runtimerecognition.Family {
		return runtimerecognition.Family{
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{HostID: "host-a", BootID: boot, RootProcess: root},
				Members: []instancepresence.ProcessIdentity{root, launch},
			},
			LaunchProcess:    launch,
			WorkingDirectory: cwd,
			Argv:             []string{"codex"},
		}
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idA, rootA, instancepresence.ProcessIdentity{PID: 80, StartedAt: nodeA}, "/tmp/untrusted"),
		family(idB, rootB, instancepresence.ProcessIdentity{PID: 81, StartedAt: nodeB}, "/tmp/trusted"),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	a, _ := registry.Get(idA)
	b, _ := registry.Get(idB)
	if !a.StartupPending || a.State != instancepresence.StateAttention {
		t.Fatalf("untrusted A = %#v", a)
	}
	if b.StartupPending || b.State != instancepresence.StateIdle {
		t.Fatalf("trusted B = %#v", b)
	}
}

func TestCodexUnknownThenNotTrustedBecomesPending(t *testing.T) {
	// Production regression: empty WorkingDirectory on first poll → Unknown →
	// idle; later poll with cwd → NotTrusted → attention without hook revision.
	clock := &fakeClock{now: time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)}
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
	trustByDir := map[string]codextrust.Status{"": codextrust.Unknown}
	sync.projectTrust = func(_, projectDir, _ string) codextrust.Status {
		if status, ok := trustByDir[projectDir]; ok {
			return status
		}
		return codextrust.Unknown
	}
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 900, StartedAt: start})
	family := func(cwd string) runtimerecognition.Family {
		return runtimerecognition.Family{
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 900, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: 900, StartedAt: start}},
			},
			WorkingDirectory: cwd,
			Argv:             []string{"codex"},
		}
	}

	// Poll 1: empty cwd → Unknown → idle, not pending, tracked unresolved.
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("")}}, boot); err != nil {
		t.Fatal(err)
	}
	first, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if first.StartupPending || first.State != instancepresence.StateIdle || first.Revisions.HookRevision != 0 {
		t.Fatalf("first poll = %#v", first)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustUnresolved {
		t.Fatalf("trust state = %v ok=%t, want unresolved", state, ok)
	}
	slot := first.Slot
	rev1 := first.Revisions.RuntimeRevision

	// Poll 2: valid cwd → NotTrusted → attention + pending; same ID/slot; hook_rev=0.
	trustByDir["/tmp/project"] = codextrust.NotTrusted
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("/tmp/project")}}, boot); err != nil {
		t.Fatal(err)
	}
	second, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !second.StartupPending || second.State != instancepresence.StateAttention || second.Revisions.HookRevision != 0 {
		t.Fatalf("second poll = %#v", second)
	}
	if second.ID != id || second.Slot != slot {
		t.Fatalf("id/slot changed: %#v", second)
	}
	if second.Revisions.RuntimeRevision <= rev1 {
		t.Fatalf("runtime revision did not advance: %d -> %d", rev1, second.Revisions.RuntimeRevision)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustPending {
		t.Fatalf("trust state after NotTrusted = %v ok=%t", state, ok)
	}
}

func TestCodexUnknownNotTrustedTrustedPath(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC)}
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
	statusSeq := []codextrust.Status{codextrust.Unknown, codextrust.NotTrusted, codextrust.Trusted}
	call := 0
	sync.projectTrust = func(_, _, _ string) codextrust.Status {
		if call >= len(statusSeq) {
			return statusSeq[len(statusSeq)-1]
		}
		s := statusSeq[call]
		call++
		return s
	}
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 901, StartedAt: start})
	family := func(cwd string) runtimerecognition.Family {
		return runtimerecognition.Family{
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 901, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: 901, StartedAt: start}},
			},
			WorkingDirectory: cwd,
			Argv:             []string{"codex"},
		}
	}
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("")}}, boot)
	a, _ := registry.Get(id)
	if a.StartupPending || a.State != instancepresence.StateIdle || a.Revisions.HookRevision != 0 {
		t.Fatalf("unknown = %#v", a)
	}
	slot := a.Slot
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("/tmp/p")}}, boot)
	b, _ := registry.Get(id)
	if !b.StartupPending || b.State != instancepresence.StateAttention || b.Revisions.HookRevision != 0 || b.Slot != slot {
		t.Fatalf("not trusted = %#v", b)
	}
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("/tmp/p")}}, boot)
	c, _ := registry.Get(id)
	if c.StartupPending || c.State != instancepresence.StateIdle || c.Revisions.HookRevision != 0 || c.Slot != slot {
		t.Fatalf("trusted = %#v", c)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustResolved {
		t.Fatalf("resolved state = %v ok=%t", state, ok)
	}
}

func TestCodexUnknownTrustedThenNotTrustedStaysIdle(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)}
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
	status := codextrust.Unknown
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return status }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 902, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 902, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 902, StartedAt: start}},
		},
		WorkingDirectory: "/tmp/p",
		Argv:             []string{"codex"},
	}
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot)
	status = codextrust.Trusted
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot)
	// Project later disappears / becomes not trusted — must not re-pending.
	status = codextrust.NotTrusted
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot)
	got, _ := registry.Get(id)
	if got.StartupPending || got.State != instancepresence.StateIdle {
		t.Fatalf("after trusted then not_trusted = %#v", got)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustResolved {
		t.Fatalf("must stay resolved: %v ok=%t", state, ok)
	}
}

func TestCodexUnknownHookWorkingThenNotTrusted(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)}
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
	status := codextrust.Unknown
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return status }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 903, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 903, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 903, StartedAt: start}},
		},
		WorkingDirectory: "/tmp/p",
		Argv:             []string{"codex"},
	}
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot)
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateWorking, clock.now, "hook-1"); err != nil {
		t.Fatal(err)
	}
	status = codextrust.NotTrusted
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot)
	got, _ := registry.Get(id)
	if got.StartupPending || got.State != instancepresence.StateWorking || got.Revisions.HookRevision != 1 {
		t.Fatalf("hook must win: %#v", got)
	}
}

func TestCodexUnresolvedRemovedClearsMap(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC)}
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
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return codextrust.Unknown }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 904, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 904, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 904, StartedAt: start}},
		},
		Argv: []string{"codex"},
	}
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot)
	if _, ok := sync.codexTrustStateFor(id); !ok {
		t.Fatal("expected unresolved tracking")
	}
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{}, boot)
	ended, err := registry.Get(id)
	if err != nil || ended.Status != instancepresence.RuntimeEnded {
		t.Fatalf("ended = %#v err=%v", ended, err)
	}
	if _, ok := sync.codexTrustStateFor(id); ok {
		t.Fatal("map entry must be deleted when instance ends")
	}
}

func TestCodexTwoInstancesIndependentTrustRace(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 25, 15, 0, 0, 0, time.UTC)}
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
	// A starts Unknown then NotTrusted; B is Trusted from the start.
	sync.projectTrust = func(_, projectDir, _ string) codextrust.Status {
		switch projectDir {
		case "/tmp/a":
			return codextrust.NotTrusted
		case "/tmp/b":
			return codextrust.Trusted
		default:
			return codextrust.Unknown
		}
	}
	boot := instancepresence.BootIdentity("boot-a")
	startA := sync.ObserverStartedAt().Add(time.Second)
	startB := sync.ObserverStartedAt().Add(2 * time.Second)
	idA := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 910, StartedAt: startA})
	idB := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 911, StartedAt: startB})
	family := func(id instancepresence.InstanceID, pid uint64, start time.Time, cwd string) runtimerecognition.Family {
		return runtimerecognition.Family{
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
			},
			WorkingDirectory: cwd,
			Argv:             []string{"codex"},
		}
	}
	// A first poll unknown (empty cwd), B trusted.
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idA, 910, startA, ""),
		family(idB, 911, startB, "/tmp/b"),
	}}, boot)
	a1, _ := registry.Get(idA)
	b1, _ := registry.Get(idB)
	if a1.StartupPending || a1.State != instancepresence.StateIdle {
		t.Fatalf("A unknown = %#v", a1)
	}
	if b1.StartupPending || b1.State != instancepresence.StateIdle {
		t.Fatalf("B trusted = %#v", b1)
	}
	clock.now = clock.now.Add(time.Second)
	_ = sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idA, 910, startA, "/tmp/a"),
		family(idB, 911, startB, "/tmp/b"),
	}}, boot)
	a2, _ := registry.Get(idA)
	b2, _ := registry.Get(idB)
	if !a2.StartupPending || a2.State != instancepresence.StateAttention {
		t.Fatalf("A not trusted = %#v", a2)
	}
	if b2.StartupPending || b2.State != instancepresence.StateIdle {
		t.Fatalf("B must stay idle = %#v", b2)
	}
}

func TestCodexTrustMapNotUpdatedWhenRegistryMutationFails(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)}
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
	status := codextrust.Unknown
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return status }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 920, StartedAt: start})
	family := func(cwd string) runtimerecognition.Family {
		return runtimerecognition.Family{
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 920, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: 920, StartedAt: start}},
			},
			WorkingDirectory: cwd,
			Argv:             []string{"codex"},
		}
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("")}}, boot); err != nil {
		t.Fatal(err)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustUnresolved {
		t.Fatalf("want unresolved, got %v ok=%t", state, ok)
	}
	knownBefore := sync.known[id]

	// Advance registry revision ahead of RegistrySync.known so the next renew is stale.
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyRuntimeMutation(id, presencev2.RuntimeMutation{
		ProducerEpoch: "epoch-runtime", RuntimeRevision: 5, Status: instancepresence.RuntimeAlive,
		ObservedAt: clock.now, IdempotencyKey: "force-ahead",
	}); err != nil {
		t.Fatal(err)
	}

	status = codextrust.NotTrusted
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("/tmp/p")}}, boot); err == nil {
		t.Fatal("expected renew failure")
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustUnresolved {
		t.Fatalf("trust map must stay unresolved after failed set: %v ok=%t", state, ok)
	}
	if sync.known[id] != knownBefore {
		t.Fatalf("known advanced on failure: %d -> %d", knownBefore, sync.known[id])
	}
	got, _ := registry.Get(id)
	if got.StartupPending {
		t.Fatalf("StartupPending must stay false after failed set: %#v", got)
	}
}

func TestCodexPendingClearFailureKeepsPending(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 26, 11, 0, 0, 0, time.UTC)}
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
	status := codextrust.NotTrusted
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return status }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 921, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 921, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 921, StartedAt: start}},
		},
		WorkingDirectory: "/tmp/p",
		Argv:             []string{"codex"},
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	pending, _ := registry.Get(id)
	if !pending.StartupPending {
		t.Fatalf("want pending: %#v", pending)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustPending {
		t.Fatalf("map pending = %v ok=%t", state, ok)
	}
	knownBefore := sync.known[id]

	// Force registry revision ahead so clear renew fails as stale.
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyRuntimeMutation(id, presencev2.RuntimeMutation{
		ProducerEpoch: "epoch-runtime", RuntimeRevision: knownBefore + 5, Status: instancepresence.RuntimeAlive,
		ObservedAt: clock.now, IdempotencyKey: "force-ahead-clear",
	}); err != nil {
		t.Fatal(err)
	}
	// Setting pending was already true; force-ahead may have cleared state via leave path.
	// Re-get: ApplyRuntimeMutation leave does not clear StartupPending.
	still, _ := registry.Get(id)
	if !still.StartupPending {
		// force-ahead ordinary mutation leaves pending true
		t.Fatalf("pending should survive ordinary advance: %#v", still)
	}

	status = codextrust.Trusted
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err == nil {
		t.Fatal("expected clear renew failure")
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustPending {
		t.Fatalf("must stay pending map state, got %v ok=%t", state, ok)
	}
	if sync.known[id] != knownBefore {
		t.Fatalf("known advanced on clear failure: %d -> %d", knownBefore, sync.known[id])
	}
	got, _ := registry.Get(id)
	if !got.StartupPending {
		t.Fatalf("StartupPending must remain true: %#v", got)
	}
}

func TestCodexIdleHookBlocksPendingActivation(t *testing.T) {
	// HookRevision>0 with NoHookClaim (idle after SessionStart-style clear).
	clock := &fakeClock{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
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
	status := codextrust.Unknown
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return status }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 922, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 922, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 922, StartedAt: start}},
		},
		WorkingDirectory: "/tmp/p",
		Argv:             []string{"codex"},
	}
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateIdle, clock.now, "idle-hook"); err != nil {
		t.Fatal(err)
	}
	hooked, _ := registry.Get(id)
	if hooked.Revisions.HookRevision != 1 || hooked.HookClaim != instancepresence.NoHookClaim {
		t.Fatalf("want idle hook claim-less: %#v", hooked)
	}
	status = codextrust.NotTrusted
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	got, _ := registry.Get(id)
	if got.StartupPending || got.State != instancepresence.StateIdle || got.Revisions.HookRevision != 1 {
		t.Fatalf("idle hook must block pending: %#v", got)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustResolved {
		t.Fatalf("must resolve after hook: %v ok=%t", state, ok)
	}
}

func TestCodexRegisterRetryInstallsTrustTracking(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 26, 13, 0, 0, 0, time.UTC)}
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
	status := codextrust.Unknown
	sync.projectTrust = func(_, _, _ string) codextrust.Status { return status }
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 923, StartedAt: start})
	family := func(cwd string) runtimerecognition.Family {
		return runtimerecognition.Family{
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolCodex,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 923, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: 923, StartedAt: start}},
			},
			WorkingDirectory: cwd,
			Argv:             []string{"codex"},
		}
	}
	// Pre-register same identity with a different idempotency key so Register
	// returns identity conflict while Get still finds the instance (retry path).
	if _, err := registry.Register(instanceregistry.Registration{
		InstanceID: id, Tool: instancepresence.ToolCodex,
		Source: instancepresence.SourceDescriptor{Provider: "linux-runtime", Profile: "default", CollectorID: "runtime-presence"},
		Runtime: instancepresence.RuntimeIdentity{
			HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 923, StartedAt: start},
		},
		ProducerEpoch: "epoch-runtime", RuntimeRevision: 1, ObservedAt: clock.now,
		IdempotencyKey: "pre-existing-registration",
		Status:         instancepresence.RuntimeAlive,
	}); err != nil {
		t.Fatal(err)
	}
	// Sync does not know the instance yet; ApplyRecognition hits register-error retry.
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("")}}, boot); err != nil {
		t.Fatal(err)
	}
	if state, ok := sync.codexTrustStateFor(id); !ok || state != codexTrustUnresolved {
		t.Fatalf("retry must install unresolved tracking: %v ok=%t", state, ok)
	}
	status = codextrust.NotTrusted
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family("/tmp/p")}}, boot); err != nil {
		t.Fatal(err)
	}
	got, _ := registry.Get(id)
	if !got.StartupPending || got.State != instancepresence.StateAttention {
		t.Fatalf("after retry tracking NotTrusted = %#v", got)
	}
}

func writeCodexTrust(t *testing.T, home, project, trust string) {
	t.Helper()
	body := "[projects." + `"` + project + `"` + "]\ntrust_level = " + `"` + trust + `"` + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
