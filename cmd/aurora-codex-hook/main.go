package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/agent"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/codexproducer"
	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/sourcelifecycle"
)

const hookTimeout = time.Second
const transcriptPollInterval = 100 * time.Millisecond
const lifecycleLockTimeout = 3 * time.Second

// startPermissionWatcher launches the detached transcript watcher. Tests may
// replace it to avoid re-execing the test binary.
var startPermissionWatcher = startWatcherProcess

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--session-end-file" {
		_ = runSessionEndFile(
			context.Background(),
			os.Args[2],
			os.Getenv,
		)
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--watch-permission" {
		var watch codexhook.PermissionWatch
		if json.Unmarshal([]byte(os.Args[2]), &watch) != nil {
			return
		}
		_ = watchPermission(context.Background(), watch, os.Getenv)
		return
	}

	_ = run(context.Background(), os.Stdin, os.Getenv)
}

func run(
	ctx context.Context,
	input io.Reader,
	getenv func(string) string,
) error {
	rawInput, err := codexhook.ReadInput(input)
	if err != nil {
		return err
	}

	event, err := codexhook.ParseEvent(rawInput)
	if err != nil {
		return err
	}

	// In local multi-instance mode, deliver the verified Codex event to
	// Package 6 ingress. Transport failures remain fail-open so Codex itself
	// is never blocked by Aurora.
	localEnabled := localhooktransport.LocalHookEnabled(
		getenv(localhooktransport.EnvLocalHookEnabled),
	)
	if ingress, ingressErr := codexhook.LocalIngressObservation(event); ingressErr == nil {
		localhooktransport.TryDeliverIngress(ctx, getenv, ingress)
		// Deferred so it always runs after every ordinary delivery path
		// below (trackLocalPermission or the legacy relay publish +
		// permission-watcher start), regardless of which one this call
		// takes or how it returns — shadow forwarding must never precede
		// or delay the real flow it shadows. See deliverShadow's own doc
		// comment for its bounded, fail-open timeout contract once it does
		// run.
		defer deliverShadow(ctx, getenv, ingress)
	}
	if localEnabled {
		// Keep session-store + permission watchers so Esc/turn_aborted can
		// clear attention via local ingress. Never publish to the legacy relay.
		return trackLocalPermission(event, getenv)
	}

	config, err := codexhook.ConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return err
	}

	if err := codexhook.RecordSessionID(config.SessionIDPath, event); err != nil {
		return err
	}

	store, err := codexhook.NewSessionStore(config.StatePath, config.TTL)
	if err != nil {
		return err
	}

	var watch *codexhook.PermissionWatch
	err = sourcelifecycle.WithLock(config.StatePath, lifecycleLockTimeout, func() error {
		update, supported, updateErr := store.UpdateLifecycle(event)
		if updateErr != nil || !supported {
			return updateErr
		}
		if publishErr := publishLifecycle(ctx, config, update); publishErr != nil {
			return publishErr
		}
		watch = update.Watch
		return nil
	})
	if err != nil {
		return err
	}

	if watch != nil {
		return startPermissionWatcher(*watch, getenv)
	}
	return nil
}

// trackLocalPermission updates the session store and starts transcript watchers
// without any legacy relay publication. Permission cancel recovery then delivers
// idle through the local Unix socket for the same session_id only.
func trackLocalPermission(event codexhook.Event, getenv func(string) string) error {
	config, err := codexhook.ConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return err
	}
	store, err := codexhook.NewSessionStore(config.StatePath, config.TTL)
	if err != nil {
		return err
	}
	var watch *codexhook.PermissionWatch
	err = sourcelifecycle.WithLock(config.StatePath, lifecycleLockTimeout, func() error {
		update, supported, updateErr := store.UpdateLifecycle(event)
		if updateErr != nil || !supported {
			return updateErr
		}
		watch = update.Watch
		return nil
	})
	if err != nil {
		return err
	}
	if watch != nil {
		return startPermissionWatcher(*watch, getenv)
	}
	return nil
}

