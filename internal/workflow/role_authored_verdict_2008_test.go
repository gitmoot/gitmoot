package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// seedRoleAuthoredVerdict seeds the shape OpenExternalJob persists for a session
// review: NO agent column, an acting org role in the payload instead.
func seedRoleAuthoredVerdict(t *testing.T, store *db.Store, jobID, role, headSHA, decision string) {
	t.Helper()
	encoded, err := marshalPayload(JobPayload{
		Repo: "owner/repo", Branch: "main", PullRequest: 227, HeadSHA: headSHA,
		TaskID: "review-pr-227", ReviewRound: "review-1",
		ActingOrgRole: role,
		Result:        &AgentResult{Decision: decision, Summary: "session review"},
	})
	if err != nil {
		t.Fatalf("marshalPayload(%s): %v", jobID, err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: jobID, Agent: "", Type: "review", State: string(JobSucceeded), Payload: encoded,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: decision}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", jobID, err)
	}
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	if strings.TrimSpace(job.Agent) != "" {
		t.Fatalf("fixture drifted: seeded agent = %q, want empty; this test is about the empty-agent row", job.Agent)
	}
}

// TestFindRepeatedReviewersSeesARoleAuthoredVerdict pins #2008's agent-keyed
// half at the consumer the merge gate's own comment named as deliberately left
// on bare job.Agent: db.SucceededReviewVerdict carried only Agent, so a verdict
// authored by a ROLE was indexed under "" and dropped.
//
// The consequence was #1950 F2: a role's own objection could not be answered by
// a requester named for that same role, because the loop detector could not see
// that the role had already reviewed this head.
func TestFindRepeatedReviewersSeesARoleAuthoredVerdict(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedRoleAuthoredVerdict(t, store, "session-review-1", " GitMoot ", "head-a", "changes_requested")

	matches, err := FindRepeatedReviewers(ctx, store, "owner/repo", 227, "head-a", []string{"gitmoot"}, "")
	if err != nil {
		t.Fatalf("FindRepeatedReviewers: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %+v, want 1: a role's own verdict at this head is invisible to the role", matches)
	}
	// The match must NAME the role. Reporting the raw empty column here would
	// produce a diagnostic that blames nobody.
	if matches[0].Agent != "gitmoot" {
		t.Errorf("match agent = %q, want %q", matches[0].Agent, "gitmoot")
	}
	if matches[0].JobID != "session-review-1" {
		t.Errorf("match job = %q, want session-review-1", matches[0].JobID)
	}
}

// TestFindRepeatedReviewersStillIgnoresAnUnrelatedRole is the negative control
// that keeps the test above from passing under a rule that simply matches
// everything. A role that has not reviewed this head must remain eligible.
func TestFindRepeatedReviewersStillIgnoresAnUnrelatedRole(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedRoleAuthoredVerdict(t, store, "session-review-1", "gitmoot", "head-a", "changes_requested")

	matches, err := FindRepeatedReviewers(ctx, store, "owner/repo", 227, "head-a", []string{"phobos"}, "")
	if err != nil {
		t.Fatalf("FindRepeatedReviewers: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("matches = %+v, want none: a role with no verdict at this head is eligible", matches)
	}
}

// TestReviewDecisionAgentResolvesARole covers the funnel three more of #2008's
// agent-keyed consumers share - reviewLegsAtHead, followUpReviewScopes and the
// approval scan: the agent column wins, and a role is credited only when the
// job has no agent.
func TestReviewDecisionAgentResolvesARole(t *testing.T) {
	roleAuthored := reviewDecisionAgent(
		db.Job{Type: "review", Agent: ""},
		JobPayload{ActingOrgRole: " GitMoot "},
	)
	if roleAuthored != "gitmoot" {
		t.Errorf("role-authored review credited to %q, want %q", roleAuthored, "gitmoot")
	}

	agentWins := reviewDecisionAgent(
		db.Job{Type: "review", Agent: "g7-review"},
		JobPayload{ActingOrgRole: "gitmoot"},
	)
	if agentWins != "g7-review" {
		t.Errorf("agent column lost to a role: got %q, want g7-review", agentWins)
	}
}
