package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestEngineReclaimAgedFixCloneRefusesSameSizeSameMtimeReplacement is the
// forgeable-identity case: a writer replaces an inventoried file with unique
// SAME-SIZE content and restores the old mtime. A size+mtime check accepts that
// and deletes the new bytes; inode and inode-change time do not, and no
// unprivileged call can move ctime backwards.
//
// MUTATION PROOF: drop the ino/ctime comparison from sameProvedStat and every
// subtest deletes the replacement.
func TestEngineReclaimAgedFixCloneRefusesSameSizeSameMtimeReplacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(t *testing.T, target string, proved os.FileInfo)
	}{
		{
			name: "rewritten in place with the mtime restored",
			replace: func(t *testing.T, target string, proved os.FileInfo) {
				if err := os.WriteFile(target, []byte("UNIQUE\n"), 0o644); err != nil {
					t.Fatalf("rewrite proved file: %v", err)
				}
				if err := os.Chtimes(target, proved.ModTime(), proved.ModTime()); err != nil {
					t.Fatalf("restore mtime: %v", err)
				}
			},
		},
		{
			name: "swapped for a new inode with the mtime restored",
			replace: func(t *testing.T, target string, proved os.FileInfo) {
				replacement := target + ".new"
				if err := os.WriteFile(replacement, []byte("UNIQUE\n"), 0o644); err != nil {
					t.Fatalf("write replacement: %v", err)
				}
				if err := os.Rename(replacement, target); err != nil {
					t.Fatalf("swap replacement in: %v", err)
				}
				if err := os.Chtimes(target, proved.ModTime(), proved.ModTime()); err != nil {
					t.Fatalf("restore mtime: %v", err)
				}
			},
		},
		{
			name: "replaced by a symlink",
			replace: func(t *testing.T, target string, _ os.FileInfo) {
				if err := os.Remove(target); err != nil {
					t.Fatalf("remove proved file: %v", err)
				}
				if err := os.Symlink("/etc/hostname", target); err != nil {
					t.Fatalf("plant symlink: %v", err)
				}
			},
		},
		{
			name: "parent directory swapped for another directory",
			replace: func(t *testing.T, target string, _ os.FileInfo) {
				parent := filepath.Dir(target)
				decoy := parent + ".decoy"
				if err := os.MkdirAll(decoy, 0o755); err != nil {
					t.Fatalf("MkdirAll decoy parent: %v", err)
				}
				if err := os.WriteFile(filepath.Join(decoy, filepath.Base(target)), []byte("UNIQUE\n"), 0o644); err != nil {
					t.Fatalf("write decoy content: %v", err)
				}
				if err := os.RemoveAll(parent); err != nil {
					t.Fatalf("remove proved parent: %v", err)
				}
				if err := os.Rename(decoy, parent); err != nil {
					t.Fatalf("swap parent: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			home := t.TempDir()
			jobID := "fix-same-size-replacement"
			path, err := FixWorktreePath(home, "owner/repo", jobID)
			if err != nil {
				t.Fatalf("FixWorktreePath: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(path, "src"), 0o755); err != nil {
				t.Fatalf("MkdirAll fix worktree: %v", err)
			}
			if err := os.WriteFile(filepath.Join(path, "src", "tracked.txt"), []byte("proved\n"), 0o644); err != nil {
				t.Fatalf("write proved content: %v", err)
			}
			payload, err := marshalPayload(JobPayload{
				Repo: "owner/repo", Branch: "feature/fix", WorktreePath: path, FixWorktree: true,
			})
			if err != nil {
				t.Fatalf("marshalPayload: %v", err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID: jobID, Agent: "fixer", Type: "implement", State: string(JobSucceeded),
				Repo: "owner/repo", Payload: payload,
			}, db.JobEvent{Kind: string(JobSucceeded), Message: "seed"}); err != nil {
				t.Fatalf("CreateJobWithEvent: %v", err)
			}
			manager := &fakeWorktreeManager{cleanSet: true, clean: true}
			engine := testEngine(store)
			engine.Home = home
			engine.DelegationCheckout = t.TempDir()
			engine.DelegationWorktrees = manager
			// The interleave runs at the last seam before the unlink, after the scan
			// that took the inventory.
			replaced := false
			engine.WorktreeLiveness = func(probed string) (bool, bool) {
				if probed != path && !replaced && len(manager.cloneOnlyCalls) >= 2 {
					live := filepath.Join(probed, "src", "tracked.txt")
					proved, err := os.Lstat(live)
					if err != nil {
						t.Fatalf("stat proved file: %v", err)
					}
					tc.replace(t, live, proved)
					replaced = true
				}
				return false, true
			}

			reclaimed, err := engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, jobID, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatalf("ReclaimAgedTerminalDelegationWorktreeOutcome: %v", err)
			}
			if !replaced {
				t.Fatal("the interleaved replacement never ran")
			}
			if reclaimed {
				t.Fatal("a clone whose proved content was replaced was reclaimed")
			}
			if _, err := os.Lstat(filepath.Join(path, "src", "tracked.txt")); err != nil {
				t.Fatalf("the replacement was deleted: %v", err)
			}
		})
	}
}

// TestRemoveOwnedFenceRefusesAReplacementAfterClassification closes the prune's
// classify-to-unlink gap: the entry is re-proved through a descriptor and its
// inode compared again immediately before the unlinkat, so a name a writer swapped
// in between is never removed.
//
// MUTATION PROOF: unlink by path after the classification and the writer's file
// disappears.
func TestRemoveOwnedFenceRefusesAReplacementAfterClassification(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	fence := clone + fixCloneQuarantinePrefix + "aaaaaaaa"
	owned := NewFixCloneFenceOwnership([]string{fenceForTest(t, fence)})
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(fence, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if fences, err := FixCloneFences(clone, owned); err != nil || len(fences) != 1 {
		t.Fatalf("classification = %v (err %v), want the fence", fences, err)
	}

	// The writer takes the name after classification, keeping the old mtime so an
	// age-only recheck would still accept it.
	if err := os.Remove(fence); err != nil {
		t.Fatalf("remove fence: %v", err)
	}
	if err := os.WriteFile(fence, []byte("writer content that must survive\n"), 0o644); err != nil {
		t.Fatalf("plant replacement: %v", err)
	}
	if err := os.Chtimes(fence, past, past); err != nil {
		t.Fatalf("Chtimes replacement: %v", err)
	}

	pruned, err := PruneFixCloneFences(clone, time.Now().Add(-24*time.Hour), owned)
	if err != nil {
		t.Fatalf("PruneFixCloneFences: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("pruned %d entries, want 0: the replacement was deleted", pruned)
	}
	content, err := os.ReadFile(fence)
	if err != nil || !strings.Contains(string(content), "writer content that must survive") {
		t.Fatalf("replacement content = %q (err %v), want it intact", content, err)
	}
	survivors, err := FixCloneQuarantines(clone, owned)
	if err != nil || len(survivors) != 1 || survivors[0] != fence {
		t.Fatalf("survivors = %v (err %v), want the replacement classified as a survivor", survivors, err)
	}
}

// TestUnrecordedFenceShapedFileIsASurvivor pins the conservative upgrade path: a
// fence written before the durable record existed carries no registered nonce, so
// it is a survivor — never pruned, never counted as ours.
func TestUnrecordedFenceShapedFileIsASurvivor(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	legacy := clone + fixCloneQuarantinePrefix + "bbbbbbbb"
	if err := os.WriteFile(legacy, []byte(fixCloneFencePrefix+"deadbeef\n"), 0o444); err != nil {
		t.Fatalf("write legacy fence: %v", err)
	}
	past := time.Now().Add(-96 * time.Hour)
	if err := os.Chtimes(legacy, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	empty := FixCloneFenceOwnership{}
	if fences, err := FixCloneFences(clone, empty); err != nil || len(fences) != 0 {
		t.Fatalf("unrecorded fence counted as owned: %v (err %v)", fences, err)
	}
	survivors, err := FixCloneQuarantines(clone, empty)
	if err != nil || len(survivors) != 1 || survivors[0] != legacy {
		t.Fatalf("survivors = %v (err %v), want the unrecorded file", survivors, err)
	}
	if pruned, err := PruneFixCloneFences(clone, time.Now(), empty); err != nil || pruned != 0 {
		t.Fatalf("pruned %d unrecorded fences (err %v), want 0", pruned, err)
	}
	if _, err := os.Lstat(legacy); err != nil {
		t.Fatalf("unrecorded file was deleted: %v", err)
	}

	// A REGISTERED name whose file carries a different nonce: the writer copied the
	// public prefix but cannot produce the recorded value. Only comparing the nonce
	// separates this from an owned fence.
	registered := clone + fixCloneQuarantinePrefix + "cccccccc"
	if err := os.WriteFile(registered, []byte(fixCloneFencePrefix+"0000000000000000\n"), 0o444); err != nil {
		t.Fatalf("write mismatched fence: %v", err)
	}
	if err := os.Chtimes(registered, past, past); err != nil {
		t.Fatalf("Chtimes mismatched fence: %v", err)
	}
	claimed := NewFixCloneFenceOwnership([]string{registered + " ffffffffffffffff"})
	if fences, err := FixCloneFences(clone, claimed); err != nil || len(fences) != 0 {
		t.Fatalf("mismatched nonce counted as owned: %v (err %v)", fences, err)
	}
	if pruned, err := PruneFixCloneFences(clone, time.Now(), claimed); err != nil || pruned != 0 {
		t.Fatalf("pruned %d mismatched fences (err %v), want 0", pruned, err)
	}
	if _, err := os.Lstat(registered); err != nil {
		t.Fatalf("mismatched fence was deleted: %v", err)
	}
}

// TestPruneExpiredFixCloneFencesBoundedRotates pins the sweep's budget and
// rotation: one run stops at the budget and the next resumes where it stopped, so
// a host with more entries than the budget still reaches every repository without
// spending a whole tick in maintenance.
func TestPruneExpiredFixCloneFencesBoundedRotates(t *testing.T) {
	home := t.TempDir()
	records := []string{}
	fences := map[string]string{}
	for _, repo := range []string{"a--repo", "b--repo", "c--repo"} {
		fixes := filepath.Join(home, "worktrees", repo, "fixes")
		if err := os.MkdirAll(fixes, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		fence := filepath.Join(fixes, "job-1") + fixCloneQuarantinePrefix + "aaaaaaaa"
		records = append(records, fenceForTest(t, fence))
		past := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(fence, past, past); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		fences[repo] = fence
	}
	owned := NewFixCloneFenceOwnership(records)
	ownership := func(string) (FixCloneFenceOwnership, error) { return owned, nil }
	cutoff := time.Now().Add(-24 * time.Hour)

	pruned := 0
	for range len(fences) {
		count, scanned, err := PruneExpiredFixCloneFencesBounded(home, cutoff, 1, ownership)
		if err != nil {
			t.Fatalf("bounded sweep: %v", err)
		}
		if scanned == 0 {
			t.Fatal("a bounded run scanned nothing")
		}
		pruned += count
	}
	if pruned != len(fences) {
		t.Fatalf("bounded runs pruned %d fences across %d ticks, want %d", pruned, len(fences), len(fences))
	}
	for repo, fence := range fences {
		if _, err := os.Lstat(fence); !os.IsNotExist(err) {
			t.Fatalf("%s: fence survived the rotating sweep: %v", repo, err)
		}
	}
	if count, scanned, err := PruneExpiredFixCloneFencesBounded(home, cutoff, 0, ownership); err != nil || count != 0 || scanned != 0 {
		t.Fatalf("zero budget = (%d, %d, %v), want a no-op", count, scanned, err)
	}
}
