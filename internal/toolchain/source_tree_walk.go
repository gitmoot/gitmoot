package toolchain

import (
	"strings"

	"golang.org/x/sys/unix"
)

// walkBarrier is a TEST SEAM and is nil in production. It fires after each
// intermediate component is opened, so a test can relocate a directory at
// exactly the moment a concurrent rename would need to land.
var walkBarrier func(opened string)

// openBeneathByComponents gives openat2's RESOLVE_NO_SYMLINKS|RESOLVE_BENEATH
// guarantee without openat2.
//
//   - No symlinks: each component is opened relative to the descriptor of the
//     one before it with O_NOFOLLOW, so a symlink at any component (including
//     one swapped in mid-walk) fails with ELOOP or ENOTDIR.
//   - Beneath: absolute paths, empty components, "." and ".." are refused
//     before anything is opened (relative may be exactly "." for the boundary
//     itself). A directory relocated outside the boundary mid-walk would leave
//     the retained descriptor outside it, so after the final open the path is
//     resolved again by name from dirfd and must name the same file (device
//     and inode); otherwise the open fails closed (#2538 review).
//
// It compiles on every target so Linux tests exercise it; production uses it
// only where openat2 does not exist (source_tree_other.go).
func openBeneathByComponents(dirfd int, relative string, flags int) (int, error) {
	if relative == "." {
		return unix.Openat(dirfd, ".", flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if relative == "" || strings.HasPrefix(relative, "/") {
		return -1, unix.EINVAL
	}
	components := strings.Split(relative, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return -1, unix.EINVAL
		}
	}
	parent := dirfd
	closeParent := func() {
		if parent != dirfd {
			_ = unix.Close(parent)
		}
	}
	defer closeParent()
	for i, component := range components[:len(components)-1] {
		next, err := unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, err
		}
		closeParent()
		parent = next
		if walkBarrier != nil {
			walkBarrier(strings.Join(components[:i+1], "/"))
		}
	}
	fd, err := unix.Openat(parent, components[len(components)-1], flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := stillBeneath(dirfd, relative, fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// stillBeneath reports EXDEV unless relative, resolved by name from dirfd,
// names the file open as fd.
func stillBeneath(dirfd int, relative string, fd int) error {
	var opened, named unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if err := unix.Fstatat(dirfd, relative, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.EXDEV
	}
	if opened.Dev != named.Dev || opened.Ino != named.Ino {
		return unix.EXDEV
	}
	return nil
}
