package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/swemonstro/aurora/internal/agent"
	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/publish"
)

const hookTimeout = time.Second

func main() {
	run(context.Background(), os.Stdin, os.Getenv)
}

func run(ctx context.Context, input io.Reader, getenv func(string) string) {
	rawInput, err := claudehook.ReadInput(input)
	if err != nil {
		return
	}

	if captureDirectory := claudehook.CaptureDirectoryFromEnv(getenv); captureDirectory != "" {
		_ = claudehook.Capture(captureDirectory, rawInput)
	}

	event, err := claudehook.ParseEvent(rawInput)
	if err != nil {
		return
	}
	stateConfig, err := claudehook.StateConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return
	}
	store, err := claudehook.NewSessionStore(stateConfig.Path, stateConfig.TTL)
	if err != nil {
		return
	}
	state, supported, err := store.Update(event)
	if err != nil || !supported {
		return
	}

	config := claudehook.ConfigFromEnv(getenv)
	client := &http.Client{Timeout: hookTimeout}
	publisher, err := publish.NewHTTPPublisher(config.RelayURL, client)
	if err != nil {
		return
	}

	instance, err := agent.New(config.Source, publisher, time.Now)
	if err != nil {
		return
	}

	publishCtx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	_ = instance.Handle(publishCtx, string(state))
}
