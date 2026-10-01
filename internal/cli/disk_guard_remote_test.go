package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const diskGuardRemoteHead = "0123456789abcdef0123456789abcdef01234567"

// diskGuardRemoteFixture is a daemon home whose disk is below the guard floor,
// with a remote-capable reviewer and a [remote_exec] cost cap of maxConcurrent
// attempts at $1 each.
func diskGuardRemoteFixture(t *testing.T, remoteReviews bool, maxConcurrent int) (context.Context, *db.Store, jobWorker) {
	t.Helper()
	ctx, store, paths, worker := diskGuardDispatchFixture(t)
	if ok, err := store.TransitionJobState(ctx, "disk-guard-job", string(workflow.JobQueued), string(workflow.JobCancelled)); err != nil || !ok {
		t.Fatalf("drop fixture job: ok=%v err=%v", ok, err)
	}
	f, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\n[disk_guard]\nremote_reviews = %t\n\n[remote_exec]\nbackend = \"local\"\ncost_max_reserved_usd = 100\ncost_per_attempt_usd = 1\ncost_max_concurrent = %d\n", remoteReviews, maxConcurrent)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	worker.ConfigHomeExplicit = true
	worker.ConfigHome = filepath.Dir(paths.Home)
	for name, rt := range map[string]string{"rev-omp": "omp", "rev-claude": "claude"} {
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: rt}); err != nil {
			t.Fatalf("UpsertAgent %s: %v", name, err)
		}
	}
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 100 << 30, FreeBytes: 1 << 30}, nil
	})
	return ctx, store, worker
}

func queueDiskGuardJob(t *testing.T, ctx context.Context, store *db.Store, id, agent, jobType, extra string) {
	t.Helper()
	payload := fmt.Sprintf(`{"repo":"owner/repo","pull_request":7,"head_sha":%q%s}`, diskGuardRemoteHead, extra)
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: id, Agent: agent, Type: jobType, State: string(workflow.JobQueued), Payload: payload},
		db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent %s: %v", id, err)
	}
}

func pendingIDs(t *testing.T, ctx context.Context, worker jobWorker) []string {
	t.Helper()
	pending, err := listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs: %v", err)
	}
	ids := make([]string, 0, len(pending))
	for _, job := range pending {
		ids = append(ids, job.ID)
	}
	return ids
}

