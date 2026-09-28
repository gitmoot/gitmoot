package workflow

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewFindingsLogBriefNamesTheFileTheSalvageReads is the contract that
// makes #2224 work in production rather than in principle.
//
// The daemon salvages a dead review's findings from a file whose name only the
// daemon knows. If the brief names a different file, the reviewer writes
// somewhere nobody reads and the whole feature is inert — which was the state
// before this change, since no template wrote a log at all.
//
// MUTATION: change either the brief's filename or findingsLogName and this goes
// red.
func TestReviewFindingsLogBriefNamesTheFileTheSalvageReads(t *testing.T) {
	brief := ReviewFindingsLogBrief()
	if !strings.Contains(brief, FindingsLogName()) {
		t.Fatalf("brief does not name %q, so a reviewer would write where the salvage does not read:\n%s",
			FindingsLogName(), brief)
	}

	// The salvage resolves the same name inside the worktree it reads.
	worktree := "/tmp/example-worktree"
	if got, want := FindingsLogPath(worktree), filepath.Join(worktree, FindingsLogName()); got != want {
		t.Fatalf("FindingsLogPath = %q, want %q: the brief and the reader must agree", got, want)
	}
}

// TestReviewFindingsLogBriefAsksForIncrementalAppends pins the property that
// makes the log worth writing at all.
//
// A reviewer that writes the log only at the end has written nothing a crash can
// recover — it would be a second copy of the envelope it already returns. The
// brief has to ask for appends AS THE WORK PROCEEDS.
func TestReviewFindingsLogBriefAsksForIncrementalAppends(t *testing.T) {
	brief := strings.ToLower(ReviewFindingsLogBrief())
	for _, required := range []string{"as you go", "append"} {
		if !strings.Contains(brief, required) {
			t.Fatalf("brief does not ask for %q; a log written only at the end recovers nothing:\n%s", required, brief)
		}
	}
}

// TestReviewFindingsLogBriefRefusesToImplyAVerdict pins the safety wording.
//
// A salvaged partial is recorded with decision "failed" and satisfies no merge
// gate. If the brief let a reviewer believe the log could stand in for its
// envelope, a reviewer might skip the envelope — turning an insurance policy
// into a way to approve nothing while appearing to have reviewed.
func TestReviewFindingsLogBriefRefusesToImplyAVerdict(t *testing.T) {
	brief := ReviewFindingsLogBrief()
	lower := strings.ToLower(brief)
	if !strings.Contains(lower, "does not replace") {
		t.Fatalf("brief does not say the log does not replace the result:\n%s", brief)
	}
	if !strings.Contains(lower, "merge gate") {
		t.Fatalf("brief does not say the log satisfies no merge gate:\n%s", brief)
	}
}
