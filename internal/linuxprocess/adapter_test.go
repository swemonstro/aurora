package linuxprocess

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

func TestAdapterBuildsValidatedSnapshotFromInjectedProcRoot(t *testing.T) {
	root := newProcFixture(t)
	writeProcessFixture(t, root, 101, "worker", 1, 101, 10, 0, 250, []string{"/opaque/bin/worker", "--safe"})
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
	if got := sample.Snapshot.Processes[0].ExecutableIdentity; got != "exe:worker" {
		t.Fatalf("executable identity = %q", got)
	}
	if len(sample.Recognition.Processes[0].LaunchIdentities) != 0 {
		t.Fatalf("Linux backend assigned launch identities without configured rules: %#v", sample.Recognition.Processes[0])
	}
}

func TestAdapterKeepsCommAndArgvSignalsSeparateAndSanitized(t *testing.T) {
	root := newProcFixture(t)
	writeProcessFixture(t, root, 101, "worker", 1, 101, 10, 0, 250, []string{"/opaque/node", "/work/node_modules/@vendor/agent/bin.js", "--api-key=secret"})
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)}, LaunchIdentityRules: []runtimerecognition.LaunchIdentityRule{{Mode: runtimerecognition.LaunchRulePackagePath, Value: "@vendor/agent", Identity: "launch:agent", Argument: runtimerecognition.LaunchArgumentEntrypoint, Launchers: []string{"node"}}}})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := adapter.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	process := sample.Recognition.Processes[0]
	if process.CommIdentity != "exe:worker" || process.ExecutableIdentity != "exe:node" || len(process.LaunchIdentities) != 1 {
		t.Fatalf("recognition process = %#v", process)
	}
	if strings.Contains(fmt.Sprintf("%#v", process), "secret") || strings.Contains(fmt.Sprintf("%#v", process), "api-key") {
		t.Fatalf("sensitive argv was retained: %#v", process)
	}
}

func TestLaunchExecutableHandlesEmptyRelativeAndOddArguments(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"", ""},
		{"claude\x00", "claude"},
		{"./odd name!?\x00", "odd name!?"},
	} {
		if got := launchExecutable([]byte(test.input)); got != test.want {
			t.Fatalf("launchExecutable(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestLaunchIdentitiesUseOnlyDocumentedPositions(t *testing.T) {
	rules := []runtimerecognition.LaunchIdentityRule{
		{Mode: runtimerecognition.LaunchRuleExactBasename, Value: "wrapper-agent", Identity: "launch:wrapper", Argument: runtimerecognition.LaunchArgumentArgv0},
		{Mode: runtimerecognition.LaunchRulePackagePath, Value: "@vendor/agent", Identity: "launch:package", Argument: runtimerecognition.LaunchArgumentEntrypoint, Launchers: []string{"node"}},
	}
	tests := []struct {
		name string
		argv []string
		want []instancepresence.OpaqueIdentity
	}{
		{"wrapper argv0", []string{"/opaque/wrapper-agent"}, []instancepresence.OpaqueIdentity{"launch:wrapper"}},
		{"wrapper option is ignored", []string{"node", "--output=/tmp/wrapper-agent"}, nil},
		{"package entrypoint", []string{"node", "/work/node_modules/@vendor/agent/bin.js"}, []instancepresence.OpaqueIdentity{"launch:package"}},
		{"package cache is ignored", []string{"node", "--cache=/tmp/@vendor/agent/data"}, nil},
		{"empty argument cannot bypass bound", []string{"node", "", "--output=/tmp/wrapper-agent", "/work/node_modules/@vendor/agent/bin.js"}, nil},
		{"unrelated substring", []string{"/tmp/not-wrapper-agent-helper"}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := launchIdentities([]byte(joinCmdline(test.argv)), rules); len(got) != len(test.want) || len(got) > 0 && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("launchIdentities(%q) = %v, want %v", test.argv, got, test.want)
			}
		})
	}
}

