package producerprotocol

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var testTime = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

type testClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *testClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	clock.now = clock.now.Add(duration)
	clock.mutex.Unlock()
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

func validMessage(tool Tool) Message {
	return Message{
		ProtocolVersion: CurrentProtocolVersion,
		Tool:            tool,
		InstanceID:      "instance-fixture-1",
		ProducerEpoch:   "epoch-fixture-1",
		State:           StateWorking,
		Revision:        1,
		ObservedAt:      testTime,
		LeaseExpiresAt:  testTime.Add(time.Minute),
	}
}

func testConfig(clock Clock) Config {
	config := DefaultConfig(clock)
	return config
}

// secureTempDir returns a private (0700) directory under the user's home
// directory. It deliberately avoids /tmp: on some systems /tmp itself or an
// ancestor may be world-writable or a symlink, which the secure-socket path
// validation in this package must reject, so tests need a directory chain
// they control end to end.
func secureTempDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-producerprotocol-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove secure test directory: %v", err)
		}
	})
	return directory
}
