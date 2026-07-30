package runtimepresence

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/claudetrust"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/codextrust"
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
	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status {
		return claudetrust.ProjectMissing
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

func TestRegistrySyncClaudeTrust_PostBaselineMissingRegistersAttention(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 12, 30, 0, 0, time.UTC)}
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
	sync.userHome = func() (string, error) { return "/home/carl", nil }
	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status {
		return claudetrust.ProjectMissing
	}

	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 500, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolClaude,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 500, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 500, StartedAt: start}},
		},
		WorkingDirectory: "/home/carl/proj",
	}

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	inst, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !inst.StartupPending || inst.State != instancepresence.StateAttention || inst.Revisions.HookRevision != 0 {
		t.Fatalf("got %#v, want attention pending hook_rev=0", inst)
	}
}

func TestRegistrySyncClaudeTrust_RegisterPresentOrUnknownIsIdle(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 12, 45, 0, 0, time.UTC)}
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
	sync.userHome = func() (string, error) { return "/home/carl", nil }

	boot := instancepresence.BootIdentity("boot-a")
	startA := sync.ObserverStartedAt().Add(time.Second)
	idA := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 501, StartedAt: startA})
	startB := sync.ObserverStartedAt().Add(2 * time.Second)
	idB := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 502, StartedAt: startB})

	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status {
		if pid == 501 {
			return claudetrust.ProjectPresent
		}
		return claudetrust.Unknown
	}
	family := func(id instancepresence.InstanceID, pid uint64, start time.Time) runtimerecognition.Family {
		return runtimerecognition.Family{
			Candidate: instancepresence.RuntimeCandidate{
				InstanceID: id, Tool: instancepresence.ToolClaude,
				Runtime: instancepresence.RuntimeIdentity{
					HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
				},
				Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
			},
			WorkingDirectory: "/home/carl/proj",
		}
	}

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{
		family(idA, 501, startA),
		family(idB, 502, startB),
	}}, boot); err != nil {
		t.Fatal(err)
	}
	a, _ := registry.Get(idA)
	if a.StartupPending || a.State != instancepresence.StateIdle || a.Revisions.HookRevision != 0 {
		t.Fatalf("present got %#v want idle non-pending hook_rev=0", a)
	}
	b, _ := registry.Get(idB)
	if b.StartupPending || b.State != instancepresence.StateIdle || b.Revisions.HookRevision != 0 {
		t.Fatalf("unknown got %#v want idle non-pending hook_rev=0", b)
	}
}

func TestRegistrySyncClaudeTrust_UnknownToMissingSetsPending(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 13, 10, 0, 0, time.UTC)}
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
	sync.userHome = func() (string, error) { return "/home/carl", nil }

	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 503, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolClaude,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 503, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 503, StartedAt: start}},
		},
		WorkingDirectory: "/home/carl/proj",
	}

	state := claudetrust.Unknown
	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status { return state }

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	first, _ := registry.Get(id)
	if first.StartupPending || first.State != instancepresence.StateIdle {
		t.Fatalf("first got %#v want idle non-pending", first)
	}

	state = claudetrust.ProjectMissing
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	second, _ := registry.Get(id)
	if !second.StartupPending || second.State != instancepresence.StateAttention || second.Revisions.HookRevision != 0 {
		t.Fatalf("second got %#v want attention pending hook_rev=0", second)
	}
}

func TestRegistrySyncClaudeTrust_MissingToPresentClearsPendingPreservesSlot(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 13, 20, 0, 0, time.UTC)}
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
	sync.userHome = func() (string, error) { return "/home/carl", nil }

	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 504, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolClaude,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 504, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 504, StartedAt: start}},
		},
		WorkingDirectory: "/home/carl/proj",
	}

	state := claudetrust.ProjectMissing
	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status { return state }

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	first, _ := registry.Get(id)
	if !first.StartupPending || first.State != instancepresence.StateAttention {
		t.Fatalf("first got %#v want attention pending", first)
	}
	slot := first.Slot

	state = claudetrust.ProjectPresent
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	second, _ := registry.Get(id)
	if second.ID != id || second.Slot != slot || second.Revisions.HookRevision != 0 {
		t.Fatalf("id/slot/hook changed: %#v", second)
	}
	if second.StartupPending || second.State != instancepresence.StateIdle {
		t.Fatalf("cleared got %#v want idle non-pending", second)
	}
}

