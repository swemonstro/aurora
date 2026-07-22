package linuxprocess

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestCaptureGenerationStableProcess(t *testing.T) {
	root := newProcFixture(t)
	writeProcessFixture(t, root, 4242, "hook", 100, 100, 10, 0, 500, []string{"/bin/aurora-claude-hook"})
	clock := fixedClock{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: clock, ClockTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	capture := adapter.CaptureGeneration(context.Background(), 4242)
	if !capture.OK {
		t.Fatalf("capture = %#v", capture)
	}
	if capture.Identity.PID != 4242 || capture.Identity.StartedAt.IsZero() {
		t.Fatalf("identity = %#v", capture.Identity)
	}
	// boot 1784707200 + 500/100s = 1784707205
	want := time.Unix(1784707200, 0).UTC().Add(5 * time.Second)
	if !capture.Identity.StartedAt.Equal(want) {
		t.Fatalf("started = %s want %s", capture.Identity.StartedAt, want)
	}
}

func TestCaptureGenerationMissingProcess(t *testing.T) {
	root := newProcFixture(t)
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Now().UTC()}, ClockTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	capture := adapter.CaptureGeneration(context.Background(), 99999)
	if capture.OK {
		t.Fatalf("expected failure, got %#v", capture)
	}
	if len(capture.ReasonCodes) == 0 {
		t.Fatal("expected reason codes")
	}
}

func TestCaptureGenerationPIDReuseBetweenReads(t *testing.T) {
	root := newProcFixture(t)
	writeProcessFixture(t, root, 77, "hook", 1, 1, 1, 0, 100, []string{"/bin/hook"})
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Now().UTC()}, ClockTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	// Inject a reader that changes start ticks between the two stat reads
	// performed by readProcess (before and after optional fields).
	open := adapter.open
	adapter.open = func() (procReader, error) {
		base, err := open()
		if err != nil {
			return nil, err
		}
		return &generationFlipReader{procReader: base, path: filepath.Join("77", "stat")}, nil
	}
	capture := adapter.CaptureGeneration(context.Background(), 77)
	if capture.OK {
		t.Fatalf("expected unstable generation, got %#v", capture)
	}
	found := false
	for _, code := range capture.ReasonCodes {
		if code == ReasonPIDReused || code == ReasonProcessDisappeared || code == ReasonInvalidProcData {
			found = true
		}
	}
	if !found {
		t.Fatalf("reason codes = %#v", capture.ReasonCodes)
	}
}

func TestCaptureGenerationRejectsZeroPID(t *testing.T) {
	root := newProcFixture(t)
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Now().UTC()}, ClockTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	capture := adapter.CaptureGeneration(context.Background(), 0)
	if capture.OK || len(capture.ReasonCodes) == 0 {
		t.Fatalf("capture = %#v", capture)
	}
}

type generationFlipReader struct {
	procReader
	path  string
	reads int
}

func (reader *generationFlipReader) ReadFile(name string, limit int64) ([]byte, error) {
	data, err := reader.procReader.ReadFile(name, limit)
	if err != nil {
		return nil, err
	}
	if name != reader.path {
		return data, nil
	}
	reader.reads++
	if reader.reads < 2 {
		return data, nil
	}
	// Rewrite start ticks field (field 22 / index 19 after comm) to a new generation.
	return []byte(statLine(77, "hook", 1, 1, 1, 0, 9999)), nil
}

func TestFormatProcessIdentity(t *testing.T) {
	identity := instancepresence.ProcessIdentity{
		PID: 1, StartedAt: time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
	}
	if got := FormatProcessIdentity(identity); got == "" {
		t.Fatal("empty format")
	}
}

func TestCaptureAncestryChainVerifiedParents(t *testing.T) {
	root := newProcFixture(t)
	// parent 100 start 100 ticks; child 200 start 200 ticks
	writeProcessFixture(t, root, 100, "claude", 1, 100, 10, 0, 100, []string{"/bin/claude"})
	writeProcessFixture(t, root, 200, "hook", 100, 100, 10, 0, 200, []string{"/bin/aurora-claude-hook"})
	clock := fixedClock{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: clock, ClockTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	chain := adapter.CaptureAncestryChain(context.Background(), 200, 6)
	if !chain.OK || len(chain.Hops) != 2 {
		t.Fatalf("chain = %#v", chain)
	}
	if chain.Hops[0].PID != 200 || chain.Hops[1].PID != 100 {
		t.Fatalf("hops = %#v", chain.Hops)
	}
}

func TestCaptureAncestryChainPeerMissing(t *testing.T) {
	root := newProcFixture(t)
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Now().UTC()}, ClockTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	chain := adapter.CaptureAncestryChain(context.Background(), 99999, 3)
	if chain.OK || len(chain.Hops) != 0 {
		t.Fatalf("chain = %#v", chain)
	}
}

func TestCaptureAncestryChainDepthExceededOnlyWhenParentRemains(t *testing.T) {
	clock := fixedClock{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}

	t.Run("ends_exactly_at_max_depth", func(t *testing.T) {
		root := newProcFixture(t)
		// peer -> parent (ParentPID 0): walk fills maxDepth=1 and stops with no further parent.
		writeProcessFixture(t, root, 100, "parent", 0, 100, 10, 0, 100, []string{"/bin/parent"})
		writeProcessFixture(t, root, 200, "hook", 100, 100, 10, 0, 200, []string{"/bin/hook"})
		adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: clock, ClockTicks: 100})
		if err != nil {
			t.Fatal(err)
		}
		chain := adapter.CaptureAncestryChain(context.Background(), 200, 1)
		if !chain.OK || len(chain.Hops) != 2 {
			t.Fatalf("chain = %#v", chain)
		}
		if hasReason(chain.ReasonCodes, ReasonAncestryDepthExceeded) {
			t.Fatalf("expected no depth exceeded at exact max depth, codes=%#v", chain.ReasonCodes)
		}
	})

	t.Run("further_parent_beyond_max_depth", func(t *testing.T) {
		root := newProcFixture(t)
		// grandparent remains beyond maxDepth=1.
		writeProcessFixture(t, root, 50, "grand", 0, 50, 10, 0, 50, []string{"/bin/grand"})
		writeProcessFixture(t, root, 100, "parent", 50, 100, 10, 0, 100, []string{"/bin/parent"})
		writeProcessFixture(t, root, 200, "hook", 100, 100, 10, 0, 200, []string{"/bin/hook"})
		adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: clock, ClockTicks: 100})
		if err != nil {
			t.Fatal(err)
		}
		chain := adapter.CaptureAncestryChain(context.Background(), 200, 1)
		if !chain.OK || len(chain.Hops) != 2 {
			t.Fatalf("chain = %#v", chain)
		}
		if chain.Hops[0].PID != 200 || chain.Hops[1].PID != 100 {
			t.Fatalf("hops = %#v", chain.Hops)
		}
		if !hasReason(chain.ReasonCodes, ReasonAncestryDepthExceeded) {
			t.Fatalf("expected ancestry_depth_exceeded, codes=%#v", chain.ReasonCodes)
		}
	})
}
