package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreeLiveness(t *testing.T) {
	t.Run("unknown when proc scan cannot start", func(t *testing.T) {
		missingProc := filepath.Join(t.TempDir(), "missing-proc")
		live, known := worktreeLiveness(t.TempDir(), missingProc)
		if live || known {
			t.Fatalf("worktreeLiveness with unreadable proc = (%v, %v), want (false, false)", live, known)
		}
	})

	t.Run("unknown when a process cwd is unreadable", func(t *testing.T) {
		worktree := t.TempDir()
		procRoot := t.TempDir()
		processDir := filepath.Join(procRoot, "999999998")
		if err := os.Mkdir(processDir, 0o755); err != nil {
			t.Fatalf("Mkdir process dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(processDir, "cwd"), []byte("not a symlink"), 0o644); err != nil {
			t.Fatalf("WriteFile cwd: %v", err)
		}
		live, known := worktreeLiveness(worktree, procRoot)
		if live || known {
			t.Fatalf("worktreeLiveness with unreadable cwd = (%v, %v), want (false, false)", live, known)
		}
	})

	t.Run("known live cwd", func(t *testing.T) {
		worktree := t.TempDir()
		procRoot := t.TempDir()
		processDir := filepath.Join(procRoot, "999999999")
		if err := os.Mkdir(processDir, 0o755); err != nil {
			t.Fatalf("Mkdir process dir: %v", err)
		}
		if err := os.Symlink(worktree, filepath.Join(processDir, "cwd")); err != nil {
			t.Fatalf("Symlink cwd: %v", err)
		}
		live, known := worktreeLiveness(worktree, procRoot)
		if !live || !known {
			t.Fatalf("worktreeLiveness with matching cwd = (%v, %v), want (true, true)", live, known)
		}
	})

	t.Run("legacy bool wrapper ignores certainty", func(t *testing.T) {
		worktree := t.TempDir()
		live, known := WorktreeLiveness(worktree)
		if !known {
			t.Skip("host process table is not readable")
		}
		if got := WorktreeHasLiveProcess(worktree); got != live {
			t.Fatalf("WorktreeHasLiveProcess = %v, WorktreeLiveness live = %v", got, live)
		}
	})
}