func storedPayload(t *testing.T, ctx context.Context, store *db.Store, id string) workflow.JobPayload {
	t.Helper()
	job, err := store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func storedBackend(t *testing.T, ctx context.Context, store *db.Store, id string) string {
	t.Helper()
	backend, _ := storedPayload(t, ctx, store, id).ExecBackendOverride()
	return backend
}

func TestLowDiskSendsReviewsRemoteAndKeepsEverythingElseWaiting(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	queueDiskGuardJob(t, ctx, store, "review-local", "rev-omp", "review", "")
	queueDiskGuardJob(t, ctx, store, "implement", "rev-omp", "implement", "")
	queueDiskGuardJob(t, ctx, store, "review-claude", "rev-claude", "review", "")
	queueDiskGuardJob(t, ctx, store, "review-pinned-local", "rev-omp", "review", `,"exec_backend":"local"`)
	queueDiskGuardJob(t, ctx, store, "review-no-head", "rev-omp", "review", `,"head_sha":""`)

	got := pendingIDs(t, ctx, worker)
	if strings.Join(got, ",") != "review-local" {
		t.Fatalf("pending while disk low = %v, want only the remote-capable review", got)
	}
	if b := storedBackend(t, ctx, store, "review-local"); b != "remote" {
		t.Fatalf("routed review stored backend %q, want remote (the worker reads the stored payload)", b)
	}
	if p := storedPayload(t, ctx, store, "review-local"); !p.PolicyRoutedReview || !p.DiskGuardRouted {
		t.Fatalf("routed review payload %+v: want green-CI admission (policy_routed_review) and the disk_guard_routed mark", p)
	}
	for _, id := range []string{"implement", "review-claude", "review-pinned-local", "review-no-head"} {
		if b := storedBackend(t, ctx, store, id); b == "remote" {
			t.Fatalf("%s was switched to remote", id)
		}
	}
	events, _ := store.ListJobEvents(ctx, "review-local")
	routed := 0
	for _, e := range events {
		if e.Kind == diskGuardRoutedRemoteEventKind {
			routed++
		}
		if e.Kind == diskGuardRefusalEventKind {
			t.Fatalf("routed review also got a disk-guard refusal event")
		}
	}
	if routed != 1 {
		t.Fatalf("%d %s events, want 1", routed, diskGuardRoutedRemoteEventKind)
	}
	if events, _ := store.ListJobEvents(ctx, "implement"); !hasEventKind(events, diskGuardRefusalEventKind) {
		t.Fatalf("waiting implement job lacks the disk-guard refusal event")
	}
}

func TestLowDiskRoutesNoMoreReviewsThanTheCostCapAdmits(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 2)
	// One cloud attempt is already billing, and one remote review is running
	// but has not reserved yet: no slot is left for the three queued reviews.
	queueDiskGuardJob(t, ctx, store, "billing", "rev-omp", "review", `,"exec_backend":"remote"`)
	if err := store.ReserveExecBackendAttempt(ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "billing", Attempt: 1},
		Provider:              "e2b", DaemonFencingToken: "t", BootID: "b",
		TTLExpiresAt: time.Now().Add(time.Hour), CostReservedUSD: 1,
	}, db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 100, PerAttemptUSD: 1, MaxConcurrent: 2}); err != nil {
		t.Fatalf("ReserveExecBackendAttempt: %v", err)
	}
	queueDiskGuardJob(t, ctx, store, "starting", "rev-omp", "review", `,"exec_backend":"remote"`)
	for _, id := range []string{"billing", "starting"} {
		if ok, err := store.TransitionJobState(ctx, id, string(workflow.JobQueued), string(workflow.JobRunning)); err != nil || !ok {
			t.Fatalf("start %s: ok=%v err=%v", id, ok, err)
		}
	}
	for i := 1; i <= 3; i++ {
		queueDiskGuardJob(t, ctx, store, fmt.Sprintf("review-%d", i), "rev-omp", "review", "")
	}
	if got := pendingIDs(t, ctx, worker); len(got) != 0 {
		t.Fatalf("pending with a full cost cap = %v, want none", got)
	}

	// The running review finishes: exactly one slot opens, for one review.
	if ok, err := store.TransitionJobState(ctx, "starting", string(workflow.JobRunning), string(workflow.JobSucceeded)); err != nil || !ok {
		t.Fatalf("finish starting: ok=%v err=%v", ok, err)
	}
	got := pendingIDs(t, ctx, worker)
	if len(got) != 1 {
		t.Fatalf("pending with one free slot = %v, want exactly one review", got)
	}
	remote := 0
	for i := 1; i <= 3; i++ {
		if storedBackend(t, ctx, store, fmt.Sprintf("review-%d", i)) == "remote" {
			remote++
		}
	}
	if remote != 1 {
		t.Fatalf("%d reviews switched to remote with one free slot, want 1", remote)
	}
}

func TestLowDiskWithoutTheSwitchStillPausesEveryReview(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, false, 4)
	queueDiskGuardJob(t, ctx, store, "review-local", "rev-omp", "review", "")
	if got := pendingIDs(t, ctx, worker); len(got) != 0 {
		t.Fatalf("pending with remote_reviews off = %v, want none", got)
	}
	if b := storedBackend(t, ctx, store, "review-local"); b == "remote" {
		t.Fatalf("review switched to remote with remote_reviews off")
	}
}

func TestLowDiskDoesNotRerouteAReviewThatAlreadyUsedItsCloudAttempt(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	queueDiskGuardJob(t, ctx, store, "retried", "rev-omp", "review", "")
	if err := store.ReserveExecBackendAttempt(ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "retried", Attempt: 1},
		Provider:              "e2b", DaemonFencingToken: "t", BootID: "b",
		TTLExpiresAt: time.Now().Add(time.Hour), CostReservedUSD: 1,
	}, db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 100, PerAttemptUSD: 1, MaxConcurrent: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkExecBackendAttemptFailed(ctx, db.ExecBackendAttemptKey{JobID: "retried", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, ctx, worker); len(got) != 0 {
		t.Fatalf("pending = %v; remote admission would refuse (and fail) a second cloud attempt", got)
	}
}