func TestRegistrySyncClaudeTrust_HookWinsNoReactivation(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 24, 13, 30, 0, 0, time.UTC)}
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
	sync.userHome = func() (string, error) { return "/home/carl", nil }

	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 505, StartedAt: start})
	family := runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolClaude,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 505, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: 505, StartedAt: start}},
		},
		WorkingDirectory: "/home/carl/proj",
	}

	state := claudetrust.ProjectPresent
	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status { return state }

	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	first, _ := registry.Get(id)
	if first.StartupPending || first.State != instancepresence.StateIdle {
		t.Fatalf("register got %#v want idle non-pending", first)
	}

	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateWorking, clock.now, "hook"); err != nil {
		t.Fatal(err)
	}

	state = claudetrust.ProjectMissing
	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	after, _ := registry.Get(id)
	if after.StartupPending {
		t.Fatalf("startup pending reactivated: %#v", after)
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
	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status {
		return claudetrust.ProjectMissing
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
	sync.claudeTrust = func(pid uint64, userHome, cwd string) claudetrust.Status {
		return claudetrust.ProjectMissing
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

// The tests below are the G.4 false-red regression suite for the monolith's
// registry path (RegistrySync). They replace an earlier generation of tests
// that asserted the removed behavior itself — a missing or untrusted Codex
// project trust entry used to set StartupPending (attention) before any hook
// was ever observed. That inference has been deleted (see
// startupAtRegister and codexhook.CodexStartupAttention); these tests assert
// its replacement contract instead, and — critically — assert the full
// published state sequence via repeated ApplyRecognition + registry.Get
// calls, not just a final snapshot, so a transient attention that resolved
// itself before a test's last read could never hide here.
//
// A real internal/codextrust config file is still written and read directly
// in some tests below, specifically to prove RegistrySync now ignores it
// completely for Codex startup purposes — not merely that no test ever
// asked it to run.

func newCodexFalseRedSync(t *testing.T) (*RegistrySync, *instanceregistry.Registry, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)}
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
	// Claude trust observation is out of scope for this suite; keep it a
	// no-op so only Codex behavior is under test.
	sync.claudeTrust = func(uint64, string, string) claudetrust.Status { return claudetrust.Unknown }
	return sync, registry, clock
}

func codexFalseRedFamily(id instancepresence.InstanceID, boot instancepresence.BootIdentity, pid uint64, start time.Time, cwd, codexHome string, argv []string) runtimerecognition.Family {
	return runtimerecognition.Family{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: id, Tool: instancepresence.ToolCodex,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
			},
			Members: []instancepresence.ProcessIdentity{{PID: pid, StartedAt: start}},
		},
		WorkingDirectory: cwd,
		EnvCodexHome:     codexHome,
		Argv:             argv,
	}
}

func codexRuntimeProcess(pid uint64, start time.Time, argv []string) runtimerecognition.ProcessObservation {
	return runtimerecognition.ProcessObservation{
		Process:            instancepresence.ProcessIdentity{PID: pid, StartedAt: start},
		ParentPIDHint:      1,
		CommIdentity:       "exe:codex",
		ExecutableIdentity: "exe:codex",
		ProcessGroupOrJob:  "pgrp:codex-test",
		OSSession:          "session:codex-test",
		OwnerIdentity:      "uid:1000",
		WorkingDirectory:   "/tmp/codex-project",
		EnvCodexHome:       "/tmp/codex-home",
		Argv:               argv,
	}
}