// EnvShadowConnectTimeoutMS and EnvShadowWriteTimeoutMS optionally override
// codexproducer's default shadow connect/write timeouts (each in whole
// milliseconds, > 0). Unset or unparseable values silently fall back to the
// package defaults — an invalid override must fail open onto a safe
// default, never propagate an error or disable shadow forwarding entirely.
// A positive but absurdly large override (a typo, minutes mistaken for
// milliseconds) is still hard-capped by
// codexproducer.MaxShadowConnectTimeout / MaxShadowWriteTimeout inside
// ShadowDeliveryConfig.withDefaults — this env layer only parses the value,
// it never widens the hard ceiling codexproducer itself enforces.
const (
	EnvShadowConnectTimeoutMS = "AURORA_CODEX_SHADOW_CONNECT_TIMEOUT_MS"
	EnvShadowWriteTimeoutMS   = "AURORA_CODEX_SHADOW_WRITE_TIMEOUT_MS"
)

// deliverShadow is a package variable (like startPermissionWatcher above) so
// tests can substitute a deterministic fake instead of exercising real
// socket timing to verify run()/watchPermission()'s own behavior (ordinary
// delivery unaffected, exit code unchanged) — see deliverShadowDefault for
// the production implementation's actual timeout contract, and
// internal/codexproducer's own tests for deterministic proof that the
// underlying connect/write timeouts are enforced.
var deliverShadow = deliverShadowDefault

// testHookBeforeRecoveryAttempt is a narrow, package-private test
// synchronization point (never exported, never referenced by any
// production code path besides the one call site in watchPermission): it
// always defaults to a no-op, and production code never assigns anything
// else to it. Tests that need to deterministically reproduce the race
// between this watcher's own recovery attempt and a concurrent, real Stop
// hook invocation substitute a function here instead of coordinating with
// sleeps or goroutines racing a real file lock.
var testHookBeforeRecoveryAttempt = func() {}

// deliverShadowDefault best-effort forwards an already-sanitized ingress
// observation to a standalone aurora-codex-presence producer's hook ingress
// socket, for shadow-mode evaluation only. It is fully opt-in
// (AURORA_CODEX_SHADOW_SOCKET unset by default = no-op) and fail-open: any
// failure here — the env var unset, an invalid override, the socket
// missing, a dial or write error, or either timeout firing — must never
// affect, delay, or fail the real Codex hook flow this function shadows, so
// its result is deliberately ignored, and it never logs or returns error
// text that could contain the observation, socket path, or session id.
func deliverShadowDefault(ctx context.Context, getenv func(string) string, ingress hookadapter.IngressObservation) {
	socketPath := strings.TrimSpace(getenv(codexproducer.EnvShadowSocket))
	if socketPath == "" {
		return
	}
	config := codexproducer.ShadowDeliveryConfig{
		ConnectTimeout: shadowTimeoutFromEnv(getenv, EnvShadowConnectTimeoutMS, codexproducer.DefaultShadowConnectTimeout),
		WriteTimeout:   shadowTimeoutFromEnv(getenv, EnvShadowWriteTimeoutMS, codexproducer.DefaultShadowWriteTimeout),
	}
	codexproducer.TryDeliverShadowWithConfig(ctx, config, socketPath, ingress)
}

