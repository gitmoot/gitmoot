//go:build linux

package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// worktreeRootWorld is a gitmoot home whose [worktrees] root sits under a directory that a
// synthetic mount table presents as a fuse.sshfs mount (#2375).
type worktreeRootWorld struct {
	home, mount, root string
}

func newWorktreeRootWorld(t *testing.T) worktreeRootWorld {
	t.Helper()
	base := t.TempDir()
	w := worktreeRootWorld{home: filepath.Join(base, "home"), mount: filepath.Join(base, "box")}
	w.root = filepath.Join(w.mount, "worktrees")
	for _, dir := range []string{w.home, w.root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.writeConfig(t, w.root, "user@host:share", "fuse.sshfs")
	w.setMountTable(t, w.mount, "fuse.sshfs", "user@host:share", 0)
	return w
}

func (w worktreeRootWorld) writeConfig(t *testing.T, root, source, fstype string) {
	t.Helper()
	cfg := fmt.Sprintf("[worktrees]\nroot = %q\nmount_point = %q\nmount_source = %q\nmount_fstype = %q\n", root, w.mount, source, fstype)
	if err := os.WriteFile(filepath.Join(w.home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setMountTable installs a mount table whose single interesting line names mountPoint with the
// REAL mount id of the temp filesystem plus idDelta, so the statx identity check is exercised.
func (w worktreeRootWorld) setMountTable(t *testing.T, mountPoint, fstype, source string, idDelta uint64) {
	t.Helper()
	id, err := mountIDAt(unix.AT_FDCWD, filepath.Dir(w.mount), 0)
	if err != nil {
		t.Skipf("statx mount id unavailable: %v", err)
	}
	table := fmt.Sprintf("1 0 0:1 / / rw - ext4 /dev/root rw\n%d 1 0:2 / %s rw,nosuid shared:7 - %s %s rw\n",
		id+idDelta, strings.ReplaceAll(mountPoint, " ", `\040`), fstype, source)
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := mountInfoPath
	mountInfoPath = path
	t.Cleanup(func() { mountInfoPath = previous })
}

func allocateUnderRoot(t *testing.T, w worktreeRootWorld) (string, *fakeWorktreeManager, error) {
	t.Helper()
	store := openEngineStore(t)
	manager := &fakeWorktreeManager{}
	path, err := AllocateReadOnlyWorktree(context.Background(), store, w.home, "owner/repo", t.TempDir(), "job-1", "readonly-seat", 0, "main", 0, manager)
	return path, manager, err
}

func TestWorktreeRootAllocatesNewWorktreesOnTheVerifiedMount(t *testing.T) {
	w := newWorktreeRootWorld(t)
	path, manager, err := allocateUnderRoot(t, w)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	want := filepath.Join(w.root, "owner--repo", "delegations", "job-1", "readonly-seat")
	if path != want || len(manager.detachedCalls) != 1 || manager.detachedCalls[0].path != want {
		t.Fatalf("path = %q calls = %+v, want %q", path, manager.detachedCalls, want)
	}
	if info, err := os.Stat(filepath.Dir(want)); err != nil || !info.IsDir() {
		t.Fatalf("parent not prepared on the root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.home, "worktrees")); !os.IsNotExist(err) {
		t.Fatalf("allocation touched the local <home>/worktrees: %v", err)
	}
	// The worktree keeps a cleanup identity under the configured root.
	payload := JobPayload{Repo: "owner/repo", WorktreePath: path, ReadOnlyWorktree: true}
	if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", payload); err != nil {
		t.Fatalf("cleanup identity of a configured-root worktree: %v", err)
	}
}

// The core #2375 guarantee: an unprovable root refuses the job and creates nothing, anywhere.
func TestWorktreeRootRefusesWithoutFallingBackToTheLocalDisk(t *testing.T) {
	for name, breakIt := range map[string]func(t *testing.T, w worktreeRootWorld){
		"mount absent": func(t *testing.T, w worktreeRootWorld) {
			w.setMountTable(t, "/somewhere/else", "fuse.sshfs", "user@host:share", 0)
		},
		"wrong filesystem type": func(t *testing.T, w worktreeRootWorld) {
			w.setMountTable(t, w.mount, "ext4", "user@host:share", 0)
		},
		"wrong source": func(t *testing.T, w worktreeRootWorld) {
			w.setMountTable(t, w.mount, "fuse.sshfs", "other@host:share", 0)
		},
		"root not on that mount": func(t *testing.T, w worktreeRootWorld) {
			w.setMountTable(t, w.mount, "fuse.sshfs", "user@host:share", 1)
		},
		"root directory missing": func(t *testing.T, w worktreeRootWorld) {
			if err := os.Remove(w.root); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable config": func(t *testing.T, w worktreeRootWorld) {
			if err := os.WriteFile(filepath.Join(w.home, "config.toml"), []byte("[worktrees]\nroot = \"relative\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorktreeRootWorld(t)
			breakIt(t, w)
			path, manager, err := allocateUnderRoot(t, w)
			if !errors.Is(err, ErrWorktreeRootUnavailable) {
				t.Fatalf("allocate = %q, %v; want ErrWorktreeRootUnavailable", path, err)
			}
			if len(manager.detachedCalls) != 0 {
				t.Fatalf("git worktree add ran despite the refusal: %+v", manager.detachedCalls)
			}
			if _, err := os.Stat(filepath.Join(w.home, "worktrees")); !os.IsNotExist(err) {
				t.Fatalf("refusal fell back to <home>/worktrees: %v", err)
			}
			if _, err := os.Stat(filepath.Join(w.root, "owner--repo")); !os.IsNotExist(err) {
				t.Fatalf("refusal still created directories under the root: %v", err)
			}
		})
	}
}

// Existing worktrees are not moved: after [worktrees] is configured, a worktree recorded under
// <home>/worktrees keeps its cleanup identity, while a configured-root worktree is not cleanable
// (and so never reported as removed) while its mount is unavailable.
func TestWorktreeRootKeepsLegacyIdentityAndGuardsConfiguredCleanup(t *testing.T) {
	w := newWorktreeRootWorld(t)
	legacy, err := DelegationWorktreePath(w.home, "owner/repo", "job-1", "readonly-seat", 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := JobPayload{Repo: "owner/repo", WorktreePath: legacy, ReadOnlyWorktree: true}
	if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", payload); err != nil {
		t.Fatalf("legacy worktree lost its cleanup identity: %v", err)
	}
	configured := filepath.Join(w.root, "owner--repo", "delegations", "job-1", "readonly-seat")
	payload.WorktreePath = configured
	w.setMountTable(t, "/somewhere/else", "fuse.sshfs", "user@host:share", 0)
	if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", payload); !errors.Is(err, ErrWorktreeRootUnavailable) {
		t.Fatalf("cleanup of a configured-root worktree with the mount gone = %v, want ErrWorktreeRootUnavailable", err)
	}
	if err := PreflightWorktreePath(w.home, configured); !errors.Is(err, ErrWorktreeRootUnavailable) {
		t.Fatalf("reopen with the mount gone = %v, want ErrWorktreeRootUnavailable", err)
	}
	if err := PreflightWorktreePath(w.home, legacy); err != nil {
		t.Fatalf("a legacy worktree must not depend on the configured mount: %v", err)
	}
	// A path that matches neither root's derived identity is still refused.
	payload.WorktreePath = filepath.Join(w.mount, "elsewhere", "readonly-seat")
	if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", payload); err == nil {
		t.Fatal("an unmanaged path on the mount was accepted for cleanup")
	}
}

func TestReadMountInfoDecodesEscapes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	line := `36 25 0:32 / /mnt/my\040box rw,relatime shared:1 master:2 - fuse.sshfs u@h:dir\134x rw` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := readMountInfo(path)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if got := entries[0]; got.id != 36 || got.mountPoint != "/mnt/my box" || got.fsType != "fuse.sshfs" || got.source != `u@h:dir\x` {
		t.Fatalf("entry = %+v", got)
	}
	if err := os.WriteFile(path, []byte("36 25 0:32 / /mnt rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMountInfo(path); err == nil {
		t.Fatal("a line without the optional-field separator was accepted")
	}
}

func TestWorktreeStorageDeviceKeepsUnverifiedAndLocalMounts(t *testing.T) {
	w := newWorktreeRootWorld(t)
	placement, err := ResolveWorktreePlacement(w.home)
	if err != nil {
		t.Fatal(err)
	}
	cfg := placement.config
	for _, tc := range []struct {
		name, point, fstype, source string
		delta                       uint64
		wantNetwork                 bool
	}{
		{"verified", w.mount, "fuse.sshfs", cfg.MountSource, 0, true},
		{"missing", w.mount + "-absent", "fuse.sshfs", cfg.MountSource, 0, false},
		{"replaced", w.mount, "fuse.sshfs", cfg.MountSource, 1, false},
		{"wrong-source", w.mount, "fuse.sshfs", "other:share", 0, false},
		{"local-mount", w.mount, "ext4", "/dev/root", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.setMountTable(t, tc.point, tc.fstype, tc.source, tc.delta)
			_, network, err := WorktreeStorageDevice(filepath.Join(w.root, "not-created"), cfg)
			if err != nil || network != tc.wantNetwork {
				t.Fatalf("network=%v err=%v, want %v", network, err, tc.wantNetwork)
			}
		})
	}
	cfg.MountFSType = "ext4"
	cfg.MountSource = "/dev/root"
	w.setMountTable(t, w.mount, "ext4", "/dev/root", 0)
	if _, network, err := WorktreeStorageDevice(w.root, cfg); err != nil || network {
		t.Fatalf("configured and verified local mount exempted: network=%v err=%v", network, err)
	}
}

func TestWorktreeStoragePinsIdentityAcrossPathAndConfigReplacement(t *testing.T) {
	w := newWorktreeRootWorld(t)
	placement, err := ResolveWorktreePlacement(w.home)
	if err != nil {
		t.Fatal(err)
	}
	cfg := placement.config
	local, err := os.MkdirTemp("/dev/shm", "gm-device-")
	if err != nil {
		t.Skipf("separate local mount unavailable: %v", err)
	}
	defer os.RemoveAll(local)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(local, alias); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(alias, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var original unix.Stat_t
	if err := unix.Fstat(fd, &original); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(w.root, alias); err != nil {
		t.Fatal(err)
	}
	networkDev, network, err := WorktreeStorageDevice(alias, cfg)
	if err != nil || !network {
		t.Fatalf("positive control: network=%v err=%v", network, err)
	}
	if networkDev == uint64(original.Dev) {
		t.Skip("fixture requires distinct devices")
	}
	if worktreeNetworkFD(fd, cfg) {
		t.Fatal("path replacement retargeted a pinned LOCAL descriptor")
	}
	// Reloading the on-disk config cannot alter the snapshot used by this
	// admission, nor make the local descriptor match the network mount.
	w.writeConfig(t, w.root, "different:share", "fuse.sshfs")
	if worktreeNetworkFD(fd, cfg) {
		t.Fatal("config replacement exempted a pinned local path")
	}
	updated, err := ResolveWorktreePlacement(w.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, network, err := WorktreeStorageDevice(alias, updated.config); err != nil || network {
		t.Fatalf("new mismatched config did not fail closed: network=%v err=%v", network, err)
	}
	// An unrelated local path does not require cfg.Root to be accessible.
	cfg.Root = filepath.Join(alias, "unavailable-root")
	if dev, network, err := WorktreeStorageDevice(local, cfg); err != nil || network || dev != uint64(original.Dev) {
		t.Fatalf("local measurement depends on remote root: dev=%d network=%v err=%v", dev, network, err)
	}
}
