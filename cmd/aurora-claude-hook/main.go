package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/swemonstro/aurora/internal/agent"
	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/sourcelifecycle"
)

const hookTimeout = time.Second
const lifecycleLockTimeout = 3 * time.Second

func main() {
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
	// Package 6 local ingress is fail-open and independent of v1 state/relay.
	if ingress, ingressErr := claudehook.LocalIngressObservation(event); ingressErr == nil {
		localhooktransport.TryDeliverIngress(ctx, getenv, ingress)
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
	return sourcelifecycle.WithLock(stateConfig.Path, lifecycleLockTimeout, func() error {
		update, supported, updateErr := store.UpdateLifecycle(event)
		if updateErr != nil || !supported {
			return updateErr
		}
		return publishLifecycle(ctx, config, update)
	})
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