// shadowTimeoutFromEnv parses a positive whole-millisecond override from
// name, falling back to fallback for anything unset, unparseable, or
// non-positive — never erroring, since an invalid configuration value must
// fail open onto a safe default rather than disable shadow forwarding or
// propagate an error.
func shadowTimeoutFromEnv(getenv func(string) string, name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback
	}
	milliseconds, err := strconv.Atoi(value)
	if err != nil || milliseconds <= 0 {
		return fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func publishLifecycle(
	ctx context.Context,
	config codexhook.Config,
	update codexhook.LifecycleUpdate,
) error {
	client := &http.Client{Timeout: hookTimeout}
	publisher, err := publish.NewHTTPPublisher(config.RelayURL, client)
	if err != nil {
		return fmt.Errorf("create lifecycle publisher: %w", err)
	}
	instance, err := agent.New(config.Source, publisher, time.Now)
	if err != nil {
		return fmt.Errorf("create lifecycle agent: %w", err)
	}
	publishCtx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	if err := instance.HandleLifecycle(publishCtx, string(update.State), update.Active); err != nil {
		return fmt.Errorf("handle lifecycle: %w", err)
	}
	return nil
}

func startWatcherProcess(watch codexhook.PermissionWatch, getenv func(string) string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(watch)
	if err != nil {
		return err
	}
	command := exec.Command(executable, "--watch-permission", string(payload))
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func registerWatcher(directory string, pid int) (func(), error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, strconv.Itoa(pid))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

func watchPermission(
	ctx context.Context,
	watch codexhook.PermissionWatch,
	getenv func(string) string,
) error {
	unregister, err := registerWatcher(getenv(codexhook.WatcherFileEnv), os.Getpid())
	if err != nil {
		return err
	}
	defer unregister()

	config, err := codexhook.ConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return err
	}
	store, err := codexhook.NewSessionStore(config.StatePath, config.TTL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, config.TTL)
	defer cancel()
	ticker := time.NewTicker(transcriptPollInterval)
	defer ticker.Stop()
	offset := watch.TranscriptOffset
	for {
		pending, pendingErr := store.PermissionPending(watch)
		if pendingErr != nil {
			return pendingErr
		}
		if !pending {
			return nil
		}
		if !wrapperAlive(getenv(codexhook.WrapperPIDEnv)) {
			return nil
		}
		matched, nextOffset, scanErr := codexhook.ScanTranscript(
			watch.TranscriptPath,
			watch.TurnID,
			offset,
		)
		if scanErr == nil {
			offset = nextOffset
		}
		if matched {
			// Test-only synchronization point (always a no-op in
			// production — see its own doc comment): fires synchronously,
			// exactly here, so a test can deterministically apply a
			// competing state change (simulating a concurrent, real Stop
			// hook invocation) immediately before this watcher's own
			// RecoverCancelled attempt below, without any sleep-based
			// timing guess.
			testHookBeforeRecoveryAttempt()
			wonRecovery := false
			lockErr := sourcelifecycle.WithLock(config.StatePath, lifecycleLockTimeout, func() error {
				update, recovered, recoverErr := store.RecoverCancelled(watch)
				if recoverErr != nil || !recovered {
					// Stop (or another lifecycle event) already cleared this
					// permission watch — do not emit a duplicate transition,
					// and do not shadow-forward a Stop that never actually
					// happened in this invocation (see wonRecovery below).
					return recoverErr
				}
				wonRecovery = true
				if localhooktransport.LocalHookEnabled(getenv(localhooktransport.EnvLocalHookEnabled)) {
					// Per-session idle only; never aggregate via legacy relay.
					ingress, ingressErr := codexhook.LocalIngressObservation(codexhook.Event{
						HookEventName: "Stop",
						SessionID:     watch.SessionID,
					})
					if ingressErr != nil {
						return ingressErr
					}
					localhooktransport.TryDeliverIngress(ctx, getenv, ingress)
					return nil
				}
				return publishLifecycle(ctx, config, update)
			})
			// Shadow forwarding runs only after the lock above has been
			// released and the ordinary delivery (TryDeliverIngress or
			// publishLifecycle) has already completed — never before, never
			// while holding the session lock, so a slow or hanging shadow
			// target can never extend how long other lifecycle events for
			// this session are blocked behind sourcelifecycle.WithLock. It
			// also only runs when wonRecovery is true: this watcher process
			// may lose the race against a genuine Stop hook event that
			// already resolved the same permission watch (recovered=false
			// above) — in that case a real Stop was, or will be, forwarded
			// by that other hook invocation's own run() call, and this one
			// must not also emit a second, phantom Stop for the same
			// underlying transition.
			if wonRecovery {
				if ingress, ingressErr := codexhook.LocalIngressObservation(codexhook.Event{
					HookEventName: "Stop",
					SessionID:     watch.SessionID,
				}); ingressErr == nil {
					deliverShadow(ctx, getenv, ingress)
				}
			}
			return lockErr
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func wrapperAlive(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func runSessionEndFile(
	ctx context.Context,
	sessionIDPath string,
	getenv func(string) string,
) error {
	content, err := os.ReadFile(sessionIDPath)
	if err != nil {
		return nil
	}

	sessionID := strings.TrimSpace(string(content))
	if sessionID == "" {
		return nil
	}

	payload, err := json.Marshal(codexhook.Event{
		HookEventName: "SessionEnd",
		SessionID:     sessionID,
	})
	if err != nil {
		return err
	}

	return run(ctx, bytes.NewReader(payload), getenv)
}
