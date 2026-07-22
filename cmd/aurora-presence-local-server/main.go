//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/swemonstro/aurora/internal/linuxidentitymeasure"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

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
	server, cleanup, err := composeServer(arguments, stderr, getenv)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	defer server.Close()
	fmt.Fprintln(stdout, "Aurora local hook receiver: observe-only, foreground, no binding performed")
	if server.IdentityObserverEnabled() {
		fmt.Fprintln(stdout, "Package 7.0 identity measure: enabled (read-only JSONL; no binding)")
	}
	return server.Serve(ctx)
}

func composeServer(arguments []string, stderr io.Writer, getenv func(string) string) (*localhooktransport.Server, func(), error) {
	flags := flag.NewFlagSet("aurora-presence-local-server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "absolute private Unix socket path; defaults below XDG_RUNTIME_DIR")
	procRoot := flags.String("proc-root", "/proc", "read-only proc filesystem root")
	hostID := flags.String("host-id", "", "required opaque local host ID")
	bootID := flags.String("boot-id", "", "optional opaque boot ID; default reads proc boot_id")
	identityMeasurePath := flags.String("identity-measure-file", "", "default-off Package 7.0 read-only JSONL path for peer identity measurements")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Foreground observe-only local hook receiver; it never performs a binding or state mutation.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-presence-local-server -host-id OPAQUE [-socket PATH] [-identity-measure-file PATH]")
		fmt.Fprintln(flags.Output(), "Package 7.0 identity measure is off unless -identity-measure-file is set (manual Blue1 diagnostic only).")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return nil, nil, err
	}
	if *hostID == "" {
		flags.Usage()
		return nil, nil, errors.New("host-id is required")
	}
	if *socketPath == "" {
		runtimeDirectory := getenv("XDG_RUNTIME_DIR")
		if runtimeDirectory == "" || !filepath.IsAbs(runtimeDirectory) {
			return nil, nil, errors.New("socket is required when XDG_RUNTIME_DIR is unavailable")
		}
		*socketPath = filepath.Join(runtimeDirectory, "aurora", "presence-hook.sock")
	}
	if err := localhooktransport.PrepareSocketDirectory(*socketPath); err != nil {
		return nil, nil, err
	}
	clock := systemClock{}
	adapter, err := linuxprocess.New(linuxprocess.Config{
		ProcRoot: *procRoot, HostID: *hostID, BootID: instancepresence.BootIdentity(*bootID), Clock: clock,
		LaunchIdentityRules: append(claudehook.LaunchIdentityRules(), codexhook.LaunchIdentityRules()...),
	})
	if err != nil {
		return nil, nil, err
	}
	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		return nil, nil, err
	}
	config := localhooktransport.DefaultConfig(clock)
	config.SocketPath = *socketPath
	runtimeSource, err := runtimerecognition.NewSource(adapter, *hostID, claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		return nil, nil, err
	}
	service, err := localhooktransport.NewCorrelationService(runtimeSource, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		return nil, nil, err
	}
	receiver, err := localhooktransport.NewReceiver(config, service)
	if err != nil {
		return nil, nil, err
	}
	server, err := localhooktransport.NewServer(config, receiver, localhooktransport.DefaultAuthenticator(), nil)
	if err != nil {
		return nil, nil, err
	}
	// Package 6 ingest stays off by default. Enable only when the same feature
	// flag used by hook clients is explicitly on.
	if localhooktransport.LocalHookEnabled(getenv(localhooktransport.EnvLocalHookEnabled)) {
		if err := server.EnableIngest(localhooktransport.DefaultIngestServerConfig(clock)); err != nil {
			_ = server.Close()
			return nil, nil, err
		}
	}

	var cleanup func()
	measurePath := strings.TrimSpace(*identityMeasurePath)
	if measurePath == "" {
		measurePath = strings.TrimSpace(getenv("AURORA_IDENTITY_MEASURE_FILE"))
	}
	if measurePath != "" {
		if !filepath.IsAbs(measurePath) {
			_ = server.Close()
			return nil, nil, errors.New("identity-measure-file must be an absolute path")
		}
		file, err := linuxidentitymeasure.OpenFileWriter(measurePath)
		if err != nil {
			_ = server.Close()
			return nil, nil, fmt.Errorf("open identity measure file: %w", err)
		}
		observer, err := linuxidentitymeasure.NewObserver(
			adapter,
			*hostID,
			file,
			linuxidentitymeasure.Config{HostID: *hostID},
			claudehook.RuntimeRecognizer(),
			codexhook.RuntimeRecognizer(),
		)
		if err != nil {
			_ = file.Close()
			_ = server.Close()
			return nil, nil, err
		}
		server.SetIdentityObserver(observer)
		cleanup = func() { _ = file.Close() }
		fmt.Fprintf(stderr, "identity measure JSONL: %s (read-only; no binding; stop server to disable)\n", measurePath)
	}
	return server, cleanup, nil
}
