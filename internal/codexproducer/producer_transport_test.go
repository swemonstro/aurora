//go:build linux

package codexproducer

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// rawRecordingPeer is a minimal, direct producerprotocol peer (Listen +
// Accept + ReadMessage only — never the full broker/registry stack) used to
// inspect the exact, ordered sequence of decoded wire messages a Producer
// sends, including across a connection that is deliberately torn down
// mid-stream. Recording at this level (rather than only checking a
// registry's resulting state) is what proves the actual wire contract, not
// just an eventually-consistent outcome.
type rawRecordingPeer struct {
	listener *producerprotocol.Listener
	config   producerprotocol.Config
}

func newRawRecordingPeer(t *testing.T, clock producerprotocol.Clock) *rawRecordingPeer {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-codexproducer-transport-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	config := producerprotocol.DefaultConfig(clock)
	config.SocketPath = filepath.Join(directory, "codex.sock")
	config.BoundTool = producerprotocol.ToolCodex
	listener, err := producerprotocol.Listen(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return &rawRecordingPeer{listener: listener, config: config}
}

// acceptAndRecord accepts exactly one connection and reads up to maxMessages
// complete, decoded messages from it, then closes the connection without
// attempting to read anything further. This constructs case A of the
// transport retry contract (see send's doc comment in producer.go): "peer
// receives a complete frame, then the connection closes." It deliberately
// proves nothing about any message the producer may have started writing
// after the maxMessages-th one — that write may have already completed in
// full inside the OS socket buffer, may have partially landed there, or may
// have failed outright, and this helper cannot tell which and never claims
// to: WriteMessage's own return value is not reliable evidence either way
// (see send's doc comment for why). Every test using this helper must reason
// only from what it can prove — the messages it actually decoded — never
// from an assumption about the fate of any message beyond that.
func (peer *rawRecordingPeer) acceptAndRecord(t *testing.T, ctx context.Context, maxMessages int) []producerprotocol.Message {
	t.Helper()
	conn, _, err := peer.listener.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()
	var messages []producerprotocol.Message
	for len(messages) < maxMessages {
		msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		messages = append(messages, msg)
	}
	return messages
}

// TestProducer_ReconnectAfterPeerClosesConnectionCleanlyUsesFreshRevisionAndTimestamps
// covers case A of the transport retry contract (see send's doc comment in
// producer.go): the peer receives one complete frame, then closes the
// connection without reading anything further. It proves, by inspecting the
// actual decoded producerprotocol.Message sequence on the wire (not just
// eventual registry state), that this producer's chosen contract — "no
// transport replay, ever; every send builds a fresh message from current
// Machine state and the clock's current instant" — holds across such a
// close: a second, unread send happens right at the close boundary, its
// real outcome is never observed or assumed by this test (see send's doc
// comment: WriteMessage's return value is not reliable evidence of whether
// the peer decoded it), and after reconnecting the producer must still send
// a fresh message with strictly higher revision and later timestamps than
// the one message this test did observe.
func TestProducer_ReconnectAfterPeerClosesConnectionCleanlyUsesFreshRevisionAndTimestamps(t *testing.T) {
	clock := &mutableClock{now: time.Now().UTC()}
	peer := newRawRecordingPeer(t, clock)
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	sources := testSources(t, "", SourceEntry{Label: "business", Path: testCodexHomeDir(t, "business")})
	dialConfig := producerprotocol.DefaultConfig(clock)
	dialConfig.SocketPath = peer.config.SocketPath
	dialConfig.BoundTool = producerprotocol.ToolCodex
	producer, err := NewProducer(Config{
		DialConfig: dialConfig, Sources: sources, PollInterval: time.Second, LeaseDuration: 5 * time.Second,
		ReconnectMinDelay: 5 * time.Millisecond, ReconnectMaxDelay: 20 * time.Millisecond, PendingHookTTL: time.Second,
		ProcRoot: "/proc", Clock: clock, Stderr: nil,
	}, epoch)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	acceptDone := make(chan []producerprotocol.Message, 1)
	go func() {
		// Read only the first message, then close — simulating an
		// interruption exactly as (or before) the second report would have
		// been confirmed.
		acceptDone <- peer.acceptAndRecord(t, ctx, 1)
	}()
	producer.tryConnect(ctx)
	if producer.conn == nil {
		t.Fatal("expected initial connect to succeed")
	}

	instanceID := DeriveInstanceID("business", 3000, clock.Now())
	state, revision, _ := producer.machine.Discover(instanceID, "business")
	producer.send(instanceID, state, revision) // message 1: idle, revision 1.

	firstConnMessages := <-acceptDone
	if len(firstConnMessages) != 1 {
		t.Fatalf("expected exactly 1 message recorded on the first connection, got %d", len(firstConnMessages))
	}
	firstMsg := firstConnMessages[0]
	if firstMsg.Revision != 1 || firstMsg.State != producerprotocol.StateIdle {
		t.Fatalf("unexpected first message: %+v", firstMsg)
	}

	// A state change happens next — Machine advances to revision 2
	// in-memory regardless of what the transport does with it.
	state, revision, changed := producer.machine.ApplyHookEvent(instanceID, "business", producerprotocol.StateWorking)
	if !changed || revision != 2 {
		t.Fatalf("expected revision 2, got state=%v revision=%d changed=%v", state, revision, changed)
	}
	// This send races the peer's close (goroutine above already closed
	// after recording message 1). Its real outcome is deliberately left
	// unresolved and unobserved by this test: WriteMessage may return an
	// error, or may "succeed" as a write the OS accepted but the peer never
	// read — and per send's doc comment, neither outcome is reliable
	// evidence of whether the peer's ingest layer actually decoded and
	// applied this specific message. This test does not need to know which
	// happened; it only needs the next send after reconnect to carry a
	// strictly higher revision and fresh timestamps, which is what makes
	// convergence certain regardless of this message's real fate.
	producer.send(instanceID, state, revision)
	_ = producer.conn   // do not assert nil/non-nil here: both transport outcomes are valid per the documented contract.
	producer.conn = nil // force the reconnect path regardless of which race outcome occurred, like a real interruption would.

	// Reconnect: a second, independent connection.
	secondAcceptDone := make(chan []producerprotocol.Message, 1)
	go func() {
		secondAcceptDone <- peer.acceptAndRecord(t, ctx, 1)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for producer.conn == nil && time.Now().Before(deadline) {
		producer.tryConnect(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	if producer.conn == nil {
		t.Fatal("expected producer to reconnect")
	}

	// Advance the clock, simulating a slow reconnect past what would have
	// been message 2's lease window, to prove the resumed send is fresh —
	// never a replay of the uncertain revision-2 attempt's own timestamps.
	clock.Advance(time.Minute)
	renewState, renewRevision, ok := producer.machine.Renew(instanceID)
	if !ok {
		t.Fatal("expected Renew to succeed")
	}
	producer.send(instanceID, renewState, renewRevision)

	secondConnMessages := <-secondAcceptDone
	if len(secondConnMessages) != 1 {
		t.Fatalf("expected exactly 1 message recorded on the second connection, got %d", len(secondConnMessages))
	}
	secondMsg := secondConnMessages[0]

	// Core wire-level assertions.
	if secondMsg.Revision <= firstMsg.Revision {
		t.Fatalf("revision must strictly increase across the interruption: first=%d second=%d", firstMsg.Revision, secondMsg.Revision)
	}
	if secondMsg.State != producerprotocol.StateWorking {
		t.Fatalf("the state change made before the interruption must not be lost: got %v, want working", secondMsg.State)
	}
	if secondMsg.ProducerEpoch != firstMsg.ProducerEpoch {
		t.Fatalf("epoch must not change across a reconnect (only across a producer restart): first=%q second=%q", firstMsg.ProducerEpoch, secondMsg.ProducerEpoch)
	}
	if secondMsg.InstanceID != firstMsg.InstanceID {
		t.Fatalf("instance id must not change across a reconnect: first=%q second=%q", firstMsg.InstanceID, secondMsg.InstanceID)
	}
	if !secondMsg.ObservedAt.After(firstMsg.ObservedAt) {
		t.Fatalf("resumed report must carry a fresh, later observed_at, never a replay of a stale one: first=%v second=%v", firstMsg.ObservedAt, secondMsg.ObservedAt)
	}
	if !secondMsg.LeaseExpiresAt.After(firstMsg.LeaseExpiresAt) {
		t.Fatalf("resumed report's lease must extend from the fresh clock, never reuse an earlier lease window: first=%v second=%v", firstMsg.LeaseExpiresAt, secondMsg.LeaseExpiresAt)
	}

	// Idempotency invariant across every message ever observed: no two
	// messages may ever share a revision while differing in any other
	// field (this would violate internal/instanceregistry's
	// same-revision-same-payload contract).
	allMessages := append(append([]producerprotocol.Message{}, firstConnMessages...), secondConnMessages...)
	seenRevisions := make(map[producerprotocol.Revision]producerprotocol.Message)
	for _, msg := range allMessages {
		if previous, seen := seenRevisions[msg.Revision]; seen && previous != msg {
			t.Fatalf("same revision %d observed with different payloads: %+v vs %+v", msg.Revision, previous, msg)
		}
		seenRevisions[msg.Revision] = msg
	}
}

// TestProducer_MultipleInstancesHaveIndependentRevisionsAcrossReconnect
// proves, again by inspecting the actual wire sequence, that an
// interruption and reconnect affecting the whole connection never lets one
// instance's revision or state leak into another's.
func TestProducer_MultipleInstancesHaveIndependentRevisionsAcrossReconnect(t *testing.T) {
	clock := &mutableClock{now: time.Now().UTC()}
	peer := newRawRecordingPeer(t, clock)
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	sources := testSources(t, "", SourceEntry{Label: "business", Path: testCodexHomeDir(t, "business")})
	dialConfig := producerprotocol.DefaultConfig(clock)
	dialConfig.SocketPath = peer.config.SocketPath
	dialConfig.BoundTool = producerprotocol.ToolCodex
	producer, err := NewProducer(Config{
		DialConfig: dialConfig, Sources: sources, PollInterval: time.Second, LeaseDuration: 5 * time.Second,
		ReconnectMinDelay: 5 * time.Millisecond, ReconnectMaxDelay: 20 * time.Millisecond, PendingHookTTL: time.Second,
		ProcRoot: "/proc", Clock: clock,
	}, epoch)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	firstConnDone := make(chan []producerprotocol.Message, 1)
	go func() { firstConnDone <- peer.acceptAndRecord(t, ctx, 2) }()
	producer.tryConnect(ctx)
	if producer.conn == nil {
		t.Fatal("expected initial connect to succeed")
	}

	idA := DeriveInstanceID("business", 4001, clock.Now())
	idB := DeriveInstanceID("business", 4002, clock.Now())
	stateA, revA, _ := producer.machine.Discover(idA, "business")
	producer.send(idA, stateA, revA)
	stateB, revB, _ := producer.machine.Discover(idB, "business")
	producer.send(idB, stateB, revB)

	firstMessages := <-firstConnDone
	if len(firstMessages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(firstMessages))
	}

	// Only A changes and reconnects; B is untouched in Machine.
	stateA, revA, changed := producer.machine.ApplyHookEvent(idA, "business", producerprotocol.StateAttention)
	if !changed {
		t.Fatal("expected A to change")
	}
	producer.send(idA, stateA, revA)
	producer.conn = nil // force reconnect regardless of the race outcome.

	secondConnDone := make(chan []producerprotocol.Message, 1)
	go func() { secondConnDone <- peer.acceptAndRecord(t, ctx, 2) }()
	deadline := time.Now().Add(2 * time.Second)
	for producer.conn == nil && time.Now().Before(deadline) {
		producer.tryConnect(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	if producer.conn == nil {
		t.Fatal("expected producer to reconnect")
	}
	clock.Advance(time.Minute)

	// Renew both on the new connection: A carries its attention state
	// forward at a higher revision; B is untouched and must renew from
	// exactly where it left off (revision 2, still idle), never inheriting
	// A's state or revision.
	stateA, revA, ok := producer.machine.Renew(idA)
	if !ok {
		t.Fatal("expected Renew(A) to succeed")
	}
	producer.send(idA, stateA, revA)
	stateB, revB, ok = producer.machine.Renew(idB)
	if !ok {
		t.Fatal("expected Renew(B) to succeed")
	}
	producer.send(idB, stateB, revB)

	secondMessages := <-secondConnDone
	if len(secondMessages) != 2 {
		t.Fatalf("expected 2 messages on the second connection, got %d", len(secondMessages))
	}
	byInstance := map[producerprotocol.InstanceID]producerprotocol.Message{}
	for _, msg := range secondMessages {
		byInstance[msg.InstanceID] = msg
	}
	finalA, finalB := byInstance[idA], byInstance[idB]
	if finalA.State != producerprotocol.StateAttention {
		t.Fatalf("A must carry its attention state forward: %+v", finalA)
	}
	if finalB.State != producerprotocol.StateIdle {
		t.Fatalf("B must remain idle, unaffected by A's transition: %+v", finalB)
	}
	if finalB.Revision != 2 {
		t.Fatalf("B's revision must be exactly 2 (one renewal past its initial 1), got %d — A's transitions must never leak into B's revision counter", finalB.Revision)
	}
	if finalA.Revision <= finalB.Revision {
		t.Fatalf("A (which changed state and renewed) should have a higher revision than B: A=%d B=%d", finalA.Revision, finalB.Revision)
	}
}

// readRawFrame reads one length-prefixed frame directly off conn and decodes
// it. It duplicates the handful of lines producerprotocol.Conn.ReadMessage
// performs internally (readFrame plus DecodeMessageJSON), because
// producerprotocol.Conn's raw net.Conn field is unexported: there is no way
// to get raw, undecoded byte-level access to a connection through the
// public producerprotocol API, and that raw access is exactly what case B
// below needs in order to genuinely receive only part of a frame.
func readRawFrame(t *testing.T, conn net.Conn, maximum uint32) producerprotocol.Message {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(conn, body); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	msg, err := producerprotocol.DecodeMessageJSON(body, maximum)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return msg
}

// TestProducer_ReconnectAfterPeerReceivesOnlyPartOfAFrameUsesFreshRevisionAndTimestamps
// covers case B of the transport retry contract (see send's doc comment in
// producer.go): the peer receives strictly fewer bytes than one complete
// frame — here, fewer than even the 4-byte length header, so there is no
// ambiguity about whether a decode was ever remotely possible — before the
// connection closes. This is a deterministic fault injection, not a race:
// io.ReadFull(conn, buffer[:2]) always blocks until exactly 2 bytes of the
// next frame have arrived and then returns, regardless of whether the
// producer's write for that frame had, by that point, already been
// completely accepted into the kernel's socket send buffer. The peer's own
// choice to stop reading after 2 bytes and close is what constructs "peer
// received only part of a frame" — irrespective of that timing, this test
// proves the producer never replays that frame's specific bytes, instead
// sending a brand-new message with strictly higher revision and later
// timestamps once reconnected (see TestProducer_ReconnectAfterPeerClosesConnectionCleanlyUsesFreshRevisionAndTimestamps
// for case A: a complete frame received, then the connection closes).
func TestProducer_ReconnectAfterPeerReceivesOnlyPartOfAFrameUsesFreshRevisionAndTimestamps(t *testing.T) {
	clock := &mutableClock{now: time.Now().UTC()}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aurora-codexproducer-partial-frame-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "codex.sock")

	// A bare net.Listener, never wrapped in producerprotocol.Listener: only
	// this gives raw, undecoded byte-level access to the connection.
	// producerprotocol.Dial (which the producer under test uses) does not
	// require the peer side to have been created via producerprotocol.Listen
	// — it only dials the configured socket path — so this is a faithful
	// peer for exercising the wire contract.
	rawListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawListener.Close() })

	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	sources := testSources(t, "", SourceEntry{Label: "business", Path: testCodexHomeDir(t, "business")})
	dialConfig := producerprotocol.DefaultConfig(clock)
	dialConfig.SocketPath = socketPath
	dialConfig.BoundTool = producerprotocol.ToolCodex
	producer, err := NewProducer(Config{
		DialConfig: dialConfig, Sources: sources, PollInterval: time.Second, LeaseDuration: 5 * time.Second,
		ReconnectMinDelay: 5 * time.Millisecond, ReconnectMaxDelay: 20 * time.Millisecond, PendingHookTTL: time.Second,
		ProcRoot: "/proc", Clock: clock, Stderr: nil,
	}, epoch)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	firstMsgDone := make(chan producerprotocol.Message, 1)
	partialReadDone := make(chan struct{})
	go func() {
		conn, err := rawListener.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			close(firstMsgDone)
			close(partialReadDone)
			return
		}
		defer conn.Close()
		firstMsgDone <- readRawFrame(t, conn, dialConfig.MaximumMessageBytes)
		// Deliberately read only 2 raw bytes of whatever comes next — fewer
		// than the 4-byte length header itself — then close via defer
		// without reading anything further.
		var partial [2]byte
		_, _ = io.ReadFull(conn, partial[:])
		close(partialReadDone)
	}()
	producer.tryConnect(ctx)
	if producer.conn == nil {
		t.Fatal("expected initial connect to succeed")
	}

	instanceID := DeriveInstanceID("business", 3200, clock.Now())
	state, revision, _ := producer.machine.Discover(instanceID, "business")
	producer.send(instanceID, state, revision) // message 1: idle, revision 1 — fully received and decoded.

	firstMsg := <-firstMsgDone
	if firstMsg.Revision != 1 || firstMsg.State != producerprotocol.StateIdle {
		t.Fatalf("unexpected first message: %+v", firstMsg)
	}

	// A state change happens next — Machine advances to revision 2
	// in-memory regardless of what the transport does with it.
	state, revision, changed := producer.machine.ApplyHookEvent(instanceID, "business", producerprotocol.StateWorking)
	if !changed || revision != 2 {
		t.Fatalf("expected revision 2, got state=%v revision=%d changed=%v", state, revision, changed)
	}
	// This send's bytes are the ones the peer above deliberately reads only
	// 2 of before closing. Its real outcome — whether the broker's ingest
	// layer would have decoded and applied it, had this been a real
	// connection all the way through — is unknown and irrelevant per send's
	// doc comment: this producer never needs to resolve it.
	producer.send(instanceID, state, revision)
	<-partialReadDone   // wait for the peer to have read its 2 bytes and closed.
	producer.conn = nil // force the reconnect path.

	// Reconnect: a second, independent connection.
	secondMsgDone := make(chan producerprotocol.Message, 1)
	go func() {
		conn, err := rawListener.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			close(secondMsgDone)
			return
		}
		defer conn.Close()
		secondMsgDone <- readRawFrame(t, conn, dialConfig.MaximumMessageBytes)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for producer.conn == nil && time.Now().Before(deadline) {
		producer.tryConnect(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	if producer.conn == nil {
		t.Fatal("expected producer to reconnect")
	}

	// Advance the clock, simulating a slow reconnect, to prove the resumed
	// send is fresh — never a replay of the partially-received frame's own
	// bytes or timestamps.
	clock.Advance(time.Minute)
	renewState, renewRevision, ok := producer.machine.Renew(instanceID)
	if !ok {
		t.Fatal("expected Renew to succeed")
	}
	producer.send(instanceID, renewState, renewRevision)

	secondMsg := <-secondMsgDone

	if secondMsg.Revision <= firstMsg.Revision {
		t.Fatalf("revision must strictly increase across the interruption: first=%d second=%d", firstMsg.Revision, secondMsg.Revision)
	}
	if secondMsg.State != producerprotocol.StateWorking {
		t.Fatalf("the state change made before the interruption must not be lost: got %v, want working", secondMsg.State)
	}
	if secondMsg.ProducerEpoch != firstMsg.ProducerEpoch {
		t.Fatalf("epoch must not change across a reconnect (only across a producer restart): first=%q second=%q", firstMsg.ProducerEpoch, secondMsg.ProducerEpoch)
	}
	if secondMsg.InstanceID != firstMsg.InstanceID {
		t.Fatalf("instance id must not change across a reconnect: first=%q second=%q", firstMsg.InstanceID, secondMsg.InstanceID)
	}
	if !secondMsg.ObservedAt.After(firstMsg.ObservedAt) {
		t.Fatalf("resumed report must carry a fresh, later observed_at, never a replay of a stale one: first=%v second=%v", firstMsg.ObservedAt, secondMsg.ObservedAt)
	}
	if !secondMsg.LeaseExpiresAt.After(firstMsg.LeaseExpiresAt) {
		t.Fatalf("resumed report's lease must extend from the fresh clock, never reuse an earlier lease window: first=%v second=%v", firstMsg.LeaseExpiresAt, secondMsg.LeaseExpiresAt)
	}

	// Same idempotency invariant as case A: no two messages may ever share a
	// revision while differing in any other field.
	if firstMsg.Revision == secondMsg.Revision && firstMsg != secondMsg {
		t.Fatalf("same revision %d observed with different payloads: %+v vs %+v", firstMsg.Revision, firstMsg, secondMsg)
	}
}