// assertNeverAttention fails the test if any state in the sequence is
// attention, reporting the full sequence for diagnosis.
func assertNeverAttention(t *testing.T, label string, sequence []instancepresence.EffectiveState) {
	t.Helper()
	for index, state := range sequence {
		if state == instancepresence.StateAttention {
			t.Fatalf("%s: attention observed at step %d of published sequence %v", label, index, sequence)
		}
	}
}

// TestFalseRedA_MissingProjectTrustEntryNeverAttention is scenario A: no
// project trust entry exists at all (CODEX_HOME points at an empty,
// freshly-created directory with no config.toml), and no hook is ever
// observed. The full published sequence across several polls must never
// contain attention, and the final state is idle.
func TestFalseRedA_MissingProjectTrustEntryNeverAttention(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	boot := instancepresence.BootIdentity("boot-a")
	codexHome := t.TempDir() // no config.toml written: a genuinely missing trust entry.
	project := t.TempDir()
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 900, StartedAt: start})
	family := codexFalseRedFamily(id, boot, 900, start, project, codexHome, []string{"codex"})

	// Sanity: codextrust itself, asked directly, really does report the
	// missing entry as NotTrusted (existing, unchanged codextrust
	// behavior) — proving this test's fixture is a real missing-entry case,
	// not an accidental Trusted/Unknown one.
	if status := codextrust.ProjectTrust(codexHome, project, ""); status != codextrust.NotTrusted {
		t.Fatalf("fixture sanity: expected NotTrusted for a missing project entry, got %v", status)
	}

	var sequence []instancepresence.EffectiveState
	for poll := 0; poll < 3; poll++ {
		clock.now = clock.now.Add(time.Second)
		if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
			t.Fatal(err)
		}
		inst, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if inst.StartupPending {
			t.Fatalf("poll %d: StartupPending must never be set from a missing trust entry alone: %#v", poll, inst)
		}
		sequence = append(sequence, inst.State)
	}
	assertNeverAttention(t, "scenario A", sequence)
	if sequence[len(sequence)-1] != instancepresence.StateIdle {
		t.Fatalf("scenario A final state = %v, want idle (sequence %v)", sequence[len(sequence)-1], sequence)
	}
}

