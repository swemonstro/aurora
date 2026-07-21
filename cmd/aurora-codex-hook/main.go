package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/agent"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/publish"
)

const hookTimeout = time.Second

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--session-end-file" {
		runSessionEndFile(
			context.Background(),
			os.Args[2],
			os.Getenv,
		)
		return
	}

	run(context.Background(), os.Stdin, os.Getenv)
}

func run(
	ctx context.Context,
	input io.Reader,
	getenv func(string) string,
) {
	rawInput, err := codexhook.ReadInput(input)
	if err != nil {
		return
	}

	event, err := codexhook.ParseEvent(rawInput)
	if err != nil {
		return
	}

	config, err := codexhook.ConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return
	}

	if err := codexhook.RecordSessionID(config.SessionIDPath, event); err != nil {
		return
	}

	store, err := codexhook.NewSessionStore(config.StatePath, config.TTL)
	if err != nil {
		return
	}

	state, supported, err := store.Update(event)
	if err != nil || !supported {
		return
	}

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

func runSessionEndFile(
	ctx context.Context,
	sessionIDPath string,
	getenv func(string) string,
) {
	content, err := os.ReadFile(sessionIDPath)
	if err != nil {
		return
	}

	sessionID := strings.TrimSpace(string(content))
	if sessionID == "" {
		return
	}

	payload, err := json.Marshal(codexhook.Event{
		HookEventName: "SessionEnd",
		SessionID:     sessionID,
	})
	if err != nil {
		return
	}

	run(ctx, bytes.NewReader(payload), getenv)
}
