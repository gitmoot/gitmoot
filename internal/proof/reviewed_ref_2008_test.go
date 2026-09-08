package proof

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

const receiptHead = "abc123"

// headlessReviewFixture is proofFixture with the review row's own head removed
// and everything else left alone. The PR receipt still records receiptHead, so
// the receipt fallback is live and the wrong answer is reachable.
func headlessReviewFixture(t *testing.T) Manifest {
	t.Helper()
	root, jobs, results, receipts, events := proofFixture(t, false)
	var patched bool
	for i, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload := workflow.JobPayload{
			Repo: "owner/repo", PullRequest: 42, WorkflowID: "proof/42",
			RootJobID: "root", ParentJobID: "root", DelegationID: "review", DelegationDepth: 1,
			DelegatedBy: "implementer", ActingOrgRole: "gitmoot", Result: results["review-job"],
		}
		jobs[i].Agent = ""
		jobs[i].Payload = proofPayload(t, payload)
		jobs[i].ResultHash = resultHashForPayload(t, jobs[i].Payload)
		patched = true
	}
	if !patched {
		t.Fatal("fixture has no review row to make headless")
	}
	// The hazard only exists while the receipt carries a head to borrow.
	var receiptHasHead bool
	for _, receipt := range receipts {
		if receipt.HeadSHA == receiptHead {
			receiptHasHead = true
		}
	}
	if !receiptHasHead {
		t.Fatalf("fixture receipt does not record %q, so this test cannot observe the over-attribution", receiptHead)
	}
	return Project(root, jobs, results, receipts, events)
}

func reviewNode(t *testing.T, manifest Manifest) Node {
	t.Helper()
	for id := range manifest.Nodes {
		if node := manifest.Nodes[id]; node.Kind == KindReview {
			return node
		}
	}
	t.Fatal("no review node in the manifest")
	return Node{}
}

// TestHeadlessReviewDoesNotBorrowThePullRequestHead is the #2008 over-attribution.
//
// IT ASSERTS THE ABSENCE OF THE WRONG ANSWER, not the presence of a right one.
// This slice fixes a FALSE CLAIM rather than an omission, and a test that only
// proves the new value leaves the old claim provable by nobody: any later change
// that reintroduces the receipt fallback would satisfy "reviewed_ref is
// non-empty" perfectly well.
func TestHeadlessReviewDoesNotBorrowThePullRequestHead(t *testing.T) {
	review := reviewNode(t, headlessReviewFixture(t))

	got := review.Attrs["reviewed_ref"]
	if got == receiptHead {
		t.Fatalf("reviewed_ref = %q, which is the PULL REQUEST RECEIPT's head: the row records no head and this asserts it reviewed that commit", got)
	}
	if strings.Contains(got, receiptHead) {
		t.Fatalf("reviewed_ref = %q still carries the receipt head %q", got, receiptHead)
	}
}

// TestHeadlessReviewStillReportsAnHonestRef is the other half. Refusing the
// borrowed head must not turn the attribute into a blank: the row genuinely has
// a ref to name, the implementer result hash it was delegated from, and an
// over-correction that reported nothing would replace a false claim with a
// missing one.
func TestHeadlessReviewStillReportsAnHonestRef(t *testing.T) {
	review := reviewNode(t, headlessReviewFixture(t))

	got := review.Attrs["reviewed_ref"]
	if got == "" || got == "-" {
		t.Fatalf("reviewed_ref = %q: refusing the borrowed head must not erase the honest ref", got)
	}
	if !strings.HasPrefix(got, hashPrefix) {
		t.Errorf("reviewed_ref = %q, want the implementer result-hash ref (%s...)", got, hashPrefix)
	}
}

// TestReviewWithItsOwnHeadIsUnchanged is the control that keeps the two tests
// above from passing under a rule that simply stops reporting heads. A review
// row that DID record a head must still report exactly that head.
func TestReviewWithItsOwnHeadIsUnchanged(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	review := reviewNode(t, Project(root, jobs, results, receipts, events))

	if got := review.Attrs["reviewed_ref"]; got != receiptHead {
		t.Fatalf("reviewed_ref = %q, want %q: a row that recorded its own head must still report it", got, receiptHead)
	}
}