func TestRegistrySyncCodexAppServerDoesNotCreateSecondInstance(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	interactive := codexRuntimeProcess(910, start, []string{"codex", "fix the tests"})

	result, err := runtimerecognition.Recognize(
		runtimerecognition.Snapshot{
			ObservedAt: clock.Now(),
			BootID:     boot,
			Processes:  []runtimerecognition.ProcessObservation{interactive},
		},
		"host-a",
		codexhook.RuntimeRecognizer(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Families) != 1 {
		t.Fatalf("initial recognition = %#v", result)
	}
	if err := sync.ApplyRecognition(result, boot); err != nil {
		t.Fatal(err)
	}
	originalID := result.Families[0].Candidate.InstanceID
	original, err := registry.Get(originalID)
	if err != nil {
		t.Fatal(err)
	}

	clock.now = clock.now.Add(time.Second)
	appServer := codexRuntimeProcess(911, start.Add(time.Minute), []string{"codex", "app-server", "--stdio"})
	appServer.ProcessGroupOrJob = "pgrp:codex-app-server"
	appServer.OSSession = "session:codex-app-server"
	result, err = runtimerecognition.Recognize(
		runtimerecognition.Snapshot{
			ObservedAt: clock.Now(),
			BootID:     boot,
			Processes:  []runtimerecognition.ProcessObservation{interactive, appServer},
		},
		"host-a",
		codexhook.RuntimeRecognizer(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.ApplyRecognition(result, boot); err != nil {
		t.Fatal(err)
	}

	presentation, err := registry.Presentation(5)
	if err != nil {
		t.Fatal(err)
	}
	if presentation.ActiveCount != 1 || presentation.VisibleCount != 1 {
		t.Fatalf("presentation = %#v, want only established Codex active", presentation)
	}
	current, err := registry.Get(originalID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != original.ID || current.Slot.Index != original.Slot.Index {
		t.Fatalf("established Codex moved: before=%#v after=%#v", original, current)
	}
}

// TestFalseRedB_UntrustedProjectEntryNeverAttention is scenario B: an actual
// config.toml exists and explicitly marks the project untrusted, and no
// hook is ever observed. Same contract as A: never attention, final idle —
// proving RegistrySync ignores this real, on-disk untrusted signal
// completely, not merely that it happens to lack one.
func TestFalseRedB_UntrustedProjectEntryNeverAttention(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	boot := instancepresence.BootIdentity("boot-a")
	codexHome := t.TempDir()
	project := t.TempDir()
	body := "[projects." + `"` + project + `"` + "]\ntrust_level = " + `"` + "untrusted" + `"` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sanity: codextrust itself confirms this fixture really is untrusted.
	if status := codextrust.ProjectTrust(codexHome, project, ""); status != codextrust.NotTrusted {
		t.Fatalf("fixture sanity: expected NotTrusted for an explicitly untrusted entry, got %v", status)
	}

	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 901, StartedAt: start})
	family := codexFalseRedFamily(id, boot, 901, start, project, codexHome, []string{"codex"})

	var sequence []instancepresence.EffectiveState
	for poll := 0; poll < 3; poll++ {
		clock.now = clock.now.Add(time.Second)
		if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
			t.Fatal(err)
		}
		inst, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if inst.StartupPending {
			t.Fatalf("poll %d: StartupPending must never be set from an untrusted entry alone: %#v", poll, inst)
		}
		sequence = append(sequence, inst.State)
	}
	assertNeverAttention(t, "scenario B", sequence)
	if sequence[len(sequence)-1] != instancepresence.StateIdle {
		t.Fatalf("scenario B final state = %v, want idle (sequence %v)", sequence[len(sequence)-1], sequence)
	}
}

// TestFalseRedC_StartsWorkingDirectlyNeverTransientAttention is scenario C:
// Codex begins an active turn immediately (the first hook ever observed for
// this instance is a working-claim event), with an untrusted project entry
// present. The first active published state must be working, never
// attention, at any point in the sequence.
func TestFalseRedC_StartsWorkingDirectlyNeverTransientAttention(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	boot := instancepresence.BootIdentity("boot-a")
	codexHome := t.TempDir()
	project := t.TempDir()
	body := "[projects." + `"` + project + `"` + "]\ntrust_level = " + `"` + "untrusted" + `"` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 902, StartedAt: start})
	family := codexFalseRedFamily(id, boot, 902, start, project, codexHome, []string{"codex"})

	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	registered, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var sequence []instancepresence.EffectiveState
	sequence = append(sequence, registered.State)

	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateWorking, clock.now, "hook-1"); err != nil {
		t.Fatal(err)
	}
	working, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	sequence = append(sequence, working.State)

	assertNeverAttention(t, "scenario C", sequence)
	if sequence[0] != instancepresence.StateIdle {
		t.Fatalf("scenario C pre-hook state = %v, want idle", sequence[0])
	}
	if sequence[len(sequence)-1] != instancepresence.StateWorking {
		t.Fatalf("scenario C final state = %v, want working (sequence %v)", sequence[len(sequence)-1], sequence)
	}
}

// TestFalseRedD_PermissionRequestProducesAttention is scenario D: a real
// observed PermissionRequest hook event must still produce attention — the
// false-red fix removes a false signal, it must never suppress the true one.
func TestFalseRedD_PermissionRequestProducesAttention(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 903, StartedAt: start})
	family := codexFalseRedFamily(id, boot, 903, start, t.TempDir(), t.TempDir(), []string{"codex"})

	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateAttention, clock.now, "hook-permission"); err != nil {
		t.Fatal(err)
	}
	inst, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != instancepresence.StateAttention {
		t.Fatalf("scenario D: got state=%v, want attention", inst.State)
	}
}

