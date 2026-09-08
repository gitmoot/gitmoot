package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// seedHeadlessReviewVerdict seeds a succeeded review verdict with NO head.
// externallyDriven selects the two populations #2008 must tell apart.
func seedHeadlessReviewVerdict(t *testing.T, store *db.Store, jobID, agent string, externallyDriven bool) {
	t.Helper()
	ctx := context.Background()
	encoded, err := marshalPayload(JobPayload{
		Repo: "owner/repo", Branch: "main", PullRequest: 227, HeadSHA: "",
		TaskID: "review-pr-227", ReviewRound: "review-1",
		Result: &AgentResult{Decision: "changes_requested", Summary: "headless verdict"},
	})
	if err != nil {
		t.Fatalf("marshalPayload(%s): %v", jobID, err)
	}
	job := db.Job{ID: jobID, Agent: agent, Type: "review", State: string(JobSucceeded), Payload: encoded}
	event := db.JobEvent{Kind: string(JobSucceeded), Message: "changes_requested"}
	if externallyDriven {
		if err := store.CreateExternallyDrivenJobWithEvent(ctx, job, event); err != nil {
			t.Fatalf("CreateExternallyDrivenJobWithEvent(%s): %v", jobID, err)
		}
	} else if err := store.CreateJobWithEvent(ctx, job, event); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", jobID, err)
	}
}

func headBoundExclusionEvents(t *testing.T, store *db.Store, jobID string) []string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	var out []string
	for _, event := range events {
		if event.Kind == HeadBoundExclusionEventKind {
			out = append(out, event.Message)
		}
	}
	return out
}

// TestHeadBoundExclusionSaysWhichPopulationItExcluded pins the distinction the
// measurement forced: 211 review rows on this box carry no head and only 41 are
// session rows, so one reason string for all of them would be an
// over-attribution committed by the change meant to improve honesty.
func TestHeadBoundExclusionSaysWhichPopulationItExcluded(t *testing.T) {
	if reason, excluded := HeadBoundExclusion(true, "abc123"); excluded || reason != "" {
		t.Errorf("a row WITH a head must not be excluded: reason=%q excluded=%v", reason, excluded)
	}
	if reason, excluded := HeadBoundExclusion(true, ""); !excluded || reason != HeadBoundExclusionSessionRow {
		t.Errorf("session row reason = %q (excluded=%v), want %q", reason, excluded, HeadBoundExclusionSessionRow)
	}
	if reason, excluded := HeadBoundExclusion(false, ""); !excluded || reason != HeadBoundExclusionNoHeadRecorded {
		t.Errorf("non-session headless reason = %q (excluded=%v), want %q", reason, excluded, HeadBoundExclusionNoHeadRecorded)
	}
	if HeadBoundExclusionSessionRow == HeadBoundExclusionNoHeadRecorded {
		t.Fatal("the two reasons are identical, so the distinction the measurement forced does not exist")
	}
}

// TestFindRepeatedReviewersRecordsWhyItExcludedAHeadlessRow is the seam: the row
// stays excluded and the exclusion becomes legible.
func TestFindRepeatedReviewersRecordsWhyItExcludedAHeadlessRow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedHeadlessReviewVerdict(t, store, "session-review-1", "g7-review", true)

	matches, err := FindRepeatedReviewers(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"})
	if err != nil {
		t.Fatalf("FindRepeatedReviewers: %v", err)
	}

	// BEHAVIOUR UNCHANGED: a headless row still does not satisfy a head-bound
	// decision. If this ever passes, the visibility change has become a
	// head-binding change.
	if len(matches) != 0 {
		t.Fatalf("matches = %+v, want none: a headless row must not satisfy a decision keyed on head-a", matches)
	}

	messages := headBoundExclusionEvents(t, store, "session-review-1")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d (%v), want exactly 1: the row was dropped silently", len(messages), messages)
	}
	if !strings.Contains(messages[0], HeadBoundExclusionSessionRow) {
		t.Errorf("exclusion message = %q, want it to carry %q", messages[0], HeadBoundExclusionSessionRow)
	}
	if !strings.Contains(messages[0], "review_loop.FindRepeatedReviewers") {
		t.Errorf("exclusion message = %q, want it to name the consumer that excluded the row", messages[0])
	}
}

