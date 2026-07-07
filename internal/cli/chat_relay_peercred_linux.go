//go:build linux

package cli

import (
	"errors"
	"net"
	"syscall"
)

// peerUID returns the uid of the process on the other end of a unix-socket
// connection via SO_PEERCRED (Linux). It is the daemon relay's peer-identity
// check: the daemon rejects any connection whose uid differs from its own.
func peerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("relay peer is not a unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		ucred   *syscall.Ucred
		credErr error
	)
	if ctrlErr := raw.Control(func(fd uintptr) {
		ucred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); ctrlErr != nil {
		return 0, ctrlErr
	}
	if credErr != nil {
		return 0, credErr
	}
	return ucred.Uid, nil
}
