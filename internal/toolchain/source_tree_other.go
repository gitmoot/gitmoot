//go:build !linux

package toolchain

// openBeneath has no openat2 outside Linux, so it walks the path one component
// at a time (openBeneathByComponents).
func openBeneath(dirfd int, relative string, flags int) (int, error) {
	return openBeneathByComponents(dirfd, relative, flags)
}
