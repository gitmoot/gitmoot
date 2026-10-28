//go:build !linux

package workflow

import "time"

// measureCleanupTree is only implemented on Linux, where statx reports the mount id needed to
// detect crossed mounts. Elsewhere every measurement is incomplete, so the budget gate refuses.
func measureCleanupTree(root string, limits CleanupSizeLimits, now func() time.Time) CleanupSizeMeasurement {
	return CleanupSizeMeasurement{
		Unit: CleanupSizeUnitAllocatedBytes, Scope: CleanupSizeScopeWorktreeTree, Limits: limits,
		ObservedAt:       now().UTC().Format(time.RFC3339Nano),
		IncompleteReason: "allocated-bytes sizing is only supported on linux",
	}
}
