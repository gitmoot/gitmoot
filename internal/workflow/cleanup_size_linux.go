//go:build linux

package workflow

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// measureCleanupTree sizes a cleanup resource in ALLOCATED bytes (#2360, schema v2, agreement
// 356319). It is the only measurement a byte budget may be compared with, so every way it can be
// wrong produces an INCOMPLETE result (Bytes nil) that refuses destruction, never a low number:
//
//   - bytes = sum of st_blocks*512 over the root, every descendant directory, regular file,
//     symlink inode and other inode; symlinks are never followed;
//   - every inode is counted once; a regular file whose link count exceeds the links found inside
//     the completely enumerated tree is shared with something outside it -> incomplete;
//   - an entry on a different mount (statx mount id), a vanished, changing or unreadable entry,
//     an unsupported identity, an exhausted entry/depth/time limit or an integer overflow -> incomplete.
//
// Directories are read in bounded batches through descriptors opened relative to their parent
// with O_NOFOLLOW, so a path swapped for a symlink mid-walk is never traversed. Memory is bounded
// by the depth stack plus the multi-link inode inventory, itself bounded by MaxEntries.
func measureCleanupTree(root string, limits CleanupSizeLimits, now func() time.Time) CleanupSizeMeasurement {
	m := CleanupSizeMeasurement{Unit: CleanupSizeUnitAllocatedBytes, Scope: CleanupSizeScopeWorktreeTree, Limits: limits}
	started := now()
	m.ObservedAt = started.UTC().Format(time.RFC3339Nano)
	if limits.MaxEntries <= 0 || limits.MaxDepth <= 0 || limits.TimeoutMS <= 0 {
		m.IncompleteReason = "sizing limits must all be positive"
		return m
	}
	w := &cleanupTreeWalk{
		limits:   limits,
		deadline: started.Add(time.Duration(limits.TimeoutMS) * time.Millisecond),
		now:      now,
		links:    map[cleanupInodeKey]*cleanupInodeLinks{},
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		m.IncompleteReason = fmt.Sprintf("open resource root: %v", err)
		return m
	}
	defer unix.Close(fd)
	var st unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, cleanupStatxMask, &st); err != nil {
		m.IncompleteReason = fmt.Sprintf("statx resource root: %v", err)
		return m
	}
	if st.Mask&unix.STATX_MNT_ID == 0 || st.Mask&unix.STATX_BLOCKS == 0 || st.Mask&unix.STATX_INO == 0 || st.Mask&unix.STATX_NLINK == 0 {
		m.IncompleteReason = "the kernel does not report mount id, blocks, inode and link count for the root"
		return m
	}
	w.mountID = st.Mnt_id
	if reason := w.account(st); reason != "" {
		m.IncompleteReason = reason
		return m
	}
	if reason := w.walkDir(fd, 1); reason != "" {
		m.IncompleteReason = reason
		return m
	}
	for key, links := range w.links {
		if false && links.found < links.nlink { // MUTANT M3
			m.IncompleteReason = fmt.Sprintf("inode %d:%d has %d links, only %d inside the resource (shared with outside)", key.dev, key.ino, links.nlink, links.found)
			return m
		}
	}
	total := w.total
	m.Bytes = &total
	m.Complete = true
	return m
}

const cleanupStatxMask = unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_INO | unix.STATX_NLINK | unix.STATX_BLOCKS | unix.STATX_MNT_ID

// cleanupDirBatch bounds one directory read; directories are never read whole.
const cleanupDirBatch = 256

type cleanupInodeKey struct {
	dev uint64
	ino uint64
}

type cleanupInodeLinks struct {
	nlink uint32
	found uint32
}

type cleanupTreeWalk struct {
	limits   CleanupSizeLimits
	deadline time.Time
	now      func() time.Time
	mountID  uint64
	entries  int64
	total    int64
	links    map[cleanupInodeKey]*cleanupInodeLinks
}

// account adds one inode, counting each physical inode once. A non-empty return is the
// incomplete reason.
func (w *cleanupTreeWalk) account(st unix.Statx_t) string {
	w.entries++
	if w.entries > w.limits.MaxEntries {
		return fmt.Sprintf("entry limit %d exceeded", w.limits.MaxEntries)
	}
	if w.now().After(w.deadline) {
		return fmt.Sprintf("time limit %dms exceeded", w.limits.TimeoutMS)
	}
	if st.Mnt_id != w.mountID {
		return "an entry is on a different mount"
	}
	key := cleanupInodeKey{dev: uint64(st.Dev_major)<<32 | uint64(st.Dev_minor), ino: st.Ino}
	if st.Nlink > 1 && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if links, seen := w.links[key]; seen {
			links.found++
			return "" // already counted once
		}
		w.links[key] = &cleanupInodeLinks{nlink: st.Nlink, found: 1}
	}
	if st.Blocks > math.MaxInt64/512 {
		return "allocated size overflows"
	}
	bytes := int64(st.Blocks) * 512
	if w.total > math.MaxInt64-bytes {
		return "allocated size overflows"
	}
	w.total += bytes
	return ""
}

func (w *cleanupTreeWalk) walkDir(dirfd int, depth int) string {
	if depth > w.limits.MaxDepth {
		return fmt.Sprintf("depth limit %d exceeded", w.limits.MaxDepth)
	}
	// os.NewFile takes ownership of a duplicate so ReadDir batches without closing dirfd.
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return fmt.Sprintf("dup directory descriptor: %v", err)
	}
	dir := os.NewFile(uintptr(dup), "")
	defer dir.Close()
	for {
		batch, err := dir.ReadDir(cleanupDirBatch)
		for _, entry := range batch {
			if reason := w.entry(dirfd, entry.Name(), depth); reason != "" {
				return reason
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			return ""
		}
		if err != nil {
			return fmt.Sprintf("read directory: %v", err)
		}
	}
}

func (w *cleanupTreeWalk) entry(dirfd int, name string, depth int) string {
	var st unix.Statx_t
	if err := unix.Statx(dirfd, name, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_SYNC_AS_STAT, cleanupStatxMask, &st); err != nil {
		return fmt.Sprintf("statx %q: %v (entry vanished, changed or unreadable)", name, err)
	}
	if st.Mask&unix.STATX_MNT_ID == 0 || st.Mask&unix.STATX_BLOCKS == 0 {
		return fmt.Sprintf("statx %q: mount id or blocks not reported", name)
	}
	if reason := w.account(st); reason != "" {
		return reason
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ""
	}
	child, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Sprintf("open directory %q: %v (vanished, replaced or unreadable)", name, err)
	}
	defer unix.Close(child)
	// The directory opened must be the inode just measured: a swap in between is a change.
	var opened unix.Statx_t
	if err := unix.Statx(child, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, cleanupStatxMask, &opened); err != nil || opened.Ino != st.Ino || opened.Mnt_id != st.Mnt_id {
		return fmt.Sprintf("directory %q changed while it was measured", name)
	}
	return w.walkDir(child, depth+1)
}
