package sourcelifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const lockRetry = 10 * time.Millisecond

// WithLock serializes a source's persisted state transition and its relay
// publication so concurrent hook processes cannot publish stale aggregates.
func WithLock(statePath string, timeout time.Duration, action func() error) error {
	if strings.TrimSpace(statePath) == "" {
		return fmt.Errorf("state path must not be empty")
	}
	if timeout <= 0 {
		return fmt.Errorf("lock timeout must be positive")
	}
	if action == nil {
		return fmt.Errorf("action must not be nil")
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return fmt.Errorf("create lifecycle state directory: %w", err)
	}
	file, err := os.OpenFile(statePath+".lifecycle.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lifecycle lock: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("set lifecycle lock permissions: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock lifecycle: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lock lifecycle: timed out after %s", timeout)
		}
		time.Sleep(lockRetry)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)

	return action()
}
