//go:build linux

// Command aurora-presence-broker is a standalone, tool-agnostic presence
// broker shell: it owns an instance registry and accepts normalized
// producerprotocol messages over three permanently tool-bound Unix sockets
// (Claude, Codex, Grok), applying them via internal/presencebroker.
//
// This binary performs no process observation, no hook parsing, and no
// tool-specific interpretation of any kind — Tool is data it passes
// through, never a switch. It does not connect to the relay or ESP, and it
// does not affect aurora-presence-local-server or aurora-runtime-presence:
// it is a separate, independent process with its own registry and its own
// sockets, safe to run alongside them for testing.
//
// Known G.3 gap, deliberately deferred: the producer protocol has no
// application-level acknowledgement — see presencebroker.handleProducerConnection's
// doc comment. Revisit before cutting any real producer over to this broker.
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/swemonstro/aurora/internal/instanceregistry"
	"github.com/swemonstro/aurora/internal/presencebroker"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// collectorID identifies this broker as the source of the registrations it
// creates. It is not user-configurable: it is broker-internal metadata, not
// an operational knob.
const collectorID = "aurora-presence-broker"

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

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
	broker, err := composeBroker(arguments, stderr)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Aurora presence broker: shadow mode (no relay, no ESP); independent of aurora-presence-local-server and aurora-runtime-presence")
	return broker.Serve(ctx)
}

// socketFlag bundles one tool's CLI values so composeBroker can loop over
// the three sockets identically instead of repeating per-tool code three
// times — the repetition itself would be exactly the kind of "switch tool"
// tool-specific shape this broker must not contain.
type socketFlag struct {
	tool       producerprotocol.Tool
	path       *string
	allowedUID *string
}

// Broker holds every internal component composeBroker wires up, exported so
// tests can drive and inspect them directly rather than only through the
// sockets (e.g. registry state, or accepting on one listener without the
// others running).
type Broker struct {
	Registry       *instanceregistry.Registry
	Ingestor       *presencebroker.Ingestor
	ClaudeListener *producerprotocol.Listener
	CodexListener  *producerprotocol.Listener
	GrokListener   *producerprotocol.Listener

	authenticators   map[producerprotocol.Tool]producerprotocol.Authenticator
	snapshotFile     string
	snapshotInterval time.Duration
	expiryInterval   time.Duration
	stderr           io.Writer
}

