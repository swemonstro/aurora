package localhooktransport

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestIngestReceiverValidationRejectsBeforeSequencing(t *testing.T) {
	clock := &testClock{now: testTime}
	receiver := newTestIngestReceiver(t, clock, DefaultIngestServerConfig(clock))

	response := receiver.HandleJSON(context.Background(), []byte(`{"protocol_version":1,"operation":"ingest_hook_event","request_id":"0123456789abcdef0123456789abcdef","payload":{"tool":"claude","hook_session_ref":"session-a","lifecycle":"active"}}`))
	if response.Status != StatusRejected || !hasIngestCode(response, CodeUnsupportedProtocolVersion) || !response.NoBindingPerformed {
		t.Fatalf("response = %#v", response)
	}
	if _, ok := receiver.LastAccepted(instancepresence.ToolClaude, "session-a"); ok {
		t.Fatal("invalid request was sequenced")
	}

	response = receiver.HandleJSON(context.Background(), []byte(`{"protocol_version":2,"operation":"ingest_hook_event","request_id":"0123456789abcdef0123456789abcdef","payload":{"tool":"claude","hook_session_ref":"session-a","lifecycle":"active","producer_epoch":"x"}}`))
	if response.Status != StatusRejected || !hasIngestCode(response, CodeUnknownField) || response.RequestID != "" {
		t.Fatalf("unknown field response = %#v", response)
	}

	response = receiver.Handle(context.Background(), IngestRequest{
		ProtocolVersion: IngestProtocolVersion,
		Operation:       OperationIngestHookEvent,
		RequestID:       "0123456789abcdef0123456789abcdef",
		Payload:         IngressPayload{Tool: "hermes", HookSessionRef: "session-a", Lifecycle: instancecorrelation.LifecycleActive},
	})
	if response.Status != StatusRejected || !hasIngestCode(response, CodeInvalidIngress) || !response.NoBindingPerformed {
		t.Fatalf("invalid ingress response = %#v", response)
	}
}

func TestIngestReceiverRevisionAndTimestampAssignment(t *testing.T) {
	clock := &testClock{now: testTime}
	receiver := newTestIngestReceiver(t, clock, DefaultIngestServerConfig(clock))
	epoch := receiver.ProducerEpoch()
	if len(epoch) < 32 {
		t.Fatalf("epoch entropy too low: %q", epoch)
	}

	first := testIngestRequest("request-01", instancepresence.ToolClaude, "session-a", instancecorrelation.LifecycleActive)
	response := receiver.Handle(context.Background(), first)
	if response.Status != StatusOK || !response.NoBindingPerformed || len(response.ErrorCodes) != 0 {
		t.Fatalf("first response = %#v", response)
	}
	accepted, ok := receiver.LastAccepted(instancepresence.ToolClaude, "session-a")
	if !ok || accepted.Revision != 1 || accepted.ObservedAt != testTime || accepted.ProducerEpoch != epoch {
		t.Fatalf("accepted = %#v ok=%t", accepted, ok)
	}

	clock.Advance(time.Second)
	second := testIngestRequest("request-02", instancepresence.ToolClaude, "session-a", instancecorrelation.LifecycleIdle)
	response = receiver.Handle(context.Background(), second)
	if response.Status != StatusOK {
		t.Fatalf("second response = %#v", response)
	}
	accepted, ok = receiver.LastAccepted(instancepresence.ToolClaude, "session-a")
	if !ok || accepted.Revision != 2 || accepted.ObservedAt != testTime.Add(time.Second) || accepted.Lifecycle != instancecorrelation.LifecycleIdle {
		t.Fatalf("second accepted = %#v", accepted)
	}

	third := testIngestRequest("request-03", instancepresence.ToolClaude, "session-a", instancecorrelation.LifecycleEnded)
	response = receiver.Handle(context.Background(), third)
	if response.Status != StatusOK {
		t.Fatalf("ended response = %#v", response)
	}
	revision, ok := receiver.StreamRevision(instancepresence.ToolClaude, "session-a")
	if !ok || revision != 3 {
		t.Fatalf("ended revision = %d ok=%t", revision, ok)
	}
	// After ended, the stream remains and continues sequencing without reuse.
	fourth := testIngestRequest("request-04", instancepresence.ToolClaude, "session-a", instancecorrelation.LifecycleActive)
	response = receiver.Handle(context.Background(), fourth)
	if response.Status != StatusOK {
		t.Fatalf("post-ended response = %#v", response)
	}
	revision, ok = receiver.StreamRevision(instancepresence.ToolClaude, "session-a")
	if !ok || revision != 4 {
		t.Fatalf("post-ended revision = %d ok=%t", revision, ok)
	}
}

