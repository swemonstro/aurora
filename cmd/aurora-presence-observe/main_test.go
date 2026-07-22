package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

type testClock struct{ now time.Time }

func (clock testClock) Now() time.Time { return clock.now }

func TestDefaultObservationIsFiniteAndSanitized(t *testing.T) {
	root := cliProcFixture(t)
	var stdout, stderr bytes.Buffer
	waited := false
	err := run(
		[]string{"-proc-root", root, "-host-id", "host-test"},
		&stdout,
		&stderr,
		testClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)},
		func(time.Duration) { waited = true },
	)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %q", err, stderr.String())
	}
	if waited {
		t.Fatal("single default sample waited or behaved like a daemon")
	}
	var report outputReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode output: %v; output = %q", err, stdout.String())
	}
	if report.Mode != "observe-only" || report.Sample != 1 || report.Summary.ClaudeFamilies != 1 {
		t.Fatalf("report = %#v", report)
	}
	lower := strings.ToLower(stdout.String())
	for _, forbidden := range []string{"argv", "cmdline", "/sensitive/project", "api_key", "token="} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("standard output contains forbidden command data %q: %s", forbidden, stdout.String())
		}
	}
}

func TestHelpDeclaresObserveOnlyMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, &stdout, &stderr, testClock{}, func(time.Duration) {})
	if err == nil || !strings.Contains(strings.ToLower(stderr.String()), "observe-only") {
		t.Fatalf("help error = %v, stderr = %q", err, stderr.String())
	}
}

func TestObserverReportAggregatesRecognitionDiagnosticsDeterministically(t *testing.T) {
	observed := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	root := instancepresence.ProcessIdentity{PID: 101, StartedAt: observed.Add(-time.Second)}
	candidate := instancepresence.RuntimeCandidate{InstanceID: "observe-fixture", Tool: instancepresence.ToolClaude, Runtime: instancepresence.RuntimeIdentity{HostID: "host-a", BootID: "boot-a", RootProcess: root}, Members: []instancepresence.ProcessIdentity{root}}
	recognition := runtimerecognition.Result{
		UnknownProcesses:  2,
		Families:          []runtimerecognition.Family{{Candidate: candidate, ReasonCodes: []runtimerecognition.ReasonCode{runtimerecognition.ReasonIdentifiedAgentFamily, runtimerecognition.ReasonRootMissingChildAlive}}},
		UncertainFamilies: []runtimerecognition.UncertainFamily{{Tool: instancepresence.ToolCodex, PossibleRoots: []instancepresence.ProcessIdentity{root}, Members: []instancepresence.ProcessIdentity{root}, ReasonCodes: []runtimerecognition.ReasonCode{runtimerecognition.ReasonAmbiguousRoot, runtimerecognition.ReasonMultipleRoots}}},
	}
	sample := linuxprocess.Sample{Snapshot: instancepresence.ProcessSnapshot{ObservedAt: observed}, Diagnostics: []linuxprocess.Diagnostic{{Code: linuxprocess.ReasonPermissionDenied, Count: 3}, {Code: linuxprocess.ReasonCode(runtimerecognition.ReasonUnknownProcess), Count: 4}, {Code: linuxprocess.ReasonPIDReused, Count: 0}}}
	first := makeOutputReport(1, sample, recognition, nil)
	second := makeOutputReport(1, sample, recognition, nil)
	want := []linuxprocess.Diagnostic{
		{Code: linuxprocess.ReasonCode(runtimerecognition.ReasonAmbiguousRoot), Count: 1},
		{Code: linuxprocess.ReasonCode(runtimerecognition.ReasonIdentifiedAgentFamily), Count: 1},
		{Code: linuxprocess.ReasonCode(runtimerecognition.ReasonMultipleRoots), Count: 1},
		{Code: linuxprocess.ReasonPermissionDenied, Count: 3},
		{Code: linuxprocess.ReasonCode(runtimerecognition.ReasonRootMissingChildAlive), Count: 1},
		{Code: linuxprocess.ReasonCode(runtimerecognition.ReasonUnknownProcess), Count: 6},
	}
	if !reflect.DeepEqual(first.Diagnostics, want) || !reflect.DeepEqual(first.Diagnostics, second.Diagnostics) {
		t.Fatalf("diagnostics = %#v, want %#v", first.Diagnostics, want)
	}
	if first.Families[0].CandidateRef != string(candidate.InstanceID) || first.Uncertain[0].Tool != instancepresence.ToolCodex || first.Summary.UnknownProcesses != 2 {
		t.Fatalf("report = %#v", first)
	}
}

func cliProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"stat":                      "cpu 1 2 3 4\nbtime 1784707200\n",
		"sys/kernel/random/boot_id": "fixture-boot\n",
		"101/stat":                  cliStatLine(),
		"101/comm":                  "claude\n",
		"101/cmdline":               "/sensitive/project/claude\x00--token=not-output\x00",
		"101/status":                "Name:\tclaude\nUid:\t1000\t1000\t1000\t1000\n",
	}
	for name, contents := range files {
		filename := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func cliStatLine() string {
	fields := make([]string, 23)
	for index := range fields {
		fields[index] = "0"
	}
	fields[0], fields[1], fields[2], fields[3], fields[19] = "R", "1", "101", "10", "250"
	return "101 (claude) " + strings.Join(fields, " ")
}