// Serve owns the full lifecycle of every goroutine this broker runs: the
// three listener loops (each of which, per presencebroker.RunProducerListener,
// does not return until all of its own accepted connections have also
// returned), the snapshot loop, and the expiry loop.
//
// On context cancellation, in order:
//  1. stop accepting new connections — each Listener.Accept(ctx) call
//     already stops on ctx.Done() on its own, before this method does
//     anything else;
//  2. close active connections so blocked reads are interrupted — each
//     accepted connection's own ctx-watcher goroutine closes it;
//  3. wait for every listener goroutine, and therefore every connection
//     goroutine, snapshot loop, and expiry loop, to actually return;
//  4. only then remove the socket files;
//  5. return.
//
// No goroutine Serve started is still running once it returns.
func (broker *Broker) Serve(ctx context.Context) error {
	listeners := []struct {
		tool     producerprotocol.Tool
		listener *producerprotocol.Listener
	}{
		{producerprotocol.ToolClaude, broker.ClaudeListener},
		{producerprotocol.ToolCodex, broker.CodexListener},
		{producerprotocol.ToolGrok, broker.GrokListener},
	}

	var wait sync.WaitGroup
	for _, entry := range listeners {
		wait.Add(1)
		go func(tool producerprotocol.Tool, listener *producerprotocol.Listener) {
			defer wait.Done()
			presencebroker.RunProducerListener(ctx, listener, broker.authenticators[tool], broker.Ingestor, broker.stderr)
		}(entry.tool, entry.listener)
	}
	if broker.snapshotFile != "" {
		wait.Add(1)
		go func() {
			defer wait.Done()
			presencebroker.RunSnapshotLoop(ctx, broker.Registry, broker.snapshotFile, broker.snapshotInterval, broker.stderr)
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		presencebroker.RunExpiryLoop(ctx, broker.Registry, broker.expiryInterval, broker.stderr)
	}()

	<-ctx.Done()
	wait.Wait() // every listener, connection, snapshot, and expiry goroutine has now returned.

	var closeErr error
	for _, entry := range listeners {
		if err := entry.listener.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func composeBroker(arguments []string, stderr io.Writer) (*Broker, error) {
	flags := flag.NewFlagSet("aurora-presence-broker", flag.ContinueOnError)
	flags.SetOutput(stderr)
	hostID := flags.String("host-id", "", "required opaque local host ID")
	claudeSocket := flags.String("claude-socket", "/run/aurora/broker-claude.sock", "absolute private Unix socket path for Claude producers")
	codexSocket := flags.String("codex-socket", "/run/aurora/broker-codex.sock", "absolute private Unix socket path for Codex producers")
	grokSocket := flags.String("grok-socket", "/run/aurora/broker-grok.sock", "absolute private Unix socket path for Grok producers")
	snapshotFile := flags.String("snapshot-file", "", "default-off absolute path for periodic read-only CanonicalSnapshot JSON")
	snapshotInterval := flags.Duration("snapshot-interval", presencebroker.DefaultSnapshotInterval, "snapshot write interval")
	gracePeriod := flags.Duration("grace-period", 15*time.Second, "grace period after lease expiry before an instance ends")
	maximumLease := flags.Duration("maximum-lease", 2*time.Minute, "maximum lease span, measured from this broker's own clock (not the report's observed_at), that a producer report may request; longer spans are rejected before any mutation")
	maximumClockSkew := flags.Duration("maximum-clock-skew", time.Minute, "maximum amount a report's observed_at may be ahead of this broker's own clock; further-future reports are rejected before any mutation")
	maximumReportAge := flags.Duration("maximum-report-age", 5*time.Minute, "maximum amount a report's observed_at may be behind this broker's own clock; older (stale/replayed) reports are rejected before any mutation")
	expiryInterval := flags.Duration("expiry-interval", presencebroker.DefaultExpiryInterval, "how often to scan for due lease transitions")
	allowSelfUID := flags.Bool("allow-self-uid", false, "allow this broker process's own UID on every socket; a shadow-mode/test convenience only — off by default so a deployment must explicitly opt in, rather than silently trusting the broker's own UID if an operator forgets to configure a dedicated per-tool *-uid")
	claudeAllowedUID := flags.String("claude-uid", "", "UID a Claude producer connects as (in addition to -allow-self-uid, if enabled)")
	codexAllowedUID := flags.String("codex-uid", "", "UID a Codex producer connects as (in addition to -allow-self-uid, if enabled)")
	grokAllowedUID := flags.String("grok-uid", "", "UID a Grok producer connects as (in addition to -allow-self-uid, if enabled)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Standalone, tool-agnostic presence broker: accepts normalized producer messages over per-tool Unix sockets.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-presence-broker -host-id OPAQUE [-claude-socket PATH] [-codex-socket PATH] [-grok-socket PATH] [-snapshot-file PATH]")
		fmt.Fprintln(flags.Output(), "Shadow mode: no relay, no ESP; does not affect aurora-presence-local-server or aurora-runtime-presence.")
		fmt.Fprintln(flags.Output(), "Each socket accepts exactly one tool for its lifetime. UID policy is explicit and deny-by-default: -allow-self-uid (off by default; opt in for local/shadow-mode testing) plus an optional per-tool *-uid for a future systemd-managed producer's own account; at least one must apply to each socket, or startup fails.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return nil, err
	}
	if strings.TrimSpace(*hostID) == "" {
		flags.Usage()
		return nil, errors.New("host-id is required")
	}
	snapshotPath := strings.TrimSpace(*snapshotFile)
	if snapshotPath != "" && !filepath.IsAbs(snapshotPath) {
		return nil, errors.New("snapshot-file must be an absolute path")
	}

	clock := systemClock{}
	registry, err := instanceregistry.New(instanceregistry.Config{
		Clock: clock, SlotNamespace: "default",
		// LeaseDuration only bounds instanceregistry.Config.validate(); this
		// broker exclusively uses ApplyProducerReport, which takes its lease
		// deadline from each report (bounded by MaximumProducerLeaseDuration
		// below), never from this value. It is not exposed as a flag because
		// it would not do anything if it were.
		LeaseDuration: time.Minute, GracePeriod: *gracePeriod,
		MaximumProducerLeaseDuration: *maximumLease,
		MaximumClockSkew:             *maximumClockSkew,
		MaximumReportAge:             *maximumReportAge,
	})
	if err != nil {
		return nil, err
	}
	ingestor, err := presencebroker.NewIngestor(registry, *hostID, collectorID)
	if err != nil {
		return nil, err
	}

	sockets := []socketFlag{
		{producerprotocol.ToolClaude, claudeSocket, claudeAllowedUID},
		{producerprotocol.ToolCodex, codexSocket, codexAllowedUID},
		{producerprotocol.ToolGrok, grokSocket, grokAllowedUID},
	}
	listeners := make(map[producerprotocol.Tool]*producerprotocol.Listener, len(sockets))
	authenticators := make(map[producerprotocol.Tool]producerprotocol.Authenticator, len(sockets))
	rollback := func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}

	for _, socket := range sockets {
		listener, authenticator, err := bindProducerSocket(clock, socket, *allowSelfUID)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("%s socket: %w", socket.tool, err)
		}
		listeners[socket.tool] = listener
		authenticators[socket.tool] = authenticator
	}

	return &Broker{
		Registry:         registry,
		Ingestor:         ingestor,
		ClaudeListener:   listeners[producerprotocol.ToolClaude],
		CodexListener:    listeners[producerprotocol.ToolCodex],
		GrokListener:     listeners[producerprotocol.ToolGrok],
		authenticators:   authenticators,
		snapshotFile:     snapshotPath,
		snapshotInterval: *snapshotInterval,
		expiryInterval:   *expiryInterval,
		stderr:           stderr,
	}, nil
}

// bindProducerSocket prepares and binds one tool's socket, permanently
// bound to that tool (Config.BoundTool), and builds its authenticator.
//
// The UID policy is explicit and deny-by-default, not a hidden permanent
// rule: allowSelfUID must be true for this broker process's own effective
// UID to be accepted (an opt-in shadow-mode/test convenience, off unless
// -allow-self-uid is passed), and/or socket.allowedUID must name a
// specific UID (the shape a future systemd-managed producer's own
// dedicated account would use). If neither applies, this returns an error
// (closing the listener it just bound) instead of leaving an unauthenticated
// socket behind — composeBroker's own rollback then tears down any other
// sockets already bound, so a single misconfigured tool fails the whole
// broker at startup rather than silently accepting nobody on one socket.
func bindProducerSocket(clock producerprotocol.Clock, socket socketFlag, allowSelfUID bool) (*producerprotocol.Listener, producerprotocol.Authenticator, error) {
	if err := producerprotocol.PrepareSocketDirectory(*socket.path); err != nil {
		return nil, nil, err
	}
	config := producerprotocol.DefaultConfig(clock)
	config.SocketPath = *socket.path
	config.BoundTool = socket.tool
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		return nil, nil, err
	}

	var serverUID uint32
	hasServerUID := false
	allowed := make(map[uint32]struct{})
	if allowSelfUID {
		serverUID = uint32(os.Geteuid())
		hasServerUID = true
	}
	if extra := strings.TrimSpace(*socket.allowedUID); extra != "" {
		parsed, parseErr := strconv.ParseUint(extra, 10, 32)
		if parseErr != nil {
			_ = listener.Close()
			return nil, nil, fmt.Errorf("uid: %w", parseErr)
		}
		if hasServerUID {
			allowed[uint32(parsed)] = struct{}{}
		} else {
			serverUID = uint32(parsed)
			hasServerUID = true
		}
	}
	if !hasServerUID {
		_ = listener.Close()
		return nil, nil, errors.New("no UID configured: enable -allow-self-uid or set this tool's *-uid flag")
	}
	return listener, producerprotocol.SameUIDAuthenticator{ServerUID: serverUID, AllowedUIDs: allowed}, nil
}