func commitNodes(manifest Manifest) []Node {
	var out []Node
	for id := range manifest.Nodes {
		if node := manifest.Nodes[id]; node.Kind == KindCommit {
			out = append(out, node)
		}
	}
	return out
}

// TestHeadlessReviewCommitNodeDoesNotCarryTheBorrowedHead is the same
// over-attribution wearing a different node type, fed by the identical local.
//
// A commit node on a job asserts that the commit is part of the evidence for
// that job. A review row that never recorded the head did not review it, so the
// receipt's head is not evidence about that row.
//
// NOTE THE SHAPE THIS TEST HAS TO TAKE. The fixture's review row carries a
// ResultHash, which independently justifies a commit node, so the honest
// assertion is that the node must not CARRY the borrowed head - not that no node
// exists. The pure "acquired from the receipt alone" case is the test below.
func TestHeadlessReviewCommitNodeDoesNotCarryTheBorrowedHead(t *testing.T) {
	for _, node := range commitNodes(headlessReviewFixture(t)) {
		if node.Attrs["job_id"] != "review-job" {
			continue
		}
		if node.Attrs["head_sha"] == receiptHead {
			t.Fatalf("review commit node head_sha = %q, the receipt's head: the row never recorded it", receiptHead)
		}
		if node.Ref == receiptHead {
			t.Fatalf("review commit node Ref = %q, the receipt's head", receiptHead)
		}
	}
}

// TestReviewWithNoOwnEvidenceAcquiresNoCommitNode is the pure case: strip the
// ResultHash too, so the ONLY thing that could create a commit node is the
// borrowed head. Before the fix the receipt manufactured one.
func TestReviewWithNoOwnEvidenceAcquiresNoCommitNode(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	for i, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload := workflow.JobPayload{
			Repo: "owner/repo", PullRequest: 42, WorkflowID: "proof/42",
			RootJobID: "root", ParentJobID: "root", DelegationID: "review", DelegationDepth: 1,
			DelegatedBy: "implementer", ActingOrgRole: "gitmoot",
			Result: &workflow.AgentResult{Decision: "approved", Summary: "no changes, no tests"},
		}
		jobs[i].Agent = ""
		jobs[i].Payload = proofPayload(t, payload)
		jobs[i].ResultHash = ""
		results["review-job"] = payload.Result
	}
	for _, node := range commitNodes(Project(root, jobs, results, receipts, events)) {
		if node.Attrs["job_id"] == "review-job" {
			t.Fatalf("a review row with no head and no result hash acquired a commit node from the receipt: %+v", node.Attrs)
		}
	}
}

// TestImplementerKeepsTheReceiptFallback is the control that stops the two above
// from passing under a rule that simply deletes the receipt fallback. A job that
// CONTRIBUTED the change the pull request landed still resolves its evidence
// head from the receipt when it recorded none itself.
func TestImplementerKeepsTheReceiptFallback(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	for i, job := range jobs {
		if job.ID != "child-job" {
			continue
		}
		payload := workflow.JobPayload{
			Repo: "owner/repo", PullRequest: 42, WorkflowID: "proof/42",
			RootJobID: "root", ParentJobID: "root", DelegationID: "child", DelegationDepth: 1,
			DelegatedBy: "implementer", Result: results["child-job"],
		}
		jobs[i].Payload = proofPayload(t, payload)
		jobs[i].ResultHash = resultHashForPayload(t, jobs[i].Payload)
	}
	var sawBorrowed bool
	for _, node := range commitNodes(Project(root, jobs, results, receipts, events)) {
		if node.Attrs["job_id"] == "child-job" && node.Attrs["head_sha"] == receiptHead {
			sawBorrowed = true
		}
	}
	if !sawBorrowed {
		t.Fatal("an implement job that recorded no head lost the receipt fallback: the fix removed a legitimate path, not just the borrowed one")
	}
}
