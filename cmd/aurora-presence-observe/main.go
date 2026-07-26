package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/claudehook"
	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/grokpresence"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type outputReport struct {
	Mode        string                         `json:"mode"`
	Sample      int                            `json:"sample"`
	ObservedAt  time.Time                      `json:"observed_at"`
	Summary     outputSummary                  `json:"summary"`
	Families    []outputFamily                 `json:"families"`
	Uncertain   []outputUncertainFamily        `json:"uncertain_families"`
	Diagnostics []linuxprocess.Diagnostic      `json:"diagnostics"`
	Exits       []instancepresence.ProcessExit `json:"exits"`
}

type outputSummary struct {
	ObservedProcesses uint64 `json:"observed_processes"`
	ClaudeFamilies    uint64 `json:"claude_families"`
	CodexFamilies     uint64 `json:"codex_families"`
	GrokFamilies      uint64 `json:"grok_families"`
	UnknownProcesses  uint64 `json:"unknown_processes"`
	AmbiguousFamilies uint64 `json:"ambiguous_families"`
}

type outputFamily struct {
	CandidateRef string                           `json:"candidate_ref"`
	Tool         instancepresence.ToolKind        `json:"tool"`
	Root         instancepresence.ProcessIdentity `json:"root"`
	MemberCount  int                              `json:"member_count"`
	Shape        string                           `json:"shape"`
	ReasonCodes  []runtimerecognition.ReasonCode  `json:"reason_codes"`
}

