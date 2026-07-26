package presencebroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// acceptErrorBackoff bounds how fast RunProducerListener retries Accept
// after an unclassified (not context-cancelled, not "listener closed")
// error. Without it, a listener stuck returning the same non-fatal error on
// every call would spin a tight CPU loop instead of degrading gracefully.
const acceptErrorBackoff = 100 * time.Millisecond

// RunProducerListener accepts connections on listener, authenticates each
// peer, reads messages, and applies them via a per-connection
// presencebroker.ProducerSession, until ctx is done or listener is closed
// by some other means. It does not return until every connection goroutine
// it spawned has also returned, so a caller that waits for
// RunProducerListener transitively waits for all of that listener's
// connections too.
//
// Failures are isolated at every layer: a bad Accept, a rejected peer, a
// malformed or rejected message, or a disconnect never stops the loop or
// affects any other connection — including connections on a different
// listener. This lets a caller run one goroutine per socket without any of
// them being able to take another down, and without one producer's
// disconnect affecting any other producer.
func RunProducerListener(
	ctx context.Context,
	listener *producerprotocol.Listener,
	authenticator producerprotocol.Authenticator,
	ingestor *Ingestor,
	stderr io.Writer,
) {
	if stderr == nil {
		stderr = io.Discard
	}
	var connections sync.WaitGroup
	defer connections.Wait()

	for {
		conn, peer, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, producerprotocol.ErrPeerDisconnected) {
				// The listener itself was closed from under this loop (not
				// merely one failed accept attempt) — nothing further will
				// ever succeed here, so stop instead of busy-looping.
				return
			}
			fmt.Fprintln(stderr, "producer listener accept:", producerprotocol.ErrorCodeOf(err))
			// An unclassified Accept failure (rare: resource exhaustion,
			// transient OS error, ...) must not spin a tight CPU loop.
			select {
			case <-ctx.Done():
				return
			case <-time.After(acceptErrorBackoff):
			}
			continue
		}
		if err := authenticator.Authenticate(peer); err != nil {
			_ = conn.Close()
			fmt.Fprintln(stderr, "producer listener authenticate:", producerprotocol.ErrorCodeOf(err))
			continue
		}
		session := ingestor.NewSession()
		connections.Add(1)
		go func() {
			defer connections.Done()
			handleProducerConnection(ctx, conn, session, stderr)
		}()
	}
}

// handleProducerConnection reads messages from one producer connection
// until it disconnects, ctx is cancelled, or a non-timeout transport error
// occurs. A read timeout does not close the connection: Conn.ReadMessage
// applies a fresh deadline on every call, so a producer that goes quiet for
// a while (no state change to report) is not disconnected for it — only
// genuine disconnects and shutdown end the loop. A rejected message (bad
// revision, epoch change, cross-tool mutation, ...) is logged and the
// connection stays open: one bad message must not force a reconnect.
//
// session.Close is always called on return, releasing this connection's
// generation claim (if it ever established one) so a new connection may
// legitimately take over the same instance id.
//
// Deliberate G.3 decision: there is no application-level acknowledgement.
// A producer that has written a message to the socket and received no
// error from the write call knows only that the OS accepted the bytes into
// the socket buffer — never that this broker actually read, accepted, and
// applied it (see retryUntilInstanceState in the test suite, which exists
// precisely because a successful write is not such a signal). Every
// rejection here is logged via ClassifyIngestError, but never surfaced to
// the producer over the wire. This is acceptable for G.3's shadow-mode,
// no-relay, no-ESP deployment, where nothing downstream depends on a
// producer's report actually having landed. It stops being acceptable the
// moment a real producer's correctness depends on knowing whether a report
// was accepted — e.g. a producer that would otherwise silently retry
// forever, or one whose caller needs to surface a failure — at which point
// this must be revisited (an ack/nack response, or at minimum a
// distinguishable close reason) before that producer is cut over to
// production. Do not assume this gap has been re-evaluated just because
// this comment exists; re-evaluate it.
func handleProducerConnection(ctx context.Context, conn *producerprotocol.Conn, session *ProducerSession, stderr io.Writer) {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	defer conn.Close()
	defer session.Close()

	for {
		msg, err := conn.ReadMessage()
		if err != nil {
			if producerprotocol.ErrorCodeOf(err) == producerprotocol.CodeReadTimeout {
				continue
			}
			if !errors.Is(err, producerprotocol.ErrPeerDisconnected) {
				fmt.Fprintln(stderr, "producer connection read:", producerprotocol.ErrorCodeOf(err))
			}
			return
		}
		if _, err := session.Apply(msg); err != nil {
			fmt.Fprintln(stderr, "producer connection ingest:", ClassifyIngestError(err))
			continue
		}
	}
}
