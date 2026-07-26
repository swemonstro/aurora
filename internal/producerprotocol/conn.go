package producerprotocol

import (
	"net"
	"sync"
)

// Conn is one framed producer<->broker connection. It enforces, generically
// and without any tool-specific logic, that a connection speaks exactly one
// Tool for its lifetime: the first accepted message's tool becomes binding,
// and Bind lets a caller fix the tool before any message is read (the "one
// socket, one tool" / "one authenticated peer, one tool" primitives).
//
// A Conn must not be used from multiple goroutines concurrently for
// ReadMessage or WriteMessage; the binding state itself is safe to inspect
// concurrently with Bind/BoundTool.
type Conn struct {
	raw    net.Conn
	config Config

	mu       sync.Mutex
	bound    Tool
	hasBound bool
}

// newConn wraps raw with config. config must already be validated.
func newConn(raw net.Conn, config Config) *Conn {
	return &Conn{raw: raw, config: config}
}

// Bind fixes this connection to exactly one tool. Calling it again with the
// same tool is a no-op; calling it with a different tool never mutates the
// existing binding and returns ErrToolMismatch.
func (conn *Conn) Bind(tool Tool) error {
	if err := tool.Validate(); err != nil {
		return protocolError(CodeInvalidTool, err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.hasBound {
		if conn.bound != tool {
			return protocolError(CodeToolMismatch, ErrToolMismatch)
		}
		return nil
	}
	conn.bound = tool
	conn.hasBound = true
	return nil
}

// BoundTool reports the tool this connection is currently bound to, if any.
func (conn *Conn) BoundTool() (Tool, bool) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.bound, conn.hasBound
}

// checkAndBind enforces the bound-tool invariant for one message: if the
// connection is already bound, msg.Tool must match; otherwise this message's
// tool becomes the binding.
func (conn *Conn) checkAndBind(tool Tool) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.hasBound {
		if conn.bound != tool {
			return protocolError(CodeToolMismatch, ErrToolMismatch)
		}
		return nil
	}
	conn.bound = tool
	conn.hasBound = true
	return nil
}

// ReadMessage reads, decodes, validates, and canonicalizes exactly one
// frame. A read deadline derived from config.Clock and config.ReadTimeout is
// applied to the underlying connection before every read.
func (conn *Conn) ReadMessage() (Message, error) {
	deadline := conn.config.Clock.Now().Add(conn.config.ReadTimeout)
	if err := conn.raw.SetReadDeadline(deadline); err != nil {
		return Message{}, classifyIOError(err, true)
	}
	data, err := readFrame(conn.raw, conn.config.MaximumMessageBytes)
	if err != nil {
		return Message{}, err
	}
	msg, err := DecodeMessageJSON(data, conn.config.MaximumMessageBytes)
	if err != nil {
		return Message{}, err
	}
	msg = CanonicalMessage(msg)
	if err := ValidateMessage(conn.config, msg); err != nil {
		return Message{}, err
	}
	if err := conn.checkAndBind(msg.Tool); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// WriteMessage canonicalizes, validates, and writes exactly one frame. A
// write deadline derived from config.Clock and config.WriteTimeout is
// applied to the underlying connection before every write.
func (conn *Conn) WriteMessage(msg Message) error {
	msg = CanonicalMessage(msg)
	if err := ValidateMessage(conn.config, msg); err != nil {
		return err
	}
	if err := conn.checkAndBind(msg.Tool); err != nil {
		return err
	}
	data, err := EncodeMessageJSON(msg, conn.config.MaximumMessageBytes)
	if err != nil {
		return err
	}
	deadline := conn.config.Clock.Now().Add(conn.config.WriteTimeout)
	if err := conn.raw.SetWriteDeadline(deadline); err != nil {
		return classifyIOError(err, false)
	}
	return writeFrame(conn.raw, data, conn.config.MaximumMessageBytes)
}

// Close closes the underlying connection.
func (conn *Conn) Close() error { return conn.raw.Close() }
