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

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
