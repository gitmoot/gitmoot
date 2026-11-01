package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2470. E2B jobs run in a provider sandbox, so they are bounded by
// [remote_exec] cost_max_concurrent, not by --workers. With every host slot
// taken by local work, queued E2B jobs must still be selected and admitted up
// to the remote cap, while further local jobs keep waiting for a host slot.
func TestRemoteLaneAdmitsE2BJobsWhileHostIsFull(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	for _, agent := range []string{"local-a", "local-b", "remote-a", "remote-b", "remote-c", "sandboxd-a"} {
		seedDaemonWorkerAgent(t, store, agent, runtime.ShellRuntime, "unused", []string{"ask", "review"}, "owner/repo")
	}
	remote := "remote"
	enqueue := func(id, agent string, backend *string, provider string) db.Job {
		t.Helper()
		// Review jobs at an exact head get a per-job checkout key, as production
		// E2B reviews do, so the lane split is the only thing under test.
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: agent, Action: "review", Repo: "owner/repo", Branch: "main", PullRequest: 1, HeadSHA: "0123456789abcdef0123456789abcdef01234567", ExecBackend: backend, ExecProvider: provider})
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	pending := []db.Job{
		enqueue("local-1", "local-a", nil, ""),
		enqueue("remote-1", "remote-a", &remote, ""),
		enqueue("local-2", "local-b", nil, ""),
		enqueue("remote-2", "remote-b", &remote, ""),
		enqueue("sandboxd-1", "sandboxd-a", &remote, "sandboxd-linux"),
		enqueue("remote-3", "remote-c", &remote, ""),
	}

	const hostCap = 1
	const repoLimit = 1
	tracker := newInflightJobTracker(ctx)
	defer tracker.drain(io.Discard, 5*time.Second)
	tracker.setRemoteCap(2)
	// The host is full: one local job already in flight on another checkout.
	if !tracker.beginWithin(hostCap, "busy-local", "owner/other", "repo:owner/other", "") {
		t.Fatal("could not seed the in-flight local job")
	}
	defer tracker.end("busy-local")

	// Budgets come from the same helper the pool and barrier passes use, with
	// the repo's LOCAL remainder.
	local, remoteRoom, laneOn := laneBudgets(repoLimit-tracker.inflightLocal("owner/repo"), hostCap, tracker)
	if local != 0 || remoteRoom != 2 || !laneOn {
		t.Fatalf("budgets local=%d remote=%d laneOn=%v, want 0/2/true", local, remoteRoom, laneOn)
	}
	queued, _ := selectRunnableByLane(ctx, store, pending, local, remoteRoom, laneOn, config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue}, nil, nil)
	var ids []string
	for _, job := range queued {
		ids = append(ids, job.ID)
	}
	if want := []string{"remote-1", "remote-2"}; !slices.Equal(ids, want) {
		t.Fatalf("selected %v with the host full, want the first two E2B jobs %v (sandboxd stays on the host lane)", ids, want)
	}
	for _, job := range queued {
		admitted, onRemote := tracker.beginWithinLane(hostCap, queuedJobUsesRemoteLane(job), job.ID, "owner/repo", "", "")
		if !admitted || !onRemote {
			t.Fatalf("remote lane refused %s below its cap (admitted=%v onRemote=%v)", job.ID, admitted, onRemote)
		}
		defer tracker.end(job.ID)
	}
	if admitted, _ := tracker.beginWithinLane(hostCap, true, "remote-3", "owner/repo", "", ""); admitted {
		t.Fatal("remote lane admitted a job past cost_max_concurrent")
	}
	if admitted, _ := tracker.beginWithinLane(hostCap, false, "local-1", "owner/repo", "", ""); admitted {
		t.Fatal("a local job was admitted past --workers because remote jobs were in flight")
	}
	if got := tracker.total(); got != 1 {
		t.Fatalf("host in-flight total = %d, want 1: E2B jobs must not consume host slots", got)
	}
	// Two remote jobs of owner/repo are in flight, yet the per-repo limit (1)
	// bounds host work only: the repo's local budget is still its full limit.
	if got := tracker.inflightLocal("owner/repo"); got != 0 {
		t.Fatalf("inflightLocal(owner/repo) = %d, want 0 with only remote jobs in flight", got)
	}
	tracker.end("busy-local")
	if local, _, _ := laneBudgets(repoLimit-tracker.inflightLocal("owner/repo"), hostCap, tracker); local != 1 {
		t.Fatalf("local budget = %d with an idle host and only remote jobs in flight, want the repo limit 1", local)
	}

	// An E2B job is never isolation-dispatched onto the host lane.
	for _, job := range pending {
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatal(err)
		}
		if queuedJobUsesRemoteLane(job) && poolIsolationCandidate(job, payload) {
			t.Fatalf("E2B job %s is an isolation candidate; isolation takes a host slot", job.ID)
		}
	}

	// With the lane off (no cost_max_concurrent), E2B jobs share --workers as before.
	tracker.setRemoteCap(0)
	if !tracker.beginWithin(hostCap, "busy-local-2", "owner/other", "repo:owner/other", "") {
		t.Fatal("could not refill the host")
	}
	defer tracker.end("busy-local-2")
	if admitted, _ := tracker.beginWithinLane(hostCap, true, "remote-3", "owner/repo", "", ""); admitted {
		t.Fatal("with the remote lane off an E2B job bypassed the full host cap")
	}
}

// #2470. The lane cap follows live [remote_exec] cost_max_concurrent and turns
// the lane off when the config cannot be read.
func TestRefreshRemoteLaneCapFollowsConfig(t *testing.T) {
	home := t.TempDir()
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard, home)
	paths, err := worker.configPaths()
	if err != nil {
		t.Fatal(err)
	}
	tracker := newInflightJobTracker(context.Background())
	defer tracker.drain(io.Discard, time.Second)
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	capNow := func() int { tracker.mu.Lock(); defer tracker.mu.Unlock(); return tracker.remoteCap }
	write("[remote_exec]\ncost_max_reserved_usd = 90.0\ncost_per_attempt_usd = 4.5\ncost_max_concurrent = 20\n")
	refreshRemoteLaneCap(worker, tracker)
	if got := capNow(); got != 20 {
		t.Fatalf("remote cap = %d, want 20 from config", got)
	}
	write("[remote_exec]\ncost_max_concurrent = \"not a number\"\n")
	refreshRemoteLaneCap(worker, tracker)
	if got := capNow(); got != 0 {
		t.Fatalf("remote cap = %d after an unreadable config, want 0 (lane off)", got)
	}
}
