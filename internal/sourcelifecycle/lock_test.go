package sourcelifecycle

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWithLockSerializesActionsForStatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	errors := make(chan error, 2)

	go func() {
		errors <- WithLock(path, time.Second, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered
	go func() {
		errors <- WithLock(path, time.Second, func() error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second action entered while first held the lock")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second action did not enter after lock release")
	}
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestWithLockValidatesArguments(t *testing.T) {
	var once sync.Once
	action := func() error { once.Do(func() {}); return nil }
	if err := WithLock(" ", time.Second, action); err == nil {
		t.Fatal("empty path was accepted")
	}
	if err := WithLock("state", 0, action); err == nil {
		t.Fatal("zero timeout was accepted")
	}
	if err := WithLock("state", time.Second, nil); err == nil {
		t.Fatal("nil action was accepted")
	}
}
