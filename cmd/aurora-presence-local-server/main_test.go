//go:build linux

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunRequiresExplicitIdentityAndSafeSocketDefault(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), nil, &output, &output, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "host-id") {
		t.Fatalf("error = %v", err)
	}
	if err := run(context.Background(), []string{"-host-id", "host-fixture"}, &output, &output, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("error = %v", err)
	}
}

func TestHelpStatesObserveOnly(t *testing.T) {
	var output bytes.Buffer
	_ = run(context.Background(), []string{"-help"}, &output, &output, func(string) string { return "" })
	if !strings.Contains(output.String(), "observe-only") || !strings.Contains(output.String(), "never performs a binding") {
		t.Fatalf("help = %s", output.String())
	}
}
