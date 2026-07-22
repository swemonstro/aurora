//go:build linux

package localhooktransport

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const secureDirectoryFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW

type socketIdentity struct {
	device uint64
	inode  uint64
	uid    uint32
}

type ownedListener struct {
	listener *net.UnixListener
	path     string
	identity socketIdentity
}

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

func listenUnixSecure(socketPath string) (*ownedListener, error) {
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
			return nil, ErrInsecureSocketPath
		}
		return nil, ErrSocketAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect socket path: %w", err)
	}
	// Bind through the already verified directory descriptor. Linux resolves
	// /proc/self/fd/N to that pinned directory, so configured path components
	// cannot be swapped to symlinks between validation and bind. Unix bind also
	// creates the final name and never follows an existing final symlink.
	boundPath := fmt.Sprintf("/proc/self/fd/%d/%s", directoryFD, filepath.Base(socketPath))
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: boundPath, Net: "unix"})
	if err != nil {
		if _, statErr := os.Lstat(socketPath); statErr == nil {
			return nil, ErrSocketAlreadyExists
		}
		return nil, fmt.Errorf("listen on local socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	identity, err := statSocketIdentity(boundPath, false)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	cleanupOnError := func(cause error) (*ownedListener, error) {
		_ = listener.Close()
		if current, statErr := statSocketIdentity(boundPath, false); statErr == nil && current == identity {
			_ = os.Remove(boundPath)
		}
		return nil, cause
	}
	if err := os.Chmod(boundPath, 0o600); err != nil {
		return cleanupOnError(fmt.Errorf("set socket permissions: %w", err))
	}
	identity, err = statSocketIdentity(boundPath, true)
	if err != nil {
		return cleanupOnError(err)
	}
	if identity.uid != uint32(os.Geteuid()) {
		return cleanupOnError(ErrInsecureSocketPath)
	}
	currentDirectoryFD, err := openDirectoryNoFollow(directory)
	if err != nil {
		return cleanupOnError(ErrInsecureSocketPath)
	}
	defer syscall.Close(currentDirectoryFD)
	if same, compareErr := sameDirectory(directoryFD, currentDirectoryFD); compareErr != nil || !same {
		return cleanupOnError(ErrInsecureSocketPath)
	}
	if configuredIdentity, statErr := statSocketIdentity(socketPath, true); statErr != nil || configuredIdentity != identity {
		return cleanupOnError(ErrInsecureSocketPath)
	}
	return &ownedListener{listener: listener, path: socketPath, identity: identity}, nil
}

func (listener *ownedListener) cleanup() error {
	current, err := statSocket(listener.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrInsecureSocketPath
	}
	if current != listener.identity {
		return ErrInsecureSocketPath
	}
	return os.Remove(listener.path)
}

func statSocket(path string) (socketIdentity, error) {
	return statSocketIdentity(path, true)
}

func statSocketIdentity(path string, requirePermissions bool) (socketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return socketIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || requirePermissions && info.Mode().Perm() != 0o600 {
		return socketIdentity{}, ErrInsecureSocketPath
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return socketIdentity{}, ErrInsecureSocketPath
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
		return ErrInsecureSocketPath
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
		return -1, ErrInsecureSocketPath
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
			return -1, ErrInsecureSocketPath
		}
		next, openErr := syscall.Openat(fd, component, secureDirectoryFlags, 0)
		syscall.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, syscall.ELOOP) || errors.Is(openErr, syscall.ENOTDIR) {
				return -1, ErrInsecureSocketPath
			}
			return -1, &os.PathError{Op: "openat", Path: component, Err: openErr}
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
		return ErrInsecureSocketPath
	}
	if !filesystemRoot && stat.Mode&0o022 != 0 {
		return ErrInsecureSocketPath
	}
	return nil
}
