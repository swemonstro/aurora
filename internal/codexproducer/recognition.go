package codexproducer

import (
	"context"
	"fmt"
	"time"

	"github.com/swemonstro/aurora/internal/codexhook"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/producerprotocol"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

// recognitionHostID is an internal, fixed, opaque label fed to
// runtimerecognition.Recognize's bookkeeping. It is never sent over the wire
// and never used as this producer's own identity: DeriveInstanceID computes
// this package's actual instance_id independently, from source label + PID +
// StartedAt only, so this constant carries no semantics beyond satisfying
// runtimerecognition's non-empty-host-ID precondition.
const recognitionHostID = "aurora-codex-presence"

// RecognizedInstance is one Codex OS process generation this poll observed,
// already attributed to exactly one configured source. Processes whose
// CODEX_HOME does not match any configured source are never represented
// here (see SourceSet.Match) — they are silently excluded from every poll,
// not reported as errors, matching "processer från en okonfigurerad
// CODEX_HOME ska ignoreras."
type RecognizedInstance struct {
	InstanceID producerprotocol.InstanceID
	Source     SourceLabel
	PID        uint64
	StartedAt  time.Time
	// ProcessGroupOrJob and OSSession are the recognized root process's own
	// OS process-group and session identifiers (opaque, content-free tags
	// such as "pgrp:1234" — never a raw command line, cwd, or path). They
	// are used only for correlating a hook session to the right concurrent
	// instance in correlate.go, never logged as-is beyond these opaque tags.
	ProcessGroupOrJob instancepresence.OpaqueIdentity
	OSSession         instancepresence.OpaqueIdentity
}

// Recognizer polls /proc for Codex processes and attributes each one to a
// configured source. It reuses internal/runtimerecognition (the generic,
// tool-agnostic engine) and internal/codexhook's Codex-specific recognizer
// and launch-identity rules — the same recognition logic
// cmd/aurora-runtime-presence already uses for the monolith — rather than
// re-deriving process-recognition, generation-safety (PID + start time), or
// PID-reuse handling from scratch.
type Recognizer struct {
	adapter *linuxprocess.Adapter
	sources *SourceSet
}

// NewRecognizer builds a Recognizer rooted at procRoot (normally "/proc").
func NewRecognizer(procRoot string, clock instancepresence.Clock, sources *SourceSet) (*Recognizer, error) {
	adapter, err := linuxprocess.New(linuxprocess.Config{
		ProcRoot:            procRoot,
		HostID:              recognitionHostID,
		Clock:               clock,
		LaunchIdentityRules: codexhook.LaunchIdentityRules(),
	})
	if err != nil {
		return nil, fmt.Errorf("codex process recognizer: %w", err)
	}
	return &Recognizer{adapter: adapter, sources: sources}, nil
}

// Observe takes one bounded /proc snapshot and returns every recognized
// Codex process attributable to a configured source. Recognition uses only
// internal/codexhook.RuntimeRecognizer() — never Claude's or Grok's
// recognizer — so this producer can never attribute a non-Codex process to
// itself.
func (recognizer *Recognizer) Observe(ctx context.Context) ([]RecognizedInstance, error) {
	sample, err := recognizer.adapter.Observe(ctx)
	if err != nil {
		return nil, fmt.Errorf("observe processes: %w", err)
	}
	return RecognizeFromSnapshot(sample.Recognition, recognizer.sources)
}

// RecognizeFromSnapshot runs Codex-only recognition and source attribution
// over an already-captured runtimerecognition.Snapshot. It contains no
// filesystem or /proc access itself (that is Observe/linuxprocess's job),
// which makes it exercisable in tests with hand-built snapshots — including
// multi-instance, multi-source, and PID-reuse scenarios — without a real
// Linux proc tree.
func RecognizeFromSnapshot(snapshot runtimerecognition.Snapshot, sources *SourceSet) ([]RecognizedInstance, error) {
	result, err := runtimerecognition.Recognize(snapshot, recognitionHostID, codexhook.RuntimeRecognizer())
	if err != nil {
		return nil, fmt.Errorf("recognize codex runtimes: %w", err)
	}
	// result.Observations[i] corresponds to result.Families[i]: both are
	// built from the same ordered families slice inside Recognize, one
	// observation per family. Zipping by index gives each family's root
	// process group/session hints without re-deriving them.
	instances := make([]RecognizedInstance, 0, len(result.Families))
	for index, family := range result.Families {
		if family.Candidate.Tool != instancepresence.ToolCodex {
			continue
		}
		source, matched := sources.Match(family.EnvCodexHome)
		if !matched {
			continue
		}
		root := family.Candidate.Runtime.RootProcess
		instance := RecognizedInstance{
			InstanceID: DeriveInstanceID(source, root.PID, root.StartedAt),
			Source:     source,
			PID:        root.PID,
			StartedAt:  root.StartedAt,
		}
		if index < len(result.Observations) {
			instance.ProcessGroupOrJob = result.Observations[index].ProcessGroupOrJob
			instance.OSSession = result.Observations[index].OSSession
		}
		instances = append(instances, instance)
	}
	return instances, nil
}
