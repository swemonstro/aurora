package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
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

	ingress, ingressErr := codexhook.LocalIngressObservation(event)
	shadowAllowed := ingressErr == nil
	defer func() {
		if shadowAllowed {
			deliverShadow(ctx, getenv, ingress)
		}
	}()

	localEnabled := localhooktransport.LocalHookEnabled(
		getenv(localhooktransport.EnvLocalHookEnabled),
	)
	if localEnabled {
		applied, trackErr := trackLocalLifecycle(event, getenv)
		if trackErr != nil {
			return trackErr
		}
		if !applied {
			shadowAllowed = false
			return nil
		}
		// Transport failures remain fail-open so Codex itself is never
		// blocked by Aurora.
		if ingressErr == nil {
			localhooktransport.TryDeliverIngress(ctx, getenv, ingress)
		}
		return nil
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

	err = sourcelifecycle.WithLock(config.StatePath, lifecycleLockTimeout, func() error {
		update, supported, updateErr := store.UpdateLifecycle(event)
		if updateErr != nil || !supported {
			return updateErr
		}
		if !update.Applied {
			shadowAllowed = false
			return nil
		}
		if publishErr := publishLifecycle(ctx, config, update); publishErr != nil {
			return publishErr
		}
		return nil
	})
	return err
}

// trackLocalLifecycle applies turn-correlated ordering before local delivery.
// It stores only lifecycle state and opaque session/turn identifiers.
func trackLocalLifecycle(event codexhook.Event, getenv func(string) string) (bool, error) {
	config, err := codexhook.ConfigFromEnv(getenv, os.UserHomeDir)
	if err != nil {
		return false, err
	}
	store, err := codexhook.NewSessionStore(config.StatePath, config.TTL)
	if err != nil {
		return false, err
	}
	applied := false
	err = sourcelifecycle.WithLock(config.StatePath, lifecycleLockTimeout, func() error {
		update, supported, updateErr := store.UpdateLifecycle(event)
		if updateErr != nil || !supported {
			return updateErr
		}
		applied = update.Applied
		return nil
	})
	return applied, err
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

// deliverShadow is a package variable so tests can substitute a deterministic
// fake instead of exercising real socket timing. See deliverShadowDefault for
// the production implementation's actual timeout contract.
var deliverShadow = deliverShadowDefault

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
