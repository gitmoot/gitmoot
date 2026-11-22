package cli

import (
	"context"
	"encoding/json"
	"io"
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
	return runtime.Result{Raw: `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":["inspection"],"needs":[]}}`}, nil
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

// filter runs the dispatch listing once without dispatching, with holds as the
// scheduler's hold log, and returns the ids it lets through.
func (h *overlapHoldHarness) filter(holds *reviewHoldLog, rootFilter string) map[string]bool {
	h.t.Helper()
	worker := h.worker
	worker.reviewHolds = holds
	kept, err := listPendingQueuedJobs(h.ctx, worker, "", rootFilter, true)
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

// Reviews of several heads held by one running review stay queued. Once it
// ends the scheduler starts one head at a time: a selection admits one head per
// pull request, and a dispatched review, registered until it is reaped, holds
// the other heads before it is claimed. So the later head's prior verdicts
// carry both earlier reviews' verdicts.
func TestReviewHoldDispatchesHeldHeadsInQueueOrder(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-a", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-a")
	h.enqueue("review-b", "owner/repo", 12, overlapHeadB)
	h.enqueue("review-c", "owner/repo", 12, overlapHeadC)

	h.pass()
	for _, id := range []string{"review-b", "review-c"} {
		if _, delivered := h.delivery(id); delivered {
			t.Fatalf("%s dispatched while review-a was running", id)
		}
		if got := h.state(id); got != string(workflow.JobQueued) {
			t.Fatalf("held %s state = %q, want queued: the hold must not cancel", id, got)
		}
	}

	h.settleWithFinding("review-a", "FINDING-A")
	pending, err := listPendingQueuedJobs(h.ctx, h.worker, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	queued, remaining := selectRunnableQueuedJobs(h.ctx, h.store, pending, 4)
	if len(queued) != 1 || queued[0].ID != "review-b" || len(remaining) != 1 || remaining[0].ID != "review-c" {
		t.Fatalf("selection = %v, remaining = %v; want review-b alone, one head per pull request", queued, remaining)
	}
	registered := h.worker
	registered.dispatchedReviews = newReviewDispatchRegistry()
	registered.dispatchedReviews.register(queued[0])
	if again, err := listPendingQueuedJobs(h.ctx, registered, "", "", false); err != nil || len(again) != 1 || again[0].ID != "review-b" {
		t.Fatalf("listed %v (err %v) while review-b was dispatched and not yet claimed; want review-b alone", again, err)
	}
	registered.dispatchedReviews.release("review-b")
	if again, err := listPendingQueuedJobs(h.ctx, registered, "", "", false); err != nil || len(again) != 2 {
		t.Fatalf("listed %v (err %v) after review-b was released; want both", again, err)
	}

	h.pass()
	c, delivered := h.delivery("review-c")
	if _, deliveredB := h.delivery("review-b"); !deliveredB || !delivered {
		t.Fatalf("after review-a settled: review-b delivered=%v review-c delivered=%v, want both", deliveredB, delivered)
	}
	if !strings.Contains(c.priorVerdicts, "FINDING-A") || !strings.Contains(c.priorVerdicts, `"job_id": "review-b"`) {
		t.Fatalf("review-c prior verdicts lack review-a's finding or review-b's verdict (review-b is %s):\n%s", h.state("review-b"), c.priorVerdicts)
	}
}

// Under daemon --session, a queued review outside the session never holds one
// inside it, and a running review holds across scopes. The pass does not touch
// a held review outside its scope.
func TestReviewHoldIgnoresQueuedReviewsOutsideTheSession(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-outside", "owner/repo", 12, overlapHeadA)
	h.enqueueRequest(workflow.JobRequest{ID: "review-session", Repo: "owner/repo", PullRequest: 12, HeadSHA: overlapHeadB, RootJobID: "session-b"})

	if kept := h.filter(nil, "session-b"); !kept["review-session"] {
		t.Fatalf("filter kept %v: review-session was held by a review outside its session", kept)
	}

	h.enqueue("review-running", "owner/repo", 12, overlapHeadC)
	h.startRunning("review-running")
	if kept := h.filter(nil, "session-b"); kept["review-session"] {
		t.Fatal("review-session passed while a review of the same PR was running")
	}
	if events := h.events("review-outside", reviewHeldForRunningReviewEventKind); len(events) != 0 {
		t.Fatalf("a session-filtered pass recorded hold events on review-outside: %+v", events)
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

// One event per (held review, holder), deduped by the store even without the
// scheduler's hold log. The log forgets a hold once it ends, and every
// dispatch listing prunes it, an empty queue included.
func TestReviewHoldEventIsKeyedOnHeldAndHolderIdentity(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-first", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-first")
	h.enqueue("review-held", "owner/repo", 12, overlapHeadB)
	holds := newReviewHoldLog()

	// No hold log on the first two passes: the store alone must dedup on the
	// identity, as it does after a daemon restart.
	h.filter(nil, "")
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

	h.enqueue("review-second", "owner/repo", 12, overlapHeadC)
	h.startRunning("review-second")
	h.filter(holds, "")
	if got := holds.held["review-held"]; got != "review-second" {
		t.Fatalf("hold log entry = %q, want review-second", got)
	}
	if moved, err := h.store.TransitionJobState(h.ctx, "review-held", string(workflow.JobQueued), string(workflow.JobCancelled)); err != nil || !moved {
		t.Fatalf("cancel review-held: moved=%v err=%v", moved, err)
	}
	if kept := h.filter(holds, ""); len(kept) != 0 {
		t.Fatalf("listing an empty queue let through %v", kept)
	}
	if len(holds.held) != 0 {
		t.Fatalf("hold log kept %v after its job left an otherwise empty queue", holds.held)
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

// The tracked barrier registers a dispatched review before its goroutine runs
// and releases it when the goroutine ends (#2726): while the review prepares it
// is registered, and once delivered nothing is left behind.
func TestTrackedDispatchRegistersAReviewUntilItEnds(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-x", "owner/repo", 12, overlapHeadA)
	worker := h.worker
	registry := newReviewDispatchRegistry()
	worker.dispatchedReviews = registry
	var registeredWhilePreparing bool
	validate := worker.CheckoutValidator
	worker.CheckoutValidator = func(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent) (string, error) {
		registeredWhilePreparing = len(registry.holders()[reviewOverlapKey("owner/repo", 12)]) == 1
		return validate(ctx, job, payload, agent)
	}
	tracker := newInflightJobTracker(h.ctx)
	if err := dispatchQueuedJobsTracked(h.ctx, worker, 1, 1, "owner/repo", "", tracker); err != nil {
		t.Fatal(err)
	}
	tracker.drain(io.Discard, 10*time.Second)
	if _, delivered := h.delivery("review-x"); !delivered {
		t.Fatal("review-x was not delivered")
	}
	if !registeredWhilePreparing {
		t.Fatal("review-x was not registered while it prepared")
	}
	if left := registry.holders(); len(left) != 0 {
		t.Fatalf("registry kept %v after review-x ended", left)
	}
}
