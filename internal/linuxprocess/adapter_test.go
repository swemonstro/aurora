package linuxprocess

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

func TestAdapterBuildsValidatedSnapshotFromInjectedProcRoot(t *testing.T) {
	root := newProcFixture(t)
	writeProcessFixture(t, root, 101, "claude", 1, 101, 10, 0, 250, []string{"/opaque/bin/claude", "--safe"})
	clock := fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)}
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: clock, ClockTicks: 100})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sample, err := adapter.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if err := sample.Snapshot.Validate(); err != nil {
		t.Fatalf("snapshot validation error = %v", err)
	}
	if len(sample.Snapshot.Processes) != 1 || sample.Snapshot.Processes[0].Process.PID != 101 {
		t.Fatalf("snapshot = %#v", sample.Snapshot)
	}
	if got := sample.Snapshot.Processes[0].ExecutableIdentity; got != "exe:claude" {
		t.Fatalf("executable identity = %q", got)
	}
	if len(sample.Families) != 1 || sample.Summary.ClaudeFamilies != 1 {
		t.Fatalf("sample families = %#v, summary = %#v", sample.Families, sample.Summary)
	}
}

func TestAdapterReadsBootIdentityFromNarrowProcSource(t *testing.T) {
	root := newProcFixture(t)
	adapter, err := New(Config{
		ProcRoot: root, HostID: "host-a",
		Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := adapter.BootIdentity(context.Background())
	if err != nil || bootID != "fixture-boot-id" {
		t.Fatalf("BootIdentity() = %q, %v", bootID, err)
	}
}

func TestAdapterGroupsClaudeAndCodexWrapperNodeNativeFamilies(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		wrapperArg string
		nodeArg    string
		native     string
	}{
		{name: "claude", tool: "claude", wrapperArg: "/opaque/aurora-claude", nodeArg: "/opaque/@anthropic-ai/claude-code/cli.js", native: "claude-native-worker"},
		{name: "codex", tool: "codex", wrapperArg: "/opaque/aurora-codex", nodeArg: "/opaque/@openai/codex/bin/codex.js", native: "codex-linux-x86_64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newProcFixture(t)
			writeProcessFixture(t, root, 101, "bash", 1, 101, 10, 0, 250, []string{"bash", test.wrapperArg})
			writeProcessFixture(t, root, 102, "node", 101, 101, 10, 0, 251, []string{"node", test.nodeArg})
			writeProcessFixture(t, root, 103, test.native, 102, 101, 10, 0, 252, []string{"/opaque/" + test.native})
			adapter, err := New(Config{
				ProcRoot: root, HostID: "host-a", BootID: "boot-a",
				Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
			})
			if err != nil {
				t.Fatal(err)
			}
			sample, err := adapter.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(sample.Families) != 1 || string(sample.Families[0].Candidate.Tool) != test.tool ||
				len(sample.Families[0].Candidate.Members) != 3 {
				t.Fatalf("families = %#v", sample.Families)
			}
		})
	}
}

func TestAdapterKeepsParallelToolFamiliesSeparate(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			root := newProcFixture(t)
			writeProcessFixture(t, root, 101, tool, 1, 101, 10, 0, 250, []string{tool})
			writeProcessFixture(t, root, 202, tool, 1, 202, 20, 0, 300, []string{tool})
			adapter, err := New(Config{
				ProcRoot: root, HostID: "host-a", BootID: "boot-a",
				Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
			})
			if err != nil {
				t.Fatal(err)
			}
			sample, err := adapter.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(sample.Families) != 2 || sample.Families[0].Candidate.Runtime.RootProcess.PID != 101 ||
				sample.Families[1].Candidate.Runtime.RootProcess.PID != 202 {
				t.Fatalf("parallel families = %#v", sample.Families)
			}
		})
	}
}

