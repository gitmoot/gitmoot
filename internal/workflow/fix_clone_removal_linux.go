//go:build linux

package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// This file holds the parts of fix-clone reclaim that need DESCRIPTOR-RELATIVE
// filesystem primitives and inode-change times. Both are Linux-shaped: the
// non-Linux build refuses to remove anything rather than falling back to a weaker
// proof, because the whole point of these routines is that the kernel — not a
// sampled check — decides what may be deleted.

// fixCloneEntry is one directory entry as the final proof saw it.
//
// The identity includes the INODE and the inode CHANGE TIME, not just size and
// mtime. mtime is writable: a writer can rewrite a proved file with same-size
// content and restore the old mtime with utimensat, and a size+mtime check accepts
// that. ctime advances on any write or metadata change and no unprivileged call
// can move it backwards, so the pair (ino, ctime) is what makes "this is still the
// file I proved" decidable.
type fixCloneEntry struct {
	dir     bool
	symlink bool
	size    int64
	modTime time.Time
	ino     uint64
	ctime   unix.Timespec
	statted bool
}

// sameProvedFile reports whether a file on disk is still the one the proof read.
func (e fixCloneEntry) sameProvedFile(info fs.FileInfo) bool {
	if info.IsDir() != e.dir || (info.Mode()&fs.ModeSymlink != 0) != e.symlink {
		return false
	}
	if e.dir {
		return true
	}
	if info.Size() != e.size || !info.ModTime().Equal(e.modTime) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !e.statted {
		// Without inode metadata the size+mtime pair is all there is, and that is
		// forgeable: refuse rather than delete on a weaker proof than intended.
		return false
	}
	return stat.Ino == e.ino && stat.Ctim.Sec == e.ctime.Sec && stat.Ctim.Nsec == e.ctime.Nsec
}

// sameProvedStat is sameProvedFile for a descriptor-relative stat.
func (e fixCloneEntry) sameProvedStat(stat *unix.Stat_t) bool {
	isDir := stat.Mode&unix.S_IFMT == unix.S_IFDIR
	isSymlink := stat.Mode&unix.S_IFMT == unix.S_IFLNK
	if isDir != e.dir || isSymlink != e.symlink {
		return false
	}
	if !e.statted {
		return false
	}
	if stat.Ino != e.ino || stat.Ctim.Sec != e.ctime.Sec || stat.Ctim.Nsec != e.ctime.Nsec {
		return false
	}
	return e.dir || stat.Size == e.size
}

func fixCloneEntryFrom(entry fs.DirEntry, info fs.FileInfo) fixCloneEntry {
	captured := fixCloneEntry{
		dir:     entry.IsDir(),
		symlink: entry.Type()&fs.ModeSymlink != 0,
		size:    info.Size(),
		modTime: info.ModTime(),
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		captured.ino = stat.Ino
		captured.ctime = unix.Timespec{Sec: stat.Ctim.Sec, Nsec: stat.Ctim.Nsec}
		captured.statted = true
	}
	return captured
}

