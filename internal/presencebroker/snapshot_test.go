package presencebroker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencev2"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testRegistry(t *testing.T) (*instanceregistry.Registry, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default",
		LeaseDuration: time.Minute, GracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, clock
}

func registerRuntime(t *testing.T, registry *instanceregistry.Registry, id string, pid uint64, observed time.Time) {
	t.Helper()
	_, err := registry.Register(instanceregistry.Registration{
		InstanceID: instancepresence.InstanceID(id),
		Tool:       instancepresence.ToolClaude,
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

func TestSnapshotFileMode0600AndSleepingWhenEmpty(t *testing.T) {
	registry, _ := testRegistry(t)
	path := filepath.Join(t.TempDir(), "canonical.json")
	if err := WriteCanonicalSnapshotAtomic(path, registry); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	var snapshot presencev2.CanonicalSnapshot
	if err := json.Unmarshal(mustRead(t, path), &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if snapshot.Presence != presencev2.PresenceSleeping || len(snapshot.Instances) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Instances == nil {
		t.Fatal("instances must be non-nil empty list")
	}
}

func TestSnapshotThreeRuntimesAndIndependentStateChange(t *testing.T) {
	registry, clock := testRegistry(t)
	now := clock.Now()
	registerRuntime(t, registry, "runtime-a", 100, now)
	registerRuntime(t, registry, "runtime-b", 200, now)
	registerRuntime(t, registry, "runtime-c", 300, now)

	path := filepath.Join(t.TempDir(), "canonical.json")
	if err := WriteCanonicalSnapshotAtomic(path, registry); err != nil {
		t.Fatal(err)
	}
	var snapshot presencev2.CanonicalSnapshot
	if err := json.Unmarshal(mustRead(t, path), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Presence != presencev2.PresenceActive || len(snapshot.Instances) != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	ids := map[string]presencev2.Instance{}
	for _, inst := range snapshot.Instances {
		ids[string(inst.InstanceID)] = inst
		if inst.Tool != instancepresence.ToolClaude || inst.State != instancepresence.StateIdle {
			t.Fatalf("instance = %#v", inst)
		}
		if inst.Slot.Namespace == "" || inst.DiscoveredAt.IsZero() || inst.LeaseExpiresAt.IsZero() {
			t.Fatalf("missing required fields: %#v", inst)
		}
		// Content-free: no session ids or process paths in JSON.
	}
	raw := string(mustRead(t, path))
	for _, forbidden := range []string{"hook_session", "session_id", "cwd", "prompt", "cmdline", "/home/"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Fatalf("forbidden content %q in %s", forbidden, raw)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %#v", ids)
	}

	// Change only runtime-b to working; others stay idle.
	if _, err := registry.ApplyHookMutation("runtime-b", presencev2.HookStateMutation{
		ProducerEpoch: "epoch-a", HookRevision: 1, State: instancepresence.StateWorking,
		ObservedAt: now.Add(time.Second), IdempotencyKey: "hook-b-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCanonicalSnapshotAtomic(path, registry); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mustRead(t, path), &snapshot); err != nil {
		t.Fatal(err)
	}
	states := map[string]instancepresence.EffectiveState{}
	for _, inst := range snapshot.Instances {
		states[string(inst.InstanceID)] = inst.State
	}
	if states["runtime-a"] != instancepresence.StateIdle ||
		states["runtime-b"] != instancepresence.StateWorking ||
		states["runtime-c"] != instancepresence.StateIdle {
		t.Fatalf("states = %#v", states)
	}
}

func TestSnapshotAtomicReplaceAlwaysValidJSON(t *testing.T) {
	registry, clock := testRegistry(t)
	now := clock.Now()
	registerRuntime(t, registry, "runtime-a", 100, now)
	registerRuntime(t, registry, "runtime-b", 200, now)
	path := filepath.Join(t.TempDir(), "canonical.json")
	// Concurrent writers must never leave unreadable JSON at path.
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for round := 0; round < 25; round++ {
				if err := WriteCanonicalSnapshotAtomic(path, registry); err != nil {
					errs <- err
					return
				}
				data, err := os.ReadFile(path)
				if err != nil {
					errs <- err
					return
				}
				var snapshot presencev2.CanonicalSnapshot
				if err := json.Unmarshal(data, &snapshot); err != nil {
					errs <- err
					return
				}
				if err := snapshot.Validate(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestSnapshotLoopWritesAndStops(t *testing.T) {
	registry, _ := testRegistry(t)
	path := filepath.Join(t.TempDir(), "loop.json")
	var stderr strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunSnapshotLoop(ctx, registry, path, 30*time.Millisecond, &stderr)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("snapshot file was not created")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("snapshot loop did not stop")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// failingSource exercises WriteCanonicalSnapshotAtomic's error path without panicking callers.
type failingSource struct {
	calls atomic.Int64
}

func (s *failingSource) CanonicalSnapshot() (presencev2.CanonicalSnapshot, error) {
	s.calls.Add(1)
	return presencev2.CanonicalSnapshot{}, context.DeadlineExceeded
}

func TestSnapshotWriteFailureIsReturned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail.json")
	source := &failingSource{}
	if err := WriteCanonicalSnapshotAtomic(path, source); err == nil {
		t.Fatal("expected error")
	}
	if source.calls.Load() != 1 {
		t.Fatalf("calls = %d", source.calls.Load())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed write must not leave final snapshot path")
	}
}
