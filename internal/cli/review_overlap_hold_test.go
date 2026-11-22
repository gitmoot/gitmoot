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
	// The checkout validator is the first per-job resource step of worker.run
	// (worktree, checkout lock and remote admission all follow it), so a job it
	// never sees took none of them.
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
	enqueueDaemonWorkerJob(h.t, h.store, workflow.JobRequest{
		ID: id, Agent: "reviewer-" + strings.Split(repo, "/")[0], Action: "review", Repo: repo,
		Branch: "feature", PullRequest: pr, HeadSHA: head, ReviewPurpose: "code",
	})
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
// claimed, takes no resources, and dispatches after the first settles, with
// the first review's finding in its prior-verdict list (#2726).
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
		t.Fatal("held review-2 reached checkout validation: a held review must take no resources")
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

// Running A, then head B queued, then head C queued: B is superseded through
// the stale-head path and C runs once A settles.
func TestReviewHoldKeepsOnlyTheNewestHeldHead(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-a", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-a")
	h.enqueue("review-b", "owner/repo", 12, overlapHeadB)
	h.enqueue("review-c", "owner/repo", 12, overlapHeadC)

	h.pass()
	if got := h.state("review-b"); got != string(workflow.JobCancelled) {
		t.Fatalf("older held review-b state = %q, want cancelled (superseded)", got)
	}
	superseded := h.events("review-b", workflow.JobEventSupersededStaleHead)
	if len(superseded) != 1 || !strings.Contains(superseded[0].Message, "review-c") || !strings.Contains(superseded[0].Message, "review-a") {
		t.Fatalf("review-b supersede events = %+v, want one naming review-c and review-a", superseded)
	}
	if got := h.state("review-a"); got != string(workflow.JobRunning) {
		t.Fatalf("running review-a state = %q: a running review must not be superseded by the hold", got)
	}
	if got := h.state("review-c"); got != string(workflow.JobQueued) {
		t.Fatalf("newest held review-c state = %q, want queued", got)
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

// The held review carries one event naming the running review, and the
// stalled-PR check counts it as a review in progress at its head.
func TestReviewHeldEventNamesHolderAndIsNotStalled(t *testing.T) {
	h := newOverlapHoldHarness(t)
	h.enqueue("review-running", "owner/repo", 12, overlapHeadA)
	h.startRunning("review-running")
	h.enqueue("review-held", "owner/repo", 12, overlapHeadB)

	h.pass()
	h.pass()
	events := h.events("review-held", reviewHeldForRunningReviewEventKind)
	if len(events) != 1 || !strings.Contains(events[0].Message, "review-running") || !strings.Contains(events[0].Message, "running") {
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
