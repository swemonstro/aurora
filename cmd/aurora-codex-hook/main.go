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
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/sourcelifecycle"
)

const hookTimeout = time.Second
const transcriptPollInterval = 100 * time.Millisecond
const lifecycleLockTimeout = 3 * time.Second

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
		return startWatcherProcess(*watch, getenv)
	}
	return nil
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
			return sourcelifecycle.WithLock(config.StatePath, lifecycleLockTimeout, func() error {
				update, recovered, recoverErr := store.RecoverCancelled(watch)
				if recoverErr != nil || !recovered {
					return recoverErr
				}
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