// removeInventoriedTree deletes EXACTLY what the final proof inventoried, and
// nothing else, using DESCRIPTOR-RELATIVE operations throughout.
//
// This is the mechanism that closes the final-scan-to-removal window, and the
// kernel enforces it rather than a check sampling it:
//
//   - Every step is openat/fstatat/unlinkat against a directory descriptor this
//     function opened and verified, so no component of the path is re-resolved
//     between the proof and the unlink. A directory swapped for a symlink or a
//     different directory after the proof cannot redirect a removal, because the
//     descriptor still refers to the object that was proved.
//   - Every entry must match the inventory by inode AND inode-change time, not
//     just size and mtime: a writer can rewrite a file with same-size content and
//     restore its mtime, but cannot move ctime backwards.
//   - An entry the proof never saw is never unlinked, and the rmdir of its parent
//     then fails with ENOTEMPTY, so new content aborts the removal instead of
//     being deleted with it.
//
// The caller restores the quarantine and retains on errFixCloneTreeChanged. A
// partially removed tree is safe to leave: everything already unlinked was proved
// published, and the next pass re-proves whatever survives.
func removeInventoriedTree(root string, inventory fixCloneInventory) error {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open fix clone root %s: %w", root, err)
	}
	emptied, err := removeInventoriedChildren(rootFD, root, "", inventory)
	_ = unix.Close(rootFD)
	if err != nil {
		return err
	}
	if !emptied {
		return fmt.Errorf("%w: %s gained content after the proof", errFixCloneTreeChanged, root)
	}
	if err := os.Remove(root); err != nil {
		if isDirectoryNotEmpty(err) {
			return fmt.Errorf("%w: %s gained content after the proof", errFixCloneTreeChanged, root)
		}
		if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// removeInventoriedChildren empties one proved directory through its descriptor.
// It reports whether the directory ended up empty; a caller only removes a
// directory it emptied.
func removeInventoriedChildren(dirFD int, root, rel string, inventory fixCloneInventory) (bool, error) {
	names, err := readDirNamesAt(dirFD)
	if err != nil {
		return false, fmt.Errorf("read fix clone directory %s: %w", filepath.Join(root, rel), err)
	}
	for _, name := range names {
		childRel := filepath.Join(rel, name)
		absolute := filepath.Join(root, childRel)
		var stat unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return false, fmt.Errorf("inspect %s: %w", absolute, err)
		}
		proved, ok := inventory[childRel]
		if !ok {
			// Created after the proof: never unlinked, and it keeps this directory
			// non-empty so the removal aborts above it.
			return false, fmt.Errorf("%w: %s appeared after the proof", errFixCloneTreeChanged, absolute)
		}
		if !proved.sameProvedStat(&stat) {
			return false, fmt.Errorf("%w: %s was replaced after the proof", errFixCloneTreeChanged, absolute)
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			childFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				return false, fmt.Errorf("open %s: %w", absolute, err)
			}
			// The descriptor must be the object just stat'd: nothing swapped the
			// name between fstatat and openat.
			var opened unix.Stat_t
			if err := unix.Fstat(childFD, &opened); err != nil {
				_ = unix.Close(childFD)
				return false, fmt.Errorf("inspect opened %s: %w", absolute, err)
			}
			if opened.Ino != stat.Ino || opened.Dev != stat.Dev {
				_ = unix.Close(childFD)
				return false, fmt.Errorf("%w: %s was swapped while being opened", errFixCloneTreeChanged, absolute)
			}
			emptied, err := removeInventoriedChildren(childFD, root, childRel, inventory)
			_ = unix.Close(childFD)
			if err != nil {
				return false, err
			}
			if !emptied {
				return false, fmt.Errorf("%w: %s gained content after the proof", errFixCloneTreeChanged, absolute)
			}
			if err := unix.Unlinkat(dirFD, name, unix.AT_REMOVEDIR); err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				if isDirectoryNotEmpty(err) {
					return false, fmt.Errorf("%w: %s gained content after the proof", errFixCloneTreeChanged, absolute)
				}
				return false, fmt.Errorf("remove %s: %w", absolute, err)
			}
			continue
		}
		if err := unix.Unlinkat(dirFD, name, 0); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return false, fmt.Errorf("remove %s: %w", absolute, err)
		}
	}
	// Re-read: an entry created while this loop ran must keep the directory alive.
	remaining, err := readDirNamesAt(dirFD)
	if err != nil {
		return false, fmt.Errorf("recheck fix clone directory %s: %w", filepath.Join(root, rel), err)
	}
	return len(remaining) == 0, nil
}

// readDirNamesAt lists a directory through an existing descriptor without
// re-resolving its path. The descriptor is duplicated because os.File takes
// ownership of the fd it is given.
func readDirNamesAt(dirFD int) ([]string, error) {
	duplicate, err := unix.Dup(dirFD)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(duplicate), "fix-clone-dir")
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return names, nil
	}
	return names, nil
}

// removeOwnedFence unlinks a fence only after re-proving ownership through a
// DESCRIPTOR and unlinking relative to its parent's descriptor.
//
// Classification happened in an earlier syscall, and a same-user writer can
// replace the name in the gap: a prune that re-read by path would delete whatever
// now sits there as long as it looked old enough. Here the parent directory is
// opened once, the entry is stat'd and opened relative to that descriptor, its
// inode is compared across both, its bytes are re-read from the open file, and the
// unlink is unlinkat on the same descriptor — so the object removed is the object
// proved, and no path component can be re-resolved in between.
func removeOwnedFence(path string, cutoff time.Time, owned FixCloneFenceOwnership) (bool, error) {
	parent, name := filepath.Dir(path), filepath.Base(path)
	parentFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("open fix clone fence parent %s: %w", parent, err)
	}
	defer unix.Close(parentFD)
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("inspect fix clone fence %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return false, nil
	}
	if !time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).Before(cutoff) {
		return false, nil
	}
	fenceFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
			return false, nil
		}
		return false, fmt.Errorf("open fix clone fence %s: %w", path, err)
	}
	defer unix.Close(fenceFD)
	var opened unix.Stat_t
	if err := unix.Fstat(fenceFD, &opened); err != nil {
		return false, fmt.Errorf("inspect opened fix clone fence %s: %w", path, err)
	}
	// Same object across the stat and the open, and still the shape a fence has.
	if opened.Ino != stat.Ino || opened.Dev != stat.Dev || opened.Nlink != 1 || opened.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, nil
	}
	if !owned.provenFromDescriptor(path, fenceFD, &opened) {
		return false, nil
	}
	// The name must STILL be that inode at the moment of the unlink.
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("recheck fix clone fence %s: %w", path, err)
	}
	if current.Ino != opened.Ino || current.Dev != opened.Dev {
		return false, nil
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("prune fix clone fence %s: %w", path, err)
	}
	return true, nil
}

// provenFromDescriptor is proven() for an already-open, already-identity-checked
// descriptor, so the prune path never re-reads by path.
func (o FixCloneFenceOwnership) provenFromDescriptor(path string, fd int, stat *unix.Stat_t) bool {
	nonce, ok := o.expected(path)
	if !ok {
		return false
	}
	want := fixCloneFenceContent(nonce)
	if stat.Size != int64(len(want)) {
		return false
	}
	content := make([]byte, len(want))
	if _, err := unix.Pread(fd, content, 0); err != nil {
		return false
	}
	return bytes.Equal(content, want)
}