func TestDiskGuardRemoteReviewsConfigParses(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[disk_guard]\nremote_reviews = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := config.LoadDiskGuardPolicy(paths)
	if err != nil || !policy.RemoteReviews {
		t.Fatalf("remote_reviews = true parsed as %+v, err %v", policy, err)
	}
	if def := config.DefaultDiskGuardPolicy(); def.RemoteReviews {
		t.Fatal("remote_reviews must default to off: it spends money")
	}
}

func hasEventKind(events []db.JobEvent, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// Switching is durable, so it must not happen while the daemon is draining:
// otherwise a review that was only waiting would run (and bill) remotely after
// drain clears, even if the disk had recovered by then.
func TestLowDiskDoesNotSwitchReviewsWhileDraining(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	queueDiskGuardJob(t, ctx, store, "review-local", "rev-omp", "review", "")
	worker.DrainSentinelPath = filepath.Join(t.TempDir(), "drain")
	if err := os.WriteFile(worker.DrainSentinelPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, ctx, worker); len(got) != 0 {
		t.Fatalf("pending while draining = %v", got)
	}
	if b := storedBackend(t, ctx, store, "review-local"); b == "remote" {
		t.Fatal("review switched to remote while the daemon was draining")
	}
}

// A review whose remote run was refused once waits locally from then on.
func TestLowDiskDoesNotSwitchADeclinedReviewAgain(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	queueDiskGuardJob(t, ctx, store, "declined", "rev-omp", "review", `,"disk_guard_route_declined":true`)
	if got := pendingIDs(t, ctx, worker); len(got) != 0 || storedBackend(t, ctx, store, "declined") == "remote" {
		t.Fatalf("declined review switched again (pending %v)", got)
	}
}

// Every pre-delivery failure path ends in finishQueuedJob (or, once admitted,
// finishAdmittedReviewJob). For a review the disk guard switched, that must put
// it back to waiting locally, whatever the cause, including host checkout
// validation after the worktree was allocated (#2273 review).
func TestFailingADiskRoutedReviewBeforeDeliveryPutsItBackToWaiting(t *testing.T) {
	for _, state := range []workflow.JobState{workflow.JobFailed, workflow.JobBlocked} {
		t.Run(string(state), func(t *testing.T) {
			ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
			queueDiskGuardJob(t, ctx, store, "routed", "rev-omp", "review", "")
			queueDiskGuardJob(t, ctx, store, "plain", "rev-omp", "review", `,"exec_backend":"local"`)
			if got := pendingIDs(t, ctx, worker); len(got) != 1 {
				t.Fatalf("pending = %v, want the routed review", got)
			}
			for _, id := range []string{"routed", "plain"} {
				job, err := store.GetJob(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if err := worker.finishQueuedJob(ctx, job, state, errors.New("checkout validation: git status failed: no space left on device")); err != nil {
					t.Fatal(err)
				}
			}
			routed, _ := store.GetJob(ctx, "routed")
			if routed.State != string(workflow.JobQueued) {
				t.Fatalf("routed review is %s, want queued (waiting locally)", routed.State)
			}
			p := storedPayload(t, ctx, store, "routed")
			if _, present := p.ExecBackendOverride(); present || p.DiskGuardRouted || p.PolicyRoutedReview || !p.DiskGuardRouteDeclined {
				t.Fatalf("payload after undo %+v", p)
			}
			if plain, _ := store.GetJob(ctx, "plain"); plain.State != string(state) {
				t.Fatalf("a review the guard did not switch is %s, want %s: the undo is only for switched reviews", plain.State, state)
			}
		})
	}
}

// A switched review that ends any other way (e.g. cancelled by an operator)
// keeps that outcome: only a failure or block is turned back into waiting.
func TestUndoingADiskRouteIsOnlyForFailures(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	queueDiskGuardJob(t, ctx, store, "routed", "rev-omp", "review", "")
	if got := pendingIDs(t, ctx, worker); len(got) != 1 {
		t.Fatalf("pending = %v", got)
	}
	job, err := store.GetJob(ctx, "routed")
	if err != nil {
		t.Fatal(err)
	}
	if worker.undoDiskGuardRoute(ctx, job, workflow.JobCancelled, false, errors.New("operator cancel")) {
		t.Fatal("a cancelled switched review was put back to waiting")
	}
	if b := storedBackend(t, ctx, store, "routed"); b != "remote" {
		t.Fatalf("backend %q after a non-failure outcome, want it left as remote", b)
	}
}

// An undone route must keep what pre-delivery steps stored (a native review's
// allocated worktree) and must not post a result comment for a review that is
// only waiting again (#2273 review).
func TestUndoingADiskRouteKeepsTheStoredWorktreeAndPostsNoResult(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	queueDiskGuardJob(t, ctx, store, "routed", "rev-omp", "review", "")
	if got := pendingIDs(t, ctx, worker); len(got) != 1 {
		t.Fatalf("pending = %v", got)
	}
	snapshot, err := store.GetJob(ctx, "routed")
	if err != nil {
		t.Fatal(err)
	}
	// The worker allocates a worktree and persists its path, then checkout
	// validation fails with the snapshot it read before allocation.
	allocated := storedPayload(t, ctx, store, "routed")
	allocated.WorktreePath = "/worktrees/routed"
	allocated.ReadOnlyWorktree = true
	encoded, err := json.Marshal(allocated)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateJobPayload(ctx, "routed", string(encoded)); err != nil {
		t.Fatal(err)
	}
	comments := &cliPollFakeGitHub{}
	worker.CommenterFactory = func(string) github.Client { return comments }
	cause := errors.New("checkout validation failed")
	if err := worker.finishQueuedJob(ctx, snapshot, workflow.JobFailed, cause); err != nil {
		t.Fatal(err)
	}
	_ = worker.postJobResultComment(ctx, "routed", runtime.Agent{Name: "rev-omp"}, "", cause)

	after := storedPayload(t, ctx, store, "routed")
	if after.WorktreePath != "/worktrees/routed" || !after.ReadOnlyWorktree {
		t.Fatalf("undo dropped the allocated worktree: %+v", after)
	}
	if len(comments.posted) != 0 {
		t.Fatalf("posted %d result comments for a review that is waiting again", len(comments.posted))
	}
}

// A Mac review holds a Mac slot, not an E2B one: with the E2B cap at one
// sandbox, a running opted-in Mac review and its Mac attempt must not stop the
// disk guard from sending a waiting review to E2B. The routed review stays on
// E2B (no exec_provider): nothing is routed to the Mac automatically.
func TestLowDiskRoutingIgnoresMacReviewsInTheE2BCap(t *testing.T) {
	ctx, store, worker := diskGuardRemoteFixture(t, true, 1)
	queueDiskGuardJob(t, ctx, store, "mac-review", "rev-omp", "review", `,"exec_backend":"remote","exec_provider":"mac"`)
	if ok, err := store.TransitionJobState(ctx, "mac-review", string(workflow.JobQueued), string(workflow.JobRunning)); err != nil || !ok {
		t.Fatalf("start mac-review: ok=%v err=%v", ok, err)
	}
	queueDiskGuardJob(t, ctx, store, "review-local", "rev-omp", "review", "")
	if got := pendingIDs(t, ctx, worker); strings.Join(got, ",") != "review-local" {
		t.Fatalf("pending while a Mac review is running = %v, want the review routed to E2B", got)
	}
	if p := storedPayload(t, ctx, store, "review-local"); p.ExecBackend != "remote" || p.ExecProvider != "" {
		t.Fatalf("routed review backend/provider = %q/%q, want remote on the default E2B", p.ExecBackend, p.ExecProvider)
	}

	// Once the Mac review holds a Mac attempt, that attempt still leaves E2B alone.
	if ok, err := store.TransitionJobState(ctx, "review-local", string(workflow.JobQueued), string(workflow.JobCancelled)); err != nil || !ok {
		t.Fatalf("cancel review-local: ok=%v err=%v", ok, err)
	}
	if err := store.ReserveExecBackendAttempt(ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "mac-review", Attempt: 1},
		Provider:              "mac", DaemonFencingToken: "t", BootID: "b", TTLExpiresAt: time.Now().Add(time.Hour),
	}, db.ExecBackendCostCap{Configured: true, CapacityOnly: true, MaxConcurrent: 1}); err != nil {
		t.Fatalf("ReserveExecBackendAttempt: %v", err)
	}
	queueDiskGuardJob(t, ctx, store, "review-next", "rev-omp", "review", "")
	if got := pendingIDs(t, ctx, worker); strings.Join(got, ",") != "review-next" {
		t.Fatalf("pending while a Mac attempt is billing = %v, want the review routed to E2B", got)
	}
}
