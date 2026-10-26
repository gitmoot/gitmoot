//go:build linux

package sandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// A managed launch (#2360) must not hand the runtime any descriptor it was not
// explicitly given. The isolated boundary pass showed why: a file opened by an
// authority process and inherited across the profile transition stayed
// writable in the confined child, because the kernel does not revalidate an
// already-open file against the new profile (security/apparmor/file.c,
// aa_file_perm and aa_inherit_files on the delegated/unconfined branch). A
// profile cannot revoke a descriptor; only the launcher can refuse to pass it.
//
// sealDescriptors is called immediately before the runtime is exec'd:
//
//   - fds 0, 1 and 2 must each be a pipe, /dev/null or a terminal. A regular
//     file, directory, socket, block device or any other object is refused,
//     because it can be a descriptor to protected bytes or an authority
//     control channel. The refusal names the fd, its type and its identity.
//   - every other descriptor except keep is marked close-on-exec with
//     close_range(CLOSE_RANGE_CLOEXEC), so nothing else crosses the exec.
//     Marking instead of closing keeps the Go runtime's own descriptors
//     usable until the exec replaces the process.
//
// keep is the one descriptor the caller still needs before exec (the
// no-execution handoff socket, which the caller closes itself), or -1.
func sealDescriptors(keep int) error {
	for fd := 0; fd <= 2; fd++ {
		if err := checkStdioDescriptor(fd); err != nil {
			return err
		}
	}
	const last = ^uint(0)
	switch {
	case keep < 3:
		return cloexecRange(3, last)
	case keep == 3:
		return cloexecRange(4, last)
	default:
		if err := cloexecRange(3, uint(keep-1)); err != nil {
			return err
		}
		return cloexecRange(uint(keep+1), last)
	}
}

func cloexecRange(first, last uint) error {
	if err := unix.CloseRange(first, last, unix.CLOSE_RANGE_CLOEXEC); err != nil {
		// Fail closed: a kernel without CLOSE_RANGE_CLOEXEC (before 5.11)
		// cannot prove nothing else is inherited. Landlock ABI v3, which
		// every managed launch already requires, implies a newer kernel.
		return fmt.Errorf("seal inherited descriptors %d-%d: %w", first, last, err)
	}
	return nil
}

// stdioAllowed reports why an fd 0-2 object is acceptable, or "" if it is not.
func stdioAllowed(fd int, st *unix.Stat_t) string {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFIFO:
		return "pipe"
	case unix.S_IFCHR:
		if unix.Major(st.Rdev) == 1 && unix.Minor(st.Rdev) == 3 {
			return "/dev/null"
		}
		if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
			return "terminal"
		}
	}
	return ""
}

func checkStdioDescriptor(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("refuse launch: fd %d is not open or not inspectable: %w", fd, err)
	}
	if stdioAllowed(fd, &st) != "" {
		return nil
	}
	return fmt.Errorf("refuse launch: fd %d is a %s (dev %d:%d inode %d); a managed runtime's stdin/stdout/stderr must be a pipe, /dev/null or a terminal",
		fd, fileTypeName(st.Mode), unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino)
}

func fileTypeName(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return "regular file"
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFSOCK:
		return "socket"
	case unix.S_IFBLK:
		return "block device"
	case unix.S_IFCHR:
		return "character device"
	case unix.S_IFLNK:
		return "symlink"
	case unix.S_IFIFO:
		return "pipe"
	default:
		return "anonymous or unknown object"
	}
}
