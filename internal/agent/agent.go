package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/status"
)

type Clock func() time.Time

type Agent struct {
	source    string
	publisher publish.Publisher
	clock     Clock
}

func New(source string, publisher publish.Publisher, clock Clock) (*Agent, error) {
	source = strings.TrimSpace(source)

	if source == "" {
		return nil, fmt.Errorf("source must not be empty")
	}
	if publisher == nil {
		return nil, fmt.Errorf("publisher must not be nil")
	}
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}

	return &Agent{
		source:    source,
		publisher: publisher,
		clock:     clock,
	}, nil
}

func (a *Agent) Handle(ctx context.Context, event string) error {
	state, err := status.Normalize(event)
	if err != nil {
		return fmt.Errorf("normalize event: %w", err)
	}

	snapshot := presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    a.source,
		State:     state,
		Timestamp: a.clock().UTC(),
	}

	if err := a.publisher.Publish(ctx, snapshot); err != nil {
		return fmt.Errorf("publish presence snapshot: %w", err)
	}

	return nil
}

// HandleLifecycle publishes the aggregate while a source has sessions and
// removes the source from the relay after its final session ends.
func (a *Agent) HandleLifecycle(ctx context.Context, event string, active bool) error {
	if active {
		return a.Handle(ctx, event)
	}

	remover, ok := a.publisher.(publish.SourceRemover)
	if !ok {
		return fmt.Errorf("publisher does not support source removal")
	}
	if err := remover.Remove(ctx, a.source); err != nil {
		return fmt.Errorf("remove presence source: %w", err)
	}
	return nil
}