// TestFalseRedE_PermissionRequestThenEscStopReturnsExactInstanceToIdle is
// scenario E: after a real PermissionRequest, an Esc/Stop/cancel-equivalent
// idle hook event must return exactly that instance to idle.
func TestFalseRedE_PermissionRequestThenEscStopReturnsExactInstanceToIdle(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	boot := instancepresence.BootIdentity("boot-a")
	start := sync.ObserverStartedAt().Add(time.Second)
	id := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 904, StartedAt: start})
	family := codexFalseRedFamily(id, boot, 904, start, t.TempDir(), t.TempDir(), []string{"codex"})

	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{family}}, boot); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateAttention, clock.now, "hook-permission"); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	if _, err := registry.ApplyNextHookMutation(id, "epoch-runtime", instancepresence.StateIdle, clock.now, "hook-esc-stop"); err != nil {
		t.Fatal(err)
	}
	inst, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != instancepresence.StateIdle {
		t.Fatalf("scenario E: got state=%v, want idle after Esc/Stop", inst.State)
	}
}

// TestFalseRedF_ParallelCodexInstanceUnaffected is scenario F: a second,
// parallel Codex instance (untrusted project entry, never hooked) must stay
// byte-for-byte unchanged (state, StartupPending, revision) while the first
// instance goes through the full D+E sequence.
func TestFalseRedF_ParallelCodexInstanceUnaffected(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	boot := instancepresence.BootIdentity("boot-a")
	startA := sync.ObserverStartedAt().Add(time.Second)
	startB := sync.ObserverStartedAt().Add(2 * time.Second)
	idA := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 905, StartedAt: startA})
	idB := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 906, StartedAt: startB})
	codexHomeB := t.TempDir()
	projectB := t.TempDir()
	body := "[projects." + `"` + projectB + `"` + "]\ntrust_level = " + `"` + "untrusted" + `"` + "\n"
	if err := os.WriteFile(filepath.Join(codexHomeB, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	familyA := codexFalseRedFamily(idA, boot, 905, startA, t.TempDir(), t.TempDir(), []string{"codex"})
	familyB := codexFalseRedFamily(idB, boot, 906, startB, projectB, codexHomeB, []string{"codex"})

	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{familyA, familyB}}, boot); err != nil {
		t.Fatal(err)
	}
	before, err := registry.Get(idB)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != instancepresence.StateIdle || before.StartupPending {
		t.Fatalf("B pre-condition = %#v, want idle/not-pending", before)
	}
	beforeRevision := before.Revisions.RuntimeRevision

	steps := []struct {
		state instancepresence.EffectiveState
		key   string
	}{
		{instancepresence.StateAttention, "hook-permission"},
		{instancepresence.StateIdle, "hook-esc-stop"},
	}
	for _, step := range steps {
		clock.now = clock.now.Add(time.Second)
		if _, err := registry.ApplyNextHookMutation(idA, "epoch-runtime", step.state, clock.now, step.key); err != nil {
			t.Fatal(err)
		}
		// Re-poll recognition too, so B's runtime lease also renews — proving
		// B is unaffected even while under active, repeated observation, not
		// merely untouched because nothing ever looked at it again.
		if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{familyA, familyB}}, boot); err != nil {
			t.Fatal(err)
		}
		afterB, err := registry.Get(idB)
		if err != nil {
			t.Fatal(err)
		}
		if afterB.State != instancepresence.StateIdle || afterB.StartupPending || afterB.Revisions.HookRevision != 0 {
			t.Fatalf("B mutated by A's transition to %v: %#v", step.state, afterB)
		}
		_ = beforeRevision // runtime revision is expected to advance via lease renewal; only hook-owned fields are asserted above.
	}
	finalA, err := registry.Get(idA)
	if err != nil {
		t.Fatal(err)
	}
	if finalA.State != instancepresence.StateIdle {
		t.Fatalf("A final state = %v, want idle", finalA.State)
	}
}

