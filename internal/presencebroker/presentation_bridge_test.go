package presencebroker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

type memoryPresentationSource struct {
	mu           sync.Mutex
	presentation presencev2.Presentation
	err          error
}

func (s *memoryPresentationSource) Presentation(pixelCapacity uint64) (presencev2.Presentation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return presencev2.Presentation{}, s.err
	}
	out := clonePresentation(s.presentation)
	if out.PixelCapacity == 0 {
		out.PixelCapacity = pixelCapacity
	}
	return out, nil
}

func (s *memoryPresentationSource) set(presentation presencev2.Presentation) {
	s.mu.Lock()
	s.presentation = clonePresentation(presentation)
	s.err = nil
	s.mu.Unlock()
}

type recordingPresentationPublisher struct {
	mu             sync.Mutex
	publishes      []presencev2.Presentation
	removes        int
	publishErr     error
	removeErr      error
	failState      instancepresence.EffectiveState
	failStateCount int
	// Gate lets tests observe queued transitions before the bridge runs.
	gate chan struct{}
}

func (p *recordingPresentationPublisher) PublishPresentation(
	_ context.Context,
	presentation presencev2.Presentation,
) error {
	if p.gate != nil {
		<-p.gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.publishErr != nil {
		return p.publishErr
	}
	if p.failStateCount > 0 &&
		len(presentation.Pixels) > 0 &&
		presentation.Pixels[0].State == p.failState {
		p.failStateCount--
		return errors.New("state-specific publish failure")
	}
	p.publishes = append(p.publishes, clonePresentation(presentation))
	return nil
}

func (p *recordingPresentationPublisher) RemovePresentation(_ context.Context) error {
	if p.gate != nil {
		<-p.gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.removeErr != nil {
		return p.removeErr
	}
	p.removes++
	return nil
}

func (p *recordingPresentationPublisher) snapshot() (publishes []presencev2.Presentation, removes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]presencev2.Presentation{}, p.publishes...), p.removes
}

func emptyPresentation(capacity uint64) presencev2.Presentation {
	return presencev2.Presentation{
		APIVersion:          presencev2.APIVersion,
		Mode:                "slots",
		PixelCapacity:       capacity,
		Pixels:              []presencev2.Pixel{},
		ActiveCount:         0,
		VisibleCount:        0,
		OverflowCount:       0,
		OverflowInstanceIDs: []instancepresence.InstanceID{},
	}
}

func presentationWithPixels(capacity uint64, pixels ...presencev2.Pixel) presencev2.Presentation {
	p := emptyPresentation(capacity)
	p.Pixels = append([]presencev2.Pixel{}, pixels...)
	p.VisibleCount = uint64(len(pixels))
	p.ActiveCount = p.VisibleCount
	return p
}

func TestPresentationBridgeOneSessionPixel0(t *testing.T) {
	src := &memoryPresentationSource{}
	pub := &recordingPresentationPublisher{}
	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking,
	}))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Reconcile(context.Background())
	publishes, removes := pub.snapshot()
	if removes != 0 || len(publishes) != 1 {
		t.Fatalf("publishes=%d removes=%d", len(publishes), removes)
	}
	got := publishes[0]
	if got.PixelCapacity != 5 || got.ActiveCount != 1 || len(got.Pixels) != 1 {
		t.Fatalf("presentation = %#v", got)
	}
	if got.Pixels[0].Pixel != 0 || got.Pixels[0].InstanceID != "inst-a" {
		t.Fatalf("pixel = %#v", got.Pixels[0])
	}
}

func TestPresentationBridgeTwoSessionsSeparateSlots(t *testing.T) {
	src := &memoryPresentationSource{}
	pub := &recordingPresentationPublisher{}
	src.set(presentationWithPixels(5,
		presencev2.Pixel{Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking},
		presencev2.Pixel{Pixel: 1, InstanceID: "inst-b", State: instancepresence.StateAttention},
	))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Reconcile(context.Background())
	publishes, _ := pub.snapshot()
	if len(publishes) != 1 || len(publishes[0].Pixels) != 2 {
		t.Fatalf("publishes = %#v", publishes)
	}
	if publishes[0].Pixels[0].State != instancepresence.StateWorking ||
		publishes[0].Pixels[1].State != instancepresence.StateAttention {
		t.Fatalf("states = %#v", publishes[0].Pixels)
	}
	if publishes[0].Pixels[0].Pixel != 0 || publishes[0].Pixels[1].Pixel != 1 {
		t.Fatalf("pixels = %#v", publishes[0].Pixels)
	}
}

