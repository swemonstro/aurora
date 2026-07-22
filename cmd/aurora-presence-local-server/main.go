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
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
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
	flags := flag.NewFlagSet("aurora-presence-local-server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "absolute private Unix socket path; defaults below XDG_RUNTIME_DIR")
	procRoot := flags.String("proc-root", "/proc", "read-only proc filesystem root")
	hostID := flags.String("host-id", "", "required opaque local host ID")
	bootID := flags.String("boot-id", "", "optional opaque boot ID; default reads proc boot_id")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Foreground observe-only local hook receiver; it never performs a binding or state mutation.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-presence-local-server -host-id OPAQUE [-socket PATH]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *hostID == "" {
		flags.Usage()
		return errors.New("host-id is required")
	}
	if *socketPath == "" {
		runtimeDirectory := getenv("XDG_RUNTIME_DIR")
		if runtimeDirectory == "" || !filepath.IsAbs(runtimeDirectory) {
			return errors.New("socket is required when XDG_RUNTIME_DIR is unavailable")
		}
		*socketPath = filepath.Join(runtimeDirectory, "aurora", "presence-hook.sock")
	}
	if err := localhooktransport.PrepareSocketDirectory(*socketPath); err != nil {
		return err
	}
	clock := systemClock{}
	adapter, err := linuxprocess.New(linuxprocess.Config{
		ProcRoot: *procRoot, HostID: *hostID, BootID: instancepresence.BootIdentity(*bootID), Clock: clock,
		LaunchIdentityRules: append(claudehook.LaunchIdentityRules(), codexhook.LaunchIdentityRules()...),
	})
	if err != nil {
		return err
	}
	correlator, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		return err
	}
	config := localhooktransport.DefaultConfig(clock)
	config.SocketPath = *socketPath
	runtimeSource, err := runtimerecognition.NewSource(adapter, *hostID, claudehook.RuntimeRecognizer(), codexhook.RuntimeRecognizer())
	if err != nil {
		return err
	}
	service, err := localhooktransport.NewCorrelationService(runtimeSource, correlator, clock, config.MaximumRuntimes)
	if err != nil {
		return err
	}
	receiver, err := localhooktransport.NewReceiver(config, service)
	if err != nil {
		return err
	}
	server, err := localhooktransport.NewServer(config, receiver, localhooktransport.DefaultAuthenticator(), nil)
	if err != nil {
		return err
	}
	defer server.Close()
	fmt.Fprintln(stdout, "Aurora local hook receiver: observe-only, foreground, no binding performed")
	return server.Serve(ctx)
}
