//go:build linux

package codexproducer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

const (
	maxIngressMessageBytes  = 4096
	ingressReadTimeout      = 2 * time.Second
	ingressStatReadLimit    = 4096
	ingressEnvironReadLimit = 64 * 1024
)

// IngressListener accepts opt-in, best-effort, shadow-forwarded Codex hook
// observations from cmd/aurora-codex-hook's shadow-target (see that binary's
// AURORA_CODEX_SHADOW_SOCKET support). It is a private, same-UID
// Unix socket wholly separate from the monolith's shared hook socket
// (internal/localhooktransport / AURORA_LOCAL_HOOK_SOCKET) and from this
// producer's own broker connection. It never competes for, and never reads
// or writes, the monolith's hook socket path.
type IngressListener struct {
	listener      *net.UnixListener
	path          string
	authenticator producerprotocol.Authenticator
}

// ListenIngress binds a secure ingress socket at socketPath. The parent
// directory is prepared via producerprotocol.PrepareSocketDirectory (the
// same private, non-symlinked, own-EUID directory contract producerprotocol
// itself requires), and the socket file is created fresh (refusing to bind
// over an existing path or symlink) and locked to mode 0600.
func ListenIngress(socketPath string, authenticator producerprotocol.Authenticator) (*IngressListener, error) {
	if authenticator == nil {
		return nil, fmt.Errorf("codex hook ingress: authenticator must not be nil")
	}
	if err := producerprotocol.PrepareSocketDirectory(socketPath); err != nil {
		return nil, fmt.Errorf("prepare codex hook ingress socket directory: %w", err)
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("codex hook ingress socket path is a symlink")
		}
		return nil, fmt.Errorf("codex hook ingress socket already exists")
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect codex hook ingress socket path: %w", err)
	}
	unixListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on codex hook ingress socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = unixListener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("secure codex hook ingress socket: %w", err)
	}
	return &IngressListener{listener: unixListener, path: socketPath, authenticator: authenticator}, nil
}

// Close closes the listener and removes the socket file.
func (listener *IngressListener) Close() error {
	closeErr := listener.listener.Close()
	if removeErr := os.Remove(listener.path); removeErr != nil && closeErr == nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return closeErr
}

// HookDelivery is one accepted, peer-authenticated shadow hook observation,
// enriched with correlation hints captured independently of the delivered
// payload (kernel-verified peer PID, and that peer's own process-group,
// session, and CODEX_HOME — never its cwd, argv, or transcript path).
type HookDelivery struct {
	Observation hookadapter.IngressObservation
	// EnvCodexHome is the delivering hook process's own CODEX_HOME
	// environment value (empty if unset); it inherits Codex's environment
	// because the hook binary is invoked as Codex's child. Used only to
	// attribute this delivery to a configured source — never logged.
	EnvCodexHome string
	// ProcessGroupOrJob and OSSession are opaque, content-free tags (e.g.
	// "pgrp:1234") describing the delivering process's own OS process group
	// and session, used only to disambiguate which of several concurrent
	// same-source Codex instances a hook belongs to.
	ProcessGroupOrJob string
	OSSession         string
}

// Serve accepts connections until ctx is done or the listener is closed. Each
// accepted connection may deliver any number of JSON-encoded
// hookadapter.IngressObservation values (one per line is not required; the
// JSON stream decoder finds message boundaries itself); handle is called
// once per successfully authenticated, decoded, and validated delivery. A
// malformed or unauthenticated connection is dropped without affecting any
// other connection, and Serve itself never returns an error for a peer's
// bad behavior — only for the listener itself failing.
func (listener *IngressListener) Serve(ctx context.Context, handle func(HookDelivery)) error {
	var connections sync.WaitGroup
	defer connections.Wait()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	for {
		connection, err := listener.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept codex hook ingress connection: %w", err)
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			serveIngressConnection(connection, listener.authenticator, handle)
		}()
	}
}

func serveIngressConnection(connection *net.UnixConn, authenticator producerprotocol.Authenticator, handle func(HookDelivery)) {
	defer connection.Close()
	peer, err := ingressPeerCredentials(connection)
	if err != nil {
		return
	}
	if err := authenticator.Authenticate(producerprotocol.PeerIdentity{UID: peer.uid, GID: peer.gid, PID: peer.pid}); err != nil {
		return
	}
	envCodexHome, processGroup, osSession := readPeerCorrelationHints(peer.pid)

	bounded := &boundedReader{reader: connection, limit: maxIngressMessageBytes}
	bounded.reset()
	decoder := json.NewDecoder(bufio.NewReader(bounded))
	for {
		_ = connection.SetReadDeadline(time.Now().Add(ingressReadTimeout))
		var observation hookadapter.IngressObservation
		if err := decoder.Decode(&observation); err != nil {
			return
		}
		bounded.reset()
		if err := observation.Validate(); err != nil {
			continue
		}
		handle(HookDelivery{
			Observation:       observation,
			EnvCodexHome:      envCodexHome,
			ProcessGroupOrJob: processGroup,
			OSSession:         osSession,
		})
	}
}

