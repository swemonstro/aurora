package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

const maximumInputBytes = 1 << 20

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type inputDocument struct {
	EvaluatedAt     time.Time                           `json:"evaluated_at"`
	Runtimes        []runtimeInput                      `json:"runtimes"`
	Hooks           []hookInput                         `json:"hooks"`
	ExpectedMatches []instancecorrelation.ExpectedMatch `json:"expected_matches,omitempty"`
}

type sourceInput struct {
	Provider    string `json:"provider"`
	Profile     string `json:"profile"`
	CollectorID string `json:"collector_id"`
}

func (source sourceInput) domain() instancepresence.SourceDescriptor {
	return instancepresence.SourceDescriptor{Provider: source.Provider, Profile: source.Profile, CollectorID: source.CollectorID}
}

type runtimeIdentityInput struct {
	HostID      string                           `json:"host_id"`
	BootID      instancepresence.BootIdentity    `json:"boot_id"`
	RootProcess instancepresence.ProcessIdentity `json:"root_process"`
}

func (identity runtimeIdentityInput) domain() instancepresence.RuntimeIdentity {
	return instancepresence.RuntimeIdentity{HostID: identity.HostID, BootID: identity.BootID, RootProcess: identity.RootProcess}
}

type runtimeInput struct {
	RuntimeRef          string                             `json:"runtime_ref"`
	Tool                instancepresence.ToolKind          `json:"tool"`
	Runtime             runtimeIdentityInput               `json:"runtime"`
	Members             []instancepresence.ProcessIdentity `json:"members"`
	Source              *sourceInput                       `json:"source,omitempty"`
	ObservedAt          time.Time                          `json:"observed_at"`
	Lifecycle           instancecorrelation.Lifecycle      `json:"lifecycle"`
	EndedAt             *time.Time                         `json:"ended_at,omitempty"`
	ProcessGroupOrJob   instancepresence.OpaqueIdentity    `json:"process_group_or_job,omitempty"`
	OSSession           instancepresence.OpaqueIdentity    `json:"os_session,omitempty"`
	TerminalFingerprint instancepresence.OpaqueIdentity    `json:"terminal_fingerprint,omitempty"`
}

func (input runtimeInput) domain() instancecorrelation.RuntimeObservation {
	var source *instancepresence.SourceDescriptor
	if input.Source != nil {
		value := input.Source.domain()
		source = &value
	}
	return instancecorrelation.RuntimeObservation{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: instancepresence.InstanceID(input.RuntimeRef), Tool: input.Tool,
			Runtime: input.Runtime.domain(), Members: input.Members,
		},
		Source: source, ObservedAt: input.ObservedAt, Lifecycle: input.Lifecycle, EndedAt: input.EndedAt,
		ProcessGroupOrJob: input.ProcessGroupOrJob, OSSession: input.OSSession,
		TerminalFingerprint: input.TerminalFingerprint,
	}
}

type hookInput struct {
	Tool                instancepresence.ToolKind         `json:"tool"`
	Source              *sourceInput                      `json:"source,omitempty"`
	HookSessionRef      instancepresence.OpaqueIdentity   `json:"hook_session_ref"`
	ProducerEpoch       instancepresence.ProducerEpoch    `json:"producer_epoch"`
	Revision            uint64                            `json:"revision"`
	IdempotencyKey      string                            `json:"idempotency_key,omitempty"`
	ObservedAt          time.Time                         `json:"observed_at"`
	Lifecycle           instancecorrelation.Lifecycle     `json:"lifecycle"`
	ProcessHint         *instancepresence.ProcessIdentity `json:"process_hint,omitempty"`
	RuntimeHint         *runtimeIdentityInput             `json:"runtime_hint,omitempty"`
	ParentOrRootPIDHint *uint64                           `json:"parent_or_root_pid_hint,omitempty"`
	ProcessGroupOrJob   instancepresence.OpaqueIdentity   `json:"process_group_or_job,omitempty"`
	OSSession           instancepresence.OpaqueIdentity   `json:"os_session,omitempty"`
	TerminalFingerprint instancepresence.OpaqueIdentity   `json:"terminal_fingerprint,omitempty"`
	HostID              string                            `json:"host_id,omitempty"`
	BootID              instancepresence.BootIdentity     `json:"boot_id,omitempty"`
}

