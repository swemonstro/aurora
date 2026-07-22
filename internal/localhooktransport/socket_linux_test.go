//go:build linux

package localhooktransport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
)

type captureLogger struct {
	mutex  sync.Mutex
	events []LogEvent
}

func (logger *captureLogger) Log(event LogEvent) {
	logger.mutex.Lock()
	defer logger.mutex.Unlock()
	logger.events = append(logger.events, event)
}

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
	directory, err := os.MkdirTemp(home, ".aurora-localhooktransport-test-")
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

func TestPrepareSocketDirectoryAndSecureListener(t *testing.T) {
	parent := secureTempDir(t)
	path := filepath.Join(parent, "aurora", "hook.sock")
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
	listener, err := listenUnixSecure(path)
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
	if err := listener.listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := listener.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after verified cleanup: %v", err)
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
	if err := os.Chmod(sharedReadable, 0o755); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(sharedReadable, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := listenUnixSecure(filepath.Join(private, "hook.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := listener.cleanup(); err != nil {
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
			socketPath := filepath.Join(private, "hook.sock")
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

func TestXDGRunTimeDirectoryLikePrivateChainIsAccepted(t *testing.T) {
	base := secureTempDir(t)
	runDirectory := filepath.Join(base, "run")
	userDirectory := filepath.Join(runDirectory, "user-fixture")
	if err := os.Mkdir(runDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(userDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(userDirectory, "aurora", "hook.sock")
	if err := PrepareSocketDirectory(socketPath); err != nil {
		t.Fatal(err)
	}
	listener, err := listenUnixSecure(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := listener.cleanup(); err != nil {
		t.Fatal(err)
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
		{name: "relative", path: "private/hook.sock"},
		{name: "parent traversal", path: filepath.Join(private, "..", "private", "hook.sock") + "/../hook.sock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := listenUnixSecure(test.path); !errors.Is(err, ErrInsecureSocketPath) {
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
	link := filepath.Join(private, "hook.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSecure(link); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("final symlink error = %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("do-not-overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSecure(link); !errors.Is(err, ErrSocketAlreadyExists) {
		t.Fatalf("regular file error = %v", err)
	}
	content, err := os.ReadFile(link)
	if err != nil || string(content) != "do-not-overwrite" {
		t.Fatalf("existing file changed: %q, %v", content, err)
	}

	realDirectory := filepath.Join(parent, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkDirectory := filepath.Join(parent, "linked")
	if err := os.Symlink(realDirectory, symlinkDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSecure(filepath.Join(symlinkDirectory, "hook.sock")); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("directory symlink error = %v", err)
	}
}

func TestCleanupDoesNotRemoveReplacementSocket(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "hook.sock")
	owned, err := listenUnixSecure(path)
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
	if err := owned.cleanup(); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("cleanup error = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("replacement socket was removed: %v", err)
	}
}

func TestSameUIDAuthenticator(t *testing.T) {
	auth := SameUIDAuthenticator{ServerUID: 1000}
	if err := auth.Authenticate(PeerIdentity{UID: 1000}); err != nil {
		t.Fatalf("same UID rejected: %v", err)
	}
	if err := auth.Authenticate(PeerIdentity{UID: 1001}); !errors.Is(err, ErrUnauthorizedPeer) {
		t.Fatalf("other UID error = %v", err)
	}
	auth.AllowedUIDs = map[uint32]struct{}{1001: {}}
	if err := auth.Authenticate(PeerIdentity{UID: 1001}); err != nil {
		t.Fatalf("allowlisted UID rejected: %v", err)
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

type rejectingAuthenticator struct{}

func (rejectingAuthenticator) Authenticate(PeerIdentity) error { return ErrUnauthorizedPeer }

type acceptingAuthenticator struct{}

func (acceptingAuthenticator) Authenticate(PeerIdentity) error { return nil }

func newNetworkTestServer(t *testing.T, authenticator Authenticator, source SnapshotSource) (*Server, Client, context.CancelFunc, <-chan error) {
	t.Helper()
	clock := wallClock{}
	config := DefaultConfig(clock)
	config.SocketPath = filepath.Join(secureTempDir(t), "hook.sock")
	if err := os.Chmod(filepath.Dir(config.SocketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	config.ReadDeadline = 250 * time.Millisecond
	config.WriteDeadline = 250 * time.Millisecond
	config.MaximumHandlingTime = time.Second
	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCorrelationService(source, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(config, service)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config, receiver, authenticator, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return server, *client, cancel, done
}

func TestServerAuthenticatesBeforeDecode(t *testing.T) {
	sample := testSample()
	server, _, cancel, done := newNetworkTestServer(t, rejectingAuthenticator{}, &fakeSnapshots{samples: []linuxprocess.Sample{sample}})
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: server.config.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte{0, 0, 0, 1, '{'}); err != nil {
		t.Fatal(err)
	}
	responseData, err := readFrame(connection, server.config.MaximumResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	err = json.Unmarshal(responseData, &response)
	if err != nil {
		t.Fatal(err)
	}
	if !hasErrorCode(response, CodeUnauthorizedPeer) {
		t.Fatalf("response = %#v", response)
	}
	_ = connection.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerClientRoundTripAndCleanup(t *testing.T) {
	now := time.Now().UTC().Round(0)
	runtime := testRuntime("runtime-a", instancepresence.ToolClaude, 101)
	runtime.ObservedAt = now
	runtime.Candidate.Runtime.RootProcess.StartedAt = now.Add(-time.Second)
	runtime.Candidate.Members[0] = runtime.Candidate.Runtime.RootProcess
	sample := testSample(runtime)
	sample.Snapshot.ObservedAt = now
	source := &fakeSnapshots{samples: []linuxprocess.Sample{sample}}
	server, client, cancel, done := newNetworkTestServer(t, DefaultAuthenticator(), source)
	request := testRequest("request-01", instancepresence.ToolClaude)
	request.Observation.ObservedAt = now
	root := runtime.Candidate.Runtime.RootProcess
	request.Observation.ProcessHint = &ProcessIdentity{PID: root.PID, StartedAt: root.StartedAt}
	response, err := client.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Proposals) != 1 || !response.NoBindingPerformed {
		t.Fatalf("response = %#v", response)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(server.config.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not cleaned up: %v", err)
	}
}

func TestServerAbruptDisconnectAndReadTimeout(t *testing.T) {
	server, _, cancel, done := newNetworkTestServer(t, acceptingAuthenticator{}, &fakeSnapshots{samples: []linuxprocess.Sample{testSample()}})
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: server.config.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()

	timed, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: server.config.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	_ = timed.SetReadDeadline(time.Now().Add(time.Second))
	responseData, err := readFrame(timed, server.config.MaximumResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	err = json.Unmarshal(responseData, &response)
	if err != nil {
		t.Fatal(err)
	}
	if !hasErrorCode(response, CodeReadTimeout) {
		t.Fatalf("response = %#v", response)
	}
	_ = timed.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerConcurrencyLimitIsBounded(t *testing.T) {
	now := time.Now().UTC().Round(0)
	clock := wallClock{}
	config := DefaultConfig(clock)
	config.SocketPath = filepath.Join(secureTempDir(t), "hook.sock")
	if err := os.Chmod(filepath.Dir(config.SocketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	config.MaximumConcurrent = 1
	config.ReadDeadline = time.Second
	config.WriteDeadline = time.Second
	config.MaximumHandlingTime = time.Second
	entered, release := make(chan struct{}, 1), make(chan struct{})
	source := &fakeSnapshots{samples: []linuxprocess.Sample{testSample()}, entered: entered, release: release}
	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCorrelationService(source, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(config, service)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config, receiver, acceptingAuthenticator{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	first := testRequest("request-01", instancepresence.ToolClaude)
	first.Observation.ObservedAt = now
	firstDone := make(chan error, 1)
	go func() {
		_, sendErr := client.Send(context.Background(), first)
		firstDone <- sendErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter snapshot source")
	}
	second := first
	second.RequestID = "request-02"
	response, err := client.Send(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if !hasErrorCode(response, CodeConcurrencyLimit) {
		t.Fatalf("response = %#v", response)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLogEventCannotContainPayloadIdentity(t *testing.T) {
	event := LogEvent{
		Accepted: true, Status: StatusOK, Protocol: CurrentProtocolVersion,
		DurationBucket: "lt_10ms", RuntimeCount: 1, Confidence: "exact",
		ReasonCodes: []string{"exact_process_identity"},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"session-fixture", "host-fixture", "boot-fixture", "runtime-a", "request-01", `"pid"`, `"uid"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("log contains forbidden value %q: %s", forbidden, data)
		}
	}
}

func TestPeerIdentityUsesKernelCredentials(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(private, "peer.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan PeerIdentity, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		identity, identityErr := peerIdentity(connection)
		if identityErr == nil {
			accepted <- identity
		}
	}()
	client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case identity := <-accepted:
		if identity.UID != uint32(os.Geteuid()) || identity.PID != int32(os.Getpid()) {
			t.Fatalf("peer identity = %#v", identity)
		}
	case <-time.After(time.Second):
		t.Fatal("peer credential check did not complete")
	}
}

func TestSocketDirectoryRejectsBroadPermissions(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSecure(filepath.Join(private, "hook.sock")); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("error = %v", err)
	}
}

func TestSocketIdentityIsCurrentUser(t *testing.T) {
	private := filepath.Join(secureTempDir(t), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := listenUnixSecure(filepath.Join(private, "hook.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = listener.listener.Close()
		_ = listener.cleanup()
	}()
	if listener.identity.uid != uint32(syscall.Geteuid()) {
		t.Fatalf("socket UID = %d", listener.identity.uid)
	}
}
