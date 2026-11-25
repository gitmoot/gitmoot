package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
)

// The #2755 incident shape: PR #6 was reviewed at sharedCommitRejected and got
// changes_requested; PR #7 carries that commit and was approved at its own head.
const (
	sharedCommitBase     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sharedCommitRejected = "3d1ea615b599f7d1e6b75341bf13459e1f80d629"
	sharedCommitOther    = "99582eee00000000000000000000000000000000"
	sharedCommitHead     = "5a550224ce52c3b01edef89c559e5cca09cc6d3d"
)

// sharedCommitGateFixture is PR #7 of lowRiskTestRepo, approved at its exact
// head with green external CI, through the ordinary gate or the per-repo
// low-risk gate (classified level 1 at the head).
func sharedCommitGateFixture(t *testing.T, lowRisk bool) (*db.Store, *fakeMergeGateGitHub, PolicyMergeGate, MergeRequest) {
	t.Helper()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-pr7-head", Agent: "audit", Type: "review"}, JobPayload{
		Repo: lowRiskTestRepo, Branch: "task-7", PullRequest: 7, HeadSHA: sharedCommitHead,
		TaskID: "task-7", ReviewRound: "review-2",
		Result: &AgentResult{Decision: "approved", Summary: "ready"},
	})
	setMergeGateJobRecordedTimes(t, store, "review-pr7-head", "2026-11-25 10:13:12", "2026-11-25 10:23:23")
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 7, State: "open", HeadRef: "task-7", BaseRef: "main",
			BaseSHA: sharedCommitBase, HeadSHA: sharedCommitHead, Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success"},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
		commits:     []string{sharedCommitRejected, sharedCommitOther, sharedCommitHead},
	}
	gate := PolicyMergeGate{
		AutoMerge: true, LowRiskOnly: lowRisk, Store: store, GitHub: gh,
		Git: &fakeMergeGateGit{clean: true}, CheckoutPath: t.TempDir(),
	}
	if lowRisk {
		gate.ReviewLevel = lowRiskLevelAt(reviewlevel.LevelNoReview, sharedCommitHead, nil)
	}
	request := MergeRequest{Repo: lowRiskTestRepo, PullRequest: 7, TaskID: "task-7", Reviewer: "audit"}
	return store, gh, gate, request
}

// insertSharedCommitVerdict records a succeeded review of repo#pullRequest at
// head with decision, recorded at the given time.
func insertSharedCommitVerdict(t *testing.T, store *db.Store, id string, pullRequest int, head, decision, recorded string) {
	t.Helper()
	insertSharedCommitVerdictWith(t, store, id, pullRequest, head, decision, recorded, nil)
}

func insertSharedCommitVerdictWith(t *testing.T, store *db.Store, id string, pullRequest int, head, decision, recorded string, adjust func(*JobPayload)) {
	t.Helper()
	task := "task-6"
	if pullRequest == 7 {
		task = "task-7"
	}
	payload := JobPayload{
		Repo: lowRiskTestRepo, Branch: task, PullRequest: pullRequest, HeadSHA: head,
		TaskID: task, ReviewRound: "review-1",
		Result: &AgentResult{Decision: decision, Summary: "fixture verdict"},
	}
	if adjust != nil {
		adjust(&payload)
	}
	insertCompletedJob(t, store, db.Job{ID: id, Agent: "reviewer-" + id, Type: "review"}, payload)
	setMergeGateJobRecordedTimes(t, store, id, recorded, recorded)
}

func forEachSharedCommitGate(t *testing.T, run func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest)) {
	t.Helper()
	for _, lowRisk := range []bool{false, true} {
		name := "ordinary gate"
		if lowRisk {
			name = "low-risk gate"
		}
		t.Run(name, func(t *testing.T) {
			store, gh, gate, request := sharedCommitGateFixture(t, lowRisk)
			run(t, store, gh, gate, request)
		})
	}
}

func assertSharedCommitMerged(t *testing.T, decision MergeDecision, err error, gh *fakeMergeGateGitHub) {
	t.Helper()
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 || gh.merges[0].MatchHeadCommit != sharedCommitHead {
		t.Fatalf("decision = %+v (reason %q) merges = %+v; want an exact-head merge", decision, decision.Reason.Render(), gh.merges)
	}
	if gh.commitCalls == 0 {
		t.Fatal("the gate merged without listing the pull request's commits")
	}
}

