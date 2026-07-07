//go:build !linux

package cli

import (
	"net"
	"os"
)

// peerUID on non-Linux platforms has no portable SO_PEERCRED, so it trusts the
// socket's filesystem permissions alone (0600, dir 0700, owned by the daemon
// uid) and reports the daemon's own uid so the caller's uid check passes. The
// production daemon deployment is Linux, where the real SO_PEERCRED check applies.
func peerUID(_ net.Conn) (uint32, error) {
	return uint32(os.Getuid()), nil
}
