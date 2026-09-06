package toolchain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

// sourceTree is a descriptor-anchored, symlink-refusing view of a staging
// source boundary.
//
// WHY NOT os.Root, WHICH THIS REPLACES (#1921 panel, class 1). os.Root refuses
// escapes OUT of its root but RESOLVES symlinks that stay inside it, and it uses
// O_NOFOLLOW internally to discover them - so passing syscall.O_NOFOLLOW as a
// caller changes nothing. Measured on go1.26.4: os.Root.OpenFile with
// O_NOFOLLOW returned the TARGET's bytes for an in-root link, and a panel lens
// landed the race against the real staging code at attempt 54, publishing
// private credential bytes.
//
// openat2 with RESOLVE_NO_SYMLINKS refuses a symlink at ANY component, in the
// kernel, in the same syscall that opens - so there is no window between
// deciding and opening. RESOLVE_BENEATH keeps every resolution under the
// boundary.
//
// FAIL CLOSED, ALWAYS (directive 122816). openat2 needs Linux 5.6; ENOSYS or
// any resolution failure makes the runtime UNAVAILABLE rather than falling back
// to a weaker read path, because a fallback reinstates the defect this replaces.
// x/sys/unix is pure Go, so the static single-binary invariant holds.
type sourceTree struct {
	file *os.File
	name string
	// engineOwned marks a tree THIS ENGINE created. The strict acceptance rules
	// describe what may be taken FROM AN OPERATOR PATH; they must not be turned
	// on the engine's own published tree, whose directories are deliberately
	// 0700 and whose files are 0400/0500 so no seat can rewrite them. Symlink
	// refusal still applies - that is openat2's job and it is unconditional.
	engineOwned bool
}

// resolveFlags apply to every component of every path opened through here.
const resolveFlags = unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH

// swapBarrier is a TEST SEAM and is nil in production.
//
// It exists because directive 122816 requires the symlink-swap regression to be
// DETERMINISTIC rather than a probabilistic race loop: a high-iteration loop
// proves only that the author was patient, and it fails to prove anything at all
// on a fast or loaded machine. The barrier fires exactly between enumerating a
// name and opening it - the one window the substitution attack needs - so the
// test can replace the member there and the assertion becomes reproducible.
var swapBarrier func(relative string)

func openSourceTree(boundary string) (*sourceTree, error) {
	fd, err := unix.Open(boundary, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open source boundary %q: %v", ErrRuntimeNotStageable, boundary, err)
	}
	return &sourceTree{file: os.NewFile(uintptr(fd), boundary), name: boundary}, nil
}

func (s *sourceTree) close() {
	if s != nil && s.file != nil {
		_ = s.file.Close()
	}
}

// openMember opens one member relative to the boundary with no symlink
// traversal anywhere in the path. Metadata MUST come from the returned
// descriptor, never from the name that produced it.
func (s *sourceTree) openMember(relative string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC
	if directory {
		flags |= unix.O_DIRECTORY
	}
	how := &unix.OpenHow{Flags: uint64(flags), Resolve: resolveFlags}
	fd, err := unix.Openat2(int(s.file.Fd()), relative, how)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return nil, fmt.Errorf("%w: openat2 is unavailable on this kernel, so staging cannot refuse symlink substitution", ErrRuntimeNotStageable)
		}
		return nil, fmt.Errorf("%w: resolve %q under %q: %v", ErrRuntimeNotStageable, relative, s.name, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(s.name, relative)), nil
}

// stagedMember is one accepted source member, already open.
type stagedMember struct {
	relative string
	file     *os.File
	info     fs.FileInfo
}

func closeMembers(members []stagedMember) {
	for _, member := range members {
		_ = member.file.Close()
	}
}

