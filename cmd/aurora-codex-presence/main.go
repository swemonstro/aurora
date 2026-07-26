//go:build linux

// Command aurora-codex-presence is a standalone, shadow-mode Codex presence
// producer (Aurora migration step G.4). It observes Codex processes and
// (optionally) Codex hook events, derives an idle/working/attention state
// per Codex instance using only observed signals (never trust
// configuration — see internal/codexproducer's doc comment for the G.4
// false-red fix), and reports normalized producerprotocol v2 messages to a
// presence broker's Codex socket.
//
// This binary performs no process observation of Claude or Grok, no
// registry mutation, no relay or ESP contact, and no systemd install. It
// does not connect to, or compete for, aurora-presence-local-server's hook
// socket (/run/aurora/presence-hook.sock) or its snapshot file
// (/run/aurora/snapshot.json). It is meant to be run manually, alongside the
// existing production stack, with zero effect on it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/codexproducer"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// sourceFlags collects repeated "-source LABEL=PATH" flag occurrences.
type sourceFlags []codexproducer.SourceEntry

func (flags *sourceFlags) String() string {
	if flags == nil {
		return ""
	}
	parts := make([]string, 0, len(*flags))
	for _, entry := range *flags {
		parts = append(parts, string(entry.Label))
	}
	return strings.Join(parts, ",")
}

