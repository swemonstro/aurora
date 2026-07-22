package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

func TestDefaultRunIsFiniteObserveOnly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(nil, strings.NewReader("must not be read"), &stdout, &stderr, fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var report outputReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Mode != "observe-only" || report.Summary.Runtimes != 0 || report.Summary.Hooks != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestMixedFixtureOutputIsDeterministicAndSanitized(t *testing.T) {
	arguments := []string{"-input", "testdata/mixed.json"}
	clock := fixedClock{now: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)}
	var first, second, stderr bytes.Buffer
	if err := run(arguments, strings.NewReader(""), &first, &stderr, clock); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := run(arguments, strings.NewReader(""), &second, &stderr, clock); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("output differs:\n%s\n%s", first.String(), second.String())
	}
	var report outputReport
	if err := json.Unmarshal(first.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Summary.Exact != 1 || report.Summary.Ambiguous != 1 || report.Summary.Rejected == 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if report.Risk.Labeled != 2 || report.Risk.TruePositive != 1 || report.Risk.TrueNegative != 1 ||
		report.Risk.FalsePositive != 0 || report.Risk.FalseNegative != 0 {
		t.Fatalf("risk = %#v", report.Risk)
	}
	lower := strings.ToLower(first.String())
	for _, forbidden := range []string{"argv", "cwd", "prompt", "transcript", "terminal_output", "uid"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("output contains forbidden field %q: %s", forbidden, first.String())
		}
	}
}

func TestStdinInputIsExplicit(t *testing.T) {
	input, err := os.ReadFile("testdata/mixed.json")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-input", "-"}, bytes.NewReader(input), &stdout, &stderr, fixedClock{now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() == 0 {
		t.Fatal("stdin correlation produced no report")
	}
}

func TestInvalidOrSensitiveShapedInputFails(t *testing.T) {
	input := `{"evaluated_at":"2026-07-22T10:00:00Z","runtimes":[],"hooks":[],"prompt":"must be rejected"}`
	var stdout, stderr bytes.Buffer
	err := run([]string{"-input", "-"}, strings.NewReader(input), &stdout, &stderr, fixedClock{now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestMalformedInputFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"-input", "-"}, strings.NewReader(`{"hooks":`), &stdout, &stderr, fixedClock{now: time.Now()})
	if err == nil {
		t.Fatal("malformed input accepted")
	}
}
