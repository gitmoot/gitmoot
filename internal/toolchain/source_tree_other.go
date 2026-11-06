//go:build !linux

package toolchain

import "golang.org/x/sys/unix"

// openBeneath has no openat2 outside Linux, and a userspace walk cannot give
// RESOLVE_BENEATH's containment guarantee against concurrent renames (#2538
// review). Report ENOSYS so openMember fails closed, exactly as on a Linux
// kernel older than 5.6: runtime staging is unavailable on these systems.
func openBeneath(int, string, int) (int, error) {
	return -1, unix.ENOSYS
}
