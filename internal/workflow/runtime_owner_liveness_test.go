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

func TestWorktreeOpenFileLivenessDetectsWriterHandle(t *testing.T) {
	worktree := t.TempDir()
	procRoot := t.TempDir()
	processDir := filepath.Join(procRoot, "999999995")
	fdDir := filepath.Join(processDir, "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll fd directory: %v", err)
	}
	target := filepath.Join(worktree, "open-output")
	if err := os.Symlink(target, filepath.Join(fdDir, "3")); err != nil {
		t.Fatalf("Symlink writer fd: %v", err)
	}
	live, known := worktreeOpenFileLiveness(worktree, procRoot)
	if !live || !known {
		t.Fatalf("open writer fd liveness = (%v, %v), want (true, true)", live, known)
	}
}

// The boolean seam is an injection point: its answer must be taken verbatim, in
// both directions, so a wired test or recovery caller does not silently depend on
// the host process table. The strict scan is what production gets.
func TestWorktreeLivenessBooleanSeamIsAuthoritative(t *testing.T) {
	worktree := t.TempDir()
	for _, tc := range []struct {
		name string
		live bool
	}{
		{name: "live", live: true},
		{name: "not live", live: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := Engine{WorktreeHasLiveProcess: func(string) bool { return tc.live }}
			live, known := engine.worktreeLiveness(worktree)
			if live != tc.live || !known {
				t.Fatalf("worktreeLiveness = (%v, %v), want (%v, true)", live, known, tc.live)
			}
		})
	}
	// Compare against the STRICT scan, which is what the unwired engine calls.
	// The exported WorktreeLiveness is the best-effort variant and reports
	// known=true where the strict scan reports inconclusive, so comparing against
	// it tests two different functions and fails on any host where they disagree.
	wantLive, wantKnown := strictWorktreeLiveness(worktree)
	if live, known := (Engine{}).worktreeLiveness(worktree); live != wantLive || known != wantKnown {
		t.Fatalf("unwired engine = (%v, %v), want the strict scan's (%v, %v)", live, known, wantLive, wantKnown)
	}
}
