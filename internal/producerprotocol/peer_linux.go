//go:build linux

package producerprotocol

import (
	"fmt"
	"net"
	"syscall"
)

// peerCredentials reads the kernel-verified peer identity of a Unix domain
// socket connection via SO_PEERCRED. It must be called immediately after
// accept, before any data is read, so the identity cannot be attributed to a
// process that has since exited and had its PID reused.
func peerCredentials(connection *net.UnixConn) (PeerIdentity, error) {
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