func TestAdapterHandlesPerProcessRacesConservatively(t *testing.T) {
	tests := []struct {
		name      string
		fault     error
		faultCall int
		reason    ReasonCode
	}{
		{name: "disappears after initial stat", fault: fs.ErrNotExist, faultCall: 2, reason: ReasonProcessDisappeared},
		{name: "permission denied", fault: fs.ErrPermission, faultCall: 1, reason: ReasonPermissionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newProcFixture(t)
			writeProcessFixture(t, root, 101, "claude", 1, 101, 10, 0, 250, []string{"claude"})
			writeProcessFixture(t, root, 202, "other", 1, 202, 20, 0, 300, []string{"other"})
			config := Config{
				ProcRoot: root, HostID: "host-a", BootID: "boot-a",
				Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
			}
			adapter, err := newWithReader(config, func() (procReader, error) {
				base, openErr := openProcRoot(root)
				if openErr != nil {
					return nil, openErr
				}
				return &faultReader{
					procReader: base, path: "101/stat", call: test.faultCall, err: test.fault,
					calls: make(map[string]int),
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			sample, err := adapter.Observe(context.Background())
			if err != nil {
				t.Fatalf("Observe() error = %v", err)
			}
			if len(sample.Snapshot.Processes) != 1 || sample.Snapshot.Processes[0].Process.PID != 202 {
				t.Fatalf("snapshot = %#v", sample.Snapshot)
			}
			if !hasDiagnostic(sample.Diagnostics, test.reason) {
				t.Fatalf("diagnostics = %#v, want %q", sample.Diagnostics, test.reason)
			}
			if _, uncertain := sample.uncertainPIDs[101]; !uncertain {
				t.Fatal("unreadable PID was not marked uncertain")
			}
		})
	}
}

func TestAdapterRejectsSymlinkProcRoot(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "proc-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{
		ProcRoot: link, HostID: "host-a", BootID: "boot-a",
		Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
	})
	if !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("New(symlink root) error = %v, want %v", err, ErrUnsafeProcEntry)
	}
}

func TestAdapterRejectsSymlinkProcessFilesAndContinues(t *testing.T) {
	root := newProcFixture(t)
	outside := filepath.Join(t.TempDir(), "outside-stat")
	if err := os.WriteFile(outside, []byte(statLine(101, "claude", 1, 101, 10, 0, 250)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "101"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "101", "stat")); err != nil {
		t.Fatal(err)
	}
	writeProcessFixture(t, root, 202, "other", 1, 202, 20, 0, 300, []string{"other"})
	adapter, err := New(Config{
		ProcRoot: root, HostID: "host-a", BootID: "boot-a",
		Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := adapter.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Snapshot.Processes) != 1 || sample.Snapshot.Processes[0].Process.PID != 202 {
		t.Fatalf("snapshot = %#v", sample.Snapshot)
	}
	if !hasDiagnostic(sample.Diagnostics, ReasonInvalidProcData) {
		t.Fatalf("diagnostics = %#v", sample.Diagnostics)
	}
}

func TestAdapterSkipsMalformedProcessStatAndContinues(t *testing.T) {
	root := newProcFixture(t)
	writeFixtureFile(t, root, "101/stat", "101 malformed")
	writeProcessFixture(t, root, 202, "other", 1, 202, 20, 0, 300, []string{"other"})
	adapter, err := New(Config{
		ProcRoot: root, HostID: "host-a", BootID: "boot-a",
		Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := adapter.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Snapshot.Processes) != 1 || !hasDiagnostic(sample.Diagnostics, ReasonInvalidProcData) {
		t.Fatalf("sample = %#v", sample)
	}
}

type faultReader struct {
	procReader
	path  string
	call  int
	err   error
	calls map[string]int
}

func (reader *faultReader) ReadFile(name string, limit int64) ([]byte, error) {
	reader.calls[name]++
	if name == reader.path && reader.calls[name] == reader.call {
		return nil, reader.err
	}
	return reader.procReader.ReadFile(name, limit)
}

func newProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, root, "stat", "cpu 1 2 3 4\nbtime 1784707200\n")
	writeFixtureFile(t, root, "sys/kernel/random/boot_id", "fixture-boot-id\n")
	return root
}

func writeProcessFixture(
	t *testing.T,
	root string,
	pid uint64,
	comm string,
	ppid, group, session uint64,
	tty int64,
	startTicks uint64,
	arguments []string,
) {
	t.Helper()
	directory := filepath.Join(root, stringPID(pid))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "stat"), statLine(pid, comm, ppid, group, session, tty, startTicks))
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "comm"), comm+"\n")
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "cmdline"), joinCmdline(arguments))
	writeFixtureFile(t, root, filepath.Join(stringPID(pid), "status"), "Name:\tfixture\nUid:\t1000\t1000\t1000\t1000\n")
}

func writeFixtureFile(t *testing.T, root, name, contents string) {
	t.Helper()
	filename := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func joinCmdline(arguments []string) string {
	value := ""
	for _, argument := range arguments {
		value += argument + "\x00"
	}
	return value
}

func stringPID(pid uint64) string {
	const digits = "0123456789"
	if pid == 0 {
		return "0"
	}
	buffer := make([]byte, 0, 20)
	for pid > 0 {
		buffer = append(buffer, digits[pid%10])
		pid /= 10
	}
	for first, last := 0, len(buffer)-1; first < last; first, last = first+1, last-1 {
		buffer[first], buffer[last] = buffer[last], buffer[first]
	}
	return string(buffer)
}

func hasDiagnostic(diagnostics []Diagnostic, code ReasonCode) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && diagnostic.Count > 0 {
			return true
		}
	}
	return false
}
