package linuxprocess

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

type procReader interface {
	ReadDir(string) ([]fs.DirEntry, error)
	ReadFile(string, int64) ([]byte, error)
	Close() error
}

type rootReader struct {
	fd int
}

const (
	directoryOpenFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	fileOpenFlags      = syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
)

func openProcRoot(name string) (procReader, error) {
	fd, err := openRootDirectory(name)
	if err != nil {
		return nil, fmt.Errorf("open proc root: %w", err)
	}
	return &rootReader{fd: fd}, nil
}

// openRootDirectory walks the configured root one directory descriptor at a
// time. O_NOFOLLOW makes each openat operation atomically reject a symlink in
// the component being opened; there is no separate path-based pre-check.
func openRootDirectory(name string) (int, error) {
	if name == "" {
		return -1, fmt.Errorf("%w: empty proc root", ErrUnsafeProcEntry)
	}

	clean := filepath.Clean(name)
	start := "."
	remaining := clean
	if filepath.IsAbs(clean) {
		start = string(filepath.Separator)
		remaining = strings.TrimPrefix(clean, string(filepath.Separator))
	}

	fd, err := syscall.Open(start, directoryOpenFlags, 0)
	if err != nil {
		return -1, pathOpenError(start, err)
	}
	for _, component := range strings.Split(remaining, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			syscall.Close(fd)
			return -1, fmt.Errorf("%w: parent traversal in proc root", ErrUnsafeProcEntry)
		}
		next, openErr := syscall.Openat(fd, component, directoryOpenFlags, 0)
		syscall.Close(fd)
		if openErr != nil {
			return -1, pathOpenError(component, openErr)
		}
		fd = next
	}
	return fd, nil
}

func (r *rootReader) ReadDir(name string) ([]fs.DirEntry, error) {
	fd, err := r.openDirectory(name)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("open proc directory %q", name)
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func (r *rootReader) ReadFile(name string, limit int64) ([]byte, error) {
	file, err := r.openFile(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return data[:limit], fmt.Errorf("%w: %s", ErrReadLimit, name)
	}
	return data, nil
}

func (r *rootReader) openDirectory(name string) (int, error) {
	components, err := relativeComponents(name, true)
	if err != nil {
		return -1, err
	}
	return r.walkDirectories(components)
}

func (r *rootReader) openFile(name string) (*os.File, error) {
	components, err := relativeComponents(name, false)
	if err != nil {
		return nil, err
	}
	directoryFD, err := r.walkDirectories(components[:len(components)-1])
	if err != nil {
		return nil, err
	}
	defer syscall.Close(directoryFD)

	base := components[len(components)-1]
	fd, err := syscall.Openat(directoryFD, base, fileOpenFlags, 0)
	if err != nil {
		return nil, pathOpenError(name, err)
	}

	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		syscall.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: name, Err: err}
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		syscall.Close(fd)
		return nil, fmt.Errorf("%w: non-regular proc file %q", ErrUnsafeProcEntry, name)
	}

	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("open proc file %q", name)
	}
	return file, nil
}

func (r *rootReader) walkDirectories(components []string) (int, error) {
	fd, err := syscall.Openat(r.fd, ".", directoryOpenFlags, 0)
	if err != nil {
		return -1, pathOpenError(".", err)
	}
	for _, component := range components {
		next, openErr := syscall.Openat(fd, component, directoryOpenFlags, 0)
		syscall.Close(fd)
		if openErr != nil {
			return -1, pathOpenError(component, openErr)
		}
		fd = next
	}
	return fd, nil
}

func relativeComponents(name string, allowRoot bool) ([]string, error) {
	if name == "" || path.IsAbs(name) {
		return nil, fmt.Errorf("%w: invalid proc-relative path %q", ErrUnsafeProcEntry, name)
	}
	clean := path.Clean(name)
	if clean == "." {
		if allowRoot {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: invalid proc file path %q", ErrUnsafeProcEntry, name)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("%w: proc path escapes root %q", ErrUnsafeProcEntry, name)
	}
	return strings.Split(clean, "/"), nil
}

func pathOpenError(name string, err error) error {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return fmt.Errorf("%w: unsafe proc path %q: %v", ErrUnsafeProcEntry, name, err)
	}
	return &os.PathError{Op: "openat", Path: name, Err: err}
}

func (r *rootReader) Close() error {
	if r.fd < 0 {
		return nil
	}
	err := syscall.Close(r.fd)
	r.fd = -1
	return err
}
