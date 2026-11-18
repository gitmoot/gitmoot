package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// ownScopeRepos is a real origin and a daemon checkout cloned from it before
// the base branch moved. The pull request branch `feature` lives on the origin
// and reaches the checkout only through `git fetch origin feature`, so the
// checkout's origin/main stays at the fork point until a scope refreshes it,
// exactly as a checkout that fetched the PR ref alone would.
type ownScopeRepos struct {
	origin, checkout string
	forkPoint, prev  string
}

func newOwnScopeRepos(t *testing.T) ownScopeRepos {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := ownScopeRepos{origin: t.TempDir(), checkout: t.TempDir()}
	runGit(t, r.origin, "init", "-q", "-b", "main")
	runGit(t, r.origin, "config", "user.email", "gitmoot@example.com")
	runGit(t, r.origin, "config", "user.name", "Gitmoot")
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(nil))
	writeFile(t, filepath.Join(r.origin, "README.md"), "base\n")
	r.forkPoint = r.commit(t, "base")
	runGit(t, r.checkout, "clone", "-q", r.origin, r.checkout)
	runGit(t, r.origin, "switch", "-q", "-c", "feature")
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(map[int]string{2: "own one"}))
	r.prev = r.commit(t, "own change to y")
	return r
}

// ownScopeLines renders y.go: twelve numbered lines with some replaced.
func ownScopeLines(replace map[int]string) string {
	var b strings.Builder
	for line := 1; line <= 12; line++ {
		if text, ok := replace[line]; ok {
			b.WriteString(text + "\n")
			continue
		}
		fmt.Fprintf(&b, "line %d\n", line)
	}
	return b.String()
}

func (r ownScopeRepos) commit(t *testing.T, message string) string {
	t.Helper()
	runGit(t, r.origin, "add", "-A")
	runGit(t, r.origin, "commit", "-q", "-m", message)
	return strings.TrimSpace(runGitOutput(t, r.origin, "rev-parse", "HEAD"))
}

// advanceBase lands another PR on main: it adds x.go and edits y.go far from
// the pull request's own line. It returns the new main tip.
func (r ownScopeRepos) advanceBase(t *testing.T) string {
	t.Helper()
	runGit(t, r.origin, "switch", "-q", "main")
	writeFile(t, filepath.Join(r.origin, "x.go"), "package x\n")
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(map[int]string{11: "base eleven"}))
	tip := r.commit(t, "another pull request")
	runGit(t, r.origin, "switch", "-q", "feature")
	return tip
}

// mergeBase merges main into the pull request branch, as a seat does.
func (r ownScopeRepos) mergeBase(t *testing.T) string {
	t.Helper()
	runGit(t, r.origin, "merge", "-q", "--no-edit", "main")
	return strings.TrimSpace(runGitOutput(t, r.origin, "rev-parse", "HEAD"))
}

// sync fetches the pull request branch alone into the checkout.
func (r ownScopeRepos) sync(t *testing.T) {
	t.Helper()
	runGit(t, r.checkout, "fetch", "-q", "origin", "feature")
}

func (r ownScopeRepos) scope(t *testing.T, prev, head string) workflow.ReviewScopeDiff {
	t.Helper()
	r.sync(t)
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, &reviewCompareClient{}, r.checkout, subprocess.ExecRunner{})
	diff, err := localReviewScopeDiff(context.Background(), gitutil.NewHostClient(r.checkout),
		&reviewScopeSubject{baseBranch: "main", number: 17}, prev, head)
	if err != nil {
		t.Fatalf("localReviewScopeDiff(%s, %s): %v", prev, head, err)
	}
	// The engine seam must answer the same from the same checkout, with no API.
	viaSeam, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", prev, head)
	if err != nil {
		t.Fatalf("engine seam: %v", err)
	}
	if !slices.Equal(viaSeam.ChangedFiles, diff.ChangedFiles) || viaSeam.MergeBase != diff.MergeBase {
		t.Fatalf("engine seam scope %v from %s differs from the local resolver's %v from %s", viaSeam.ChangedFiles, viaSeam.MergeBase, diff.ChangedFiles, diff.MergeBase)
	}
	return diff
}

func rawRoundFiles(t *testing.T, checkout, prev, head string) []string {
	t.Helper()
	return strings.Fields(runGitOutput(t, checkout, "diff", "--name-only", prev+"..."+head))
}

