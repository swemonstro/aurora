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
	trigger   chan struct{}

	mu      sync.Mutex
	pending []desiredView
	// lastOK records the last successfully applied desired view per source.
	// absent (ok=false) means Remove succeeded last; present means Publish of state succeeded.
	lastOK map[string]publishedView
}

type publishedView struct {
	present bool
	state   status.State
}

type desiredView map[string]publishedView

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
		trigger:   make(chan struct{}, 1),
		lastOK:    make(map[string]publishedView),
	}, nil
}

// Trigger captures the current aggregate view and queues state changes
// in order. The wake-up signal may be coalesced, but queued transitions are not.
func (bridge *RelayBridge) Trigger() {
	desired := bridge.captureDesired()

	bridge.mu.Lock()
	previous := bridge.lastDesiredLocked()
	if len(bridge.pending) > 0 {
		previous = bridge.pending[len(bridge.pending)-1]
	}
	if desiredViewsEqual(previous, desired) {
		bridge.mu.Unlock()
		return
	}
	bridge.pending = append(bridge.pending, desired)
	bridge.mu.Unlock()

	select {
	case bridge.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles on startup, explicit triggers and the recovery ticker.
func (bridge *RelayBridge) Run(ctx context.Context) {
	// Immediate reconcile so a restarting process restores relay promptly.
	bridge.Reconcile(ctx)
	ticker := time.NewTicker(bridge.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-bridge.trigger:
			bridge.drainPending(ctx)
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
	bridge.applyDesired(ctx, bridge.captureDesired())
}

func (bridge *RelayBridge) captureDesired() desiredView {
	desired := desiredView{
		SourceClaudeRuntime: {present: false},
		SourceCodexRuntime:  {present: false},
	}

	for tool, state := range aggregateByTool(bridge.source.ActiveInstances()) {
		source := sourceForTool(tool)
		if source != "" {
			desired[source] = publishedView{
				present: true,
				state:   state,
			}
		}
	}

	return desired
}

func (bridge *RelayBridge) applyDesired(ctx context.Context, desired desiredView) {
	for _, source := range []string{
		SourceClaudeRuntime,
		SourceCodexRuntime,
	} {
		view := desired[source]
		if !view.present {
			bridge.removeSource(ctx, source)
			continue
		}
		bridge.publishSource(ctx, source, view.state)
	}
}

func (bridge *RelayBridge) drainPending(ctx context.Context) {
	for {
		bridge.mu.Lock()
		if len(bridge.pending) == 0 {
			bridge.mu.Unlock()
			return
		}

		desired := bridge.pending[0]
		bridge.pending = bridge.pending[1:]
		if len(bridge.pending) == 0 {
			bridge.pending = nil
		}
		bridge.mu.Unlock()

		bridge.applyDesired(ctx, desired)
	}
}

func (bridge *RelayBridge) lastDesiredLocked() desiredView {
	desired := desiredView{
		SourceClaudeRuntime: {present: false},
		SourceCodexRuntime:  {present: false},
	}

	for source, view := range bridge.lastOK {
		desired[source] = view
	}

	return desired
}

func desiredViewsEqual(left, right desiredView) bool {
	for _, source := range []string{
		SourceClaudeRuntime,
		SourceCodexRuntime,
	} {
		if left[source] != right[source] {
			return false
		}
	}
	return true
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
