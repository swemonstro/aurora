package sessionbinding

import (
	"errors"
	"sync"
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestReserveCommitRollback(t *testing.T) {
	reg := New()
	if err := reg.Reserve(instancepresence.ToolClaude, "session-a", "runtime-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Lookup(instancepresence.ToolClaude, "session-a"); ok {
		t.Fatal("reservation must not be visible to Lookup")
	}
	if reg.ReservedLen() != 1 || reg.Len() != 0 {
		t.Fatalf("reserved=%d committed=%d", reg.ReservedLen(), reg.Len())
	}
	// Parallel session cannot take same runtime while reserved.
	if err := reg.Reserve(instancepresence.ToolClaude, "session-b", "runtime-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict during reservation, got %v", err)
	}
	if err := reg.Commit(instancepresence.ToolClaude, "session-a"); err != nil {
		t.Fatal(err)
	}
	if id, ok := reg.Lookup(instancepresence.ToolClaude, "session-a"); !ok || id != "runtime-a" {
		t.Fatalf("lookup after commit = %q ok=%t", id, ok)
	}
	if err := reg.Reserve(instancepresence.ToolClaude, "session-b", "runtime-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict after commit, got %v", err)
	}

	reg2 := New()
	if err := reg2.Reserve(instancepresence.ToolClaude, "session-c", "runtime-c"); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Rollback(instancepresence.ToolClaude, "session-c"); err != nil {
		t.Fatal(err)
	}
	if reg2.ReservedLen() != 0 || reg2.Len() != 0 {
		t.Fatal("rollback left state")
	}
	// Runtime free after rollback.
	if err := reg2.Reserve(instancepresence.ToolClaude, "session-d", "runtime-c"); err != nil {
		t.Fatal(err)
	}
}

func TestUnbindOnlyCommitted(t *testing.T) {
	reg := New()
	if err := reg.Reserve(instancepresence.ToolClaude, "session-a", "runtime-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Unbind(instancepresence.ToolClaude, "session-a"); !errors.Is(err, ErrNotBound) {
		t.Fatalf("unbind reserved = %v", err)
	}
	if err := reg.Commit(instancepresence.ToolClaude, "session-a"); err != nil {
		t.Fatal(err)
	}
	if id, err := reg.Unbind(instancepresence.ToolClaude, "session-a"); err != nil || id != "runtime-a" {
		t.Fatalf("unbind = %q err=%v", id, err)
	}
	if _, err := reg.Unbind(instancepresence.ToolClaude, "session-a"); !errors.Is(err, ErrNotBound) {
		t.Fatalf("second unbind = %v", err)
	}
}

func TestParallelReserveSameRuntime(t *testing.T) {
	reg := New()
	var wait sync.WaitGroup
	results := make(chan error, 2)
	wait.Add(2)
	for _, session := range []string{"session-a", "session-b"} {
		session := session
		go func() {
			defer wait.Done()
			results <- reg.Reserve(instancepresence.ToolClaude, instancepresence.OpaqueIdentity(session), "runtime-shared")
		}()
	}
	wait.Wait()
	close(results)
	var ok, conflict int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatalf("unexpected err %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d, want 1 and 1", ok, conflict)
	}
	if reg.ReservedLen() != 1 {
		t.Fatalf("reserved = %d", reg.ReservedLen())
	}
}
