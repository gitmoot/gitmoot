//go:build linux

package toolchain

import "golang.org/x/sys/unix"

// openBeneath uses openat2: the kernel refuses a symlink at any component and
// any resolution outside dirfd in the same syscall that opens. ENOSYS (kernel
// older than 5.6) is returned to the caller, which fails closed.
func openBeneath(dirfd int, relative string, flags int) (int, error) {
	how := &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH}
	return unix.Openat2(dirfd, relative, how)
}