func TestIngestReceiverCompletedReplayAndConflict(t *testing.T) {
	clock := &testClock{now: testTime}
	receiver := newTestIngestReceiver(t, clock, DefaultIngestServerConfig(clock))
	request := testIngestRequest("request-01", instancepresence.ToolCodex, "session-b", instancecorrelation.LifecycleActive)
	if response := receiver.Handle(context.Background(), request); response.Status != StatusOK {
		t.Fatalf("initial = %#v", response)
	}
	revision, _ := receiver.StreamRevision(instancepresence.ToolCodex, "session-b")

	replay := receiver.Handle(context.Background(), request)
	if replay.Status != StatusDuplicate || !replay.NoBindingPerformed {
		t.Fatalf("replay = %#v", replay)
	}
	if next, _ := receiver.StreamRevision(instancepresence.ToolCodex, "session-b"); next != revision {
		t.Fatalf("replay advanced revision from %d to %d", revision, next)
	}

	conflict := request
	conflict.Payload.Lifecycle = instancecorrelation.LifecycleIdle
	response := receiver.Handle(context.Background(), conflict)
	if response.Status != StatusRejected || !hasIngestCode(response, CodeRequestIDConflict) || !response.NoBindingPerformed {
		t.Fatalf("conflict = %#v", response)
	}
	if next, _ := receiver.StreamRevision(instancepresence.ToolCodex, "session-b"); next != revision {
		t.Fatalf("conflict advanced revision from %d to %d", revision, next)
	}
}

func TestIngestReceiverSimultaneousInProgressDuplicate(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultIngestServerConfig(clock)
	config.MaximumHandlingTime = time.Second
	receiver := newTestIngestReceiver(t, clock, config)

	started := make(chan struct{})
	release := make(chan struct{})
	receiver.afterAccept = func(ctx context.Context) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}

	request := testIngestRequest("request-dup", instancepresence.ToolClaude, "session-c", instancecorrelation.LifecycleActive)
	firstDone := make(chan IngestResponse, 1)
	go func() {
		firstDone <- receiver.Handle(context.Background(), request)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach accept")
	}

	secondDone := make(chan IngestResponse, 1)
	go func() {
		secondDone <- receiver.Handle(context.Background(), request)
	}()

	var second IngestResponse
	select {
	case second = <-secondDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("in-progress duplicate waited instead of returning immediately")
	}
	if second.Status != StatusDuplicate || !hasIngestCode(second, CodeRequestInProgress) || !second.NoBindingPerformed {
		t.Fatalf("second = %#v", second)
	}
	if _, ok := receiver.LastAccepted(instancepresence.ToolClaude, "session-c"); !ok {
		t.Fatal("owner did not sequence")
	}
	revision, _ := receiver.StreamRevision(instancepresence.ToolClaude, "session-c")
	if revision != 1 {
		t.Fatalf("duplicate consumed an extra revision: %d", revision)
	}

	close(release)
	first := <-firstDone
	if first.Status != StatusOK || !first.NoBindingPerformed {
		t.Fatalf("first = %#v", first)
	}
}

func TestIngestReceiverSequencingCapacity(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultIngestServerConfig(clock)
	config.SequencingCapacity = 2
	receiver := newTestIngestReceiver(t, clock, config)

	if response := receiver.Handle(context.Background(), testIngestRequest("r1", instancepresence.ToolClaude, "s1", instancecorrelation.LifecycleActive)); response.Status != StatusOK {
		t.Fatalf("first stream = %#v", response)
	}
	if response := receiver.Handle(context.Background(), testIngestRequest("r2", instancepresence.ToolCodex, "s2", instancecorrelation.LifecycleActive)); response.Status != StatusOK {
		t.Fatalf("second stream = %#v", response)
	}
	response := receiver.Handle(context.Background(), testIngestRequest("r3", instancepresence.ToolClaude, "s3", instancecorrelation.LifecycleActive))
	if response.Status != StatusRejected || !hasIngestCode(response, CodeSequencingCapacityExceeded) || !response.NoBindingPerformed {
		t.Fatalf("capacity response = %#v", response)
	}
	// Existing streams remain writable without mutating capacity occupancy.
	if response := receiver.Handle(context.Background(), testIngestRequest("r4", instancepresence.ToolClaude, "s1", instancecorrelation.LifecycleIdle)); response.Status != StatusOK {
		t.Fatalf("existing stream = %#v", response)
	}
	if revision, _ := receiver.StreamRevision(instancepresence.ToolClaude, "s1"); revision != 2 {
		t.Fatalf("existing stream revision = %d", revision)
	}
}

