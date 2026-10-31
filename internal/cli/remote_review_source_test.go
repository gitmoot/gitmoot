package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRemoteReviewsQueueWithoutLocalSourceUnderCleanupHold(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	paths := config.PathsForHome(home)
	ctx := context.Background()
	repo, err := store.GetRepo(ctx, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	repo.CheckoutPath = filepath.Join(home, "source-does-not-exist")
	repo.PrimaryCheckoutPath = repo.CheckoutPath
	if err := store.UpsertRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, paths, "\n[remote_exec]\nbackend = \"local\"\ncost_max_reserved_usd = 100\ncost_per_attempt_usd = 1\ncost_max_concurrent = 2\n")
	enableJanitorCapacity(t, paths)
	device, err := janitorDeviceOf(paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	var sample atomic.Value
	setSample := func(available uint64) {
		volume := fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"state":"cleanup_needed","admission_hold":true,"available_bytes":%d,"reserve_bytes":1048576,"reason":"recovery target not reached"}`, device, device, available)
		sample.Store(janitorStatusJSON(time.Now(), time.Minute, janitorTestPolicyHash, volume))
	}
	setSample(2 << 30)
	previous := runJanitorStatus
	runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
		return []byte(sample.Load().(string)), nil
	}
	t.Cleanup(func() { runJanitorStatus = previous })
	var ids []string
	for _, pr := range []int{12, 13} {
		output, failure := runReviewRequestJSON(t, "--home", home, "--repo", "owner/repo", "--pr", strconv.Itoa(pr),
			"--head", head, "--branch", "feature/review", "--role", "joltra", "--full", "--no-fix-target", "--exec-backend", "remote", "--exec-provider", "e2b", "--json")
		if failure != "" {
			t.Fatal(failure)
		}
		job, err := store.GetJob(ctx, output.JobID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != string(workflow.JobQueued) || payload.HeadSHA != head || payload.PullRequest != pr {
			t.Fatalf("wrong queued subject: job=%+v payload=%+v", job, payload)
		}
		if payload.WorktreePath != "" || payload.ReadOnlyWorktree || !payload.ReadOnlySeat {
			t.Fatalf("remote review acquired local worktree or lost seat isolation: %+v", payload)
		}
		ids = append(ids, job.ID)
	}
	if ids[0] == ids[1] {
		t.Fatal("distinct review subjects shared a job")
	}
	queueDiskGuardJob(t, ctx, store, "held-local-work", "reviewer", "ask", "")
	worker := jobWorker{Store: store, ConfigHome: home, ConfigHomeExplicit: true, Stdout: io.Discard}
	allowed := pendingIDs(t, ctx, worker)
	for _, id := range ids {
		if !slices.Contains(allowed, id) {
			t.Fatalf("review %s blocked under cleanup-only hold: %v", id, allowed)
		}
	}
	if slices.Contains(allowed, "held-local-work") {
		t.Fatal("ordinary local work bypassed the unchanged pressure gate")
	}
	// The reserve is still load-bearing, independently of the cleanup latch.
	setSample(1 << 20)
	allowed = pendingIDs(t, ctx, worker)
	for _, id := range ids {
		if slices.Contains(allowed, id) {
			t.Fatalf("review %s admitted at the reserve", id)
		}
	}
	setSample(2 << 30)
	allowed = pendingIDs(t, ctx, worker)
	for _, id := range ids {
		if !slices.Contains(allowed, id) {
			t.Fatalf("same queued review did not resume: %s", id)
		}
		job, err := store.GetJob(ctx, id)
		if err != nil || job.State != string(workflow.JobQueued) {
			t.Fatalf("queued identity changed: %+v %v", job, err)
		}
	}
}
