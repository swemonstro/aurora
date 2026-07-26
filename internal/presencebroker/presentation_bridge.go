package presencebroker

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

// DefaultBridgeInterval is this package's default reconciliation interval.
// It intentionally duplicates the numeric value of
// runtimepresence.DefaultPollInterval rather than importing runtimepresence
// (which pulls in tool-specific trust/recognition packages): the broker
// core must not depend on the tool-specific layer, only the reverse.
const DefaultBridgeInterval = 2 * time.Second

// PresentationSource supplies a pixel-slot presentation from the v2 registry.
type PresentationSource interface {
	Presentation(pixelCapacity uint64) (presencev2.Presentation, error)
}

// PresentationPublisher posts or clears the relay presentation document.
type PresentationPublisher interface {
	PublishPresentation(context.Context, presencev2.Presentation) error
	RemovePresentation(context.Context) error
}

// desiredPresentation is the bridge-internal desired relay state.
// present=false means the presentation must be absent (DELETE), never a
// zero active_count document left on the relay.
type desiredPresentation struct {
	present      bool
	presentation presencev2.Presentation
}

// PresentationBridge publishes registry.Presentation(pixelCapacity) to the
// relay /presence/presentation endpoint. It is independent of any v1
// aggregate bridge and never mutates GET /presence aggregation.
//
// Trigger captures presentation snapshots synchronously and queues distinct
// states in order so rapid transitions (e.g. working→idle) are not coalesced.
// The wake-up channel may merge signals; the pending queue does not.
type PresentationBridge struct {
	source        PresentationSource
	publisher     PresentationPublisher
	pixelCapacity uint64
	interval      time.Duration
	stderr        io.Writer
	trigger       chan struct{}

	mu      sync.Mutex
	pending []desiredPresentation
	// lastOK is the last successfully applied desired state (including absent).
	lastOK desiredPresentation
	hasOK  bool
}