func TestIngestReceiverRevisionOverflow(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultIngestServerConfig(clock)
	config.MaximumRevision = 2
	receiver := newTestIngestReceiver(t, clock, config)
	if response := receiver.Handle(context.Background(), testIngestRequest("r1", instancepresence.ToolClaude, "session-o", instancecorrelation.LifecycleActive)); response.Status != StatusOK {
		t.Fatal(response)
	}
	if response := receiver.Handle(context.Background(), testIngestRequest("r2", instancepresence.ToolClaude, "session-o", instancecorrelation.LifecycleActive)); response.Status != StatusOK {
		t.Fatal(response)
	}
	response := receiver.Handle(context.Background(), testIngestRequest("r3", instancepresence.ToolClaude, "session-o", instancecorrelation.LifecycleActive))
	if response.Status != StatusRejected || !hasIngestCode(response, CodeRevisionOverflow) || !response.NoBindingPerformed {
		t.Fatalf("overflow = %#v", response)
	}
	if revision, _ := receiver.StreamRevision(instancepresence.ToolClaude, "session-o"); revision != 2 {
		t.Fatalf("revision wrapped or advanced: %d", revision)
	}
}

func TestIngestReceiverReplayCacheFull(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultIngestServerConfig(clock)
	config.ReplayCapacity = 1
	receiver := newTestIngestReceiver(t, clock, config)
	if response := receiver.Handle(context.Background(), testIngestRequest("r1", instancepresence.ToolClaude, "session-r", instancecorrelation.LifecycleActive)); response.Status != StatusOK {
		t.Fatal(response)
	}
	response := receiver.Handle(context.Background(), testIngestRequest("r2", instancepresence.ToolClaude, "session-r", instancecorrelation.LifecycleIdle))
	if response.Status != StatusRejected || !hasIngestCode(response, CodeReplayCacheFull) || !response.NoBindingPerformed {
		t.Fatalf("replay full = %#v", response)
	}
	if revision, _ := receiver.StreamRevision(instancepresence.ToolClaude, "session-r"); revision != 1 {
		t.Fatalf("revision assigned despite full replay cache: %d", revision)
	}
}

func TestIngestReceiverHandlerTimeoutConsumesRevision(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultIngestServerConfig(clock)
	config.MaximumHandlingTime = 20 * time.Millisecond
	receiver := newTestIngestReceiver(t, clock, config)
	receiver.afterAccept = func(ctx context.Context) {
		<-ctx.Done()
	}
	response := receiver.Handle(context.Background(), testIngestRequest("timeout-1", instancepresence.ToolClaude, "session-t", instancecorrelation.LifecycleActive))
	if response.Status != StatusError || !hasIngestCode(response, CodeHandlerTimeout) || !response.NoBindingPerformed {
		t.Fatalf("timeout response = %#v", response)
	}
	if revision, ok := receiver.StreamRevision(instancepresence.ToolClaude, "session-t"); !ok || revision != 1 {
		t.Fatalf("timeout revision = %d ok=%t", revision, ok)
	}

	// Cached timeout result is returned as duplicate without a new revision.
	replay := receiver.Handle(context.Background(), testIngestRequest("timeout-1", instancepresence.ToolClaude, "session-t", instancecorrelation.LifecycleActive))
	if replay.Status != StatusDuplicate || !hasIngestCode(replay, CodeHandlerTimeout) || !replay.NoBindingPerformed {
		t.Fatalf("timeout replay = %#v", replay)
	}
	if revision, _ := receiver.StreamRevision(instancepresence.ToolClaude, "session-t"); revision != 1 {
		t.Fatalf("timeout replay advanced revision: %d", revision)
	}

	// A new request continues at the next revision; the timed-out one is not reused.
	receiver.afterAccept = nil
	next := receiver.Handle(context.Background(), testIngestRequest("timeout-2", instancepresence.ToolClaude, "session-t", instancecorrelation.LifecycleIdle))
	if next.Status != StatusOK {
		t.Fatalf("next = %#v", next)
	}
	if revision, _ := receiver.StreamRevision(instancepresence.ToolClaude, "session-t"); revision != 2 {
		t.Fatalf("next revision = %d", revision)
	}
}

