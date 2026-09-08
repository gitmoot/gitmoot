package proof

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestRoleAuthoredReviewIsAttributedAndComparable pins #2008's agent-keyed half
// for internal/proof, which read bare job.Agent with zero ActingOrgRole
// references.
//
// An externally driven session review persists the acting org role IN PLACE OF
// an agent, so the review row's Agent column is empty by design. Reading it
// alone rendered the author as "-" and left the independence attribute absent,
// which reads as "not comparable" rather than as "nobody looked".
//
// Both assertions are about what a CONSUMER sees in the manifest, not about
// which helper computed it.
func TestRoleAuthoredReviewIsAttributedAndComparable(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)

	// Turn the review row into a session review: no agent, an acting role
	// instead. Nothing else about the fixture changes.
	for i, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload := workflow.JobPayload{
			Repo: "owner/repo", PullRequest: 42, HeadSHA: "abc123", WorkflowID: "proof/42",
			RootJobID: "root", ParentJobID: "root", DelegationID: "review", DelegationDepth: 1,
			DelegatedBy: "implementer", ActingOrgRole: " ReVieWer ", Result: results["review-job"],
		}
		jobs[i].Agent = ""
		jobs[i].Payload = proofPayload(t, payload)
		jobs[i].ResultHash = resultHashForPayload(t, jobs[i].Payload)
	}

	manifest := Project(root, jobs, results, receipts, events)
	if err := VerifyManifest(manifest); err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}

	var review *Node
	for id := range manifest.Nodes {
		node := manifest.Nodes[id]
		if node.Kind == KindReview {
			review = &node
			break
		}
	}
	if review == nil {
		t.Fatal("no review node in the manifest")
	}

	// The role is normalized by the same rule the merge gate uses, so the
	// surrounding whitespace and casing above must not survive into the author.
	if got := review.Attrs["agent"]; got != "reviewer" {
		t.Errorf("review author = %q, want %q: a role-authored review renders no author", got, "reviewer")
	}

	// The implementer is "implementer" and the reviewer resolves to "reviewer",
	// so this pair IS comparable and IS independent. Before the fix the attribute
	// was absent entirely, which is indistinguishable from a pair nobody could
	// compare.
	if _, ok := review.Attrs["independent"]; !ok {
		t.Fatalf("independent attribute absent: a role-authored reviewer was reported as not comparable")
	}
	if got := review.Attrs["independent"]; got != "true" {
		t.Errorf("independent = %q, want true", got)
	}
}

// TestRoleAuthoredSelfReviewIsNotIndependent is the other direction, and it is
// the one that matters for a merge decision: resolving the identity must be able
// to REFUSE independence, not only grant it. A rule that only ever produced
// "true" would satisfy the test above while destroying the attribute's meaning.
func TestRoleAuthoredSelfReviewIsNotIndependent(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)

	for i, job := range jobs {
		switch {
		case job.Type == "review":
			payload := workflow.JobPayload{
				Repo: "owner/repo", PullRequest: 42, HeadSHA: "abc123", WorkflowID: "proof/42",
				RootJobID: "root", ParentJobID: "root", DelegationID: "review", DelegationDepth: 1,
				DelegatedBy: "implementer", ActingOrgRole: "gitmoot", Result: results["review-job"],
			}
			jobs[i].Agent = ""
			jobs[i].Payload = proofPayload(t, payload)
			jobs[i].ResultHash = resultHashForPayload(t, jobs[i].Payload)
		case job.ID == "root":
			// The implementer records the SAME role, also with no agent.
			payload := workflow.JobPayload{
				Repo: "owner/repo", PullRequest: 42, HeadSHA: "abc123", WorkflowID: "proof/42",
				RootJobID: "root", ActingOrgRole: "gitmoot", Result: results["root"],
			}
			jobs[i].Agent = ""
			jobs[i].Payload = proofPayload(t, payload)
			jobs[i].ResultHash = resultHashForPayload(t, jobs[i].Payload)
			root = jobs[i]
		}
	}

	manifest := Project(root, jobs, results, receipts, events)

	var review *Node
	for id := range manifest.Nodes {
		node := manifest.Nodes[id]
		if node.Kind == KindReview {
			review = &node
			break
		}
	}
	if review == nil {
		t.Fatal("no review node in the manifest")
	}
	if got := review.Attrs["independent"]; got != "false" {
		t.Errorf("independent = %q, want false: one role reviewing its own work is not independent", got)
	}
}