// NewPresentationBridge constructs a presentation publisher bridge.
func NewPresentationBridge(
	source PresentationSource,
	publisher PresentationPublisher,
	pixelCapacity uint64,
	interval time.Duration,
	stderr io.Writer,
) (*PresentationBridge, error) {
	if source == nil {
		return nil, fmt.Errorf("presentation source must not be nil")
	}
	if publisher == nil {
		return nil, fmt.Errorf("presentation publisher must not be nil")
	}
	if pixelCapacity == 0 {
		return nil, fmt.Errorf("pixel capacity must be positive")
	}
	if interval <= 0 {
		interval = DefaultBridgeInterval
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &PresentationBridge{
		source:        source,
		publisher:     publisher,
		pixelCapacity: pixelCapacity,
		interval:      interval,
		stderr:        stderr,
		trigger:       make(chan struct{}, 1),
	}, nil
}

// Trigger captures the current presentation and queues it when distinct from
// the last queued (or last successfully applied) state.
func (bridge *PresentationBridge) Trigger() {
	desired, err := bridge.captureDesired()
	if err != nil {
		fmt.Fprintf(bridge.stderr, "presentation bridge capture: %v\n", err)
		return
	}

	bridge.mu.Lock()
	previous := bridge.lastDesiredLocked()
	if len(bridge.pending) > 0 {
		previous = bridge.pending[len(bridge.pending)-1]
	}
	if desiredPresentationsEqual(previous, desired) {
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

// Run drains queued transitions before startup or periodic reconciliation.
// A failed queue head blocks reconciliation so newer source state cannot pass
// an older transition that still requires delivery.
func (bridge *PresentationBridge) Run(ctx context.Context) {
	if bridge.drainPending(ctx) {
		bridge.Reconcile(ctx)
	}

	ticker := time.NewTicker(bridge.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-bridge.trigger:
			bridge.drainPending(ctx)

		case <-ticker.C:
			// Retry queued transitions first. Reconcile only when every
			// queued transition was delivered successfully.
			if bridge.drainPending(ctx) {
				bridge.Reconcile(ctx)
			}
		}
	}
}

// Reconcile reads the registry and applies the desired presentation now.
// Unchanged active presentations are re-published so a restarted relay recovers.
func (bridge *PresentationBridge) Reconcile(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	desired, err := bridge.captureDesired()
	if err != nil {
		fmt.Fprintf(bridge.stderr, "presentation bridge capture: %v\n", err)
		return
	}
	bridge.applyDesired(ctx, desired)
}

func (bridge *PresentationBridge) captureDesired() (desiredPresentation, error) {
	presentation, err := bridge.source.Presentation(bridge.pixelCapacity)
	if err != nil {
		return desiredPresentation{}, err
	}
	// Detach slices so later registry mutations cannot rewrite queued history.
	cloned := clonePresentation(presentation)
	if cloned.ActiveCount == 0 {
		return desiredPresentation{present: false}, nil
	}
	return desiredPresentation{present: true, presentation: cloned}, nil
}

func (bridge *PresentationBridge) applyDesired(ctx context.Context, desired desiredPresentation) bool {
	if !desired.present {
		if err := bridge.publisher.RemovePresentation(ctx); err != nil {
			fmt.Fprintf(bridge.stderr, "presentation bridge remove: %v\n", err)
			return false
		}
		bridge.mu.Lock()
		bridge.lastOK = desiredPresentation{present: false}
		bridge.hasOK = true
		bridge.mu.Unlock()
		return true
	}
	if err := bridge.publisher.PublishPresentation(ctx, desired.presentation); err != nil {
		fmt.Fprintf(bridge.stderr, "presentation bridge publish: %v\n", err)
		return false
	}
	bridge.mu.Lock()
	bridge.lastOK = desired
	bridge.hasOK = true
	bridge.mu.Unlock()
	return true
}

// drainPending applies queued presentations in order. It returns true only
// when the entire queue was delivered. A failed publish/remove leaves the head
// in place and returns false so reconciliation cannot bypass it.
func (bridge *PresentationBridge) drainPending(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		bridge.mu.Lock()
		if len(bridge.pending) == 0 {
			bridge.mu.Unlock()
			return true
		}
		desired := bridge.pending[0]
		bridge.mu.Unlock()

		if !bridge.applyDesired(ctx, desired) {
			return false
		}

		bridge.mu.Lock()
		if len(bridge.pending) > 0 {
			bridge.pending = bridge.pending[1:]
			if len(bridge.pending) == 0 {
				bridge.pending = nil
			}
		}
		bridge.mu.Unlock()
	}
}

func (bridge *PresentationBridge) lastDesiredLocked() desiredPresentation {
	if !bridge.hasOK {
		return desiredPresentation{}
	}
	return bridge.lastOK
}

// LastApplied reports the last successfully applied state (tests).
func (bridge *PresentationBridge) LastApplied() (presencev2.Presentation, bool, bool) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if !bridge.hasOK {
		return presencev2.Presentation{}, false, false
	}
	if !bridge.lastOK.present {
		return presencev2.Presentation{}, false, true
	}
	return clonePresentation(bridge.lastOK.presentation), true, true
}

func desiredPresentationsEqual(left, right desiredPresentation) bool {
	if left.present != right.present {
		return false
	}
	if !left.present {
		return true
	}
	return presentationsEqual(left.presentation, right.presentation)
}

func presentationsEqual(left, right presencev2.Presentation) bool {
	if left.APIVersion != right.APIVersion ||
		left.Mode != right.Mode ||
		left.PixelCapacity != right.PixelCapacity ||
		left.ActiveCount != right.ActiveCount ||
		left.VisibleCount != right.VisibleCount ||
		left.OverflowCount != right.OverflowCount {
		return false
	}
	if !reflect.DeepEqual(left.Pixels, right.Pixels) {
		return false
	}
	if !reflect.DeepEqual(left.OverflowInstanceIDs, right.OverflowInstanceIDs) {
		return false
	}
	return true
}

func clonePresentation(presentation presencev2.Presentation) presencev2.Presentation {
	clone := presentation
	if presentation.Pixels == nil {
		clone.Pixels = []presencev2.Pixel{}
	} else {
		clone.Pixels = append([]presencev2.Pixel{}, presentation.Pixels...)
	}
	if presentation.OverflowInstanceIDs == nil {
		clone.OverflowInstanceIDs = []instancepresence.InstanceID{}
	} else {
		clone.OverflowInstanceIDs = append(
			[]instancepresence.InstanceID{},
			presentation.OverflowInstanceIDs...,
		)
	}
	return clone
}
