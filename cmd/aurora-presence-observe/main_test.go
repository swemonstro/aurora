package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