func TestPresentationBridgeRapidTransitionWorkingThenIdle(t *testing.T) {
	src := &memoryPresentationSource{}
	// Gate blocks the publisher until both triggers are queued.
	pub := &recordingPresentationPublisher{gate: make(chan struct{})}
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking,
	}))
	bridge.Trigger()
	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateIdle,
	}))
	bridge.Trigger()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		bridge.Run(ctx)
		close(done)
	}()
	// Unblock publisher after Run started; both queued states must apply in order.
	close(pub.gate)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		publishes, _ := pub.snapshot()
		if len(publishes) >= 2 {
			// First may be empty reconcile from Run start if source still idle;
			// find working followed by idle in sequence.
			for i := 0; i+1 < len(publishes); i++ {
				if len(publishes[i].Pixels) == 1 &&
					publishes[i].Pixels[0].State == instancepresence.StateWorking &&
					len(publishes[i+1].Pixels) == 1 &&
					publishes[i+1].Pixels[0].State == instancepresence.StateIdle {
					cancel()
					<-done
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	publishes, _ := pub.snapshot()
	cancel()
	<-done
	t.Fatalf("rapid transitions not preserved: %#v", publishes)
}

func TestPresentationBridgeOneSlotChangeLeavesOther(t *testing.T) {
	src := &memoryPresentationSource{}
	pub := &recordingPresentationPublisher{}
	src.set(presentationWithPixels(5,
		presencev2.Pixel{Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking},
		presencev2.Pixel{Pixel: 1, InstanceID: "inst-b", State: instancepresence.StateAttention},
	))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Reconcile(context.Background())
	src.set(presentationWithPixels(5,
		presencev2.Pixel{Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateIdle},
		presencev2.Pixel{Pixel: 1, InstanceID: "inst-b", State: instancepresence.StateAttention},
	))
	bridge.Reconcile(context.Background())
	publishes, _ := pub.snapshot()
	last := publishes[len(publishes)-1]
	if last.Pixels[0].State != instancepresence.StateIdle {
		t.Fatalf("slot0 = %#v", last.Pixels[0])
	}
	if last.Pixels[1].Pixel != 1 || last.Pixels[1].State != instancepresence.StateAttention {
		t.Fatalf("slot1 changed unexpectedly: %#v", last.Pixels[1])
	}
}

func TestPresentationBridgeLastSessionRemoves(t *testing.T) {
	src := &memoryPresentationSource{}
	pub := &recordingPresentationPublisher{}
	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking,
	}))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Reconcile(context.Background())
	src.set(emptyPresentation(5))
	bridge.Reconcile(context.Background())
	publishes, removes := pub.snapshot()
	if removes != 1 {
		t.Fatalf("removes = %d, publishes = %#v", removes, publishes)
	}
	_, present, applied := bridge.LastApplied()
	if !applied || present {
		t.Fatalf("last applied present=%v applied=%v", present, applied)
	}
}

func TestPresentationBridgePeriodicRepublish(t *testing.T) {
	src := &memoryPresentationSource{}
	pub := &recordingPresentationPublisher{}
	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking,
	}))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Reconcile(context.Background())
	// Simulate cleared relay by resetting publisher history while bridge still
	// believes presentation is published; Reconcile re-POSTs unchanged state.
	pub.mu.Lock()
	pub.publishes = nil
	pub.mu.Unlock()
	bridge.Reconcile(context.Background())
	publishes, _ := pub.snapshot()
	if len(publishes) != 1 || publishes[0].ActiveCount != 1 {
		t.Fatalf("recovery publishes = %#v", publishes)
	}
}

