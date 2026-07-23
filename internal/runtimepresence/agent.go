// Package runtimepresence publishes coarse v1 presence for secure Linux
// runtime families discovered via process observation. It never mutates
// registry/slots, hook state, correlation, or ESP contracts.
package runtimepresence

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
	"github.com/swemonstro/aurora/internal/status"
)

const (
	// SourceClaudeRuntime is the v1 presence source for secure Claude families.
	SourceClaudeRuntime = "claude-runtime"
	// SourceCodexRuntime is the v1 presence source for secure Codex families.
	SourceCodexRuntime = "codex-runtime"

	// DefaultPollInterval is the default /proc observation period.
	DefaultPollInterval = 2 * time.Second
)

// Clock supplies observation timestamps for published snapshots.
type Clock func() time.Time

// PresencePublisher publishes and removes v1 presence sources.
type PresencePublisher interface {
	publish.Publisher
	publish.SourceRemover
}

// FamilyCounts is the number of secure (non-ambiguous) runtime families.
type FamilyCounts struct {
	Claude int
	Codex  int
}

// CountSecureFamilies counts only Families from recognition. Uncertain /
// ambiguous families never create presence.
func CountSecureFamilies(result runtimerecognition.Result) FamilyCounts {
	var counts FamilyCounts
	for _, family := range result.Families {
		switch family.Candidate.Tool {
		case instancepresence.ToolClaude:
			counts.Claude++
		case instancepresence.ToolCodex:
			counts.Codex++
		}
	}
	return counts
}

// Agent tracks whether each runtime source is currently published and only
// emits Publish/Remove when presence of a tool family crosses 0.
type Agent struct {
	publisher PresencePublisher
	clock     Clock
	claudeOn  bool
	codexOn   bool
}

// New constructs a runtime presence agent.
func New(publisher PresencePublisher, clock Clock) (*Agent, error) {
	if publisher == nil {
		return nil, fmt.Errorf("publisher must not be nil")
	}
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}
	return &Agent{publisher: publisher, clock: clock}, nil
}

// Apply reconciles presence sources with the latest secure family counts.
// Publishing is change-only: 1→2 and 2→1 do not re-publish or delete.
func (agent *Agent) Apply(ctx context.Context, counts FamilyCounts) error {
	if err := agent.sync(ctx, SourceClaudeRuntime, counts.Claude > 0, &agent.claudeOn); err != nil {
		return err
	}
	if err := agent.sync(ctx, SourceCodexRuntime, counts.Codex > 0, &agent.codexOn); err != nil {
		return err
	}
	return nil
}

// ApplyRecognition is a convenience wrapper over CountSecureFamilies + Apply.
func (agent *Agent) ApplyRecognition(ctx context.Context, result runtimerecognition.Result) error {
	return agent.Apply(ctx, CountSecureFamilies(result))
}

// Shutdown removes any runtime sources this agent currently has published.
func (agent *Agent) Shutdown(ctx context.Context) error {
	var first error
	if agent.claudeOn {
		if err := agent.publisher.Remove(ctx, SourceClaudeRuntime); err != nil && first == nil {
			first = fmt.Errorf("remove %s: %w", SourceClaudeRuntime, err)
		} else if err == nil {
			agent.claudeOn = false
		}
	}
	if agent.codexOn {
		if err := agent.publisher.Remove(ctx, SourceCodexRuntime); err != nil && first == nil {
			first = fmt.Errorf("remove %s: %w", SourceCodexRuntime, err)
		} else if err == nil {
			agent.codexOn = false
		}
	}
	return first
}

// ClaudePublished reports whether claude-runtime is currently considered on.
func (agent *Agent) ClaudePublished() bool { return agent.claudeOn }

// CodexPublished reports whether codex-runtime is currently considered on.
func (agent *Agent) CodexPublished() bool { return agent.codexOn }

func (agent *Agent) sync(ctx context.Context, source string, present bool, active *bool) error {
	source = strings.TrimSpace(source)
	if present == *active {
		return nil
	}
	if present {
		snapshot := presence.Snapshot{
			Version:   presence.ProtocolVersion,
			Source:    source,
			State:     status.Idle,
			Timestamp: agent.clock().UTC(),
		}
		if err := agent.publisher.Publish(ctx, snapshot); err != nil {
			return fmt.Errorf("publish %s: %w", source, err)
		}
		*active = true
		return nil
	}
	if err := agent.publisher.Remove(ctx, source); err != nil {
		return fmt.Errorf("remove %s: %w", source, err)
	}
	*active = false
	return nil
}
