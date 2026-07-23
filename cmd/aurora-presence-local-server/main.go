//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/linuxidentitymeasure"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/runtimepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
	"github.com/swemonstro/aurora/internal/sessionbinding"
)

// EnvRelayURL is an optional default for -relay; the CLI flag always wins when set.
const EnvRelayURL = "AURORA_RELAY_URL"

const relayHTTPTimeout = 2 * time.Second

// relayPresentationPixelCapacity is the fixed ESP slot count for v2 presentation.
const relayPresentationPixelCapacity uint64 = 5

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type notifyingHookMutator struct {
	localhooktransport.HookMutator
	notify func()
}

func (mutator notifyingHookMutator) ApplyNextHookMutation(
	id instancepresence.InstanceID,
	producerEpoch instancepresence.ProducerEpoch,
	state instancepresence.EffectiveState,
	observedAt time.Time,
	idempotencyKey string,
) (instancepresence.Instance, error) {
	instance, err := mutator.HookMutator.ApplyNextHookMutation(
		id,
		producerEpoch,
		state,
		observedAt,
		idempotencyKey,
	)
	if err == nil && mutator.notify != nil {
		mutator.notify()
	}
	return instance, err
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer, getenv func(string) string) error {
	server, cleanup, bindingEnabled, err := composeServer(arguments, stderr, getenv)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	defer server.Close()
	if bindingEnabled {
		fmt.Fprintln(stdout, "Aurora local hook receiver: verified per-instance binding enabled (content-free responses)")
	} else {
		fmt.Fprintln(stdout, "Aurora local hook receiver: observe-only, foreground, no binding performed")
	}
	if server.IdentityObserverEnabled() {
		fmt.Fprintln(stdout, "Package 7.0 identity measure: enabled (read-only JSONL independent of binding)")
	}
	return server.Serve(ctx)
}

