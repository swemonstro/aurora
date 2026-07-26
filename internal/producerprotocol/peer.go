package producerprotocol

// PeerIdentity is the kernel-verified identity of a local Unix domain socket
// peer, captured immediately after accept. It is transport-local: this
// package never persists it and never echoes it back to the peer.
type PeerIdentity struct {
	UID uint32
	GID uint32
	PID int32
}

// Authenticator decides whether a peer is allowed to use the transport at
// all. It knows nothing about which tool the peer speaks.
type Authenticator interface {
	Authenticate(PeerIdentity) error
}

// IdentityBinder maps an authenticated peer to the single Tool it is
// permitted to speak. Implementations belong to the broker composition root
// and may encode any peer-UID-to-tool policy; this package never inspects
// tool identity itself and only enforces the result via Conn.Bind.
type IdentityBinder interface {
	BindTool(PeerIdentity) (Tool, error)
}

// SameUIDAuthenticator accepts peers running as the server's own effective
// UID, plus any explicitly allowed UID. It carries no tool knowledge.
type SameUIDAuthenticator struct {
	ServerUID   uint32
	AllowedUIDs map[uint32]struct{}
}

func (auth SameUIDAuthenticator) Authenticate(peer PeerIdentity) error {
	if peer.UID == auth.ServerUID {
		return nil
	}
	if _, allowed := auth.AllowedUIDs[peer.UID]; allowed {
		return nil
	}
	return ErrUnauthorizedPeer
}

// BindPeerTool resolves the tool a peer is permitted to speak via binder and
// binds conn to it. It is a thin composition helper: all tool-to-peer policy
// lives in binder, supplied by the caller.
func BindPeerTool(conn *Conn, peer PeerIdentity, binder IdentityBinder) error {
	tool, err := binder.BindTool(peer)
	if err != nil {
		return err
	}
	return conn.Bind(tool)
}