// #2682 (a). The base gains x.go, the pull request merges the base in and also
// changes its own y.go again. The round's raw range names both files; the scope
// names only y.go, read from the NEW merge base, which the scope learns only by
// refreshing the checkout's stale origin/main.
func TestReviewScopeExcludesFilesABaseMergeBroughtIn(t *testing.T) {
	r := newOwnScopeRepos(t)
	tip := r.advanceBase(t)
	r.mergeBase(t)
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(map[int]string{2: "own two", 11: "base eleven"}))
	head := r.commit(t, "own change to y again")

	diff := r.scope(t, r.prev, head)
	if raw := rawRoundFiles(t, r.checkout, r.prev, head); !slices.Contains(raw, "x.go") {
		t.Fatalf("fixture: the raw round %v does not carry the base's x.go, so this test proves nothing", raw)
	}
	if !slices.Equal(diff.ChangedFiles, []string{"y.go"}) {
		t.Fatalf("scope = %v, want [y.go]: x.go arrived with the base merge and is not this pull request's change", diff.ChangedFiles)
	}
	if diff.MergeBase != tip {
		t.Fatalf("merge base = %s, want the new merge base %s (fork point was %s)", diff.MergeBase, tip, r.forkPoint)
	}
	if !slices.Equal(diff.OwnFiles, []string{"y.go"}) {
		t.Fatalf("own files = %v, want [y.go]", diff.OwnFiles)
	}
}

// #2682 (b). With no base merge between the rounds the scope is exactly the
// round's own range, renames reported by their new path as before.
func TestReviewScopeWithoutABaseMergeIsTheRoundRange(t *testing.T) {
	r := newOwnScopeRepos(t)
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(map[int]string{2: "own two"}))
	writeFile(t, filepath.Join(r.origin, "w.go"), "package w\n")
	runGit(t, r.origin, "mv", "README.md", "README.txt")
	head := r.commit(t, "second round")

	diff := r.scope(t, r.prev, head)
	today := sortedUniqueReviewPaths(rawNameStatusPaths(t, r.checkout, r.prev, head))
	if !slices.Equal(diff.ChangedFiles, today) {
		t.Fatalf("scope = %v, want today's prev...head list %v", diff.ChangedFiles, today)
	}
	if !slices.Equal(today, []string{"README.txt", "w.go", "y.go"}) {
		t.Fatalf("fixture: today's list = %v", today)
	}
	if diff.MergeBase != r.forkPoint {
		t.Fatalf("merge base = %s, want the fork point %s", diff.MergeBase, r.forkPoint)
	}
}

func rawNameStatusPaths(t *testing.T, checkout, prev, head string) []string {
	t.Helper()
	paths, err := gitutil.NewHostClient(checkout).ChangedFiles(context.Background(), prev, head)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// #2682 (c). y.go moved between the rounds only because the base merge edited
// it far from the pull request's own line: its own patch is the same, so it is
// not in scope. z.go, the round's one own change, is.
func TestReviewScopeExcludesAFileMovedOnlyByTheBaseMerge(t *testing.T) {
	r := newOwnScopeRepos(t)
	r.advanceBase(t)
	r.mergeBase(t)
	writeFile(t, filepath.Join(r.origin, "z.go"), "package z\n")
	head := r.commit(t, "own z")

	diff := r.scope(t, r.prev, head)
	if raw := rawRoundFiles(t, r.checkout, r.prev, head); !slices.Contains(raw, "y.go") {
		t.Fatalf("fixture: y.go did not move in the raw round %v", raw)
	}
	if !slices.Equal(diff.ChangedFiles, []string{"z.go"}) {
		t.Fatalf("scope = %v, want [z.go]: y.go's own patch did not change", diff.ChangedFiles)
	}
	if !slices.Equal(diff.OwnFiles, []string{"y.go", "z.go"}) {
		t.Fatalf("own files = %v, want y.go and z.go", diff.OwnFiles)
	}
}

// An own edit whose neighbouring base lines moved stays in scope: the context
// the reviewer reads it in changed.
func TestReviewScopeKeepsAnOwnEditWhoseContextTheBaseMoved(t *testing.T) {
	r := newOwnScopeRepos(t)
	runGit(t, r.origin, "switch", "-q", "main")
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(map[int]string{5: "base five"}))
	r.commit(t, "base edits next to the own line")
	runGit(t, r.origin, "switch", "-q", "feature")
	head := r.mergeBase(t)

	diff := r.scope(t, r.prev, head)
	if !slices.Equal(diff.ChangedFiles, []string{"y.go"}) {
		t.Fatalf("scope = %v, want [y.go]: its own hunk now reads against different context", diff.ChangedFiles)
	}
}

// #2682 (d). The round that reviewed the base merge reported findings in x.go
// (the base's file) and on y.go line 11 (the base's line); neither is in the
// pull request's own diff, so neither is carried. The finding on y.go line 2,
// the pull request's own line, is.
func TestReviewScopeDropsPriorFindingsOutsideTheOwnDiff(t *testing.T) {
	r := newOwnScopeRepos(t)
	r.advanceBase(t)
	merged := r.mergeBase(t)
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(map[int]string{2: "own two", 11: "base eleven"}))
	head := r.commit(t, "fix the own finding")

	diff := r.scope(t, merged, head)
	inX := `{"severity":"P2","title":"x is wrong","file":"x.go","line":1}`
	onBaseLine := `{"severity":"P2","title":"base line","location":"y.go:11"}`
	onOwnLine := `{"severity":"P2","title":"own line","file":"y.go","line":2}`
	kept, outside := workflow.SplitOutsidePRFindings([]string{inX, onBaseLine, onOwnLine}, diff)
	if !slices.Equal(outside, []string{inX, onBaseLine}) {
		t.Fatalf("outside-PR findings = %q, want the x.go and y.go:11 findings", outside)
	}
	if !slices.Equal(kept, []string{onOwnLine}) {
		t.Fatalf("carried findings = %q, want only the own-line finding", kept)
	}
}