// boundedReader caps the bytes readable before the next call to reset,
// bounding one JSON message's size without limiting the connection's
// lifetime traffic (reset is called after each successful Decode).
type boundedReader struct {
	reader    io.Reader
	limit     int64
	remaining int64
}

func (bounded *boundedReader) reset() { bounded.remaining = bounded.limit }

func (bounded *boundedReader) Read(buffer []byte) (int, error) {
	if bounded.remaining <= 0 {
		return 0, fmt.Errorf("codex hook ingress message exceeds %d bytes", bounded.limit)
	}
	if int64(len(buffer)) > bounded.remaining {
		buffer = buffer[:bounded.remaining]
	}
	read, err := bounded.reader.Read(buffer)
	bounded.remaining -= int64(read)
	return read, err
}

type ingressPeer struct {
	uid, gid uint32
	pid      int32
}

// ingressPeerCredentials reads the kernel-verified peer identity via
// SO_PEERCRED immediately after accept, mirroring
// internal/producerprotocol's peerCredentials (unexported there). This small
// duplication follows the precedent producerprotocol's own doc.go already
// sets for socket_linux.go relative to internal/localhooktransport: sharing
// it would mean this package depending on producerprotocol's internals (or a
// new shared package) for a dozen lines, which is not worth the coupling.
func ingressPeerCredentials(connection *net.UnixConn) (ingressPeer, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return ingressPeer{}, err
	}
	var credentials *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return ingressPeer{}, err
	}
	if socketErr != nil || credentials == nil {
		return ingressPeer{}, fmt.Errorf("read codex hook ingress peer credentials: %w", socketErr)
	}
	return ingressPeer{uid: credentials.Uid, gid: credentials.Gid, pid: credentials.Pid}, nil
}

// readPeerCorrelationHints best-effort reads /proc/<pid>/stat (process group,
// session) and /proc/<pid>/environ (CODEX_HOME only) for the delivering hook
// process. Every value is optional: a read failure (process already exited,
// permission denied) yields empty hints rather than an error, since a
// correlation hint is never required for delivery to still be accepted —
// only for it to be matched to the right concurrent instance.
//
// Known race, disclosed rather than hidden: cmd/aurora-codex-hook's shadow
// delivery dials, writes, and returns (exiting almost immediately after),
// so this read can occasionally lose the race against that short-lived
// process's own exit — /proc/<pid> stops existing and every hint comes back
// empty. This degrades gracefully: Correlator treats a hint-less delivery
// exactly like any other still-ambiguous one (stays pending, resolved once
// unambiguous, or expires unbound after PendingHookTTL) — it never guesses
// and never mis-binds because of a lost race.
func readPeerCorrelationHints(pid int32) (envCodexHome, processGroup, osSession string) {
	if pid <= 0 {
		return "", "", ""
	}
	base := strconv.FormatInt(int64(pid), 10)
	if data, err := readBoundedFile(filepath.Join("/proc", base, "stat"), ingressStatReadLimit); err == nil {
		if pgrp, session, ok := parseStatGroupAndSession(data); ok {
			processGroup = "pgrp:" + strconv.FormatUint(pgrp, 10)
			osSession = "session:" + strconv.FormatUint(session, 10)
		}
	}
	if data, err := readBoundedFile(filepath.Join("/proc", base, "environ"), ingressEnvironReadLimit); err == nil {
		envCodexHome = parseCodexHomeFromEnviron(data)
	}
	return envCodexHome, processGroup, osSession
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// parseStatGroupAndSession extracts pgrp and session from /proc/<pid>/stat.
// The comm field (2nd field) is parenthesized and may itself contain spaces
// or parentheses, so parsing starts after the last ')' as /proc/[pid]/stat
// documents.
func parseStatGroupAndSession(data []byte) (pgrp, session uint64, ok bool) {
	text := string(data)
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 || closeParen+2 >= len(text) {
		return 0, 0, false
	}
	fields := strings.Fields(text[closeParen+2:])
	// After stripping "pid (comm) ", field 0 is state(3rd overall), field 1
	// is ppid(4th), field 2 is pgrp(5th), field 3 is session(6th).
	if len(fields) < 4 {
		return 0, 0, false
	}
	parsedGroup, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	parsedSession, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return parsedGroup, parsedSession, true
}

func parseCodexHomeFromEnviron(data []byte) string {
	const prefix = "CODEX_HOME="
	for _, entry := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(entry, prefix) {
			value := strings.TrimSpace(strings.TrimPrefix(entry, prefix))
			if value != "" && filepath.IsAbs(value) {
				return filepath.Clean(value)
			}
			return ""
		}
	}
	return ""
}
