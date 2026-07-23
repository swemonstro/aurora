package runtimepresence

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

// DefaultBridgeInterval matches runtime polling so relay recovery stays aligned.
const DefaultBridgeInterval = DefaultPollInterval

// InstanceSource supplies active v2 instances for aggregation.
type InstanceSource interface {
	ActiveInstances() []instancepresence.Instance
}

// RelayBridge publishes a coarse per-tool v1 presence view into an existing
// relay. It never changes ESP/relay contracts and never includes content data.
//
// When active, this bridge owns the v1 sources claude-runtime and codex-runtime.
// A separate aurora-runtime-presence process must not publish the same sources
// at the same time (stop it at future deploy time to avoid dual producers).
type RelayBridge struct {
	source    InstanceSource
	publisher PresencePublisher
	clock     Clock
	interval  time.Duration
	stderr    io.Writer

	mu sync.Mutex
	// lastOK records the last successfully applied desired view per source.
	// absent (ok=false) means Remove succeeded last; present means Publish of state succeeded.
	lastOK map[string]publishedView
}

type publishedView struct {
	present bool
	state   status.State
}

// NewRelayBridge constructs a v2→v1 bridge. interval defaults to DefaultBridgeInterval.
func NewRelayBridge(
	source InstanceSource,
	publisher PresencePublisher,
	clock Clock,
	interval time.Duration,
	stderr io.Writer,
) (*RelayBridge, error) {
	if source == nil {
		return nil, fmt.Errorf("instance source must not be nil")
	}
	if publisher == nil {
		return nil, fmt.Errorf("publisher must not be nil")
	}
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}
	if interval <= 0 {
		interval = DefaultBridgeInterval
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &RelayBridge{
		source:    source,
		publisher: publisher,
		clock:     clock,
		interval:  interval,
		stderr:    stderr,
		lastOK:    make(map[string]publishedView),
	}, nil
}

// Run periodically reconciles until ctx is cancelled.
func (bridge *RelayBridge) Run(ctx context.Context) {
	// Immediate reconcile so a restarting process restores relay promptly.
	bridge.Reconcile(ctx)
	ticker := time.NewTicker(bridge.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bridge.Reconcile(ctx)
		}
	}
}

// Reconcile computes desired per-tool v1 state and publishes or removes.
// Unchanged active states are re-published so an empty/restarted relay recovers.
// Failures are logged and retried on the next call; lastOK is only updated on success.
func (bridge *RelayBridge) Reconcile(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	desired := aggregateByTool(bridge.source.ActiveInstances())
	for _, tool := range []instancepresence.ToolKind{instancepresence.ToolClaude, instancepresence.ToolCodex} {
		source := sourceForTool(tool)
		view, present := desired[tool]
		if !present {
			bridge.removeSource(ctx, source)
			continue
		}
		bridge.publishSource(ctx, source, view)
	}
}

// LastPublished reports the last successfully published view for a source (tests).
func (bridge *RelayBridge) LastPublished(source string) (state status.State, present bool) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	view, ok := bridge.lastOK[source]
	if !ok || !view.present {
		return "", false
	}
	return view.state, true
}

func (bridge *RelayBridge) publishSource(ctx context.Context, source string, state status.State) {
	snapshot := presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    source,
		State:     state,
		Timestamp: bridge.clock().UTC(),
	}
	if err := bridge.publisher.Publish(ctx, snapshot); err != nil {
		fmt.Fprintf(bridge.stderr, "relay bridge publish %s: %v\n", source, err)
		return
	}
	bridge.mu.Lock()
	bridge.lastOK[source] = publishedView{present: true, state: state}
	bridge.mu.Unlock()
}

func (bridge *RelayBridge) removeSource(ctx context.Context, source string) {
	if err := bridge.publisher.Remove(ctx, source); err != nil {
		fmt.Fprintf(bridge.stderr, "relay bridge remove %s: %v\n", source, err)
		return
	}
	bridge.mu.Lock()
	bridge.lastOK[source] = publishedView{present: false}
	bridge.mu.Unlock()
}

func sourceForTool(tool instancepresence.ToolKind) string {
	switch tool {
	case instancepresence.ToolClaude:
		return SourceClaudeRuntime
	case instancepresence.ToolCodex:
		return SourceCodexRuntime
	default:
		return ""
	}
}

// aggregateByTool returns the highest-priority effective state per tool among
// active instances. Priority: error > attention > working > idle.
func aggregateByTool(instances []instancepresence.Instance) map[instancepresence.ToolKind]status.State {
	out := make(map[instancepresence.ToolKind]status.State)
	for _, instance := range instances {
		if !instance.Status.Active() {
			continue
		}
		mapped, ok := effectiveToV1(instance.State)
		if !ok {
			continue
		}
		current, exists := out[instance.Tool]
		if !exists || v1Priority(mapped) > v1Priority(current) {
			out[instance.Tool] = mapped
		}
	}
	return out
}

func effectiveToV1(state instancepresence.EffectiveState) (status.State, bool) {
	switch state {
	case instancepresence.StateIdle:
		return status.Idle, true
	case instancepresence.StateWorking:
		return status.Working, true
	case instancepresence.StateAttention:
		return status.Attention, true
	case instancepresence.StateError:
		return status.Error, true
	default:
		return "", false
	}
}

func v1Priority(state status.State) int {
	switch state {
	case status.Error:
		return 4
	case status.Attention:
		return 3
	case status.Working:
		return 2
	case status.Idle:
		return 1
	default:
		return 0
	}
}