func TestAdapterRetainsSafeCommWhenArgvIsUnavailable(t *testing.T) {
	root := newProcFixture(t)
	writeProcessFixture(t, root, 101, "worker", 1, 101, 10, 0, 250, []string{"/private/worker", "--secret=value"})
	adapter, err := newWithReader(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)}}, func() (procReader, error) {
		base, err := openProcRoot(root)
		if err != nil {
			return nil, err
		}
		return &faultReader{procReader: base, path: "101/cmdline", call: 1, err: fs.ErrPermission, calls: make(map[string]int)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := adapter.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	process := sample.Recognition.Processes[0]
	if process.CommIdentity != "exe:worker" || process.ExecutableIdentity != "exe:unknown" || !hasDiagnostic(sample.Diagnostics, ReasonPermissionDenied) {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestAdapterDoesNotExposeManipulatedSensitiveArgv0(t *testing.T) {
	for _, test := range []struct {
		argv0 string
		want  instancepresence.OpaqueIdentity
	}{
		{"/private/secret-tool", "exe:secret-tool"},
		{"/private/token-helper", "exe:token-helper"},
		{"/private/password-store", "exe:password-store"},
		{"/private/monkey", "exe:monkey"},
		{"/private/turnkey", "exe:turnkey"},
		{"/private/keyring", "exe:keyring"},
		{"--api-key=secret-value", "exe:unknown"},
	} {
		t.Run(test.argv0, func(t *testing.T) {
			root := newProcFixture(t)
			writeProcessFixture(t, root, 101, "worker", 1, 101, 10, 0, 250, []string{test.argv0, "--token=also-secret"})
			adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)}})
			if err != nil {
				t.Fatal(err)
			}
			sample, err := adapter.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			public := sample.Snapshot.Processes[0]
			recognition := sample.Recognition.Processes[0]
			if public.ExecutableIdentity != test.want || recognition.ExecutableIdentity != test.want || strings.Contains(fmt.Sprintf("%#v %#v", public, recognition), "/private/") || strings.Contains(fmt.Sprintf("%#v %#v", public, recognition), "also-secret") {
				t.Fatalf("sensitive argv0 escaped: %#v %#v", public, recognition)
			}
		})
	}
}

