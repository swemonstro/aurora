//go:build linux

package localhooktransport

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

type SameUIDAuthenticator struct {
	ServerUID   uint32
	AllowedUIDs map[uint32]struct{}
}

func DefaultAuthenticator() SameUIDAuthenticator {
	return SameUIDAuthenticator{ServerUID: uint32(os.Geteuid())}
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

func peerIdentity(connection *net.UnixConn) (PeerIdentity, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return PeerIdentity{}, err
	}
	var credentials *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return PeerIdentity{}, err
	}
	if socketErr != nil || credentials == nil {
		return PeerIdentity{}, fmt.Errorf("read peer credentials: %w", socketErr)
	}
	return PeerIdentity{UID: credentials.Uid, GID: credentials.Gid, PID: credentials.Pid}, nil
}