// #2682 merged case. A pull request merged with a merge commit has its head
// inside main, so merge-base(origin/main, head) is the head itself. Both ends
// are then measured from the merge commit's first parent, as #2327 does for a
// full review, and the scope is the pull request's own change, not empty.
func TestReviewScopeOfAMergedPullRequestReadsFromTheLandingBase(t *testing.T) {
	r := newOwnScopeRepos(t)
	tip := r.advanceBase(t)
	writeFile(t, filepath.Join(r.origin, "y.go"), ownScopeLines(map[int]string{2: "own two"}))
	head := r.commit(t, "own change to y again")
	runGit(t, r.origin, "switch", "-q", "main")
	runGit(t, r.origin, "merge", "-q", "--no-ff", "-m", "Merge PR #17", "feature")
	mergeSHA := strings.TrimSpace(runGitOutput(t, r.origin, "rev-parse", "HEAD"))
	runGit(t, r.origin, "switch", "-q", "feature")
	r.sync(t)

	pull := github.PullRequest{Number: 17, BaseRef: "main", HeadSHA: head, Merged: true, MergeSHA: mergeSHA}
	diff, err := localReviewScopeDiff(context.Background(), gitutil.NewHostClient(r.checkout),
		&reviewScopeSubject{baseBranch: "main", number: 17, pull: &pull}, r.prev, head)
	if err != nil {
		t.Fatalf("localReviewScopeDiff: %v", err)
	}
	if diff.MergeBase != r.forkPoint {
		t.Fatalf("merge base = %s, want the fork point %s (main tip %s)", diff.MergeBase, r.forkPoint, tip)
	}
	if !slices.Equal(diff.ChangedFiles, []string{"y.go"}) || !slices.Equal(diff.OwnFiles, []string{"y.go"}) {
		t.Fatalf("scope = %v, own %v, want [y.go] for both", diff.ChangedFiles, diff.OwnFiles)
	}
}

// #2682 sandbox base. A scoped follow-up hands the sandbox the pull request's
// merge base frozen in its scope, the commit its brief names, never the prior
// head.
func TestRemoteReviewDiffBaseHEADUsesTheScopeMergeBase(t *testing.T) {
	r := newOwnScopeRepos(t)
	tip := r.advanceBase(t)
	head := r.mergeBase(t)
	r.sync(t)
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", r.checkout)
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature", PullRequest: 17, HeadSHA: head,
		ReviewScope: &workflow.ReviewScope{PreviousHeadSHA: r.prev, MergeBase: tip},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	base, err := worker.remoteReviewDiffBaseHEAD(context.Background(), db.Job{ID: "scoped", Agent: "reviewer", Type: "review", Payload: string(payload)}, r.checkout)
	if err != nil {
		t.Fatalf("remoteReviewDiffBaseHEAD: %v", err)
	}
	if base != tip {
		t.Fatalf("sandbox diff base = %s, want the scope's merge base %s, not the prior head %s", base, tip, r.prev)
	}
}

func TestNormalizedOwnPatchIgnoresWhatABaseMergeShifts(t *testing.T) {
	before := "diff --git a/y.go b/y.go\nindex 1111111..2222222 100644\n--- a/y.go\n+++ b/y.go\n@@ -1,4 +1,4 @@ func A()\n line 1\n-line 2\n+own\n line 3\n"
	after := "diff --git a/y.go b/y.go\nindex 3333333..4444444 100644\n--- a/y.go\n+++ b/y.go\n@@ -9,4 +9,4 @@ func B()\n line 1\n-line 2\n+own\n line 3\n"
	if normalizedOwnPatch(before) != normalizedOwnPatch(after) {
		t.Fatalf("same own edit at shifted lines normalizes differently:\n%q\n%q", normalizedOwnPatch(before), normalizedOwnPatch(after))
	}
	changed := strings.Replace(after, "+own", "+other", 1)
	if normalizedOwnPatch(before) == normalizedOwnPatch(changed) {
		t.Fatal("a different own edit normalizes the same")
	}
	binaryA := "diff --git a/b.png b/b.png\nindex 1111111..2222222 100644\nBinary files a/b.png and b/b.png differ\n"
	binaryB := "diff --git a/b.png b/b.png\nindex 1111111..3333333 100644\nBinary files a/b.png and b/b.png differ\n"
	if normalizedOwnPatch(binaryA) == normalizedOwnPatch(binaryB) {
		t.Fatal("two different binary changes normalize the same")
	}
	if got := patchLineRanges(after); len(got) != 1 || got[0] != (workflow.ReviewLineRange{Start: 9, End: 12}) {
		t.Fatalf("line ranges = %v, want 9-12", got)
	}
	if got := patchLineRanges(binaryA); got != nil {
		t.Fatalf("binary line ranges = %v, want nil", got)
	}
}