func composeServer(arguments []string, stderr io.Writer, getenv func(string) string) (*localhooktransport.Server, func(), bool, error) {
	flags := flag.NewFlagSet("aurora-presence-local-server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "absolute private Unix socket path; defaults below XDG_RUNTIME_DIR")
	procRoot := flags.String("proc-root", "/proc", "read-only proc filesystem root")
	hostID := flags.String("host-id", "", "required opaque local host ID")
	bootID := flags.String("boot-id", "", "optional opaque boot ID; default reads proc boot_id")
	identityMeasurePath := flags.String("identity-measure-file", "", "default-off Package 7.0 read-only JSONL path for peer identity measurements")
	snapshotPath := flags.String("snapshot-file", "", "default-off absolute path for periodic read-only CanonicalSnapshot JSON")
	relayFlag := flags.String("relay", "", "default-off Aurora Relay base URL for v2→v1 presence bridge (http/https)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Foreground local hook receiver with optional verified per-instance binding.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-presence-local-server -host-id OPAQUE [-socket PATH] [-identity-measure-file PATH] [-snapshot-file PATH] [-relay URL]")
		fmt.Fprintln(flags.Output(), "Binding requires AURORA_LOCAL_HOOK_ENABLED=1|true (same flag as hook clients).")
		fmt.Fprintln(flags.Output(), "When -relay is set, this process owns claude-runtime/codex-runtime; stop separate aurora-runtime-presence to avoid dual producers.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return nil, nil, false, err
	}
	if *hostID == "" {
		flags.Usage()
		return nil, nil, false, errors.New("host-id is required")
	}
	if *socketPath == "" {
		runtimeDirectory := getenv("XDG_RUNTIME_DIR")
		if runtimeDirectory == "" || !filepath.IsAbs(runtimeDirectory) {
			return nil, nil, false, errors.New("socket is required when XDG_RUNTIME_DIR is unavailable")
		}
		*socketPath = filepath.Join(runtimeDirectory, "aurora", "presence-hook.sock")
	}
	if err := localhooktransport.PrepareSocketDirectory(*socketPath); err != nil {
		return nil, nil, false, err
	}
	clock := systemClock{}
	adapter, err := linuxprocess.New(linuxprocess.Config{
		ProcRoot: *procRoot, HostID: *hostID, BootID: instancepresence.BootIdentity(*bootID), Clock: clock,
		LaunchIdentityRules: append(claudehook.LaunchIdentityRules(), codexhook.LaunchIdentityRules()...),
	})
	if err != nil {
		return nil, nil, false, err
	}
	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		return nil, nil, false, err
	}
	config := localhooktransport.DefaultConfig(clock)
	config.SocketPath = *socketPath
	runtimeSource, err := runtimerecognition.NewSource(adapter, *hostID, claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		return nil, nil, false, err
	}
	service, err := localhooktransport.NewCorrelationService(runtimeSource, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		return nil, nil, false, err
	}
	receiver, err := localhooktransport.NewReceiver(config, service)
	if err != nil {
		return nil, nil, false, err
	}
	server, err := localhooktransport.NewServer(config, receiver, localhooktransport.DefaultAuthenticator(), nil)
	if err != nil {
		return nil, nil, false, err
	}

	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	bindingEnabled := false

	snapshotFile := strings.TrimSpace(*snapshotPath)
	if snapshotFile != "" {
		if !filepath.IsAbs(snapshotFile) {
			_ = server.Close()
			return nil, nil, false, errors.New("snapshot-file must be an absolute path")
		}
	}

	var registry *instanceregistry.Registry
	var relayBridge *runtimepresence.RelayBridge
	var presentationBridge *runtimepresence.PresentationBridge

	ensureRegistry := func() error {
		if registry != nil {
			return nil
		}
		created, regErr := instanceregistry.New(instanceregistry.Config{
			Clock: clock, SlotNamespace: "default",
			LeaseDuration: 30 * time.Second, GracePeriod: 15 * time.Second,
		})
		if regErr != nil {
			return regErr
		}
		registry = created
		return nil
	}

	// Package 6 ingest stays off by default. When enabled, attach verified
	// per-instance binding: unique runtime link + session map + registry mutation.
	if localhooktransport.LocalHookEnabled(getenv(localhooktransport.EnvLocalHookEnabled)) {
		epoch, err := localhooktransport.NewProducerEpoch()
		if err != nil {
			_ = server.Close()
			return nil, nil, false, err
		}
		if err := server.EnableIngestWithEpoch(localhooktransport.DefaultIngestServerConfig(clock), epoch); err != nil {
			_ = server.Close()
			return nil, nil, false, err
		}
		if err := ensureRegistry(); err != nil {
			_ = server.Close()
			return nil, nil, false, err
		}

		var measureWriter io.Writer = io.Discard
		measurePath := strings.TrimSpace(*identityMeasurePath)
		if measurePath == "" {
			measurePath = strings.TrimSpace(getenv("AURORA_IDENTITY_MEASURE_FILE"))
		}
		if measurePath != "" {
			if !filepath.IsAbs(measurePath) {
				_ = server.Close()
				return nil, nil, false, errors.New("identity-measure-file must be an absolute path")
			}
			file, err := linuxidentitymeasure.OpenFileWriter(measurePath)
			if err != nil {
				_ = server.Close()
				return nil, nil, false, fmt.Errorf("open identity measure file: %w", err)
			}
			measureWriter = file
			cleanups = append(cleanups, func() { _ = file.Close() })
			fmt.Fprintf(stderr, "identity measure JSONL: %s (diagnostic only; independent of binding)\n", measurePath)
		}

		observer, err := linuxidentitymeasure.NewObserver(
			adapter, *hostID, measureWriter,
			linuxidentitymeasure.Config{HostID: *hostID},
			claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer(),
		)
		if err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, err
		}
		// CapturePeer still runs for binding; CompleteIngest remains diagnostic.
		server.SetIdentityObserver(observer)

		sessions := sessionbinding.New()
		mutator := notifyingHookMutator{
			HookMutator: registry,
			notify: func() {
				// Only after successful registry mutation: wake both bridges.
				if relayBridge != nil {
					relayBridge.Trigger()
				}
				if presentationBridge != nil {
					presentationBridge.Trigger()
				}
			},
		}
		engine, err := localhooktransport.NewBindingEngine(sessions, observer, mutator)
		if err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, err
		}
		if ingest := server.IngestReceiver(); ingest != nil {
			ingest.SetBinder(engine)
		}

		// Background runtime registration keeps InstanceIDs alive for binding.
		regSync, err := runtimepresence.NewRegistrySync(registry, *hostID, epoch, instancepresence.SourceDescriptor{
			Provider: "linux-runtime", Profile: "default", CollectorID: "local-server",
		}, time.Now)
		if err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, err
		}
		pollCtx, pollCancel := context.WithCancel(context.Background())
		cleanups = append(cleanups, pollCancel)
		go pollRuntimes(pollCtx, adapter, *hostID, instancepresence.BootIdentity(*bootID), regSync, stderr)
		bindingEnabled = true
	} else {
		// Observe-only identity measure without binding when ingest is off.
		measurePath := strings.TrimSpace(*identityMeasurePath)
		if measurePath == "" {
			measurePath = strings.TrimSpace(getenv("AURORA_IDENTITY_MEASURE_FILE"))
		}
		if measurePath != "" {
			if !filepath.IsAbs(measurePath) {
				_ = server.Close()
				return nil, nil, false, errors.New("identity-measure-file must be an absolute path")
			}
			file, err := linuxidentitymeasure.OpenFileWriter(measurePath)
			if err != nil {
				_ = server.Close()
				return nil, nil, false, fmt.Errorf("open identity measure file: %w", err)
			}
			observer, err := linuxidentitymeasure.NewObserver(
				adapter, *hostID, file,
				linuxidentitymeasure.Config{HostID: *hostID},
				claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer(),
			)
			if err != nil {
				_ = file.Close()
				_ = server.Close()
				return nil, nil, false, err
			}
			server.SetIdentityObserver(observer)
			cleanups = append(cleanups, func() { _ = file.Close() })
			fmt.Fprintf(stderr, "identity measure JSONL: %s (read-only; no binding; stop server to disable)\n", measurePath)
		}
	}

	// Optional read-only CanonicalSnapshot JSON for external observers (no HTTP).
	if snapshotFile != "" {
		if err := ensureRegistry(); err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, err
		}
		fmt.Fprintf(stderr, "canonical snapshot file: %s (read-only JSON; content-free instances)\n", snapshotFile)
		snapCtx, snapCancel := context.WithCancel(context.Background())
		cleanups = append(cleanups, snapCancel)
		go runSnapshotLoop(snapCtx, registry, snapshotFile, defaultSnapshotInterval, stderr)
	}

	// Optional v2→v1 relay bridge. CLI -relay wins over AURORA_RELAY_URL.
	relayURL := strings.TrimSpace(*relayFlag)
	if relayURL == "" {
		relayURL = strings.TrimSpace(getenv(EnvRelayURL))
	}
	if relayURL != "" {
		if err := ensureRegistry(); err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, err
		}
		client := &http.Client{Timeout: relayHTTPTimeout}
		publisher, err := publish.NewHTTPPublisher(relayURL, client)
		if err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, fmt.Errorf("relay publisher: %w", err)
		}
		bridge, err := runtimepresence.NewRelayBridge(
			registry, publisher, time.Now, runtimepresence.DefaultBridgeInterval, stderr,
		)
		if err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, err
		}
		relayBridge = bridge
		presBridge, err := runtimepresence.NewPresentationBridge(
			registry,
			publisher,
			relayPresentationPixelCapacity,
			runtimepresence.DefaultBridgeInterval,
			stderr,
		)
		if err != nil {
			cleanup()
			_ = server.Close()
			return nil, nil, false, err
		}
		presentationBridge = presBridge
		fmt.Fprintf(stderr,
			"relay bridge: %s (V1 aggregate sources %s/%s + V2 %d-pixel presentation; stop separate aurora-runtime-presence to avoid dual producers)\n",
			relayURL,
			runtimepresence.SourceClaudeRuntime,
			runtimepresence.SourceCodexRuntime,
			relayPresentationPixelCapacity,
		)
		bridgeCtx, bridgeCancel := context.WithCancel(context.Background())
		cleanups = append(cleanups, bridgeCancel)
		go bridge.Run(bridgeCtx)
		go presentationBridge.Run(bridgeCtx)
	}

	return server, cleanup, bindingEnabled, nil
}

func pollRuntimes(
	ctx context.Context,
	adapter *linuxprocess.Adapter,
	hostID string,
	bootID instancepresence.BootIdentity,
	sync *runtimepresence.RegistrySync,
	stderr io.Writer,
) {
	ticker := time.NewTicker(runtimepresence.DefaultPollInterval)
	defer ticker.Stop()
	for {
		sample, err := adapter.Observe(ctx)
		if err == nil {
			result, recErr := runtimerecognition.Recognize(
				sample.Recognition, hostID,
				claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer(),
			)
			if recErr == nil {
				effectiveBoot := bootID
				if effectiveBoot == "" {
					effectiveBoot = sample.Recognition.BootID
				}
				if syncErr := sync.ApplyRecognition(result, effectiveBoot); syncErr != nil {
					fmt.Fprintln(stderr, "runtime registry sync:", syncErr)
				}
			} else {
				fmt.Fprintln(stderr, "runtime recognize:", recErr)
			}
		} else if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "runtime observe:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