func TestIngestReceiverNoBindingInvariantAndContentFreeResponse(t *testing.T) {
	clock := &testClock{now: testTime}
	receiver := newTestIngestReceiver(t, clock, DefaultIngestServerConfig(clock))
	response := receiver.Handle(context.Background(), testIngestRequest("ok-1", instancepresence.ToolClaude, "session-n", instancecorrelation.LifecycleActive))
	data, err := EncodeIngestResponseJSON(response, DefaultIngestMaximumResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !response.NoBindingPerformed {
		t.Fatal("binding reported")
	}
	for _, forbidden := range []string{
		"proposals", "summary", "hook_session_ref", "producer_epoch", "revision",
		"observed_at", "process_hint", "runtime_hint", "pid", "cwd", "transcript",
	} {
		if strings.Contains(string(data), `"`+forbidden+`"`) {
			t.Fatalf("response leaked %q: %s", forbidden, data)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["no_binding_performed"] != true {
		t.Fatalf("raw = %#v", raw)
	}
}

func TestIngestReceiverOversizedRequestRejected(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultIngestServerConfig(clock)
	config.MaximumRequestBytes = 64
	receiver := newTestIngestReceiver(t, clock, config)
	payload := make([]byte, 65)
	for i := range payload {
		payload[i] = 'a'
	}
	response := receiver.HandleJSON(context.Background(), payload)
	if response.Status != StatusRejected || !hasIngestCode(response, CodeRequestTooLarge) || !response.NoBindingPerformed {
		t.Fatalf("oversized = %#v", response)
	}
}

func TestIngestReceiverConcurrentDistinctRequestsGetMonotonicRevisions(t *testing.T) {
	clock := &testClock{now: testTime}
	receiver := newTestIngestReceiver(t, clock, DefaultIngestServerConfig(clock))
	const workers = 20
	var wait sync.WaitGroup
	wait.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wait.Done()
			request := testIngestRequest(
				"concurrent-"+strings.Repeat("a", 8)+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('0'+i%10)),
				instancepresence.ToolClaude,
				"session-parallel",
				instancecorrelation.LifecycleActive,
			)
			// Ensure unique request IDs with valid opaque charset.
			request.RequestID = uniqueRequestID(i)
			if response := receiver.Handle(context.Background(), request); response.Status != StatusOK {
				t.Errorf("response = %#v", response)
			}
		}(i)
	}
	wait.Wait()
	revision, ok := receiver.StreamRevision(instancepresence.ToolClaude, "session-parallel")
	if !ok || revision != workers {
		t.Fatalf("revision = %d ok=%t", revision, ok)
	}
}

func newTestIngestReceiver(t *testing.T, clock Clock, config IngestServerConfig) *IngestReceiver {
	t.Helper()
	config.Clock = clock
	receiver, err := NewIngestReceiver(config)
	if err != nil {
		t.Fatal(err)
	}
	return receiver
}

func testIngestRequest(id string, tool instancepresence.ToolKind, session string, lifecycle instancecorrelation.Lifecycle) IngestRequest {
	return IngestRequest{
		ProtocolVersion: IngestProtocolVersion,
		Operation:       OperationIngestHookEvent,
		RequestID:       id,
		Payload: IngressPayload{
			Tool:           tool,
			HookSessionRef: instancepresence.OpaqueIdentity(session),
			Lifecycle:      lifecycle,
		},
	}
}

func hasIngestCode(response IngestResponse, code ErrorCode) bool {
	for _, existing := range response.ErrorCodes {
		if existing == code {
			return true
		}
	}
	return false
}

func uniqueRequestID(i int) string {
	// 32 hex-like characters from a fixed alphabet accepted by validateOpaque.
	const alphabet = "0123456789abcdef"
	id := make([]byte, 32)
	for idx := range id {
		id[idx] = alphabet[(i+idx*7)%len(alphabet)]
	}
	// Ensure uniqueness across 20 workers by encoding i into the prefix.
	prefix := []byte{
		alphabet[(i/256)%16],
		alphabet[(i/16)%16],
		alphabet[i%16],
	}
	copy(id, prefix)
	return string(id)
}
