package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// overlapDelivery is what the fake adapter saw for one dispatched review: the
// prior-verdict list rendered at that dispatch, exactly as the read-only seat
// (stagePriorVerdicts) and a remote instance (installRemotePriorVerdicts) render
// it, from the same function.
type overlapDelivery struct {
	jobID         string
	priorVerdicts string
}

type overlapHoldHarness struct {
	t          *testing.T
	ctx        context.Context
	store      *db.Store
	home       string
	worker     jobWorker
	mu         sync.Mutex
	delivered  []overlapDelivery
	checkedOut map[string]bool
}

func newOverlapHoldHarness(t *testing.T) *overlapHoldHarness {
	t.Helper()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerRepo(t, store, "other/repo", checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	// One reviewer per repository: a seat is scoped to the repository it reviews.
	seedDaemonWorkerAgent(t, store, "reviewer-owner", runtime.ShellRuntime, "", []string{"review"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "reviewer-other", runtime.ShellRuntime, "", []string{"review"}, "other/repo")
	h := &overlapHoldHarness{t: t, ctx: context.Background(), store: store, home: home, checkedOut: map[string]bool{}}
	worker := blockerE2EWorker(store, home, checkout)
	validate := worker.CheckoutValidator
	// The checkout validator is the first per-job step of worker.run; the
	// checkout lock, runtime lock and remote admission all follow it, so a job
	// it never sees took none of them.
	worker.CheckoutValidator = func(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent) (string, error) {
		h.mu.Lock()
		h.checkedOut[job.ID] = true
		h.mu.Unlock()
		return validate(ctx, job, payload, agent)
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return overlapRecordingAdapter{h: h}, nil
	}
	h.worker = worker
	return h
}

type overlapRecordingAdapter struct{ h *overlapHoldHarness }

func (a overlapRecordingAdapter) Deliver(ctx context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	body, diagnostic := renderPriorVerdictsFile(ctx, config.PathsForHome(a.h.home), job.Repository)
	if body == nil {
		a.h.t.Errorf("prior verdicts for %s not rendered: %s", job.ID, diagnostic)
	}
	a.h.mu.Lock()
	a.h.delivered = append(a.h.delivered, overlapDelivery{jobID: job.ID, priorVerdicts: string(body)})
	a.h.mu.Unlock()
	return runtime.Result{Decision: "approved", Summary: "ok"}, nil
}

func (h *overlapHoldHarness) enqueue(id, repo string, pr int, head string) {
	h.t.Helper()
	h.enqueueRequest(workflow.JobRequest{ID: id, Repo: repo, PullRequest: pr, HeadSHA: head})
}

func (h *overlapHoldHarness) enqueueRequest(request workflow.JobRequest) {
	h.t.Helper()
	request.Agent = "reviewer-" + strings.Split(request.Repo, "/")[0]
	request.Action, request.Branch, request.ReviewPurpose = "review", "feature", "code"
	enqueueDaemonWorkerJob(h.t, h.store, request)
}

// setStoredHead records the pull request's current head as the PR watcher does.
func (h *overlapHoldHarness) setStoredHead(repo string, pr int, head string) {
	h.t.Helper()
	if err := h.store.UpsertPullRequest(h.ctx, db.PullRequest{RepoFullName: repo, Number: int64(pr), HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, State: "open"}); err != nil {
		h.t.Fatal(err)
	}
}

// startRunning claims a queued review as the daemon does when it dispatches
// one, leaving it running.
func (h *overlapHoldHarness) startRunning(id string) {
	h.t.Helper()
	claimed, err := h.store.ClaimRunningJob(h.ctx, id, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{Kind: string(workflow.JobRunning)}, 1, "boot")
	if err != nil || !claimed {
		h.t.Fatalf("claim %s: claimed=%v err=%v", id, claimed, err)
	}
}

// settleWithFinding records a changes_requested verdict carrying finding on a
// running review.
func (h *overlapHoldHarness) settleWithFinding(id, finding string) {
	h.t.Helper()
	job, payload := blockerE2EJobPayload(h.t, h.store, id)
	payload.Result = &workflow.AgentResult{
		Decision: "changes_requested", Severity: "P2", Evidence: workflow.EvidenceStaticOnly,
		Summary:  "one finding",
		Findings: []json.RawMessage{json.RawMessage(`{"severity":"P2","title":"` + finding + `"}`)},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	moved, err := h.store.TransitionJobStatePayloadWithEvent(h.ctx, job.ID, string(workflow.JobRunning), string(workflow.JobSucceeded), string(encoded), db.JobEvent{JobID: job.ID, Kind: string(workflow.JobSucceeded), Message: "verdict"})
	if err != nil || !moved {
		h.t.Fatalf("settle %s: moved=%v err=%v", id, moved, err)
	}
}

func (h *overlapHoldHarness) pass() {
	h.t.Helper()
	if err := runQueuedJobsForRepo(h.ctx, h.worker, 4, "", ""); err != nil {
		h.t.Fatalf("dispatch pass: %v", err)
	}
}

// filter runs the hold alone over a fresh queue snapshot, as the dispatch
// listing does, and returns the ids it lets through.
func (h *overlapHoldHarness) filter(holds *reviewHoldLog, rootFilter string) map[string]bool {
	h.t.Helper()
	queued, err := h.store.ListQueuedJobs(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.filterSnapshot(holds, queued, rootFilter)
}

func (h *overlapHoldHarness) filterSnapshot(holds *reviewHoldLog, queued []db.Job, rootFilter string) map[string]bool {
	h.t.Helper()
	kept, err := holdOverlappingReviews(h.ctx, h.store, holds, queued, "", rootFilter, true)
	if err != nil {
		h.t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, job := range kept {
		ids[job.ID] = true
	}
	return ids
}

func (h *overlapHoldHarness) delivery(id string) (overlapDelivery, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.delivered {
		if d.jobID == id {
			return d, true
		}
	}
	return overlapDelivery{}, false
}

func (h *overlapHoldHarness) state(id string) string {
	h.t.Helper()
	job, _ := blockerE2EJobPayload(h.t, h.store, id)
	return job.State
}

func (h *overlapHoldHarness) events(id, kind string) []db.JobEvent {
	h.t.Helper()
	events, err := h.store.ListJobEvents(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []db.JobEvent
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

var (
	overlapHeadA = strings.Repeat("a", 40)
	overlapHeadB = strings.Repeat("b", 40)
	overlapHeadC = strings.Repeat("c", 40)
)

// A review of a new head requested while a review of the same PR runs is not
// claimed, takes no lock, and dispatches after the first settles, with the
// first review's finding in its prior-verdict list (#2726).
func TestReviewHeldForRunningReviewDispatchesWithItsVerdict(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-1", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-1")
	h.enqueue("review-2", "owner/repo", 12, overlapHeadB)

	h.pass()
	if _, delivered := h.delivery("review-2"); delivered {
		t.Fatal("review-2 dispatched while review-1 of the same PR was running")
	}
	if got := h.state("review-2"); got != string(workflow.JobQueued) {
		t.Fatalf("held review-2 state = %q, want queued", got)
	}
	if h.checkedOut["review-2"] {
		t.Fatal("held review-2 reached checkout validation: a held review must take no lock")
	}

	h.settleWithFinding("review-1", "PRIOR-FINDING-2726")
	h.pass()
	got, delivered := h.delivery("review-2")
	if !delivered {
		t.Fatal("review-2 did not dispatch after review-1 settled")
	}
	if !strings.Contains(got.priorVerdicts, "PRIOR-FINDING-2726") || !strings.Contains(got.priorVerdicts, `"job_id": "review-1"`) {
		t.Fatalf("review-2 prior verdicts lack review-1's finding:\n%s", got.priorVerdicts)
	}
}

// Running A, stored head C, then head B queued, then head C queued: B is
// superseded through the stale-head path, its dispatch-time worktree is marked
// for reclaim, and C runs once A settles.
func TestReviewHoldKeepsOnlyTheCurrentHeadHeldReview(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-a", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-a")
	h.setStoredHead("owner/repo", 12, overlapHeadC)
	worktree := t.TempDir()
	h.enqueueRequest(workflow.JobRequest{ID: "review-b", Repo: "owner/repo", PullRequest: 12, HeadSHA: overlapHeadB, WorktreePath: worktree, ReadOnlyWorktree: true, ReadOnlySeat: true})
	h.enqueue("review-c", "owner/repo", 12, overlapHeadC)

	h.pass()
	if got := h.state("review-b"); got != string(workflow.JobCancelled) {
		t.Fatalf("off-head held review-b state = %q, want cancelled (superseded)", got)
	}
	superseded := h.events("review-b", workflow.JobEventSupersededStaleHead)
	if len(superseded) != 1 || !strings.Contains(superseded[0].Message, "review-c") || !strings.Contains(superseded[0].Message, "review-a") {
		t.Fatalf("review-b supersede events = %+v, want one naming review-c and review-a", superseded)
	}
	if reclaim := h.events("review-b", "delegation_worktree_cleanup_skipped"); len(reclaim) != 1 || !strings.Contains(reclaim[0].Message, worktree) {
		t.Fatalf("review-b reclaim markers = %+v, want one for its worktree %s", reclaim, worktree)
	}
	if got := h.state("review-a"); got != string(workflow.JobRunning) {
		t.Fatalf("running review-a state = %q: a running review must not be superseded by the hold", got)
	}
	if got := h.state("review-c"); got != string(workflow.JobQueued) {
		t.Fatalf("current-head held review-c state = %q, want queued", got)
	}
	if _, delivered := h.delivery("review-c"); delivered {
		t.Fatal("review-c dispatched while review-a was running")
	}

	h.settleWithFinding("review-a", "FINDING-A")
	h.pass()
	if _, delivered := h.delivery("review-c"); !delivered {
		t.Fatal("review-c did not dispatch after review-a settled")
	}
	if _, delivered := h.delivery("review-b"); delivered {
		t.Fatal("superseded review-b dispatched")
	}
}

// Queue order alone never cancels: with no stored head nothing is superseded,
// and with the stored head at B, a later request for the older head C is the
// one superseded while B survives.
func TestReviewHoldNeverSupersedesTheCurrentHeadForQueueOrder(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-a", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-a")
	h.enqueue("review-b", "owner/repo", 12, overlapHeadB)
	h.enqueue("review-c", "owner/repo", 12, overlapHeadC)

	if kept := h.filter(nil, ""); kept["review-b"] || kept["review-c"] {
		t.Fatalf("held reviews passed the filter: %v", kept)
	}
	for _, id := range []string{"review-b", "review-c"} {
		if got := h.state(id); got != string(workflow.JobQueued) {
			t.Fatalf("with no stored head %s state = %q, want queued", id, got)
		}
	}

	h.setStoredHead("owner/repo", 12, overlapHeadB)
	h.filter(nil, "")
	if got := h.state("review-b"); got != string(workflow.JobQueued) {
		t.Fatalf("current-head review-b state = %q, want queued: queue order cancelled the current head", got)
	}
	if got := h.state("review-c"); got != string(workflow.JobCancelled) {
		t.Fatalf("off-head review-c state = %q, want cancelled", got)
	}
}

// A review a worker claimed after the queue snapshot is never cancelled by a
// decision made from that snapshot.
func TestReviewHoldLeavesAJobClaimedAfterTheSnapshotRunning(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-a", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-a")
	h.setStoredHead("owner/repo", 12, overlapHeadC)
	h.enqueue("review-b", "owner/repo", 12, overlapHeadB)
	h.enqueue("review-c", "owner/repo", 12, overlapHeadC)

	snapshot, err := h.store.ListQueuedJobs(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.startRunning("review-b")
	kept := h.filterSnapshot(nil, snapshot, "")
	if got := h.state("review-b"); got != string(workflow.JobRunning) {
		t.Fatalf("review-b claimed after the snapshot is %q, want running", got)
	}
	if events := h.events("review-b", workflow.JobEventSupersededStaleHead); len(events) != 0 {
		t.Fatalf("claimed review-b carries supersede events %+v", events)
	}
	if kept["review-b"] || kept["review-c"] {
		t.Fatalf("filter let through %v; review-b is no longer queued and review-c is held", kept)
	}
}

// Under daemon --session, a queued review outside the session neither holds
// nor supersedes one inside it. A running review still holds across scopes.
func TestReviewHoldIgnoresQueuedReviewsOutsideTheSession(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-outside", "owner/repo", 12, overlapHeadA)
	h.enqueueRequest(workflow.JobRequest{ID: "review-session", Repo: "owner/repo", PullRequest: 12, HeadSHA: overlapHeadB, RootJobID: "session-b"})

	if kept := h.filter(nil, "session-b"); !kept["review-session"] {
		t.Fatalf("filter kept %v: review-session was held by a review outside its session", kept)
	}

	h.enqueue("review-running", "owner/repo", 12, overlapHeadC)
	h.startRunning("review-running")
	h.setStoredHead("owner/repo", 12, overlapHeadA)
	if kept := h.filter(nil, "session-b"); kept["review-session"] {
		t.Fatal("review-session passed while a review of the same PR was running")
	}
	if got := h.state("review-session"); got != string(workflow.JobQueued) {
		t.Fatalf("review-session state = %q: a review outside the session superseded it", got)
	}
	if got := h.state("review-outside"); got != string(workflow.JobQueued) {
		t.Fatalf("review-outside state = %q: the session pass touched a review outside it", got)
	}
}

// The hold is keyed by (repo, PR): a review of a different PR of the same
// repo, or of the same PR number in another repo, is not held, while the
// same-PR new-head review is.
func TestReviewHoldIsScopedToTheSamePullRequestOfTheSameRepo(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-running", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-running")
	h.enqueue("review-same-pr", "owner/repo", 12, overlapHeadB)
	h.enqueue("review-other-pr", "owner/repo", 13, overlapHeadB)
	h.enqueue("review-other-repo", "other/repo", 12, overlapHeadB)

	h.pass()
	if _, delivered := h.delivery("review-same-pr"); delivered {
		t.Fatal("same-PR new-head review dispatched while one was running")
	}
	for _, id := range []string{"review-other-pr", "review-other-repo"} {
		if _, delivered := h.delivery(id); !delivered {
			t.Fatalf("%s was held by a review of a different pull request", id)
		}
		if events := h.events(id, reviewHeldForRunningReviewEventKind); len(events) != 0 {
			t.Fatalf("%s carries hold events %+v", id, events)
		}
	}
}

// One event per (held review, holder): the holder moving from queued to
// running writes no second event, and the scheduler's hold log forgets the
// hold once it ends.
func TestReviewHoldEventIsKeyedOnHeldAndHolderIdentity(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-first", "owner/repo", 12, overlapHeadA)
	h.enqueue("review-held", "owner/repo", 12, overlapHeadB)
	holds := newReviewHoldLog()

	// No hold log on the first two passes: the store alone must dedup on the
	// identity, as it does after a daemon restart.
	h.filter(nil, "")
	h.startRunning("review-first")
	h.filter(nil, "")
	if events := h.events("review-held", reviewHeldForRunningReviewEventKind); len(events) != 1 {
		t.Fatalf("hold events = %+v, want one for the one holder", events)
	}
	h.filter(holds, "")
	if got := holds.held["review-held"]; got != "review-first" {
		t.Fatalf("hold log entry = %q, want review-first", got)
	}

	h.settleWithFinding("review-first", "FINDING-FIRST")
	if kept := h.filter(holds, ""); !kept["review-held"] {
		t.Fatal("review-held still held after its holder settled")
	}
	if len(holds.held) != 0 {
		t.Fatalf("hold log kept %v after the hold ended", holds.held)
	}
}

// The held review carries an event naming the running review, and the
// stalled-PR check counts it as a review in progress at its head.
func TestReviewHeldEventNamesHolderAndIsNotStalled(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-running", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-running")
	h.enqueue("review-held", "owner/repo", 12, overlapHeadB)

	h.pass()
	h.pass()
	events := h.events("review-held", reviewHeldForRunningReviewEventKind)
	if len(events) != 1 || !strings.Contains(events[0].Message, "review-running") {
		t.Fatalf("hold events = %+v, want exactly one naming review-running", events)
	}

	reviews, err := prStalledReviewsAtHead(h.ctx, h.store, "owner/repo", []int{12})
	if err != nil {
		t.Fatal(err)
	}
	review := reviews[prHeadKey(12, overlapHeadB)]
	facts := greenStallFacts(5 * time.Hour)
	facts.PR.Number, facts.PR.HeadSHA, facts.Level = 12, overlapHeadB, reviewlevel.LevelRequired
	facts.ReviewRunning, facts.Verdict, facts.VerdictAt = review.running, review.verdict, review.verdictAt
	if stall, stalled := classifyStalledPR(facts, config.DefaultPRStalledPolicy(), stallNow); stalled {
		t.Fatalf("PR with a held review at its head classified stalled: %+v", stall)
	}
}
