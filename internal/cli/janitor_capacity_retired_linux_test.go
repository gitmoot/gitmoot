//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	_ "unsafe" // go:linkname

	"github.com/gitmoot/gitmoot/internal/db"
	"golang.org/x/sys/unix"
)

// workflowMountInfoPath is the workflow package's mount table seam (the file read for
// /proc/self/mountinfo), so this test can present a synthetic fuse.sshfs mount.
//
//go:linkname workflowMountInfoPath github.com/gitmoot/gitmoot/internal/workflow.mountInfoPath
var workflowMountInfoPath string

// #2763 (P2-4): the active root moved to local disk with no mount keys, and the old sshfs root is
// retired with its mount keys. A repo checkout still on that mount is network storage, skipped by
// janitor/review capacity exactly as under an active sshfs root; a checkout on a local device is
// still measured, and the retired root itself is never added as a host path.
func TestJanitorHostDevicesSkipRetiredSSHFSMount(t *testing.T) {
	ctx, store, paths, _ := diskGuardDispatchFixture(t)
	box := filepath.Join(t.TempDir(), "box")
	retired := filepath.Join(box, "worktrees")
	onMount := filepath.Join(box, "checkouts", "repo")
	active := filepath.Join(t.TempDir(), "ssd", "worktrees")
	for _, dir := range []string{retired, onMount, active} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	local, err := os.MkdirTemp("/dev/shm", "gm-local-checkout-")
	if err != nil {
		t.Skipf("separate local mount unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(local) })
	var mounted, other unix.Stat_t
	if err := unix.Stat(onMount, &mounted); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(local, &other); err != nil {
		t.Fatal(err)
	}
	if mounted.Dev == other.Dev {
		t.Skip("fixture requires distinct devices")
	}

	// The synthetic table presents the temp filesystem's own mount as the sshfs mount at box, so
	// the statx mount-identity check is the real one.
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, box, 0, unix.STATX_MNT_ID, &st); err != nil || st.Mask&unix.STATX_MNT_ID == 0 {
		t.Skipf("statx mount id unavailable: %v", err)
	}
	table := fmt.Sprintf("1 0 0:1 / / rw - ext4 /dev/root rw\n%d 1 0:2 / %s rw,nosuid shared:7 - fuse.sshfs user@host:share rw\n", st.Mnt_id, box)
	mountInfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountInfo, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := workflowMountInfoPath
	workflowMountInfoPath = mountInfo
	t.Cleanup(func() { workflowMountInfoPath = previous })

	appendConfig(t, paths, fmt.Sprintf("\n[worktrees]\nroot = %q\n\n[[worktrees.retired]]\nroot = %q\nmount_point = %q\nmount_source = \"user@host:share\"\nmount_fstype = \"fuse.sshfs\"\n", active, retired, box))
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: onMount}); err != nil {
		t.Fatal(err)
	}
	hostPaths, roots, err := janitorHostPaths(ctx, store, paths, "owner/repo", local)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hostPaths, onMount) || !slices.Contains(hostPaths, local) || !slices.Contains(hostPaths, active) {
		t.Fatalf("host paths %v miss a checkout or the local active root", hostPaths)
	}
	if slices.Contains(hostPaths, retired) {
		t.Fatalf("host paths %v measure the retired root", hostPaths)
	}
	devices, err := janitorHostDevices([]string{onMount, local}, roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[uint64(other.Dev)] != local {
		t.Fatalf("devices = %v, want only the local checkout's device %d (the checkout on the retired sshfs mount is network storage)", devices, other.Dev)
	}
}
