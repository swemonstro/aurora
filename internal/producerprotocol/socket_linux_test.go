//go:build linux

package producerprotocol

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPrepareSocketDirectoryAndSecureListen(t *testing.T) {
	parent := secureTempDir(t)
	path := filepath.Join(parent, "aurora", "producer.sock")
	if err := PrepareSocketDirectory(path); err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o", directoryInfo.Mode().Perm())
	}
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = path
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v", socketInfo.Mode())
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after close: %v", err)
	}
}

func TestSecureNonWritableAncestorChainIsAccepted(t *testing.T) {
	rootInfo, err := os.Stat(string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || rootStat.Uid != 0 || rootInfo.Mode()&os.ModeDir == 0 {
		t.Fatalf("filesystem root is not a root-owned directory: %#v", rootInfo.Sys())
	}
	base := secureTempDir(t)
	sharedReadable := filepath.Join(base, "shared-readable")
	if err := os.Mkdir(sharedReadable, 0o755); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(sharedReadable, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = filepath.Join(private, "producer.sock")
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWritableAncestorModesAreRejectedWithoutCreatingSocket(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
	}{
		{name: "group writable", mode: 0o770},
		{name: "world writable", mode: 0o707},
		{name: "sticky world writable", mode: os.ModeSticky | 0o777},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := secureTempDir(t)
			unsafeAncestor := filepath.Join(base, "unsafe")
			if err := os.Mkdir(unsafeAncestor, test.mode.Perm()); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(unsafeAncestor, test.mode); err != nil {
				t.Fatal(err)
			}
			private := filepath.Join(unsafeAncestor, "aurora")
			socketPath := filepath.Join(private, "producer.sock")
			if err := PrepareSocketDirectory(socketPath); !errors.Is(err, ErrInsecureSocketPath) {
				t.Fatalf("error = %v", err)
			}
			if _, err := os.Lstat(private); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private directory was created: %v", err)
			}
			if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("socket was created: %v", err)
			}
		})
	}
}

func TestSocketPathRejectsUnsafeForms(t *testing.T) {
	parent := secureTempDir(t)
	private := filepath.Join(parent, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{name: "relative", path: "private/producer.sock"},
		{name: "parent traversal", path: filepath.Join(private, "..", "private", "producer.sock") + "/../producer.sock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultConfig(&testClock{now: testTime})
			config.SocketPath = test.path
			if _, err := Listen(config); !errors.Is(err, ErrInsecureSocketPath) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSocketRejectsSymlinksAndExistingFile(t *testing.T) {
	parent := secureTempDir(t)
	private := filepath.Join(parent, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(private, "target")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "producer.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = link
	if _, err := Listen(config); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("final symlink error = %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("do-not-overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(config); !errors.Is(err, ErrSocketAlreadyExists) {
		t.Fatalf("regular file error = %v", err)
	}
	content, err := os.ReadFile(link)
	if err != nil || string(content) != "do-not-overwrite" {
		t.Fatalf("existing file changed: %q, %v", content, err)
	}
}

func TestCloseDoesNotRemoveReplacementSocket(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "producer.sock")
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = path
	owned, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.listener.Close(); err != nil {
		t.Fatal(err)
	}
	displaced := path + ".owned"
	if err := os.Rename(path, displaced); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	replacement.SetUnlinkOnClose(false)
	defer func() {
		_ = replacement.Close()
		_ = os.Remove(path)
		_ = os.Remove(displaced)
	}()
	if err := owned.Close(); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("close error = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("replacement socket was removed: %v", err)
	}
}

func TestSocketDirectoryRejectsBroadPermissions(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = filepath.Join(private, "producer.sock")
	if _, err := Listen(config); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("error = %v", err)
	}
}

func TestSocketIdentityIsCurrentUser(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = filepath.Join(private, "producer.sock")
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if listener.identity.uid != uint32(syscall.Geteuid()) {
		t.Fatalf("socket UID = %d", listener.identity.uid)
	}
}

// TestCloseSucceedsWhenSocketAlreadyRemoved covers an external cleanup
// racing this Listener: something else (a crash-recovery script, a
// conflicting owner) removes the socket file before Close runs. Close must
// still treat this as "already gone, nothing to do" rather than surfacing
// an error — see the errors.Is(err, os.ErrNotExist) branch in Close, which
// depends on statSocketIdentity's wrapOpaque-wrapped Lstat failure still
// unwrapping to os.ErrNotExist.
func TestCloseSucceedsWhenSocketAlreadyRemoved(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(&testClock{now: testTime})
	config.SocketPath = filepath.Join(private, "instance-removed-canary.sock")
	listener, err := Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config.SocketPath); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close after external removal = %v, want nil", err)
	}
}

// TestStatSocketIdentityNotExistPreservesChainAndRedactsPath pins the
// property TestCloseSucceedsWhenSocketAlreadyRemoved depends on at the unit
// level: statSocketIdentity wraps its os.Lstat failure in wrapOpaque (see
// errors.go) so the socket path never appears in Error() text, and that
// wrapping must not break errors.Is(err, os.ErrNotExist) — Close relies on
// exactly that check to treat a missing socket as success rather than an
// insecure-path failure.
func TestStatSocketIdentityNotExistPreservesChainAndRedactsPath(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(private, "instance-never-existed-canary.sock")

	_, err := statSocketIdentity(missing, true)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want errors.Is(err, os.ErrNotExist)", err)
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("statSocketIdentity leaked the socket path: %v", err)
	}
	if strings.Contains(err.Error(), filepath.Base(missing)) {
		t.Fatalf("statSocketIdentity leaked the socket filename: %v", err)
	}
}
