//go:build !linux

package workflow

import (
	"errors"
	"io/fs"
	"time"
)

// The reclaim's removal proof relies on descriptor-relative unlinks and on the
// inode change time, which is where a same-user writer cannot reach. Both are
// spelled differently outside Linux, and a weaker stand-in would be exactly the
// forgeable size+mtime check this design replaced.
//
// So the non-Linux build FAILS CLOSED: the inventory proves nothing, the removal
// refuses, and fences are never pruned. The daemon's other same-host guarantees
// (the /proc liveness probe) are already Linux-only for the same reason.

type fixCloneEntry struct {
	dir     bool
	symlink bool
	size    int64
	modTime time.Time
	statted bool
}

func (e fixCloneEntry) sameProvedFile(fs.FileInfo) bool { return false }

func fixCloneEntryFrom(entry fs.DirEntry, info fs.FileInfo) fixCloneEntry {
	return fixCloneEntry{
		dir:     entry.IsDir(),
		symlink: entry.Type()&fs.ModeSymlink != 0,
		size:    info.Size(),
		modTime: info.ModTime(),
	}
}

// removeInventoriedTree refuses on this platform: see the file comment.
func removeInventoriedTree(root string, _ fixCloneInventory) error {
	return errors.New("fix clone removal requires descriptor-relative unlinks, which this platform build does not provide")
}

// removeOwnedFence never prunes on this platform: a fence left in place is inert,
// while an unlink decided by a weaker proof is not.
func removeOwnedFence(string, time.Time, FixCloneFenceOwnership) (bool, error) {
	return false, nil
}

// provenFromDescriptor cannot be evaluated without the descriptor primitives, so
// nothing is provably ours here.
func (o FixCloneFenceOwnership) provenFromDescriptor(string, int, any) bool { return false }
