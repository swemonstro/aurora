//go:build linux

package codexproducer

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
)

// EnvShadowSocket is the environment variable cmd/aurora-codex-hook checks
// to decide whether to best-effort forward one already-sanitized Codex hook
// observation to a standalone aurora-codex-presence producer's hook ingress
// socket, in addition to (never instead of) its existing production
// behavior. Unset (the default) means shadow forwarding is fully disabled:
// this is an explicit opt-in, never a default-on path.
const EnvShadowSocket = "AURORA_CODEX_SHADOW_SOCKET"

// DefaultShadowConnectTimeout and DefaultShadowWriteTimeout each bound one
// stage of a shadow delivery attempt independently, so a producer that is
// down, slow to accept, or slow to read can never meaningfully delay the
// real Codex hook flow. Both are deliberately short and independently
// configurable (see ShadowDeliveryConfig) rather than combined into one
// end-to-end budget, so a slow connect cannot silently eat into the write
// stage's own bound or vice versa: connect uses its own context
// (context.WithTimeout(ctx, ConnectTimeout)) and only starts counting once
// dialing begins; write starts a brand-new deadline only after connect has
// already returned, never inheriting or extending connect's elapsed time.
//
// MaxShadowConnectTimeout and MaxShadowWriteTimeout are hard ceilings
// withDefaults enforces on top of whatever a caller configures (including a
// value read from an operator-controlled environment variable — see
// cmd/aurora-codex-hook's shadowTimeoutFromEnv): a positive but absurdly
// large override (a typo, a misconfigured deployment, "600000" meaning
// minutes instead of milliseconds) must never turn a voluntary shadow path
// into a noticeable delay of the real Codex hook. Together they form a
// documented, hard total budget for one delivery attempt —
// MaxShadowConnectTimeout + MaxShadowWriteTimeout, currently
// 100ms + 150ms = 250ms worst case — enforced here regardless of what any
// caller or environment override requests. The package defaults
// (100ms + 100ms = 200ms) already sit comfortably under this ceiling, so
// the common case never even approaches it.
const (
	DefaultShadowConnectTimeout = 100 * time.Millisecond
	DefaultShadowWriteTimeout   = 100 * time.Millisecond
	MaxShadowConnectTimeout     = 100 * time.Millisecond
	MaxShadowWriteTimeout       = 150 * time.Millisecond
)

// ShadowDial connects to socketPath, honoring ctx's deadline. It exists as a
// named type so ShadowDeliveryConfig.Dial can be replaced in tests with a
// deterministic fake (e.g. a net.Pipe endpoint, or a connector that blocks
// until ctx.Done()) instead of a real, potentially timing-flaky Unix socket.
type ShadowDial func(ctx context.Context, socketPath string) (net.Conn, error)

// ShadowDeliveryConfig bounds and wires one shadow delivery attempt.
type ShadowDeliveryConfig struct {
	// ConnectTimeout bounds only reaching the peer. Zero means
	// DefaultShadowConnectTimeout.
	ConnectTimeout time.Duration
	// WriteTimeout bounds only writing the message once connected — it
	// covers a peer that accepts a connection but never reads (a full or
	// stalled receive buffer eventually blocks Write without this). Zero
	// means DefaultShadowWriteTimeout.
	WriteTimeout time.Duration
	// Dial connects to socketPath. Nil means dialUnixSocket (a real Unix
	// domain socket dial).
	Dial ShadowDial
}

func (config ShadowDeliveryConfig) withDefaults() ShadowDeliveryConfig {
	switch {
	case config.ConnectTimeout <= 0:
		config.ConnectTimeout = DefaultShadowConnectTimeout
	case config.ConnectTimeout > MaxShadowConnectTimeout:
		config.ConnectTimeout = MaxShadowConnectTimeout
	}
	switch {
	case config.WriteTimeout <= 0:
		config.WriteTimeout = DefaultShadowWriteTimeout
	case config.WriteTimeout > MaxShadowWriteTimeout:
		config.WriteTimeout = MaxShadowWriteTimeout
	}
	if config.Dial == nil {
		config.Dial = dialUnixSocket
	}
	return config
}

func dialUnixSocket(ctx context.Context, socketPath string) (net.Conn, error) {
	dialer := net.Dialer{}
	return dialer.DialContext(ctx, "unix", socketPath)
}

// DefaultShadowDeliveryConfig returns the production configuration: real
// Unix socket dial, both timeouts at their package defaults.
func DefaultShadowDeliveryConfig() ShadowDeliveryConfig {
	return ShadowDeliveryConfig{}.withDefaults()
}

// TryDeliverShadow best-effort delivers one already-sanitized ingress
// observation to socketPath using DefaultShadowDeliveryConfig. It is the
// convenience entry point cmd/aurora-codex-hook uses in production; see
// TryDeliverShadowWithConfig for the full, testable contract.
func TryDeliverShadow(ctx context.Context, socketPath string, observation hookadapter.IngressObservation) bool {
	return TryDeliverShadowWithConfig(ctx, DefaultShadowDeliveryConfig(), socketPath, observation)
}

// TryDeliverShadowWithConfig best-effort delivers one already-sanitized
// ingress observation to socketPath and returns whether it was written.
// Every failure (empty/invalid input, an already-done parent context, dial
// error or timeout, write error or timeout) is reported only via the
// returned bool, never as an error the caller must handle specially, and
// never via any log or error text: this function is designed to be called
// and its result ignored, exactly like
// internal/localhooktransport.TryDeliverIngress, so a shadow producer being
// absent, slow, hanging, or misconfigured can never affect, meaningfully
// delay, or fail the real Codex hook flow it is shadowing. Nothing this
// function does can block longer than config's ConnectTimeout plus
// WriteTimeout combined (each independently enforced and hard-capped by
// withDefaults — see MaxShadowConnectTimeout/MaxShadowWriteTimeout), and an
// earlier deadline already set on ctx by the caller always wins over both:
// the connect stage's context.WithTimeout already yields the earlier of the
// two deadlines by construction, and the write stage's own deadline is
// explicitly clamped to ctx's deadline below rather than only ever
// extending it.
func TryDeliverShadowWithConfig(ctx context.Context, config ShadowDeliveryConfig, socketPath string, observation hookadapter.IngressObservation) bool {
	if socketPath == "" {
		return false
	}
	if err := observation.Validate(); err != nil {
		return false
	}
	if ctx.Err() != nil {
		// An already-cancelled or already-expired parent context must fail
		// immediately: no fallback to some default budget, no ignoring the
		// caller's own deadline.
		return false
	}
	config = config.withDefaults()

	dialCtx, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	defer cancel()
	connection, err := config.Dial(dialCtx, socketPath)
	if err != nil {
		return false
	}
	defer connection.Close()

	writeDeadline := time.Now().Add(config.WriteTimeout)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(writeDeadline) {
		// The parent context's own, earlier deadline wins over the write
		// stage's own budget — this stage does not otherwise consult ctx at
		// all (SetWriteDeadline is a raw connection deadline, not
		// context-aware), so without this clamp a caller's own shorter
		// deadline would silently be ignored for the write stage.
		writeDeadline = parentDeadline
	}
	if err := connection.SetWriteDeadline(writeDeadline); err != nil {
		return false
	}
	if err := json.NewEncoder(connection).Encode(observation); err != nil {
		return false
	}
	return true
}