// collectMembers returns every member of a boundary that may be copied, each
// already open through a symlink-refusing resolution, in a stable order.
//
// IT REFUSES RATHER THAN CLASSIFIES (#1921 panel, class 2). Mode bits cannot
// tell a secret from a payload: a 0640 config under a 0750 or setgid tree reads
// "group-readable", and a POSIX ACL granting one principal appears in the GROUP
// bits, so an ACL-shared secret reads "public". An ambiguous tree is therefore
// not filtered but REFUSED WHOLE - a non-regular member, setuid or setgid, an
// access OR default ACL, or a member that is not world-readable ends the stage,
// and the caller publishes the runtime unavailable.
//
// Measured before choosing strictness over filtering: codex's real package is 11
// files with zero non-world-readable, zero setuid/setgid, zero non-regular
// members and zero ACLs, and claude, kimi and node are mode-0755 regular files
// with no ACLs. Strictness costs nothing on the real artifacts and turns an
// ambiguous tree into an explicit exit-126 instead of a silent copy.
func (s *sourceTree) collectMembers(entrypoint string) ([]stagedMember, error) {
	var members []stagedMember
	ok := false
	defer func() {
		if !ok {
			closeMembers(members)
		}
	}()

	var walk func(relative string) error
	walk = func(relative string) error {
		directory, err := s.openMember(relative, true)
		if err != nil {
			return err
		}
		if err := checkDirectoryMember(relative, directory, s.engineOwned); err != nil {
			_ = directory.Close()
			return err
		}
		names, readErr := directory.Readdirnames(-1)
		closeErr := directory.Close()
		if readErr != nil {
			return fmt.Errorf("%w: list %q: %v", ErrRuntimeNotStageable, relative, readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Strings(names)
		for _, name := range names {
			child := name
			if relative != "." {
				child = filepath.Join(relative, name)
			}
			// THE SUBSTITUTION WINDOW IS HERE: the name has been enumerated and
			// is about to be opened. In production the barrier is nil; in the
			// regression it replaces the member with a symlink to a private
			// file, and the open below must fail rather than follow it.
			if swapBarrier != nil {
				swapBarrier(child)
			}
			handle, err := s.openMember(child, false)
			if err != nil {
				if directoryHandle, dirErr := s.openMember(child, true); dirErr == nil {
					_ = directoryHandle.Close()
					if err := walk(child); err != nil {
						return err
					}
					continue
				}
				return err
			}
			info, statErr := handle.Stat()
			if statErr != nil {
				_ = handle.Close()
				return fmt.Errorf("%w: stat %q: %v", ErrRuntimeNotStageable, child, statErr)
			}
			if info.IsDir() {
				_ = handle.Close()
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			if err := acceptableMember(child, info, handle, entrypoint, s.engineOwned); err != nil {
				_ = handle.Close()
				return err
			}
			members = append(members, stagedMember{relative: child, file: handle, info: info})
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	ok = true
	return members, nil
}

// checkDirectoryMember refuses a directory whose own attributes make its
// descendants' permissions unreadable from mode bits.
func checkDirectoryMember(relative string, handle *os.File, engineOwned bool) error {
	info, err := handle.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat directory %q: %v", ErrRuntimeNotStageable, relative, err)
	}
	if info.Mode()&os.ModeSetgid != 0 {
		return fmt.Errorf("%w: directory %q is setgid (%s), so its members' group bits do not describe their permissions", ErrRuntimeNotStageable, relative, info.Mode())
	}
	// A DIRECTORY THE WORLD CANNOT TRAVERSE MAKES ITS DESCENDANTS PRIVATE
	// WHATEVER THEIR OWN MODE SAYS. This is the bridge F1 shape - a 0700
	// credentials/ holding a mode-0644 token - and it is why the leaf test alone
	// was never enough. Refusing the whole tree rather than skipping the subtree
	// is deliberate: a runtime whose package contains operator-private state is
	// not a runtime this engine can stage safely at all.
	if engineOwned {
		return nil
	}
	if relative != "." && info.Mode().Perm()&0o005 != 0o005 {
		return fmt.Errorf("%w: directory %q is not world-traversable (%s), so its members cannot be distinguished from operator-private state", ErrRuntimeNotStageable, relative, info.Mode().Perm())
	}
	if hasACL(handle) {
		return fmt.Errorf("%w: directory %q carries a POSIX ACL, so its members' group bits are an ACL mask", ErrRuntimeNotStageable, relative)
	}
	return nil
}

// acceptableMember decides whether one OPEN member may be copied at all.
func acceptableMember(relative string, info fs.FileInfo, handle *os.File, entrypoint string, engineOwned bool) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %q is not a regular file (%s); an ambiguous member refuses the whole tree", ErrRuntimeNotStageable, relative, info.Mode())
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fmt.Errorf("%w: %q carries setuid/setgid (%s), whose semantics mode bits do not describe", ErrRuntimeNotStageable, relative, info.Mode())
	}
	if hasACL(handle) {
		return fmt.Errorf("%w: %q carries a POSIX ACL, so its group bits are an ACL mask rather than a permission", ErrRuntimeNotStageable, relative)
	}
	if engineOwned {
		return nil
	}
	// THE ENTRYPOINT IS EXEMPT FROM THE WORLD-READABLE TEST ONLY: a runtime
	// installed mode 0700 must still run, and it is the one member whose
	// identity the caller resolved explicitly rather than discovered.
	if relative == entrypoint {
		return nil
	}
	if info.Mode().Perm()&0o004 == 0 {
		return fmt.Errorf("%w: %q is not world-readable (%s), so it cannot be distinguished from operator-private state", ErrRuntimeNotStageable, relative, info.Mode().Perm())
	}
	return nil
}

// hasACL reports whether an open file or directory carries a POSIX ACL, access
// or default. An unreadable xattr counts as PRESENT: "cannot tell" must refuse.
func hasACL(handle *os.File) bool {
	for _, attribute := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		size, err := unix.Fgetxattr(int(handle.Fd()), attribute, nil)
		switch {
		case err == nil && size > 0:
			return true
		case err == nil:
		case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
			// Genuinely absent, or a filesystem without ACL support.
		default:
			return true
		}
	}
	return false
}
