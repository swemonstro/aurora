package claudetrust

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeProcClaudeJSON(t *testing.T, procRoot string, pid int, userHome string, contents string) string {
	t.Helper()
	relHome := strings.TrimPrefix(filepath.Clean(userHome), string(filepath.Separator))
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "root", relHome)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestObserver_ProjectMissingExact(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj-b"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{"/home/carl/proj-a":{}}}`)

	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectMissing {
		t.Fatalf("got %v want %v", got, ProjectMissing)
	}
}

func TestObserver_ProjectPresent_True(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{"/home/carl/proj":{"hasTrustDialogAccepted":true}}}`)

	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectPresent {
		t.Fatalf("got %v want %v", got, ProjectPresent)
	}
}

func TestObserver_ProjectPresent_FalseRequiresTrust(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{"/home/carl/proj":{"hasTrustDialogAccepted":false}}}`)

	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectMissing {
		t.Fatalf("got %v want %v", got, ProjectMissing)
	}
}

func TestObserver_ProjectPresent_Null(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{"/home/carl/proj":{"hasTrustDialogAccepted":null}}}`)

	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectPresent {
		t.Fatalf("got %v want %v", got, ProjectPresent)
	}
}

func TestObserver_ProjectPresent_NoTrustField(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{"/home/carl/proj":{}}}`)

	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectPresent {
		t.Fatalf("got %v want %v", got, ProjectPresent)
	}
}

func TestObserver_MalformedJSON_IsUnknown(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":`)

	if got := obs.Observe(uint64(pid), home, cwd); got != Unknown {
		t.Fatalf("got %v want %v", got, Unknown)
	}
}

func TestObserver_MissingFile_IsUnknown(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"

	if got := obs.Observe(uint64(pid), home, cwd); got != Unknown {
		t.Fatalf("got %v want %v", got, Unknown)
	}
}

func TestObserver_RelativePathsOrZeroPID_IsUnknown(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}

	if got := obs.Observe(0, "/home/carl", "/home/carl/proj"); got != Unknown {
		t.Fatalf("zero PID got %v want %v", got, Unknown)
	}
	if got := obs.Observe(123, "home/carl", "/home/carl/proj"); got != Unknown {
		t.Fatalf("relative home got %v want %v", got, Unknown)
	}
	if got := obs.Observe(123, "/home/carl", "proj"); got != Unknown {
		t.Fatalf("relative cwd got %v want %v", got, Unknown)
	}

	obsRelProc := Observer{ProcRoot: "proc"}
	if got := obsRelProc.Observe(123, "/home/carl", "/home/carl/proj"); got != Unknown {
		t.Fatalf("relative procRoot got %v want %v", got, Unknown)
	}
}

func TestObserver_OversizedInput_IsUnknown(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot, MaxBytes: 32}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{"/home/carl/proj":{}}}`+strings.Repeat(" ", 128))

	if got := obs.Observe(uint64(pid), home, cwd); got != Unknown {
		t.Fatalf("got %v want %v", got, Unknown)
	}
}

func TestObserver_ExactMatchNotPrefixMatch(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj/sub"
	writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{"/home/carl/proj":{}}}`)

	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectMissing {
		t.Fatalf("got %v want %v", got, ProjectMissing)
	}
}

func TestObserver_ReplacedClaudeJSONFollowedThroughProcRoot(t *testing.T) {
	procRoot := t.TempDir()
	obs := Observer{ProcRoot: procRoot}
	pid := 123
	home := "/home/carl"
	cwd := "/home/carl/proj"
	path := writeProcClaudeJSON(t, procRoot, pid, home, `{"projects":{}}`)

	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectMissing {
		t.Fatalf("first got %v want %v", got, ProjectMissing)
	}
	if err := os.WriteFile(path, []byte(`{"projects":{"/home/carl/proj":{"hasTrustDialogAccepted":null}}}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := obs.Observe(uint64(pid), home, cwd); got != ProjectPresent {
		t.Fatalf("second got %v want %v", got, ProjectPresent)
	}
}
