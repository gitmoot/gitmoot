package toolchain

import (
	"strings"

	"golang.org/x/sys/unix"
)

// openBeneathByComponents gives openat2's RESOLVE_NO_SYMLINKS|RESOLVE_BENEATH
// guarantee without openat2. Each component is opened relative to the
// descriptor of the one before it with O_NOFOLLOW, so a symlink at any
// component (including one swapped in mid-walk) fails with ELOOP or ENOTDIR,
// and no step is resolved by name from the boundary again. Absolute paths,
// empty components, "." and ".." are refused before anything is opened.
//
// It compiles on every target so Linux tests exercise it; production uses it
// only where openat2 does not exist (source_tree_other.go).
func openBeneathByComponents(dirfd int, relative string, flags int) (int, error) {
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
	for _, component := range components[:len(components)-1] {
		next, err := unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, err
		}
		closeParent()
		parent = next
	}
	return unix.Openat(parent, components[len(components)-1], flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}