func (flags *sourceFlags) Set(value string) error {
	entry, err := codexproducer.ParseSourceFlag(value)
	if err != nil {
		return err
	}
	*flags = append(*flags, entry)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("aurora-codex-presence", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var sources sourceFlags
	flags.Var(&sources, "source", "explicit CODEX_HOME source in LABEL=PATH form (repeatable); e.g. -source business=/home/carl/.codex-business -source api=/home/carl/.codex")
	defaultSource := flags.String("default-source", "", "source LABEL that a process with no CODEX_HOME environment variable set should be attributed to (must match one -source label); omit to ignore such processes")
	socketPath := flags.String("socket", "/run/aurora/broker-codex.sock", "absolute path of the presence broker's Codex socket to report to")
	procRoot := flags.String("proc-root", "/proc", "read-only proc filesystem root")
	pollInterval := flags.Duration("poll-interval", 5*time.Second, "how often to rescan /proc and renew every tracked instance's lease")
	leaseDuration := flags.Duration("lease-duration", 45*time.Second, "lease span requested for every report; must stay under the broker's -maximum-lease (2 minutes by default) and above -poll-interval")
	pendingHookTTL := flags.Duration("pending-hook-ttl", 30*time.Second, "how long an unresolved hook delivery waits for a matching recognized process before being dropped")
	reconnectMinDelay := flags.Duration("reconnect-min-delay", time.Second, "initial delay between broker reconnect attempts")
	reconnectMaxDelay := flags.Duration("reconnect-max-delay", 30*time.Second, "maximum delay between broker reconnect attempts (exponential backoff cap)")
	hookIngressSocket := flags.String("hook-ingress-socket", "", "absolute path for an optional, opt-in shadow hook ingress socket; empty (default) disables hook ingress entirely and this producer reports process-recognition-only idle/discovery state")
	hookIngressAllowUID := flags.String("hook-ingress-uid", "", "additional UID allowed to deliver hook events on -hook-ingress-socket, beyond this process's own EUID")
	diagnostics := flags.Bool("diagnostics", false, "print extra diagnostic output to stderr (test/shadow-mode use only; never includes CODEX_HOME paths, payload content, or session identifiers)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Standalone, shadow-mode Codex presence producer (Aurora migration step G.4).")
		fmt.Fprintln(flags.Output(), "Usage: aurora-codex-presence -source LABEL=PATH [-source LABEL=PATH ...] [options]")
		fmt.Fprintln(flags.Output(), "Shadow mode: no relay, no ESP, no registry, no systemd install; does not affect aurora-presence-local-server or the shared hook socket.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	if len(sources) == 0 {
		flags.Usage()
		return errors.New("at least one -source LABEL=PATH is required")
	}
	sourceSet, err := codexproducer.NewSourceSet(sources, codexproducer.SourceLabel(*defaultSource))
	if err != nil {
		return err
	}
	if !filepathIsAbs(*socketPath) {
		return errors.New("socket must be an absolute path")
	}
	if *pollInterval <= 0 {
		return errors.New("poll-interval must be positive")
	}
	if *leaseDuration <= *pollInterval {
		return errors.New("lease-duration must exceed poll-interval")
	}
	if *leaseDuration >= 2*time.Minute {
		return errors.New("lease-duration must stay comfortably under the broker's 2 minute maximum lease")
	}

	clock := systemClock{}
	epoch, err := codexproducer.NewProducerEpoch()
	if err != nil {
		return fmt.Errorf("generate producer epoch: %w", err)
	}

	dialConfig := producerprotocol.DefaultConfig(clock)
	dialConfig.SocketPath = *socketPath
	dialConfig.BoundTool = producerprotocol.ToolCodex

	producer, err := codexproducer.NewProducer(codexproducer.Config{
		DialConfig:        dialConfig,
		Sources:           sourceSet,
		PollInterval:      *pollInterval,
		LeaseDuration:     *leaseDuration,
		ReconnectMinDelay: *reconnectMinDelay,
		ReconnectMaxDelay: *reconnectMaxDelay,
		PendingHookTTL:    *pendingHookTTL,
		ProcRoot:          *procRoot,
		Clock:             clock,
		Stderr:            stderr,
	}, epoch)
	if err != nil {
		return err
	}

	if *diagnostics {
		go runDiagnostics(ctx, producer, stdout, *pollInterval)
	}

	var ingress *codexproducer.IngressListener
	if strings.TrimSpace(*hookIngressSocket) != "" {
		authenticator := producerprotocol.SameUIDAuthenticator{ServerUID: uint32(os.Geteuid()), AllowedUIDs: map[uint32]struct{}{}}
		if trimmed := strings.TrimSpace(*hookIngressAllowUID); trimmed != "" {
			uid, parseErr := parseUID(trimmed)
			if parseErr != nil {
				return fmt.Errorf("hook-ingress-uid: %w", parseErr)
			}
			authenticator.AllowedUIDs[uid] = struct{}{}
		}
		ingress, err = codexproducer.ListenIngress(*hookIngressSocket, authenticator)
		if err != nil {
			return fmt.Errorf("hook ingress: %w", err)
		}
		defer ingress.Close()
		go func() {
			if serveErr := ingress.Serve(ctx, producer.DeliverHook); serveErr != nil {
				fmt.Fprintln(stderr, "codex hook ingress:", serveErr)
			}
		}()
		fmt.Fprintf(stdout, "aurora-codex-presence: hook ingress enabled at %s\n", *hookIngressSocket)
	} else {
		fmt.Fprintln(stdout, "aurora-codex-presence: hook ingress disabled (no -hook-ingress-socket); reporting process-recognition-only state")
	}

	fmt.Fprintln(stdout, "aurora-codex-presence: shadow mode (no relay, no ESP, no registry); reporting to", *socketPath)
	return producer.Run(ctx)
}

// runDiagnostics periodically prints content-free instance/pending counts
// (never CODEX_HOME paths, session identifiers, or payload content) when
// -diagnostics is enabled. It is a test/shadow-mode convenience only.
func runDiagnostics(ctx context.Context, producer *codexproducer.Producer, stdout io.Writer, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tracked, pending := producer.Diagnostics()
			fmt.Fprintf(stdout, "aurora-codex-presence: diagnostics tracked=%d pending=%d\n", tracked, pending)
		}
	}
}

func filepathIsAbs(path string) bool { return len(path) > 0 && path[0] == '/' }

func parseUID(value string) (uint32, error) {
	var uid uint64
	if _, err := fmt.Sscanf(value, "%d", &uid); err != nil {
		return 0, fmt.Errorf("invalid UID %q", value)
	}
	return uint32(uid), nil
}