func (input hookInput) domain() instancecorrelation.HookObservation {
	var source *instancepresence.SourceDescriptor
	if input.Source != nil {
		value := input.Source.domain()
		source = &value
	}
	var runtimeHint *instancepresence.RuntimeIdentity
	if input.RuntimeHint != nil {
		value := input.RuntimeHint.domain()
		runtimeHint = &value
	}
	return instancecorrelation.HookObservation{
		Tool: input.Tool, Source: source, HookSessionRef: input.HookSessionRef,
		ProducerEpoch: input.ProducerEpoch, Revision: input.Revision, IdempotencyKey: input.IdempotencyKey,
		ObservedAt: input.ObservedAt, Lifecycle: input.Lifecycle, ProcessHint: input.ProcessHint,
		RuntimeHint: runtimeHint, ParentOrRootPIDHint: input.ParentOrRootPIDHint,
		ProcessGroupOrJob: input.ProcessGroupOrJob, OSSession: input.OSSession,
		TerminalFingerprint: input.TerminalFingerprint, HostID: input.HostID, BootID: input.BootID,
	}
}

type outputReport struct {
	Mode string `json:"mode"`
	instancecorrelation.CorrelationResult
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, systemClock{}); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(arguments []string, stdin io.Reader, stdout, stderr io.Writer, clock instancepresence.Clock) error {
	flags := flag.NewFlagSet("aurora-presence-correlate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inputPath := flags.String("input", "", "sanitized JSON input file; use - for stdin (default: empty finite report)")
	pretty := flags.Bool("pretty", false, "indent deterministic JSON output")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Observe-only Aurora correlation; no binding or state mutation is performed.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-presence-correlate [-input FILE|-] [-pretty]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if clock == nil {
		return errors.New("clock must not be nil")
	}

	document := inputDocument{EvaluatedAt: clock.Now().UTC(), Runtimes: []runtimeInput{}, Hooks: []hookInput{}}
	if strings.TrimSpace(*inputPath) != "" {
		reader, closeReader, err := inputReader(*inputPath, stdin)
		if err != nil {
			return err
		}
		if closeReader != nil {
			defer closeReader()
		}
		if err := decodeInput(reader, &document); err != nil {
			return err
		}
		if document.EvaluatedAt.IsZero() {
			document.EvaluatedAt = clock.Now().UTC()
		}
	}

	input := instancecorrelation.CorrelationInput{
		EvaluatedAt: document.EvaluatedAt, ExpectedMatches: document.ExpectedMatches,
	}
	for _, runtime := range document.Runtimes {
		input.Runtimes = append(input.Runtimes, runtime.domain())
	}
	for _, hook := range document.Hooks {
		input.Hooks = append(input.Hooks, hook.domain())
	}
	engine, err := instancecorrelation.New(instancecorrelation.DefaultConfig())
	if err != nil {
		return fmt.Errorf("create correlation engine: %w", err)
	}
	result, err := engine.Correlate(input)
	if err != nil {
		return fmt.Errorf("correlate sanitized observations: %w", err)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if *pretty {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(outputReport{Mode: "observe-only", CorrelationResult: result}); err != nil {
		return fmt.Errorf("encode correlation report: %w", err)
	}
	return nil
}

func inputReader(name string, stdin io.Reader) (io.Reader, func() error, error) {
	if name == "-" {
		return stdin, nil, nil
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, nil, fmt.Errorf("open sanitized correlation input: %w", err)
	}
	return file, file.Close, nil
}

func decodeInput(reader io.Reader, document *inputDocument) error {
	limited := &io.LimitedReader{R: reader, N: maximumInputBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(document); err != nil {
		return fmt.Errorf("decode sanitized correlation input: %w", err)
	}
	if limited.N <= 0 {
		return errors.New("sanitized correlation input exceeds 1 MiB")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("sanitized correlation input contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing correlation input: %w", err)
	}
	return nil
}
