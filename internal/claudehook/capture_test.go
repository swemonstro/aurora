package claudehook

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureDirectoryFromEnvDisabled(t *testing.T) {
	for _, value := range []string{"", " \t\n "} {
		if got := CaptureDirectoryFromEnv(func(string) string { return value }); got != "" {
			t.Errorf("CaptureDirectoryFromEnv() = %q, want empty", got)
		}
	}
}

func TestCapturePreservesExactInputAndCreatesDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private", "captures")
	input := []byte("  {\"hook_event_name\":\"UserPromptSubmit\",\"prompt\":\"sensitive\"}\n")

	if err := Capture(directory, input); err != nil {
		t.Fatalf("Capture returned error: %v", err)
	}

	filename, content, mode := readOnlyCapture(t, directory)
	if !bytes.Equal(content, input) {
		t.Fatalf("captured bytes = %q, want %q", content, input)
	}
	if !strings.Contains(filename, "_UserPromptSubmit_") {
		t.Errorf("capture filename = %q", filename)
	}
	if runtime.GOOS != "windows" && mode.Perm() != 0o600 {
		t.Errorf("capture permissions = %o, want 600", mode.Perm())
	}
}

func TestCaptureRetainsUnsupportedMalformedAndEmptyInput(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{name: "unsupported", input: []byte(`{"hook_event_name":"PreToolUse"}`)},
		{name: "malformed", input: []byte(`{"hook_event_name":`)},
		{name: "empty", input: []byte{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := Capture(directory, test.input); err != nil {
				t.Fatalf("Capture returned error: %v", err)
			}
			_, content, _ := readOnlyCapture(t, directory)
			if !bytes.Equal(content, test.input) {
				t.Fatalf("captured bytes = %q, want %q", content, test.input)
			}
		})
	}
}

func TestCaptureDoesNotOverwriteAcrossInvocations(t *testing.T) {
	directory := t.TempDir()
	input := []byte(`{"hook_event_name":"Stop"}`)
	if err := Capture(directory, input); err != nil {
		t.Fatal(err)
	}
	if err := Capture(directory, input); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("capture file count = %d, want 2", len(entries))
	}
	if entries[0].Name() == entries[1].Name() {
		t.Fatalf("capture filenames are identical: %q", entries[0].Name())
	}
}

func readOnlyCapture(t *testing.T, directory string) (string, []byte, os.FileMode) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("capture file count = %d, want 1", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return entries[0].Name(), content, info.Mode()
}
