// Command aurora-runtime-presence is a long-lived Linux agent that polls
// process observations and publishes coarse v1 presence for secure Claude and
// Codex runtime families. It never mutates hook state, correlation, registry
// slots, or ESP contracts.
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
	"strings"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/runtimepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

const publishTimeout = 5 * time.Second

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("aurora-runtime-presence", flag.ContinueOnError)
	flags.SetOutput(stderr)
	relayURL := flags.String("relay", "", "Aurora Relay base URL")
	procRoot := flags.String("proc-root", "/proc", "read-only proc filesystem root")
	hostID := flags.String("host-id", "", "required opaque local host ID")
	bootID := flags.String("boot-id", "", "optional opaque boot ID; default reads proc boot_id")
	interval := flags.Duration("interval", runtimepresence.DefaultPollInterval, "poll interval for /proc observation")
	clockTicks := flags.Uint64("clock-ticks", 100, "Linux USER_HZ used by proc start ticks")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Long-lived Linux runtime presence agent; publishes claude-runtime / codex-runtime on change only.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-runtime-presence -host-id OPAQUE -relay URL [options]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*hostID) == "" {
		flags.Usage()
		return errors.New("host-id is required")
	}
	if strings.TrimSpace(*relayURL) == "" {
		flags.Usage()
		return errors.New("relay is required")
	}
	if *interval <= 0 {
		return errors.New("interval must be positive")
	}

	adapter, err := linuxprocess.New(linuxprocess.Config{
		ProcRoot:            *procRoot,
		HostID:              *hostID,
		BootID:              instancepresence.BootIdentity(*bootID),
		Clock:               systemClock{},
		ClockTicks:          *clockTicks,
		LaunchIdentityRules: append(claudehook.LaunchIdentityRules(), codexhook.LaunchIdentityRules()...),
	})
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: publishTimeout}
	publisher, err := publish.NewHTTPPublisher(*relayURL, client)
	if err != nil {
		return fmt.Errorf("create HTTP publisher: %w", err)
	}
	agent, err := runtimepresence.New(publisher, time.Now)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		if shutdownErr := agent.Shutdown(shutdownCtx); shutdownErr != nil {
			fmt.Fprintln(stderr, "shutdown:", shutdownErr)
		}
	}()

	fmt.Fprintf(stdout, "aurora-runtime-presence: polling %s every %s\n", *procRoot, interval.String())
	return pollLoop(ctx, agent, adapter, *hostID, *interval, stderr)
}

func pollLoop(
	ctx context.Context,
	agent *runtimepresence.Agent,
	adapter *linuxprocess.Adapter,
	hostID string,
	interval time.Duration,
	stderr io.Writer,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := observeOnce(ctx, agent, adapter, hostID); err != nil {
			fmt.Fprintln(stderr, "observe:", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func observeOnce(
	ctx context.Context,
	agent *runtimepresence.Agent,
	adapter *linuxprocess.Adapter,
	hostID string,
) error {
	sample, err := adapter.Observe(ctx)
	if err != nil {
		return fmt.Errorf("observe processes: %w", err)
	}
	result, err := runtimerecognition.Recognize(
		sample.Recognition,
		hostID,
		claudehook.RuntimeRecognizer(),
		codexhook.RuntimeRecognizer(),
	)
	if err != nil {
		return fmt.Errorf("recognize runtimes: %w", err)
	}
	// Publish with an independent budget so a cancelled poll loop does not
	// leave the agent out of sync with a completed relay write.
	publishCtx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := agent.ApplyRecognition(publishCtx, result); err != nil {
		return fmt.Errorf("apply presence: %w", err)
	}
	return nil
}
