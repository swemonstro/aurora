package main

import (
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
	"time"

	"github.com/swemonstro/aurora/internal/agent"
	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/sourcelifecycle"
)

const hookTimeout = time.Second
const transcriptPollInterval = 100 * time.Millisecond
const lifecycleLockTimeout = 3 * time.Second

// startQuestionWatcher launches the detached transcript watcher. Tests may
// replace it to avoid re-execing the test binary.
var startQuestionWatcher = startWatcherProcess

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--watch-question" {
		var watch claudehook.QuestionWatch
		if json.Unmarshal([]byte(os.Args[2]), &watch) != nil {
			return
		}
		_ = watchQuestion(context.Background(), watch, os.Getenv)
		return
	}
	_ = run(context.Background(), os.Stdin, os.Getenv)
}

func run(ctx context.Context, input io.Reader, getenv func(string) string) error {
	rawInput, err := claudehook.ReadInput(input)
	if err != nil {
		return err
	}

	if captureDirectory := claudehook.CaptureDirectoryFromEnv(getenv); captureDirectory != "" {
		_ = claudehook.Capture(captureDirectory, rawInput)
	}

	event, err := claudehook.ParseEvent(rawInput)
	if err != nil {
		return err
	}

	// When local multi-instance presence is enabled, this process delivers only
	// to Package 6 ingress. claude-runtime is owned by the local server bridge;
	// legacy claude-code session-store/relay publish is skipped so there is
	// exactly one relay producer for runtime presence.
	localEnabled := localhooktransport.LocalHookEnabled(getenv(localhooktransport.EnvLocalHookEnabled))
	if ingress, ingressErr := claudehook.LocalIngressObservation(event); ingressErr == nil {
		// Fail-open: transport errors must not block Claude.
		localhooktransport.TryDeliverIngress(ctx, getenv, ingress)
	}
	if localEnabled {
		// Maintain session store + AskUserQuestion transcript watchers so Esc
		// (tool_result is_error) can clear attention via local ingress only.
		return trackLocalQuestion(event, getenv)
	}

	stateConfig, err := claudehook.StateConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return err
	}
	store, err := claudehook.NewSessionStore(stateConfig.Path, stateConfig.TTL)
	if err != nil {
		return err
	}
	config := claudehook.ConfigFromEnv(getenv)
	var watch *claudehook.QuestionWatch
	err = sourcelifecycle.WithLock(stateConfig.Path, lifecycleLockTimeout, func() error {
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
		return startQuestionWatcher(*watch, getenv)
	}
	return nil
}

// trackLocalQuestion updates the session store and starts transcript watchers
// without any legacy relay publication.
func trackLocalQuestion(event claudehook.Event, getenv func(string) string) error {
	stateConfig, err := claudehook.StateConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return err
	}
	store, err := claudehook.NewSessionStore(stateConfig.Path, stateConfig.TTL)
	if err != nil {
		return err
	}
	var watch *claudehook.QuestionWatch
	err = sourcelifecycle.WithLock(stateConfig.Path, lifecycleLockTimeout, func() error {
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
		return startQuestionWatcher(*watch, getenv)
	}
	return nil
}

func publishLifecycle(
	ctx context.Context,
	config claudehook.Config,
	update claudehook.LifecycleUpdate,
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

func startWatcherProcess(watch claudehook.QuestionWatch, getenv func(string) string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(watch)
	if err != nil {
		return err
	}
	command := exec.Command(executable, "--watch-question", string(payload))
	// Inherit ambient environment so AURORA_LOCAL_HOOK_ENABLED / SOCKET and
	// watcher registration paths reach the detached process.
	command.Env = os.Environ()
	if getenv != nil {
		// Overlay test/getenv values for the common local-hook knobs when the
		// parent used an explicit getenv (production getenv is os.Getenv).
		command.Env = overlayWatcherEnv(command.Env, getenv)
	}
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func overlayWatcherEnv(base []string, getenv func(string) string) []string {
	keys := []string{
		localhooktransport.EnvLocalHookEnabled,
		localhooktransport.EnvLocalHookSocket,
		claudehook.StateFileEnv,
		claudehook.SessionTTLEnv,
		claudehook.WatcherFileEnv,
		claudehook.RelayURLEnv,
		claudehook.SourceEnv,
	}
	result := append([]string{}, base...)
	for _, key := range keys {
		value := getenv(key)
		if value == "" {
			continue
		}
		prefix := key + "="
		replaced := false
		for i, entry := range result {
			if strings.HasPrefix(entry, prefix) {
				result[i] = prefix + value
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, prefix+value)
		}
	}
	return result
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

func watchQuestion(
	ctx context.Context,
	watch claudehook.QuestionWatch,
	getenv func(string) string,
) error {
	unregister, err := registerWatcher(getenv(claudehook.WatcherFileEnv), os.Getpid())
	if err != nil {
		return err
	}
	defer unregister()

	stateConfig, err := claudehook.StateConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return err
	}
	store, err := claudehook.NewSessionStore(stateConfig.Path, stateConfig.TTL)
	if err != nil {
		return err
	}

	// TTL ends the watcher without inventing idle — only transcript evidence
	// (or RecoverCancelledQuestion) may clear attention.
	ctx, cancel := context.WithTimeout(ctx, stateConfig.TTL)
	defer cancel()
	ticker := time.NewTicker(transcriptPollInterval)
	defer ticker.Stop()
	offset := watch.TranscriptOffset
	for {
		pending, pendingErr := store.QuestionPending(watch)
		if pendingErr != nil {
			return pendingErr
		}
		if !pending {
			return nil
		}
		matched, nextOffset, scanErr := claudehook.ScanQuestionTranscript(
			watch.TranscriptPath,
			watch.ToolUseID,
			offset,
		)
		if scanErr == nil {
			offset = nextOffset
		}
		if matched {
			return sourcelifecycle.WithLock(stateConfig.Path, lifecycleLockTimeout, func() error {
				update, recovered, recoverErr := store.RecoverCancelledQuestion(watch)
				if recoverErr != nil || !recovered {
					// Stop / PostToolUse / newer AskUserQuestion already moved on.
					return recoverErr
				}
				if localhooktransport.LocalHookEnabled(getenv(localhooktransport.EnvLocalHookEnabled)) {
					ingress, ingressErr := claudehook.LocalIngressObservation(claudehook.Event{
						HookEventName: "Stop",
						SessionID:     watch.SessionID,
					})
					if ingressErr != nil {
						return ingressErr
					}
					localhooktransport.TryDeliverIngress(ctx, getenv, ingress)
					return nil
				}
				config := claudehook.ConfigFromEnv(getenv)
				return publishLifecycle(ctx, config, update)
			})
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
