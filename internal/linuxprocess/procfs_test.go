package linuxprocess

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRootReaderReadsRegularFixtureFile(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "101/stat", "regular fixture")
	reader := openTestRootReader(t, root)

	got, err := reader.ReadFile("101/stat", 64)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "regular fixture" {
		t.Fatalf("ReadFile() = %q", got)
	}
}

func TestRootReaderRejectsSymlinkFile(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-stat")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "101"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "101", "stat")); err != nil {
		t.Fatal(err)
	}
	reader := openTestRootReader(t, root)

	if _, err := reader.ReadFile("101/stat", 64); !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("ReadFile(symlink) error = %v, want %v", err, ErrUnsafeProcEntry)
	}
}

func TestRootReaderRejectsSymlinkDirectoryComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFixtureFile(t, outside, "kernel/random/boot_id", "outside")
	if err := os.Mkdir(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sys", "kernel")); err != nil {
		t.Fatal(err)
	}
	reader := openTestRootReader(t, root)

	if _, err := reader.ReadFile("sys/kernel/random/boot_id", 64); !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("ReadFile(directory symlink) error = %v, want %v", err, ErrUnsafeProcEntry)
	}
}

func TestOpenProcRootRejectsSymlinkInConfiguredRootPath(t *testing.T) {
	parent := t.TempDir()
	realDirectory := filepath.Join(parent, "real")
	if err := os.MkdirAll(filepath.Join(realDirectory, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, filepath.Join(parent, "linked-component")); err != nil {
		t.Fatal(err)
	}

	_, err := openProcRoot(filepath.Join(parent, "linked-component", "proc"))
	if !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("openProcRoot(path with symlink component) error = %v, want %v", err, ErrUnsafeProcEntry)
	}
}

func TestRootReaderRejectsSymlinkPIDDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFixtureFile(t, outside, "stat", "outside")
	if err := os.Symlink(outside, filepath.Join(root, "101")); err != nil {
		t.Fatal(err)
	}
	reader := openTestRootReader(t, root)

	if _, err := reader.ReadFile("101/stat", 64); !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("ReadFile(PID directory symlink) error = %v, want %v", err, ErrUnsafeProcEntry)
	}
}

func TestRootReaderKeepsReadLimit(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "101/stat", "0123456789")
	reader := openTestRootReader(t, root)

	got, err := reader.ReadFile("101/stat", 4)
	if !errors.Is(err, ErrReadLimit) {
		t.Fatalf("ReadFile() error = %v, want %v", err, ErrReadLimit)
	}
	if string(got) != "0123" {
		t.Fatalf("ReadFile() = %q, want read-limited prefix", got)
	}
}

func TestRootReaderOpenedDescriptorCannotBeRedirectedByPathSwap(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "101/stat", "original")
	outside := filepath.Join(t.TempDir(), "outside-stat")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader := openTestRootReader(t, root)

	file, err := reader.openFile("101/stat")
	if err != nil {
		t.Fatalf("openFile() error = %v", err)
	}
	defer file.Close()
	if err := os.Remove(filepath.Join(root, "101", "stat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "101", "stat")); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read opened descriptor: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("opened descriptor read %q after path swap", got)
	}
	if _, err := reader.ReadFile("101/stat", 64); !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("ReadFile(swapped symlink) error = %v, want %v", err, ErrUnsafeProcEntry)
	}
}

func openTestRootReader(t *testing.T, root string) *rootReader {
	t.Helper()
	reader, err := openProcRoot(root)
	if err != nil {
		t.Fatalf("openProcRoot() error = %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	rootReader, ok := reader.(*rootReader)
	if !ok {
		t.Fatalf("openProcRoot() returned %T", reader)
	}
	return rootReader
}
