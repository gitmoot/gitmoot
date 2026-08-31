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
		// A userland process: PF_KTHREAD (0x00200000) clear in stat field 9.
		const stat = "999999998 (agent) S 1 0 0 0 -1 4194560 0\n"
		if err := os.WriteFile(filepath.Join(processDir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatalf("WriteFile stat: %v", err)
		}
		live, known := worktreeLiveness(worktree, procRoot)
		if live || known {
			t.Fatalf("worktreeLiveness with unreadable cwd = (%v, %v), want (false, false)", live, known)
		}
		live, known = bestEffortWorktreeLiveness(worktree, procRoot)
		if live || !known {
			t.Fatalf("best-effort liveness with unreadable foreign cwd = (%v, %v), want (false, true)", live, known)
		}
	})

	t.Run("process that exits during the probe is irrelevant", func(t *testing.T) {
		worktree := t.TempDir()
		procRoot := t.TempDir()
		processDir := filepath.Join(procRoot, "999999996")
		if err := os.Mkdir(processDir, 0o755); err != nil {
			t.Fatalf("Mkdir process dir: %v", err)
		}
		// cwd is unreadable and stat is already gone: the process exited between the
		// directory listing and the probe, so it holds no cwd anywhere.
		if err := os.WriteFile(filepath.Join(processDir, "cwd"), []byte("not a symlink"), 0o644); err != nil {
			t.Fatalf("WriteFile cwd: %v", err)
		}
		live, known := worktreeLiveness(worktree, procRoot)
		if live || !known {
			t.Fatalf("worktreeLiveness with an exited process = (%v, %v), want (false, true)", live, known)
		}
	})

	t.Run("unreadable kernel-thread cwd is safely irrelevant", func(t *testing.T) {
		worktree := t.TempDir()
		procRoot := t.TempDir()
		processDir := filepath.Join(procRoot, "999999997")
		if err := os.Mkdir(processDir, 0o755); err != nil {
			t.Fatalf("Mkdir process dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(processDir, "cwd"), []byte("not a symlink"), 0o644); err != nil {
			t.Fatalf("WriteFile cwd: %v", err)
		}
		const stat = "999999997 (kworker/0:0) I 0 0 0 0 0 2097152 0\n"
		if err := os.WriteFile(filepath.Join(processDir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatalf("WriteFile stat: %v", err)
		}
		live, known := worktreeLiveness(worktree, procRoot)
		if live || !known {
			t.Fatalf("worktreeLiveness with unreadable kernel-thread cwd = (%v, %v), want (false, true)", live, known)
		}
	})

	t.Run("live cwd wins after an unreadable process", func(t *testing.T) {
		worktree := t.TempDir()
		procRoot := t.TempDir()
		unreadableDir := filepath.Join(procRoot, "100000001")
		liveDir := filepath.Join(procRoot, "200000002")
		if err := os.Mkdir(unreadableDir, 0o755); err != nil {
			t.Fatalf("Mkdir unreadable process dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(unreadableDir, "cwd"), []byte("not a symlink"), 0o644); err != nil {
			t.Fatalf("WriteFile unreadable cwd: %v", err)
		}
		if err := os.Mkdir(liveDir, 0o755); err != nil {
			t.Fatalf("Mkdir live process dir: %v", err)
		}
		if err := os.Symlink(worktree, filepath.Join(liveDir, "cwd")); err != nil {
			t.Fatalf("Symlink live cwd: %v", err)
		}
		live, known := worktreeLiveness(worktree, procRoot)
		if !live || !known {
			t.Fatalf("worktreeLiveness with later matching cwd = (%v, %v), want (true, true)", live, known)
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