type outputUncertainFamily struct {
	Tool          instancepresence.ToolKind          `json:"tool"`
	PossibleRoots []instancepresence.ProcessIdentity `json:"possible_roots"`
	MemberCount   int                                `json:"member_count"`
	ReasonCodes   []runtimerecognition.ReasonCode    `json:"reason_codes"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, systemClock{}, time.Sleep); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	clock instancepresence.Clock,
	wait func(time.Duration),
) error {
	flags := flag.NewFlagSet("aurora-presence-observe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	procRoot := flags.String("proc-root", "/proc", "read-only proc filesystem root")
	hostID := flags.String("host-id", "", "required opaque local host ID")
	bootID := flags.String("boot-id", "", "optional opaque boot ID; default reads proc boot_id")
	samples := flags.Int("samples", 1, "bounded number of snapshots")
	interval := flags.Duration("interval", 2*time.Second, "delay between bounded samples")
	format := flags.String("format", "json", "output format: json or text")
	clockTicks := flags.Uint64("clock-ticks", 100, "Linux USER_HZ used by proc start ticks")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Observe-only Aurora Linux process measurement; no state is written.")
		fmt.Fprintln(flags.Output(), "Usage: aurora-presence-observe -host-id OPAQUE [options]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*hostID) == "" {
		flags.Usage()
		return errors.New("host-id is required")
	}
	if *samples < 1 || *samples > 100 {
		return errors.New("samples must be between 1 and 100")
	}
	if *interval < 0 {
		return errors.New("interval must not be negative")
	}
	if *format != "json" && *format != "text" {
		return errors.New("format must be json or text")
	}
	if wait == nil {
		return errors.New("wait function must not be nil")
	}
	adapter, err := linuxprocess.New(linuxprocess.Config{
		ProcRoot: *procRoot, HostID: *hostID, BootID: instancepresence.BootIdentity(*bootID),
		Clock: clock, ClockTicks: *clockTicks,
		LaunchIdentityRules: append(claudehook.LaunchIdentityRules(), codexhook.LaunchIdentityRules()...),
	})
	if err != nil {
		return err
	}
	var previous linuxprocess.Sample
	for index := 1; index <= *samples; index++ {
		sample, observeErr := adapter.Observe(context.Background())
		if observeErr != nil {
			return fmt.Errorf("observe sample %d: %w", index, observeErr)
		}
		exits := []instancepresence.ProcessExit{}
		if index > 1 {
			diff, diffErr := linuxprocess.DiffSamples(previous, sample)
			if diffErr != nil {
				return fmt.Errorf("diff sample %d: %w", index, diffErr)
			}
			exits = diff.Exits
		}
		recognition, recognitionErr := runtimerecognition.Recognize(
			sample.Recognition,
			*hostID,
			claudehook.RuntimeRecognizer(),
			codexhook.RuntimeRecognizer(),
			grokpresence.RuntimeRecognizer(),
		)
		if recognitionErr != nil {
			return fmt.Errorf("recognize sample %d: %w", index, recognitionErr)
		}
		report := makeOutputReport(index, sample, recognition, exits)
		if *format == "json" {
			encoder := json.NewEncoder(stdout)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(report); err != nil {
				return fmt.Errorf("encode observe-only report: %w", err)
			}
		} else {
			writeTextReport(stdout, report)
		}
		previous = sample
		if index < *samples {
			wait(*interval)
		}
	}
	return nil
}

func makeOutputReport(sampleNumber int, sample linuxprocess.Sample, recognition runtimerecognition.Result, exits []instancepresence.ProcessExit) outputReport {
	families := make([]outputFamily, 0, len(recognition.Families))
	summary := outputSummary{ObservedProcesses: uint64(len(sample.Snapshot.Processes)), UnknownProcesses: recognition.UnknownProcesses, AmbiguousFamilies: uint64(len(recognition.UncertainFamilies))}
	for _, family := range recognition.Families {
		if family.Candidate.Tool == instancepresence.ToolClaude {
			summary.ClaudeFamilies++
		} else if family.Candidate.Tool == instancepresence.ToolCodex {
			summary.CodexFamilies++
		} else if family.Candidate.Tool == instancepresence.ToolGrok {
			summary.GrokFamilies++
		}
		families = append(families, outputFamily{
			CandidateRef: string(family.Candidate.InstanceID), Tool: family.Candidate.Tool,
			Root: family.Candidate.Runtime.RootProcess, MemberCount: len(family.Candidate.Members),
			Shape: family.Shape, ReasonCodes: family.ReasonCodes,
		})
	}
	uncertain := make([]outputUncertainFamily, 0, len(recognition.UncertainFamilies))
	for _, family := range recognition.UncertainFamilies {
		uncertain = append(uncertain, outputUncertainFamily{
			Tool: family.Tool, PossibleRoots: family.PossibleRoots,
			MemberCount: len(family.Members), ReasonCodes: family.ReasonCodes,
		})
	}
	return outputReport{
		Mode: "observe-only", Sample: sampleNumber, ObservedAt: sample.Snapshot.ObservedAt,
		Summary:  summary,
		Families: families, Uncertain: uncertain, Diagnostics: mergedDiagnostics(sample.Diagnostics, recognition), Exits: exits,
	}
}

// mergedDiagnostics preserves the established CLI diagnostics channel while
// keeping collection and recognition responsibilities separate internally.
func mergedDiagnostics(collection []linuxprocess.Diagnostic, recognition runtimerecognition.Result) []linuxprocess.Diagnostic {
	counts := make(map[linuxprocess.ReasonCode]uint64, len(collection)+5)
	for _, diagnostic := range collection {
		counts[diagnostic.Code] += diagnostic.Count
	}
	counts[linuxprocess.ReasonCode(runtimerecognition.ReasonUnknownProcess)] += recognition.UnknownProcesses
	for _, family := range recognition.Families {
		for _, reason := range family.ReasonCodes {
			counts[linuxprocess.ReasonCode(reason)]++
		}
	}
	for _, family := range recognition.UncertainFamilies {
		for _, reason := range family.ReasonCodes {
			counts[linuxprocess.ReasonCode(reason)]++
		}
	}
	diagnostics := make([]linuxprocess.Diagnostic, 0, len(counts))
	for code, count := range counts {
		if count > 0 {
			diagnostics = append(diagnostics, linuxprocess.Diagnostic{Code: code, Count: count})
		}
	}
	sort.Slice(diagnostics, func(first, second int) bool {
		return diagnostics[first].Code < diagnostics[second].Code
	})
	return diagnostics
}

func writeTextReport(writer io.Writer, report outputReport) {
	fmt.Fprintf(
		writer,
		"observe-only sample=%d processes=%d claude=%d codex=%d grok=%d unknown=%d ambiguous=%d exits=%d\n",
		report.Sample, report.Summary.ObservedProcesses, report.Summary.ClaudeFamilies,
		report.Summary.CodexFamilies, report.Summary.GrokFamilies,
		report.Summary.UnknownProcesses,
		report.Summary.AmbiguousFamilies, len(report.Exits),
	)
	for _, family := range report.Families {
		fmt.Fprintf(
			writer, "family ref=%s tool=%s root_pid=%d members=%d shape=%s reasons=%v\n",
			family.CandidateRef, family.Tool, family.Root.PID, family.MemberCount, family.Shape, family.ReasonCodes,
		)
	}
	for _, family := range report.Uncertain {
		fmt.Fprintf(
			writer, "uncertain tool=%s possible_roots=%d members=%d reasons=%v\n",
			family.Tool, len(family.PossibleRoots), family.MemberCount, family.ReasonCodes,
		)
	}
}