// TestFalseRedG_ClaudeAndGrokInstancesUnaffected is scenario G: registering
// Claude and Grok instances alongside a Codex false-red scenario must never
// change their behavior. Claude retains its own, unchanged, post-baseline
// missing-project startup-pending rule (out of scope for this fix); Grok has
// no startup-pending concept at all and must simply stay idle.
func TestFalseRedG_ClaudeAndGrokInstancesUnaffected(t *testing.T) {
	sync, registry, clock := newCodexFalseRedSync(t)
	sync.claudeTrust = func(uint64, string, string) claudetrust.Status { return claudetrust.ProjectPresent }
	boot := instancepresence.BootIdentity("boot-a")

	codexStart := sync.ObserverStartedAt().Add(time.Second)
	claudeStart := sync.ObserverStartedAt().Add(2 * time.Second)
	grokStart := sync.ObserverStartedAt().Add(3 * time.Second)
	idCodex := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolCodex, instancepresence.ProcessIdentity{PID: 907, StartedAt: codexStart})
	idClaude := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolClaude, instancepresence.ProcessIdentity{PID: 908, StartedAt: claudeStart})
	idGrok := runtimerecognition.StableInstanceID("host-a", boot, instancepresence.ToolGrok, instancepresence.ProcessIdentity{PID: 909, StartedAt: grokStart})

	codexHome := t.TempDir()
	project := t.TempDir()
	body := "[projects." + `"` + project + `"` + "]\ntrust_level = " + `"` + "untrusted" + `"` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	codexFamily := codexFalseRedFamily(idCodex, boot, 907, codexStart, project, codexHome, []string{"codex"})
	claudeFamily := runtimerecognition.Family{Candidate: instancepresence.RuntimeCandidate{
		InstanceID: idClaude, Tool: instancepresence.ToolClaude,
		Runtime: instancepresence.RuntimeIdentity{HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 908, StartedAt: claudeStart}},
		Members: []instancepresence.ProcessIdentity{{PID: 908, StartedAt: claudeStart}},
	}}
	grokFamily := runtimerecognition.Family{Candidate: instancepresence.RuntimeCandidate{
		InstanceID: idGrok, Tool: instancepresence.ToolGrok,
		Runtime: instancepresence.RuntimeIdentity{HostID: "host-a", BootID: boot, RootProcess: instancepresence.ProcessIdentity{PID: 909, StartedAt: grokStart}},
		Members: []instancepresence.ProcessIdentity{{PID: 909, StartedAt: grokStart}},
	}}

	clock.now = clock.now.Add(time.Second)
	if err := sync.ApplyRecognition(runtimerecognition.Result{Families: []runtimerecognition.Family{codexFamily, claudeFamily, grokFamily}}, boot); err != nil {
		t.Fatal(err)
	}

	codex, err := registry.Get(idCodex)
	if err != nil {
		t.Fatal(err)
	}
	if codex.StartupPending || codex.State != instancepresence.StateIdle {
		t.Fatalf("Codex = %#v, want idle/not-pending", codex)
	}
	claude, err := registry.Get(idClaude)
	if err != nil {
		t.Fatal(err)
	}
	if claude.StartupPending || claude.State != instancepresence.StateIdle {
		t.Fatalf("Claude with project present = %#v, want idle/not-pending (Claude's own rule is unchanged and out of scope)", claude)
	}
	grok, err := registry.Get(idGrok)
	if err != nil {
		t.Fatal(err)
	}
	if grok.StartupPending || grok.State != instancepresence.StateIdle {
		t.Fatalf("Grok = %#v, want idle/not-pending", grok)
	}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