func TestPresentationBridgeFailedHeadBlocksPeriodicReconcile(t *testing.T) {
	src := &memoryPresentationSource{}
	pub := &recordingPresentationPublisher{
		failState:      instancepresence.StateWorking,
		failStateCount: 2,
	}
	src.set(emptyPresentation(5))

	bridge, err := NewPresentationBridge(
		src,
		pub,
		5,
		20*time.Millisecond,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		bridge.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Wait for startup reconciliation to establish the absent state.
	startupDeadline := time.Now().Add(time.Second)
	for time.Now().Before(startupDeadline) {
		_, removes := pub.snapshot()
		if removes > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel:      0,
		InstanceID: "inst-a",
		State:      instancepresence.StateWorking,
	}))
	bridge.Trigger()

	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel:      0,
		InstanceID: "inst-a",
		State:      instancepresence.StateIdle,
	}))
	bridge.Trigger()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		publishes, _ := pub.snapshot()

		if len(publishes) > 0 {
			if len(publishes[0].Pixels) != 1 ||
				publishes[0].Pixels[0].State != instancepresence.StateWorking {
				t.Fatalf(
					"newer state bypassed failed queue head: %#v",
					publishes,
				)
			}
		}

		if len(publishes) >= 2 {
			if len(publishes[1].Pixels) != 1 ||
				publishes[1].Pixels[0].State != instancepresence.StateIdle {
				t.Fatalf(
					"queued states published out of order: %#v",
					publishes,
				)
			}
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	publishes, _ := pub.snapshot()
	t.Fatalf("ordered retry did not complete: %#v", publishes)
}

func TestPresentationBridgePublishFailureRetried(t *testing.T) {
	src := &memoryPresentationSource{}
	var stderr strings.Builder
	pub := &recordingPresentationPublisher{publishErr: errors.New("relay down")}
	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking,
	}))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Reconcile(context.Background())
	if _, _, applied := bridge.LastApplied(); applied {
		t.Fatal("failed publish must not mark applied")
	}
	if !strings.Contains(stderr.String(), "presentation bridge publish") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	pub.mu.Lock()
	pub.publishErr = nil
	pub.mu.Unlock()
	bridge.Reconcile(context.Background())
	if _, present, applied := bridge.LastApplied(); !applied || !present {
		t.Fatal("retry did not apply")
	}
}

func TestPresentationBridgeRemoveFailureRetried(t *testing.T) {
	src := &memoryPresentationSource{}
	var stderr strings.Builder
	pub := &recordingPresentationPublisher{}
	src.set(presentationWithPixels(5, presencev2.Pixel{
		Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking,
	}))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Hour, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Reconcile(context.Background())
	src.set(emptyPresentation(5))
	pub.mu.Lock()
	pub.removeErr = errors.New("delete failed")
	pub.mu.Unlock()
	bridge.Reconcile(context.Background())
	if _, present, applied := bridge.LastApplied(); !applied || !present {
		t.Fatal("failed delete must keep previous applied present state")
	}
	if !strings.Contains(stderr.String(), "presentation bridge remove") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	pub.mu.Lock()
	pub.removeErr = nil
	pub.mu.Unlock()
	bridge.Reconcile(context.Background())
	if _, present, applied := bridge.LastApplied(); !applied || present {
		t.Fatal("retry delete did not clear presentation")
	}
}

func TestPresentationBridgeRace(t *testing.T) {
	src := &memoryPresentationSource{}
	pub := &recordingPresentationPublisher{}
	src.set(emptyPresentation(5))
	bridge, err := NewPresentationBridge(src, pub, 5, time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		bridge.Run(ctx)
	}()
	go func() {
		defer wait.Done()
		for i := 0; i < 40; i++ {
			src.set(presentationWithPixels(5, presencev2.Pixel{
				Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateWorking,
			}))
			bridge.Trigger()
			src.set(presentationWithPixels(5, presencev2.Pixel{
				Pixel: 0, InstanceID: "inst-a", State: instancepresence.StateIdle,
			}))
			bridge.Trigger()
			src.set(emptyPresentation(5))
			bridge.Trigger()
			bridge.Reconcile(context.Background())
		}
		cancel()
	}()
	wait.Wait()
}

func TestNewPresentationBridgeValidates(t *testing.T) {
	if _, err := NewPresentationBridge(nil, &recordingPresentationPublisher{}, 5, 0, nil); err == nil {
		t.Fatal("nil source")
	}
	if _, err := NewPresentationBridge(&memoryPresentationSource{}, nil, 5, 0, nil); err == nil {
		t.Fatal("nil publisher")
	}
	if _, err := NewPresentationBridge(&memoryPresentationSource{}, &recordingPresentationPublisher{}, 0, 0, nil); err == nil {
		t.Fatal("zero capacity")
	}
}
