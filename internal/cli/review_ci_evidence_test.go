package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type fakeCheckLister struct {
	checks []github.PullRequestCheck
	err    error
	calls  int
	ref    string
}

func (f *fakeCheckLister) ListCheckRunsForRef(_ context.Context, _ github.Repository, ref string) ([]github.PullRequestCheck, error) {
	f.calls++
	f.ref = ref
	return f.checks, f.err
}

var ciEvidenceRepo = github.Repository{Owner: "gitmoot", Name: "gitmoot"}

// A green head is the case the slice exists for: the reviewer must be told the
// suite is already established AND told to keep the control its own mutation
// runs need. Asserting both, because a block that only said "do not run tests"
// would cut the verification #1824 explicitly protects.
func TestReviewCIEvidenceReportsAnEstablishedTreeAndStillDemandsAControl(t *testing.T) {
	lister := &fakeCheckLister{checks: []github.PullRequestCheck{
		{Name: "build / vet / test", Bucket: "pass"},
		{Name: "race (internal/cli, shard 0/8)", State: "success"},
		{Name: "classify diff", Bucket: "skipping"},
	}}
	out := dispatchReviewCIEvidence(context.Background(), lister, ciEvidenceRepo, "0123456789abcdef0123456789abcdef01234567")
	if out == "" {
		t.Fatal("a fully green head produced no evidence block")
	}
	if lister.ref != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("checks were read for ref %q, not the dispatch head", lister.ref)
	}
	for _, want := range []string{"3 check runs", "3 successful", "ALREADY established", "Do not re-run the full suite"} {
		if !strings.Contains(out, want) {
			t.Fatalf("green block missing %q:\n%s", want, out)
		}
	}
	// The half that protects verification. #1824: "that work should not be cut."
	for _, want := range []string{"baseline your own verification", "mutation run is worthless without one", "what CI cannot do"} {
		if !strings.Contains(out, want) {
			t.Fatalf("green block does not preserve the reviewer's own control (%q):\n%s", want, out)
		}
	}
	// A skipped conditional job counts as passed, or a repo with a docs-only
	// path would never read as established. Same rule the merge gate applies.
	if strings.Contains(out, "not successful.") && !strings.Contains(out, "0 not successful.") {
		t.Fatalf("a skipping check was counted as a failure:\n%s", out)
	}
}

// A FAILING head must never be described as green, and the reviewer must not be
// invited to override it with a local pass: CI builds the branch merged into its
// base and a local run does not.
func TestReviewCIEvidenceRefusesToCallAFailingHeadEstablished(t *testing.T) {
	lister := &fakeCheckLister{checks: []github.PullRequestCheck{
		{Name: "build / vet / test", Bucket: "fail"},
		{Name: "race (internal/db, shard 1/2)", Bucket: "pass"},
	}}
	out := dispatchReviewCIEvidence(context.Background(), lister, ciEvidenceRepo, "aaaabbbbccccddddeeeeffff0000111122223333")
	if out == "" {
		t.Fatal("a failing head produced no evidence block")
	}
	if strings.Contains(out, "ALREADY established") || strings.Contains(out, "Do not re-run the full suite") {
		t.Fatalf("a failing head was described as established:\n%s", out)
	}
	for _, want := range []string{"NOT green", "build / vet / test", "Do not report this tree as green"} {
		if !strings.Contains(out, want) {
			t.Fatalf("failing block missing %q:\n%s", want, out)
		}
	}
}

// PENDING IS NOT GREEN AND NOT FAILING. An empty-or-incomplete rollup right
// after a push means "not started", never "not required" - the #1783 lesson.
func TestReviewCIEvidenceTreatsPendingAsNoEvidence(t *testing.T) {
	lister := &fakeCheckLister{checks: []github.PullRequestCheck{
		{Name: "build / vet / test", Bucket: "pending"},
		{Name: "race (internal/workflow, shard 0/4)", Bucket: "pass"},
	}}
	out := dispatchReviewCIEvidence(context.Background(), lister, ciEvidenceRepo, "1111222233334444555566667777888899990000")
	if strings.Contains(out, "ALREADY established") {
		t.Fatalf("a head with a pending check was called established:\n%s", out)
	}
	for _, want := range []string{"Not yet established", "no CI evidence for this head yet", "establish your own baseline"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pending block missing %q:\n%s", want, out)
		}
	}
}

// FAILS OPEN in three distinct ways, each of which must produce a
// byte-identical prompt. A review that cannot dispatch because GitHub was
// unreachable is a worse outcome than a review with no CI hint, and a reviewer
// told "CI is green" on a read that never happened is worse than both.
func TestReviewCIEvidenceFailsOpenRatherThanBlockingOrGuessing(t *testing.T) {
	ctx := context.Background()
	head := "9999888877776666555544443333222211110000"

	if out := dispatchReviewCIEvidence(ctx, nil, ciEvidenceRepo, head); out != "" {
		t.Fatalf("a nil client produced a block: %q", out)
	}
	if out := dispatchReviewCIEvidence(ctx, &fakeCheckLister{err: errors.New("gh: unreachable")}, ciEvidenceRepo, head); out != "" {
		t.Fatalf("a read error produced a block: %q", out)
	}
	if out := dispatchReviewCIEvidence(ctx, &fakeCheckLister{}, ciEvidenceRepo, head); out != "" {
		t.Fatalf("a head with zero checks produced a block: %q", out)
	}
	// And no head means no read at all: nothing should call GitHub for a review
	// with no pinned commit.
	lister := &fakeCheckLister{checks: []github.PullRequestCheck{{Name: "x", Bucket: "pass"}}}
	if out := dispatchReviewCIEvidence(ctx, lister, ciEvidenceRepo, "   "); out != "" || lister.calls != 0 {
		t.Fatalf("an empty head produced out=%q after %d call(s)", out, lister.calls)
	}
}

// The gate's own status is a statement ABOUT THE GATE, not evidence about the
// tree, so counting it would let a head with only that context read as
// established CI.
func TestReviewCIEvidenceExcludesTheMergeGateContext(t *testing.T) {
	lister := &fakeCheckLister{checks: []github.PullRequestCheck{
		{Name: workflow.GitmootMergeGateContext, Bucket: "pass"},
	}}
	if out := dispatchReviewCIEvidence(context.Background(), lister, ciEvidenceRepo, "abcdefabcdefabcdefabcdefabcdefabcdefabcd"); out != "" {
		t.Fatalf("the merge-gate context alone was reported as CI evidence:\n%s", out)
	}
}
