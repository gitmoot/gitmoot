//go:build linux || darwin

package keychain

import (
	"fmt"
	"os"
	"syscall"
)

// CurrentUID is the operator uid the keychain file must be owned by. It is a
// variable so tests can simulate a foreign owner.
var CurrentUID = func() uint32 {
	return uint32(syscall.Geteuid())
}

func ownerUID(info os.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("owner information unavailable")
	}
	return stat.Uid, nil
}
