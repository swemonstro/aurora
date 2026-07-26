//go:build linux

package producerprotocol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const secureDirectoryFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW

func validateSocketPathSyntax(socketPath string) error {
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	for _, component := range strings.Split(socketPath, string(filepath.Separator)) {
		if component == ".." {
			return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
		}
	}
	if filepath.Base(socketPath) == "." || filepath.Base(socketPath) == string(filepath.Separator) {
		return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	return nil
}

type socketIdentity struct {
	device uint64
	inode  uint64
	uid    uint32
}

// Listener is a secure, single-owner Unix domain socket acceptor. The socket
// directory must be a private (0700), non-symlinked directory owned by the
// current effective user; the socket file itself is created with mode 0600
// and bound atomically through a pinned directory descriptor so a
// concurrent attacker cannot swap a path component for a symlink between
// validation and bind.
type Listener struct {
	listener *net.UnixListener
	config   Config
	path     string
	identity socketIdentity
}

// PrepareSocketDirectory ensures the parent directory of socketPath exists,
// is private (0700), and is owned by the current effective user, creating it
// if necessary. It does not create or bind the socket itself.
func PrepareSocketDirectory(socketPath string) error {
	if err := validateSocketPathSyntax(socketPath); err != nil {
		return err
	}
	directory := filepath.Dir(socketPath)
	if _, err := os.Lstat(directory); err == nil {
		return validatePrivateDirectory(directory, uint32(os.Geteuid()))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: runtime directory", ErrInsecureSocketPath)
	}
	parent := filepath.Dir(directory)
	parentFD, err := openDirectoryNoFollow(parent)
	if err != nil {
		return err
	}
	defer syscall.Close(parentFD)
	base := filepath.Base(directory)
	if err := syscall.Mkdirat(parentFD, base, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("create private runtime directory: %w", err)
	}
	return validatePrivateDirectory(directory, uint32(os.Geteuid()))
}

// Listen creates (if needed) a private socket directory and binds a secure
// Unix domain socket at config.SocketPath. config must have SocketPath set
// and pass Config.Validate(true).
func Listen(config Config) (*Listener, error) {
	if err := config.Validate(true); err != nil {
		return nil, err
	}
	socketPath := config.SocketPath
	if err := validateSocketPathSyntax(socketPath); err != nil {
		return nil, err
	}
	directory := filepath.Dir(socketPath)
	directoryFD, err := openDirectoryNoFollow(directory)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(directoryFD)
	if err := validatePrivateDirectoryFD(directoryFD, uint32(os.Geteuid())); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
		}
		return nil, protocolError(CodeSocketAlreadyExists, ErrSocketAlreadyExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, wrapOpaque("inspect socket path", err)
	}
	// Bind through the already verified directory descriptor so configured
	// path components cannot be swapped to symlinks between validation and
	// bind. Unix bind also creates the final name and never follows an
	// existing final symlink.
	boundPath := fmt.Sprintf("/proc/self/fd/%d/%s", directoryFD, filepath.Base(socketPath))
	unixListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: boundPath, Net: "unix"})
	if err != nil {
		if _, statErr := os.Lstat(socketPath); statErr == nil {
			return nil, protocolError(CodeSocketAlreadyExists, ErrSocketAlreadyExists)
		}
		return nil, wrapOpaque("listen on local socket", err)
	}
	unixListener.SetUnlinkOnClose(false)
	identity, err := statSocketIdentity(boundPath, false)
	if err != nil {
		_ = unixListener.Close()
		return nil, err
	}
	cleanupOnError := func(cause error) (*Listener, error) {
		_ = unixListener.Close()
		if current, statErr := statSocketIdentity(boundPath, false); statErr == nil && current == identity {
			_ = os.Remove(boundPath)
		}
		return nil, cause
	}
	if err := os.Chmod(boundPath, 0o600); err != nil {
		return cleanupOnError(wrapOpaque("set socket permissions", err))
	}
	identity, err = statSocketIdentity(boundPath, true)
	if err != nil {
		return cleanupOnError(err)
	}
	if identity.uid != uint32(os.Geteuid()) {
		return cleanupOnError(protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath))
	}
	currentDirectoryFD, err := openDirectoryNoFollow(directory)
	if err != nil {
		return cleanupOnError(protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath))
	}
	defer syscall.Close(currentDirectoryFD)
	if same, compareErr := sameDirectory(directoryFD, currentDirectoryFD); compareErr != nil || !same {
		return cleanupOnError(protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath))
	}
	if configuredIdentity, statErr := statSocketIdentity(socketPath, true); statErr != nil || configuredIdentity != identity {
		return cleanupOnError(protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath))
	}
	return &Listener{listener: unixListener, config: config, path: socketPath, identity: identity}, nil
}