// (a) A pull request carrying another pull request's changes_requested head
// is held, and the reason names that pull request, its job and the commit.
func TestMergeGateHoldsPullRequestCarryingAnotherPullRequestsChangesRequestedCommit(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr6-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:21:03")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		reason := decision.Reason.Render()
		if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() || len(gh.merges) != 0 {
			t.Fatalf("decision = %+v (reason %q) merges = %+v; want a held gate miss", decision, reason, gh.merges)
		}
		for _, want := range []string{SharedCommitReviewGate, "PR #6", "review-pr6-rejected", sharedCommitRejected} {
			if !strings.Contains(reason, want) {
				t.Fatalf("reason %q does not name %q", reason, want)
			}
		}
	})
}

// (b) The other pull request's later approval at that exact commit answers
// the objection; an approval recorded before it does not.
func TestMergeGateMergesWhenSharedChangesRequestedCommitWasLaterApproved(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr6-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:21:03")
		insertSharedCommitVerdict(t, store, "review-pr6-approved", 6, sharedCommitRejected, "approved", "2026-11-25 10:40:00")

		decision, err := gate.Evaluate(context.Background(), request)
		assertSharedCommitMerged(t, decision, err, gh)
	})
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr6-approved", 6, sharedCommitRejected, "approved", "2026-11-25 10:00:00")
		insertSharedCommitVerdict(t, store, "review-pr6-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:21:03")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Merged || !strings.Contains(decision.Reason.Render(), "review-pr6-rejected") {
			t.Fatalf("decision = %+v (reason %q); an approval recorded before the objection must not answer it", decision, decision.Reason.Render())
		}
	})
}

// (c) Without a shared commit the gate merges exactly as before.
func TestMergeGateMergesWithoutSharedChangesRequestedCommit(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr6-rejected", 6, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "changes_requested", "2026-11-25 10:21:03")

		decision, err := gate.Evaluate(context.Background(), request)
		assertSharedCommitMerged(t, decision, err, gh)
	})
}

// (d) The ordinary fix round still merges: the pull request's own earlier
// changes_requested head is one of its commits, answered by the approval of
// its current head recorded after it.
func TestMergeGateMergesPullRequestReapprovedAfterItsOwnChangesRequested(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr7-rejected", 7, sharedCommitRejected, "changes_requested", "2026-11-25 10:05:00")

		decision, err := gate.Evaluate(context.Background(), request)
		assertSharedCommitMerged(t, decision, err, gh)
	})
}

// (e) A commit list that cannot be read holds the merge (fail closed).
func TestMergeGateHoldsWhenPullRequestCommitsCannotBeListed(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, _ *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		gh.commitsErr = errors.New("github: 502 bad gateway")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		reason := decision.Reason.Render()
		if decision.Merged || len(gh.merges) != 0 || !strings.Contains(reason, "cannot list the commits of pull request #7") || !strings.Contains(reason, "502 bad gateway") {
			t.Fatalf("decision = %+v (reason %q) merges = %+v; want a held merge naming the listing failure", decision, reason, gh.merges)
		}
	})
}

// cappedCommitList is a 250-entry pull request commit list, GitHub's cap,
// that omits the head: the listing cannot vouch for completeness.
func cappedCommitList() []string {
	list := make([]string, 0, pullRequestCommitListCap)
	for i := range pullRequestCommitListCap {
		list = append(list, fmt.Sprintf("%040x", i+1))
	}
	return list
}

