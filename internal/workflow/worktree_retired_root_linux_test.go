//go:build linux

package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// retiredRootWorld is the #2763 move: the old sshfs root (w.root on the synthetic mount w.mount)
// is retired, and [worktrees] now points at a local directory, or is absent.
func retiredRootWorld(t *testing.T, withCurrent bool) (worktreeRootWorld, string) {
	t.Helper()
	w := newWorktreeRootWorld(t)
	current := filepath.Join(filepath.Dir(w.home), "ssd", "worktrees")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[[worktrees.retired]]\nroot = %q\nmount_point = %q\nmount_source = \"user@host:share\"\nmount_fstype = \"fuse.sshfs\"\n", w.root, w.mount)
	if withCurrent {
		cfg = fmt.Sprintf("[worktrees]\nroot = %q\n\n", current) + cfg
	}
	if err := os.WriteFile(filepath.Join(w.home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return w, current
}

// A worktree created under the old root keeps its cleanup identity after the root moved; a path
// under an unrelated root is still refused.
func TestRetiredWorktreeRootKeepsCleanupIdentity(t *testing.T) {
	w, _ := retiredRootWorld(t, true)
	readOnly := JobPayload{Repo: "owner/repo", WorktreePath: filepath.Join(w.root, "owner--repo", "delegations", "job-1", "readonly-seat"), ReadOnlyWorktree: true}
	if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", readOnly); err != nil {
		t.Fatalf("read-only worktree under the retired root lost its cleanup identity: %v", err)
	}
	child := JobPayload{
		Repo: "owner/repo", ParentJobID: "parent", DelegationID: "leg",
		WorktreePath: filepath.Join(w.root, "owner--repo", "delegations", "parent", "leg"),
	}
	if err := ValidateDelegationCleanupTarget(w.home, "parent/delegation/leg", "implement", child); err != nil {
		t.Fatalf("delegation worktree under the retired root lost its cleanup identity: %v", err)
	}
	if err := PreflightWorktreePath(w.home, readOnly.WorktreePath); err != nil {
		t.Fatalf("reopen under an available retired root: %v", err)
	}
	unrelated := filepath.Join(t.TempDir(), "worktrees")
	readOnly.WorktreePath = filepath.Join(unrelated, "owner--repo", "delegations", "job-1", "readonly-seat")
	if err := os.MkdirAll(readOnly.WorktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", readOnly); err == nil {
		t.Fatal("a worktree under an unrelated root was accepted for cleanup")
	}
}

// An unavailable retired root is an availability error (retryable), never "not ours" or gone.
func TestRetiredWorktreeRootUnavailableRefusesCleanup(t *testing.T) {
	for name, breakIt := range map[string]func(t *testing.T, w worktreeRootWorld){
		"mount absent": func(t *testing.T, w worktreeRootWorld) {
			w.setMountTable(t, "/somewhere/else", "fuse.sshfs", "user@host:share", 0)
		},
		"root not on that mount": func(t *testing.T, w worktreeRootWorld) {
			w.setMountTable(t, w.mount, "fuse.sshfs", "user@host:share", 1)
		},
		"root directory missing, no mount keys": func(t *testing.T, w worktreeRootWorld) {
			cfg := fmt.Sprintf("[[worktrees.retired]]\nroot = %q\n", w.root)
			if err := os.WriteFile(filepath.Join(w.home, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(w.root); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, _ := retiredRootWorld(t, true)
			breakIt(t, w)
			path := filepath.Join(w.root, "owner--repo", "delegations", "job-1", "readonly-seat")
			payload := JobPayload{Repo: "owner/repo", WorktreePath: path, ReadOnlyWorktree: true}
			if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", payload); !errors.Is(err, ErrWorktreeRootUnavailable) {
				t.Fatalf("cleanup under an unavailable retired root = %v, want ErrWorktreeRootUnavailable", err)
			}
			if err := PreflightWorktreePath(w.home, path); !errors.Is(err, ErrWorktreeRootUnavailable) {
				t.Fatalf("reopen under an unavailable retired root = %v, want ErrWorktreeRootUnavailable", err)
			}
		})
	}
}

// New worktrees never go to a retired root: they go to the current root, or to <home>/worktrees
// when only retired roots are listed, even while the retired mount is gone. The retired root
// still identifies the worktrees already under it.
func TestRetiredWorktreeRootNeverAllocates(t *testing.T) {
	for name, withCurrent := range map[string]bool{"current root configured": true, "retired roots only": false} {
		t.Run(name, func(t *testing.T) {
			w, current := retiredRootWorld(t, withCurrent)
			wantRoot := current
			if !withCurrent {
				wantRoot = filepath.Join(w.home, "worktrees")
			}
			retiredPath := filepath.Join(w.root, "owner--repo", "delegations", "job-1", "readonly-seat")
			payload := JobPayload{Repo: "owner/repo", WorktreePath: retiredPath, ReadOnlyWorktree: true}
			if err := ValidateDelegationCleanupTarget(w.home, "job-1", "review", payload); err != nil {
				t.Fatalf("retired root does not identify its existing worktree: %v", err)
			}
			w.setMountTable(t, "/somewhere/else", "fuse.sshfs", "user@host:share", 0)
			path, manager, err := allocateUnderRoot(t, w)
			if err != nil {
				t.Fatalf("allocate with the retired mount gone: %v", err)
			}
			want := filepath.Join(wantRoot, "owner--repo", "delegations", "job-1", "readonly-seat")
			if path != want || len(manager.detachedCalls) != 1 || manager.detachedCalls[0].path != want {
				t.Fatalf("path = %q calls = %+v, want %q", path, manager.detachedCalls, want)
			}
			if _, err := os.Stat(filepath.Join(w.root, "owner--repo")); !os.IsNotExist(err) {
				t.Fatalf("allocation touched the retired root: %v", err)
			}
		})
	}
}
