package cli

import (
	"context"
	"io"
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
	tracker := newInflightJobTracker(ctx)
	defer tracker.drain(io.Discard, 5*time.Second)
	tracker.setRemoteCap(2)
	// The host is full: one local job already in flight on another checkout.
	if !tracker.beginWithin(hostCap, "busy-local", "owner/other", "repo:owner/other", "") {
		t.Fatal("could not seed the in-flight local job")
	}
	defer tracker.end("busy-local")

	local, remoteRoom, laneOn := tracker.laneHeadroom(hostCap)
	if local != 0 || remoteRoom != 2 || !laneOn {
		t.Fatalf("headroom local=%d remote=%d laneOn=%v, want 0/2/true", local, remoteRoom, laneOn)
	}
	queued, _ := selectRunnableByLane(ctx, store, pending, 10, local, remoteRoom, laneOn, config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue}, nil, nil)
	var ids []string
	for _, job := range queued {
		ids = append(ids, job.ID)
	}
	if want := []string{"remote-1", "remote-2"}; !slices.Equal(ids, want) {
		t.Fatalf("selected %v with the host full, want the first two E2B jobs %v (sandboxd stays on the host lane)", ids, want)
	}
	for _, job := range queued {
		if !tracker.beginWithinLane(hostCap, queuedJobUsesRemoteLane(job), job.ID, "owner/repo", "", "") {
			t.Fatalf("remote lane refused %s below its cap", job.ID)
		}
		defer tracker.end(job.ID)
	}
	if tracker.beginWithinLane(hostCap, true, "remote-3", "owner/repo", "", "") {
		t.Fatal("remote lane admitted a job past cost_max_concurrent")
	}
	if tracker.beginWithinLane(hostCap, false, "local-1", "owner/repo", "", "") {
		t.Fatal("a local job was admitted past --workers because remote jobs were in flight")
	}
	if got := tracker.total(); got != 1 {
		t.Fatalf("host in-flight total = %d, want 1: E2B jobs must not consume host slots", got)
	}

	// With the lane off (no cost_max_concurrent), E2B jobs share --workers as before.
	tracker.setRemoteCap(0)
	if tracker.beginWithinLane(hostCap, true, "remote-3", "owner/repo", "", "") {
		t.Fatal("with the remote lane off an E2B job bypassed the full host cap")
	}
}