// Accept blocks until one peer connects, immediately captures its kernel
// peer credentials (before any request data can be read), and wraps the
// connection as a Conn. If config.BoundTool is set, the returned Conn is
// already bound to it (the "one socket, one tool" primitive); otherwise the
// caller decides binding, e.g. via BindPeerTool.
//
// Accept performs no authentication: PeerIdentity is returned for the
// caller's Authenticator to decide.
func (listener *Listener) Accept(ctx context.Context) (*Conn, PeerIdentity, error) {
	stop := make(chan struct{})
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = listener.listener.Close()
			case <-stop:
			}
		}()
		defer close(stop)
	}
	unixConnection, err := listener.listener.AcceptUnix()
	if err != nil {
		return nil, PeerIdentity{}, classifyIOError(err, true)
	}
	peer, err := peerCredentials(unixConnection)
	if err != nil {
		_ = unixConnection.Close()
		return nil, PeerIdentity{}, err
	}
	conn := newConn(unixConnection, listener.config)
	if listener.config.BoundTool != "" {
		if err := conn.Bind(listener.config.BoundTool); err != nil {
			_ = unixConnection.Close()
			return nil, PeerIdentity{}, err
		}
	}
	return conn, peer, nil
}

// Close closes the listener and removes the socket file, but only if it is
// still the exact file this Listener created (matched by device/inode/uid),
// so a displaced-and-replaced socket is never removed out from under a
// concurrent owner.
func (listener *Listener) Close() error {
	closeErr := listener.listener.Close()
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return closeErr
	}
	current, err := statSocketIdentity(listener.path, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	if current != listener.identity {
		return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	return os.Remove(listener.path)
}

// Dial connects to config.SocketPath and returns a ready Conn. config must
// pass Config.Validate(true).
func Dial(ctx context.Context, config Config) (*Conn, error) {
	if err := config.Validate(true); err != nil {
		return nil, err
	}
	if err := validateSocketPathSyntax(config.SocketPath); err != nil {
		return nil, err
	}
	dialCtx := ctx
	var cancel context.CancelFunc
	if dialCtx == nil {
		dialCtx = context.Background()
	}
	if config.DialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(dialCtx, config.DialTimeout)
		defer cancel()
	}
	dialer := net.Dialer{}
	rawConnection, err := dialer.DialContext(dialCtx, "unix", config.SocketPath)
	if err != nil {
		return nil, wrapOpaque("dial local socket", err)
	}
	conn := newConn(rawConnection, config)
	if config.BoundTool != "" {
		if err := conn.Bind(config.BoundTool); err != nil {
			_ = rawConnection.Close()
			return nil, err
		}
	}
	return conn, nil
}

func statSocketIdentity(path string, requirePermissions bool) (socketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return socketIdentity{}, wrapOpaque("inspect socket path", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || requirePermissions && info.Mode().Perm() != 0o600 {
		return socketIdentity{}, protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return socketIdentity{}, protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	return socketIdentity{device: uint64(stat.Dev), inode: stat.Ino, uid: stat.Uid}, nil
}

func validatePrivateDirectory(directory string, expectedUID uint32) error {
	fd, err := openDirectoryNoFollow(directory)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return validatePrivateDirectoryFD(fd, expectedUID)
}

func validatePrivateDirectoryFD(fd int, expectedUID uint32) error {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect runtime directory: %w", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Uid != expectedUID || stat.Mode&0o077 != 0 || stat.Mode&0o700 != 0o700 {
		return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	return nil
}

func sameDirectory(firstFD, secondFD int) (bool, error) {
	var first, second syscall.Stat_t
	if err := syscall.Fstat(firstFD, &first); err != nil {
		return false, err
	}
	if err := syscall.Fstat(secondFD, &second); err != nil {
		return false, err
	}
	return first.Dev == second.Dev && first.Ino == second.Ino, nil
}

func openDirectoryNoFollow(directory string) (int, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return -1, protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	fd, err := syscall.Open(string(filepath.Separator), secureDirectoryFlags, 0)
	if err != nil {
		return -1, fmt.Errorf("open filesystem root: %w", err)
	}
	if err := validateAncestorDirectoryFD(fd, true); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(directory, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		if component == "." || component == ".." {
			syscall.Close(fd)
			return -1, protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
		}
		next, openErr := syscall.Openat(fd, component, secureDirectoryFlags, 0)
		syscall.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, syscall.ELOOP) || errors.Is(openErr, syscall.ENOTDIR) {
				return -1, protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
			}
			return -1, wrapOpaque("open socket directory component", openErr)
		}
		if statErr := validateAncestorDirectoryFD(next, false); statErr != nil {
			syscall.Close(next)
			return -1, statErr
		}
		fd = next
	}
	return fd, nil
}

func validateAncestorDirectoryFD(fd int, filesystemRoot bool) error {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect socket directory ancestor: %w", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	if !filesystemRoot && stat.Mode&0o022 != 0 {
		return protocolError(CodeInsecureSocketPath, ErrInsecureSocketPath)
	}
	return nil
}