// A listing GitHub capped (250 entries, head omitted) is replaced by the
// complete base...head range pinned to the evaluated base and head: a valid
// large pull request merges when nothing objects, objections anywhere in the
// complete range still hold, and an unreadable range holds.
func TestMergeGateReadsCompleteRangeWhenCommitListIsCapped(t *testing.T) {
	complete := append(cappedCommitList(), sharedCommitRejected, sharedCommitHead)
	forEachSharedCommitGate(t, func(t *testing.T, _ *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		gh.commits = cappedCommitList()
		gh.rangeCommits = complete

		decision, err := gate.Evaluate(context.Background(), request)
		assertSharedCommitMerged(t, decision, err, gh)
		if want := sharedCommitBase + "..." + sharedCommitHead; len(gh.rangeCalls) != 1 || gh.rangeCalls[0] != want {
			t.Fatalf("range reads = %v, want one read of %s", gh.rangeCalls, want)
		}
	})
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr6-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:21:03")
		gh.commits = cappedCommitList()
		gh.rangeCommits = complete

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Merged || !strings.Contains(decision.Reason.Render(), "review-pr6-rejected") {
			t.Fatalf("decision = %+v (reason %q); want the objection beyond the capped list held", decision, decision.Reason.Render())
		}
	})
	forEachSharedCommitGate(t, func(t *testing.T, _ *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		gh.commits = cappedCommitList()
		gh.rangeErr = errors.New("github: 502 bad gateway")

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		reason := decision.Reason.Render()
		if decision.Merged || len(gh.merges) != 0 || !strings.Contains(reason, "cannot read the complete commit range") {
			t.Fatalf("decision = %+v (reason %q) merges = %+v; want a held merge for an unreadable range", decision, reason, gh.merges)
		}
	})
	forEachSharedCommitGate(t, func(t *testing.T, _ *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		gh.commits = []string{sharedCommitRejected, sharedCommitOther}
		gh.rangeCommits = []string{sharedCommitRejected, sharedCommitOther}

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		reason := decision.Reason.Render()
		if decision.Merged || len(gh.merges) != 0 || !decision.Reason.IsGateMiss() || !strings.Contains(reason, "does not contain the head being merged") {
			t.Fatalf("decision = %+v (reason %q) merges = %+v; want a held merge for a range without the head", decision, reason, gh.merges)
		}
	})
}

// A listing that contains the head but reaches the cap is not trusted either.
func TestMergeGateReadsCompleteRangeWhenCommitListReachesCap(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr6-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:21:03")
		gh.commits = append(cappedCommitList()[:pullRequestCommitListCap-1], sharedCommitHead)
		gh.rangeCommits = []string{sharedCommitRejected, sharedCommitHead}

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if decision.Merged || len(gh.rangeCalls) != 1 || !strings.Contains(decision.Reason.Render(), "review-pr6-rejected") {
			t.Fatalf("decision = %+v (reason %q) range reads = %v; want the complete range read and the objection held", decision, decision.Reason.Render(), gh.rangeCalls)
		}
	})
}

// A post-merge review is follow-up work on a merged head, never a merge
// verdict: its changes_requested on a carried commit does not hold.
func TestMergeGateIgnoresPostMergeChangesRequestedOnCarriedCommit(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdictWith(t, store, "review-pr6-post-merge", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:21:03",
			func(payload *JobPayload) { payload.PostMergeReview = true })

		decision, err := gate.Evaluate(context.Background(), request)
		assertSharedCommitMerged(t, decision, err, gh)
	})
}

// A post-merge approval does not answer a pre-merge objection either.
func TestSharedChangesRequestedIgnoresPostMergeApproval(t *testing.T) {
	jobs := []db.Job{
		sharedCommitVerdictJob(t, "pr6-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:00:00"),
		sharedCommitVerdictJobWith(t, "pr6-post-merge-approved", 6, sharedCommitRejected, "approved", "2026-11-25 11:00:00",
			func(payload *JobPayload) { payload.PostMergeReview = true }),
	}
	held, err := unresolvedSharedChangesRequested(jobs, lowRiskTestRepo, 7, sharedCommitHead, []string{sharedCommitRejected, sharedCommitHead}, "")
	if err != nil || len(held) != 1 || held[0].jobID != "pr6-rejected" {
		t.Fatalf("held = %+v err = %v; want the pre-merge objection still held", held, err)
	}
}

// The shared-commit check runs only after review and CI pass: a conclusive CI
// miss is returned on its own, without listing the pull request's commits.
func TestMergeGateReturnsCIMissBeforeListingCommits(t *testing.T) {
	forEachSharedCommitGate(t, func(t *testing.T, store *db.Store, gh *fakeMergeGateGitHub, gate PolicyMergeGate, request MergeRequest) {
		insertSharedCommitVerdict(t, store, "review-pr6-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:21:03")
		gh.checks = []github.PullRequestCheck{{Name: "ci", Bucket: "fail", State: "FAILURE"}}

		decision, err := gate.Evaluate(context.Background(), request)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		reason := decision.Reason.Render()
		if decision.Merged || !decision.Reason.IsGateMiss() || !strings.Contains(reason, "CI gate") ||
			strings.Contains(reason, SharedCommitReviewGate) || gh.commitCalls != 0 {
			t.Fatalf("decision = %+v (reason %q) commit listings = %d; want the CI miss alone and no listing", decision, reason, gh.commitCalls)
		}
	})
}