func TestAdapterExposesParentOnlyAfterGenerationValidation(t *testing.T) {
	root := newProcFixture(t)
	writeProcessFixture(t, root, 101, "parent", 1, 101, 10, 0, 250, []string{"parent"})
	writeProcessFixture(t, root, 102, "child", 101, 101, 10, 0, 300, []string{"child"})
	adapter, err := New(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := adapter.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Recognition.Processes[1].Parent == nil || sample.Recognition.Processes[1].Parent.PID != 101 {
		t.Fatalf("parent was not verified: %#v", sample.Recognition.Processes)
	}

	faulty, err := newWithReader(Config{ProcRoot: root, HostID: "host-a", BootID: "boot-a", Clock: fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)}}, func() (procReader, error) {
		base, openErr := openProcRoot(root)
		if openErr != nil {
			return nil, openErr
		}
		return &faultReader{procReader: base, path: "101/stat", call: 3, err: fs.ErrNotExist, calls: make(map[string]int)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sample, err = faulty.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Recognition.Processes[1].Parent != nil {
		t.Fatalf("unverified parent was retained: %#v", sample.Recognition.Processes[1])
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
			writeProcessFixture(t, root, 101, "worker", 1, 101, 10, 0, 250, []string{"worker"})
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
	if err := os.WriteFile(outside, []byte(statLine(101, "worker", 1, 101, 10, 0, 250)), 0o644); err != nil {
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

func TestParseCmdlineArgvStructuralClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		want []string
	}{
		{name: "bare", argv: []string{"/usr/bin/codex"}, want: []string{"codex"}},
		{name: "help long", argv: []string{"codex", "--help"}, want: []string{"codex", "help"}},
		{name: "help short", argv: []string{"codex", "-h"}, want: []string{"codex", "help"}},
		{name: "version long", argv: []string{"codex", "--version"}, want: []string{"codex", "version"}},
		{name: "version short", argv: []string{"codex", "-V"}, want: []string{"codex", "version"}},
		{name: "app", argv: []string{"codex", "app"}, want: []string{"codex", "app"}},
		{name: "profile then exec", argv: []string{"codex", "--profile", "business", "exec", "ls"}, want: []string{"codex", "exec"}},
		{name: "config flag then login", argv: []string{"codex", "-c", "key=value", "login"}, want: []string{"codex", "login"}},
		{name: "model only", argv: []string{"codex", "--model", "gpt"}, want: []string{"codex"}},
		{name: "model and free prompt", argv: []string{"codex", "--model", "gpt", "hemlig prompt"}, want: []string{"codex"}},
		{name: "exec", argv: []string{"codex", "exec", "ls"}, want: []string{"codex", "exec"}},
		{name: "login", argv: []string{"codex", "login"}, want: []string{"codex", "login"}},
		{name: "config", argv: []string{"codex", "config"}, want: []string{"codex", "config"}},
		{name: "status", argv: []string{"codex", "status"}, want: []string{"codex", "status"}},
		{name: "resume", argv: []string{"codex", "resume", "sess-id"}, want: []string{"codex", "resume"}},
		{
			name: "node package wrapper exec",
			argv: []string{"node", "/opt/node_modules/@openai/codex/bin/codex.js", "exec", "ls"},
			want: []string{"codex", "exec"},
		},
		{
			name: "node package wrapper interactive",
			argv: []string{"node", "/opt/node_modules/@openai/codex/bin/codex.js"},
			want: []string{"codex"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := parseCmdlineArgv([]byte(joinCmdline(test.argv)), maxRecognitionArgv)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseCmdlineArgv(%q) = %#v, want %#v", test.argv, got, test.want)
			}
			joined := strings.Join(got, " ")
			if strings.Contains(joined, "hemlig") || strings.Contains(joined, "business") ||
				strings.Contains(joined, "gpt") || strings.Contains(joined, "key=value") ||
				strings.Contains(joined, "sess-id") || strings.Contains(joined, "/opt/") ||
				strings.Contains(joined, "node_modules") {
				t.Fatalf("sensitive path or free-form token retained: %#v", got)
			}
		})
	}
}

func TestAdapterObserveWorkingDirectoryAndCodexHome(t *testing.T) {
	root := newProcFixture(t)
	cwdTarget := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(cwdTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProcessFixture(t, root, 101, "codex", 1, 101, 10, 0, 250, []string{"/usr/bin/codex"})
	if err := os.Symlink(cwdTarget, filepath.Join(root, "101", "cwd")); err != nil {
		t.Fatal(err)
	}
	environ := "CODEX_HOME=/tmp/codex-profile\x00OPENAI_API_KEY=extremely-secret\x00OTHER_TOKEN=also-secret\x00"
	writeFixtureFile(t, root, "101/environ", environ)

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
	if len(sample.Recognition.Processes) != 1 {
		t.Fatalf("processes = %#v", sample.Recognition.Processes)
	}
	rec := sample.Recognition.Processes[0]
	if rec.WorkingDirectory != filepath.Clean(cwdTarget) {
		t.Fatalf("WorkingDirectory = %q, want %q", rec.WorkingDirectory, cwdTarget)
	}
	if rec.EnvCodexHome != "/tmp/codex-profile" {
		t.Fatalf("EnvCodexHome = %q", rec.EnvCodexHome)
	}
	// Public snapshot must not carry recognition-local trust fields.
	pubDump := fmt.Sprintf("%#v", sample.Snapshot)
	if strings.Contains(pubDump, "WorkingDirectory") || strings.Contains(pubDump, "EnvCodexHome") ||
		strings.Contains(pubDump, "extremely-secret") || strings.Contains(pubDump, "also-secret") ||
		strings.Contains(pubDump, cwdTarget) {
		t.Fatalf("public snapshot leaked recognition-local data: %s", pubDump)
	}
	recDump := fmt.Sprintf("%#v", rec)
	if strings.Contains(recDump, "extremely-secret") || strings.Contains(recDump, "also-secret") ||
		strings.Contains(recDump, "OPENAI_API_KEY") || strings.Contains(recDump, "OTHER_TOKEN") {
		t.Fatalf("secrets retained in recognition observation: %s", recDump)
	}
}

func TestAdapterCodexHomeRejectionAndMissing(t *testing.T) {
	for _, test := range []struct {
		name    string
		environ string
		want    string
	}{
		{name: "missing", environ: "PATH=/usr/bin\x00", want: ""},
		{name: "empty", environ: "CODEX_HOME=\x00", want: ""},
		{name: "relative", environ: "CODEX_HOME=relative/home\x00", want: ""},
		{name: "absolute", environ: "CODEX_HOME=/var/codex/home\x00", want: "/var/codex/home"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newProcFixture(t)
			writeProcessFixture(t, root, 101, "codex", 1, 101, 10, 0, 250, []string{"codex"})
			writeFixtureFile(t, root, "101/environ", test.environ)
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
			if got := sample.Recognition.Processes[0].EnvCodexHome; got != test.want {
				t.Fatalf("EnvCodexHome = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRootReaderReadLinkCwd(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "101"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "workdir")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "101", "cwd")); err != nil {
		t.Fatal(err)
	}
	reader := openTestRootReader(t, root)
	got, err := reader.ReadLink("101/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("ReadLink = %q, want %q", got, target)
	}
	// Absolute / traversing proc-relative names are rejected.
	if _, err := reader.ReadLink("/101/cwd"); !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("absolute path error = %v", err)
	}
	if _, err := reader.ReadLink("../101/cwd"); !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("traversal error = %v", err)
	}
}

func TestRootReaderReadLinkRejectsSymlinkPIDDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(filepath.Join(t.TempDir(), "cwd-target"), filepath.Join(outside, "cwd")); err != nil {
		// Create a normal target for the outer fake pid dir.
		_ = os.MkdirAll(outside, 0o755)
		if err := os.Symlink(t.TempDir(), filepath.Join(outside, "cwd")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "101")); err != nil {
		t.Fatal(err)
	}
	reader := openTestRootReader(t, root)
	if _, err := reader.ReadLink("101/cwd"); !errors.Is(err, ErrUnsafeProcEntry) {
		t.Fatalf("ReadLink via symlink PID dir error = %v, want %v", err, ErrUnsafeProcEntry)
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