// TestHeadBoundExclusionRecordDoesNotGrowPerCall pins the at-most-once
// guarantee at the CALLER's level rather than trusting ClaimJobEvent in the
// abstract. That primitive is at-most-once on the exact (job_id, kind, message)
// triple, so a message carrying anything variable would defeat it. The consumer
// that motivates this runs on every daemon poll tick, which is the mechanism
// that grew job_events past a million rows.
func TestHeadBoundExclusionRecordDoesNotGrowPerCall(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedHeadlessReviewVerdict(t, store, "session-review-1", "g7-review", true)

	for i := range 5 {
		if _, err := FindRepeatedReviewers(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"}); err != nil {
			t.Fatalf("FindRepeatedReviewers call %d: %v", i, err)
		}
	}
	if messages := headBoundExclusionEvents(t, store, "session-review-1"); len(messages) != 1 {
		t.Fatalf("exclusion events after 5 calls = %d, want 1: the message is not stable and appends per call", len(messages))
	}
}

// TestMakingTheExclusionLegibleNeverWritesAHead IS THE TRAP, as a test rather
// than as a warning in a PR body.
//
// Making an exclusion legible and making the row head-bound are opposite fixes
// that look identical from a distance, because both stop the row vanishing. A
// consumer that "fixed" its silence by recording a head on a session row would
// satisfy every visibility assertion above while binding a caller-asserted head
// into engine-observed evidence, which #1990 established must never happen.
//
// So this asserts the row's STORED head is still empty afterwards. Every later
// slice in #2008 inherits it.
func TestMakingTheExclusionLegibleNeverWritesAHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedHeadlessReviewVerdict(t, store, "session-review-1", "g7-review", true)

	if _, err := FindRepeatedReviewers(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"}); err != nil {
		t.Fatalf("FindRepeatedReviewers: %v", err)
	}

	job, err := store.GetJob(ctx, "session-review-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if strings.TrimSpace(payload.HeadSHA) != "" {
		t.Fatalf("head_sha = %q, want empty: the exclusion was made legible by BINDING A HEAD, which is the opposite fix", payload.HeadSHA)
	}
	// And the row must still be reported by the store as headless, so no later
	// consumer can read a head that no engine observed.
	verdicts, err := store.SucceededReviewVerdicts(ctx, "owner/repo", 227)
	if err != nil {
		t.Fatalf("SucceededReviewVerdicts: %v", err)
	}
	for _, verdict := range verdicts {
		if verdict.JobID == "session-review-1" && verdict.HeadSHA != "" {
			t.Fatalf("stored verdict head = %q, want empty", verdict.HeadSHA)
		}
	}
}

// TestHeadlessNonSessionRowGetsTheWeakerReason is the negative control for the
// population split. Without it the suite passes under a rule that labels every
// headless row a session row, which is the over-attribution this design exists
// to avoid.
func TestHeadlessNonSessionRowGetsTheWeakerReason(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedHeadlessReviewVerdict(t, store, "ordinary-review-1", "g7-review", false)

	if _, err := FindRepeatedReviewers(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"}); err != nil {
		t.Fatalf("FindRepeatedReviewers: %v", err)
	}
	messages := headBoundExclusionEvents(t, store, "ordinary-review-1")
	if len(messages) != 1 {
		t.Fatalf("exclusion events = %d, want 1", len(messages))
	}
	if strings.Contains(messages[0], HeadBoundExclusionSessionRow) {
		t.Errorf("message = %q: a row that is NOT externally driven was reported as a self-rooted session row", messages[0])
	}
	if !strings.Contains(messages[0], HeadBoundExclusionNoHeadRecorded) {
		t.Errorf("message = %q, want the weaker reason %q", messages[0], HeadBoundExclusionNoHeadRecorded)
	}
}
