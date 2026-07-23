//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/swemonstro/aurora/internal/localhooktransport"
)

// lockedBuffer is a race-safe stderr sink for tests that start background bridges.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunRequiresExplicitIdentityAndSafeSocketDefault(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), nil, &output, &output, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "host-id") {
		t.Fatalf("error = %v", err)
	}
	if err := run(context.Background(), []string{"-host-id", "host-fixture"}, &output, &output, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("error = %v", err)
	}
}

func TestHelpStatesBindingFlag(t *testing.T) {
	var output bytes.Buffer
	_ = run(context.Background(), []string{"-help"}, &output, &output, func(string) string { return "" })
	if !strings.Contains(output.String(), "verified per-instance binding") || !strings.Contains(output.String(), "AURORA_LOCAL_HOOK_ENABLED") {
		t.Fatalf("help = %s", output.String())
	}
	if !strings.Contains(output.String(), "-relay") {
		t.Fatalf("help missing -relay: %s", output.String())
	}
}

func TestRelayBridgeDefaultOff(t *testing.T) {
	socketPath := testSocketPath(t)
	stderr := &lockedBuffer{}
	server, cleanup, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath},
		stderr,
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	defer server.Close()
	if strings.Contains(stderr.String(), "relay bridge") {
		t.Fatalf("relay bridge started by default: %s", stderr.String())
	}
}

func TestRelayFlagRequiresAbsoluteHTTP(t *testing.T) {
	socketPath := testSocketPath(t)
	stderr := &lockedBuffer{}
	_, _, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath, "-relay", "not-a-url"},
		stderr,
		func(string) string { return "" },
	)
	if err == nil {
		t.Fatal("invalid relay accepted")
	}
}

func TestRelayEnvUsedWhenFlagEmpty(t *testing.T) {
	socketPath := testSocketPath(t)
	stderr := &lockedBuffer{}
	server, cleanup, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath},
		stderr,
		func(key string) string {
			if key == EnvRelayURL {
				return "http://127.0.0.1:9"
			}
			return ""
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		cleanup()
	}
	defer server.Close()
	if !strings.Contains(stderr.String(), "relay bridge") {
		t.Fatalf("env relay not enabled: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "V2") || !strings.Contains(stderr.String(), "presentation") {
		t.Fatalf("presentation bridge not mentioned: %s", stderr.String())
	}
}

func TestRelayStartsPresentationAndV1Bridges(t *testing.T) {
	socketPath := testSocketPath(t)
	stderr := &lockedBuffer{}
	server, cleanup, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath, "-relay", "http://127.0.0.1:9"},
		stderr,
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		cleanup()
	}
	defer server.Close()
	log := stderr.String()
	if !strings.Contains(log, "V1 aggregate") {
		t.Fatalf("missing V1 bridge log: %s", log)
	}
	if !strings.Contains(log, "5-pixel presentation") {
		t.Fatalf("missing V2 presentation log: %s", log)
	}
}

func TestIngestRemainsDisabledWhenFeatureFlagUnsetOrFalse(t *testing.T) {
	for _, value := range []string{"", "0", "false", "yes"} {
		t.Run("value_"+value, func(t *testing.T) {
			socketPath := testSocketPath(t)
			var stderr bytes.Buffer
			server, cleanup, _, err := composeServer(
				[]string{"-host-id", "host-fixture", "-socket", socketPath},
				&stderr,
				func(key string) string {
					if key == localhooktransport.EnvLocalHookEnabled {
						return value
					}
					return ""
				},
			)
			if err != nil {
				t.Fatalf("composeServer() error = %v stderr=%s", err, stderr.String())
			}
			if cleanup != nil {
				defer cleanup()
			}
			defer server.Close()
			if server.IngestReceiver() != nil {
				t.Fatalf("ingest enabled for flag value %q", value)
			}
			if server.IdentityObserverEnabled() {
				t.Fatal("identity measure enabled without flag")
			}
		})
	}
}

func TestIngestEnabledWhenFeatureFlagTrue(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", " true "} {
		t.Run("value_"+strings.TrimSpace(value), func(t *testing.T) {
			socketPath := testSocketPath(t)
			var stderr bytes.Buffer
			server, cleanup, _, err := composeServer(
				[]string{"-host-id", "host-fixture", "-socket", socketPath},
				&stderr,
				func(key string) string {
					if key == localhooktransport.EnvLocalHookEnabled {
						return value
					}
					return ""
				},
			)
			if err != nil {
				t.Fatalf("composeServer() error = %v stderr=%s", err, stderr.String())
			}
			if cleanup != nil {
				defer cleanup()
			}
			defer server.Close()
			if server.IngestReceiver() == nil {
				t.Fatalf("ingest disabled for flag value %q", value)
			}
		})
	}
}

func TestIdentityMeasureDefaultOffAndExplicitEnable(t *testing.T) {
	socketPath := testSocketPath(t)
	measurePath := filepath.Join(filepath.Dir(socketPath), "identity-measure.jsonl")

	var stderr bytes.Buffer
	server, cleanup, _, err := composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath},
		&stderr,
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatalf("composeServer() error = %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	server.Close()
	if server.IdentityObserverEnabled() {
		t.Fatal("identity measure should be off by default")
	}

	stderr.Reset()
	server, cleanup, _, err = composeServer(
		[]string{"-host-id", "host-fixture", "-socket", socketPath, "-identity-measure-file", measurePath},
		&stderr,
		func(key string) string {
			if key == localhooktransport.EnvLocalHookEnabled {
				return "true"
			}
			return ""
		},
	)
	if err != nil {
		t.Fatalf("composeServer() with measure error = %v stderr=%s", err, stderr.String())
	}
	if cleanup != nil {
		defer cleanup()
	}
	defer server.Close()
	if !server.IdentityObserverEnabled() {
		t.Fatal("identity measure not enabled with flag")
	}
	if !strings.Contains(stderr.String(), "identity measure JSONL") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestLegacyStartupValidWithAndWithoutIngest(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled string
		want    bool
	}{
		{name: "legacy default", enabled: "", want: false},
		{name: "package 6 enabled", enabled: "true", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			socketPath := testSocketPath(t)
			var stderr bytes.Buffer
			server, cleanup, _, err := composeServer(
				[]string{"-host-id", "host-fixture", "-socket", socketPath},
				&stderr,
				func(key string) string {
					if key == localhooktransport.EnvLocalHookEnabled {
						return test.enabled
					}
					return ""
				},
			)
			if err != nil {
				t.Fatalf("composeServer() error = %v stderr=%s", err, stderr.String())
			}
			if cleanup != nil {
				defer cleanup()
			}
			defer server.Close()
			if got := server.IngestReceiver() != nil; got != test.want {
				t.Fatalf("ingest attached = %t, want %t", got, test.want)
			}
			// Serving must start and stop cleanly for both compositions.
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Serve() error = %v", err)
			}
		})
	}
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-presence-local-server-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove test directory: %v", err)
		}
	})
	return filepath.Join(directory, "presence-hook.sock")
}