// The merging pull request's own changes_requested at its CURRENT head stays
// with the exact-head review gate; the shared-commit gate does not add a
// second refusal for it.
func TestSharedChangesRequestedSkipsOwnCurrentHeadVerdict(t *testing.T) {
	jobs := []db.Job{sharedCommitVerdictJob(t, "own-head", 7, sharedCommitHead, "changes_requested", "2026-11-25 10:00:00")}
	held, err := unresolvedSharedChangesRequested(jobs, lowRiskTestRepo, 7, sharedCommitHead, []string{sharedCommitHead}, "")
	if err != nil || len(held) != 0 {
		t.Fatalf("held = %+v err = %v; want the current-head verdict left to the review gate", held, err)
	}
}

// The repo match is case-insensitive and a delegation child answers nothing.
// A split review's part slug scopes only its own pull request: a same-part
// approval answers that pull request's objection, never another's.
func TestSharedChangesRequestedScopesPartApprovalsToTheirPullRequest(t *testing.T) {
	part := func(name string) func(*JobPayload) { return func(payload *JobPayload) { payload.ReviewPart = name } }
	rejected5 := sharedCommitVerdictJobWith(t, "pr5-part-a-rejected", 5, strings.ToUpper(sharedCommitRejected), "changes_requested", "2026-11-25 10:00:00", part("a"))
	rejected6 := sharedCommitVerdictJobWith(t, "pr6-part-a-rejected", 6, sharedCommitRejected, "changes_requested", "2026-11-25 10:00:00", part("a"))
	approved6 := sharedCommitVerdictJobWith(t, "pr6-part-a-approved", 6, sharedCommitRejected, "approved", "2026-11-25 11:00:00", part("a"))
	child := sharedCommitVerdictJob(t, "pr6-child", 6, sharedCommitRejected, "approved", "2026-11-25 11:00:00")
	child.ParentJobID, child.DelegationID = "pr6-part-a-rejected", "lens"
	held, err := unresolvedSharedChangesRequested([]db.Job{rejected5, rejected6, approved6, child}, strings.ToUpper(lowRiskTestRepo), 7, sharedCommitHead,
		[]string{sharedCommitRejected, sharedCommitHead}, "")
	if err != nil || len(held) != 1 || held[0].jobID != "pr5-part-a-rejected" || held[0].pullRequest != 5 || held[0].commit != sharedCommitRejected {
		t.Fatalf("held = %+v err = %v; want only PR #5's objection held (PR #6's part approval answers PR #6 alone)", held, err)
	}
	whole := sharedCommitVerdictJob(t, "pr6-whole-approved", 6, sharedCommitRejected, "approved", "2026-11-25 12:00:00")
	held, err = unresolvedSharedChangesRequested([]db.Job{rejected5, rejected6, approved6, whole}, lowRiskTestRepo, 7, sharedCommitHead,
		[]string{sharedCommitRejected, sharedCommitHead}, "")
	if err != nil || len(held) != 0 {
		t.Fatalf("held = %+v err = %v; want a later whole-head approval at the commit to answer every part", held, err)
	}
}

func sharedCommitVerdictJob(t *testing.T, id string, pullRequest int, head, decision, recorded string) db.Job {
	t.Helper()
	return sharedCommitVerdictJobWith(t, id, pullRequest, head, decision, recorded, nil)
}

func sharedCommitVerdictJobWith(t *testing.T, id string, pullRequest int, head, decision, recorded string, adjust func(*JobPayload)) db.Job {
	t.Helper()
	payload := JobPayload{
		Repo: lowRiskTestRepo, PullRequest: pullRequest, HeadSHA: head, ReviewRound: "review-1",
		Result: &AgentResult{Decision: decision, Summary: "fixture verdict"},
	}
	if adjust != nil {
		adjust(&payload)
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	return db.Job{ID: id, Type: "review", State: string(JobSucceeded), Payload: encoded, CreatedAt: recorded, UpdatedAt: recorded}
}
