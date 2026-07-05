package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// heartbeatScanFixture writes a config (DefaultConfig + body), initializes it, and
// opens a store, returning both for a runHeartbeatScanOnce test.
func heartbeatScanFixture(t *testing.T, body string) (config.Paths, *db.Store) {
	t.Helper()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	// Register the heartbeat target repo as a managed (enabled + checked-out) repo
	// so the heartbeat repo guard treats it as runnable. Tests that exercise the
	// unmanaged-repo path register their own repo state instead.
	if err := store.UpsertRepo(context.Background(), db.Repo{
		Owner: "jerryfane", Name: "gitmoot", CheckoutPath: t.TempDir(),
	}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	return paths, store
}

// recordingEnqueuer captures every request and returns a synthetic job.
func recordingEnqueuer() (heartbeatEnqueuer, *[]workflow.JobRequest) {
	var seen []workflow.JobRequest
	enq := func(_ context.Context, request workflow.JobRequest) (db.Job, error) {
		seen = append(seen, request)
		return db.Job{ID: request.ID, Agent: request.Agent, Type: request.Action, State: string(workflow.JobQueued)}, nil
	}
	return enq, &seen
}

const enabledHeartbeatBody = `
[agents.repo-maintainer]
runtime = "codex"
role = "repo-maintainer"
max_background = 1

[agents.repo-maintainer.heartbeats.daily]
enabled = true
repo = "jerryfane/gitmoot"
interval = "24h"
prompt = "Review open issues and PRs."
max_concurrent = 1
`

// TestHeartbeatScanOffByDefault is the off-by-default invariant: a config with no
// heartbeat sections must enqueue nothing and write no heartbeat_state row.
func TestHeartbeatScanOffByDefault(t *testing.T) {
	paths, store := heartbeatScanFixture(t, `
[agents.repo-maintainer]
runtime = "codex"
role = "repo-maintainer"
`)
	ctx := context.Background()
	enq := func(context.Context, workflow.JobRequest) (db.Job, error) {
		t.Fatal("enqueuer must not be called when no heartbeats are configured")
		return db.Job{}, nil
	}
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, time.Now().UTC()); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if _, found, err := store.GetHeartbeatState(ctx, "repo-maintainer", "daily"); err != nil || found {
		t.Fatalf("expected no heartbeat_state row, found=%v err=%v", found, err)
	}
}

// TestHeartbeatScanEnqueuesDueJob proves a due heartbeat enqueues exactly one job
// shaped like dispatchLocalAgentJob (sender=heartbeat, action=ask, fingerprint),
// and advances next_due so a same-now rescan does NOT duplicate (restart dedup).
func TestHeartbeatScanEnqueuesDueJob(t *testing.T) {
	paths, store := heartbeatScanFixture(t, enabledHeartbeatBody)
	ctx := context.Background()
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)

	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 enqueued job, got %d", len(*seen))
	}
	req := (*seen)[0]
	if req.Agent != "repo-maintainer" || req.Action != "ask" || req.Sender != "heartbeat" ||
		req.Repo != "jerryfane/gitmoot" || req.Instructions != "Review open issues and PRs." ||
		req.Fingerprint != "heartbeat:repo-maintainer/daily" {
		t.Fatalf("unexpected request shape: %+v", req)
	}
	state, found, err := store.GetHeartbeatState(ctx, "repo-maintainer", "daily")
	if err != nil || !found {
		t.Fatalf("expected state row after enqueue, found=%v err=%v", found, err)
	}
	wantDue := now.Add(24 * time.Hour)
	if !state.NextDueAt.Equal(wantDue) || state.LastStatus != "enqueued" || state.LastJobID != req.ID {
		t.Fatalf("state not advanced: %+v (want due %s)", state, wantDue)
	}

	// Restart dedup: a second scan at the SAME now is not yet due → no new job.
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce rescan: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("rescan duplicated the job: %d enqueues", len(*seen))
	}
}

// TestHeartbeatScanSkipsDisabled proves a disabled heartbeat never runs and never
// writes state.
func TestHeartbeatScanSkipsDisabled(t *testing.T) {
	paths, store := heartbeatScanFixture(t, `
[agents.repo-maintainer]
runtime = "codex"
role = "repo-maintainer"

[agents.repo-maintainer.heartbeats.daily]
enabled = false
repo = "jerryfane/gitmoot"
interval = "24h"
prompt = "p"
`)
	ctx := context.Background()
	enq, seen := recordingEnqueuer()
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, time.Now().UTC()); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("disabled heartbeat enqueued %d jobs", len(*seen))
	}
	if _, found, _ := store.GetHeartbeatState(ctx, "repo-maintainer", "daily"); found {
		t.Fatalf("disabled heartbeat wrote state")
	}
}

// TestHeartbeatScanSkipsNotDue proves a future next_due is honored (no enqueue).
func TestHeartbeatScanSkipsNotDue(t *testing.T) {
	paths, store := heartbeatScanFixture(t, enabledHeartbeatBody)
	ctx := context.Background()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := store.UpsertHeartbeatState(ctx, db.HeartbeatState{
		Agent: "repo-maintainer", Name: "daily", NextDueAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	enq, seen := recordingEnqueuer()
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("not-due heartbeat enqueued %d jobs", len(*seen))
	}
}

// TestHeartbeatScanSkipsOnActiveJob proves overlap protection: an active job
// carrying the heartbeat fingerprint (>= max_concurrent) blocks a new enqueue and
// does NOT advance next_due (so it retries next tick).
func TestHeartbeatScanSkipsOnActiveJob(t *testing.T) {
	paths, store := heartbeatScanFixture(t, enabledHeartbeatBody)
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{
		ID: "active-1", Agent: "repo-maintainer", Type: "ask", State: "running",
		Payload: `{"fingerprint":"heartbeat:repo-maintainer/daily"}`,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("overlap not respected: enqueued %d jobs", len(*seen))
	}
	if _, found, _ := store.GetHeartbeatState(ctx, "repo-maintainer", "daily"); found {
		t.Fatalf("overlap skip must not advance/write state")
	}
}

// TestHeartbeatScanSkipsAtMaxBackground proves agent capacity is respected: when
// the agent is already at max_background, the heartbeat skips this tick without
// advancing. The blocking jobs carry a DIFFERENT fingerprint so the overlap guard
// passes and the max_background guard is the one under test.
func TestHeartbeatScanSkipsAtMaxBackground(t *testing.T) {
	paths, store := heartbeatScanFixture(t, enabledHeartbeatBody) // max_background = 1
	ctx := context.Background()
	if err := store.CreateJob(ctx, db.Job{
		ID: "busy-1", Agent: "repo-maintainer", Type: "ask", State: "running",
		Payload: `{"fingerprint":"some-other-work"}`,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("max_background not respected: enqueued %d jobs", len(*seen))
	}
	if _, found, _ := store.GetHeartbeatState(ctx, "repo-maintainer", "daily"); found {
		t.Fatalf("capacity skip must not advance/write state")
	}
}

// TestHeartbeatScanSkipsUnmanagedRepo proves a heartbeat targeting a repo the
// daemon does not manage (not registered / disabled / no checkout) does NOT
// enqueue a job (which no worker would ever claim, wedging the heartbeat), but
// DOES advance next_due with last_status=repo_unmanaged so it self-recovers.
func TestHeartbeatScanSkipsUnmanagedRepo(t *testing.T) {
	paths, store := heartbeatScanFixture(t, enabledHeartbeatBody)
	ctx := context.Background()
	// Disable the registered repo so it is no longer a runnable target.
	if err := store.SetRepoEnabled(ctx, "jerryfane/gitmoot", false); err != nil {
		t.Fatalf("SetRepoEnabled: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("unmanaged-repo heartbeat enqueued %d jobs (zombie risk)", len(*seen))
	}
	state, found, err := store.GetHeartbeatState(ctx, "repo-maintainer", "daily")
	if err != nil || !found {
		t.Fatalf("expected state row after unmanaged skip, found=%v err=%v", found, err)
	}
	if state.LastStatus != "repo_unmanaged" {
		t.Fatalf("last_status = %q, want repo_unmanaged", state.LastStatus)
	}
	if !state.NextDueAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("unmanaged skip must advance next_due (no wedge): %+v", state)
	}
}

const implementHeartbeatBody = `
[agents.builder]
runtime = "codex"
role = "builder"
capabilities = ["ask", "implement"]
max_background = 2

[agents.builder.heartbeats.nightly]
enabled = true
repo = "jerryfane/gitmoot"
interval = "24h"
action = "implement"
prompt = "Fix the top lint error and open a small PR."
max_concurrent = 1
`

// TestHeartbeatScanEnqueuesImplementJob proves a policy-gated implement heartbeat
// enqueues an Action="implement" job when its agent holds the implement capability
// AND a write-granting autonomy policy (#611).
func TestHeartbeatScanEnqueuesImplementJob(t *testing.T) {
	paths, store := heartbeatScanFixture(t, implementHeartbeatBody)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "builder", Runtime: "codex", RepoScope: "jerryfane/gitmoot",
		Capabilities: []string{"ask", "implement"}, AutonomyPolicy: "danger-full-access", RuntimeRef: "last",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 implement job, got %d", len(*seen))
	}
	if req := (*seen)[0]; req.Action != "implement" || req.Agent != "builder" || req.Sender != "heartbeat" {
		t.Fatalf("unexpected implement request shape: %+v", req)
	}
	state, found, err := store.GetHeartbeatState(ctx, "builder", "nightly")
	if err != nil || !found || state.LastStatus != "enqueued" {
		t.Fatalf("implement heartbeat state not advanced: found=%v err=%v state=%+v", found, err, state)
	}
}

// TestHeartbeatScanImplementRejectsReadOnlyPolicy proves a due implement heartbeat
// for an agent that holds the implement capability but carries a read-only-ish
// policy (the default auto) NO-OPs: it never enqueues, and advances next_due with
// last_status=policy_readonly so it self-recovers once a write policy is granted.
func TestHeartbeatScanImplementRejectsReadOnlyPolicy(t *testing.T) {
	paths, store := heartbeatScanFixture(t, implementHeartbeatBody)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "builder", Runtime: "codex", RepoScope: "jerryfane/gitmoot",
		Capabilities: []string{"ask", "implement"}, AutonomyPolicy: "auto", RuntimeRef: "last",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("implement heartbeat under read-only policy enqueued %d jobs", len(*seen))
	}
	state, found, err := store.GetHeartbeatState(ctx, "builder", "nightly")
	if err != nil || !found {
		t.Fatalf("expected state row, found=%v err=%v", found, err)
	}
	if state.LastStatus != "policy_readonly" {
		t.Fatalf("last_status = %q, want policy_readonly", state.LastStatus)
	}
	if !state.NextDueAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("policy skip must advance next_due (no wedge): %+v", state)
	}
}

// TestHeartbeatScanImplementRejectsMissingCapability proves an implement heartbeat
// for a write-policy agent that nonetheless LACKS the implement capability also
// no-ops with last_status=policy_readonly.
func TestHeartbeatScanImplementRejectsMissingCapability(t *testing.T) {
	paths, store := heartbeatScanFixture(t, implementHeartbeatBody)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "builder", Runtime: "codex", RepoScope: "jerryfane/gitmoot",
		Capabilities: []string{"ask"}, AutonomyPolicy: "danger-full-access", RuntimeRef: "last",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("implement heartbeat without capability enqueued %d jobs", len(*seen))
	}
	state, _, err := store.GetHeartbeatState(ctx, "builder", "nightly")
	if err != nil {
		t.Fatalf("GetHeartbeatState: %v", err)
	}
	if state.LastStatus != "policy_readonly" {
		t.Fatalf("last_status = %q, want policy_readonly", state.LastStatus)
	}
}

const runtimeOverrideHeartbeatBody = `
[agents.repo-maintainer]
runtime = "codex"
role = "repo-maintainer"
max_background = 1

[agents.repo-maintainer.heartbeats.daily]
enabled = true
repo = "jerryfane/gitmoot"
interval = "24h"
action = "ask"
runtime = "claude"
prompt = "Review open issues and PRs."
max_concurrent = 1
`

// TestHeartbeatScanRuntimeOverride proves a per-heartbeat runtime override (#611)
// flows onto the enqueued job as a RuntimeOverride plus a freshly-minted override
// ref, so the scheduled job runs on the named runtime on its own session.
func TestHeartbeatScanRuntimeOverride(t *testing.T) {
	paths, store := heartbeatScanFixture(t, runtimeOverrideHeartbeatBody)
	ctx := context.Background()
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 job, got %d", len(*seen))
	}
	req := (*seen)[0]
	if req.RuntimeOverride != "claude" {
		t.Fatalf("expected RuntimeOverride=claude, got %q", req.RuntimeOverride)
	}
	if req.RuntimeOverrideRef == "" {
		t.Fatalf("expected a minted RuntimeOverrideRef for the override, got empty")
	}
}

// TestHeartbeatScanNoRuntimeOverrideByDefault is the byte-identical-default guard:
// a heartbeat WITHOUT a runtime override enqueues a job carrying no override, so
// it runs on the agent default exactly as before #611.
func TestHeartbeatScanNoRuntimeOverrideByDefault(t *testing.T) {
	paths, store := heartbeatScanFixture(t, enabledHeartbeatBody)
	ctx := context.Background()
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 job, got %d", len(*seen))
	}
	if req := (*seen)[0]; req.RuntimeOverride != "" || req.RuntimeOverrideRef != "" {
		t.Fatalf("default heartbeat must carry no runtime override: %+v", req)
	}
}

const reviewHeartbeatBody = `
[agents.reviewer]
runtime = "codex"
role = "reviewer"
capabilities = ["ask", "review"]
max_background = 2

[agents.reviewer.heartbeats.stale-prs]
enabled = true
repo = "jerryfane/gitmoot"
interval = "12h"
action = "review"
prompt = "Review stale open PRs."
max_concurrent = 1
`

// TestHeartbeatScanEnqueuesReviewJob proves a review heartbeat whose target agent
// HOLDS the review capability enqueues exactly one Action="review" job.
func TestHeartbeatScanEnqueuesReviewJob(t *testing.T) {
	paths, store := heartbeatScanFixture(t, reviewHeartbeatBody)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "reviewer", Runtime: "codex", RepoScope: "jerryfane/gitmoot",
		Capabilities: []string{"ask", "review"}, RuntimeRef: "last",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 review job, got %d", len(*seen))
	}
	if req := (*seen)[0]; req.Action != "review" || req.Agent != "reviewer" || req.Sender != "heartbeat" {
		t.Fatalf("unexpected review request shape: %+v", req)
	}
	state, found, err := store.GetHeartbeatState(ctx, "reviewer", "stale-prs")
	if err != nil || !found || state.LastStatus != "enqueued" {
		t.Fatalf("review heartbeat state not advanced: found=%v err=%v state=%+v", found, err, state)
	}
}

// TestHeartbeatScanReviewRejectsMissingCapability proves a review heartbeat for an
// agent that LACKS the review capability never enqueues, and advances next_due
// with last_status=capability_missing so it self-recovers (no wedge, no hot-loop).
func TestHeartbeatScanReviewRejectsMissingCapability(t *testing.T) {
	paths, store := heartbeatScanFixture(t, reviewHeartbeatBody)
	ctx := context.Background()
	// Register the agent WITHOUT the review capability.
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "reviewer", Runtime: "codex", RepoScope: "jerryfane/gitmoot",
		Capabilities: []string{"ask"}, RuntimeRef: "last",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	enq, seen := recordingEnqueuer()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("review heartbeat without capability enqueued %d jobs", len(*seen))
	}
	state, found, err := store.GetHeartbeatState(ctx, "reviewer", "stale-prs")
	if err != nil || !found {
		t.Fatalf("expected state row, found=%v err=%v", found, err)
	}
	if state.LastStatus != "capability_missing" {
		t.Fatalf("last_status = %q, want capability_missing", state.LastStatus)
	}
	if !state.NextDueAt.Equal(now.Add(12 * time.Hour)) {
		t.Fatalf("capability skip must advance next_due (no wedge): %+v", state)
	}
}

// TestHeartbeatScanCoalescesMissedTicks proves a long outage replays only ONCE:
// next_due is anchored to now (not the stale due time), so one scan after many
// missed intervals enqueues a single job and schedules the next from now.
func TestHeartbeatScanCoalescesMissedTicks(t *testing.T) {
	paths, store := heartbeatScanFixture(t, enabledHeartbeatBody)
	ctx := context.Background()
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	// Seed a next_due 10 days in the past (10 missed daily ticks).
	if err := store.UpsertHeartbeatState(ctx, db.HeartbeatState{
		Agent: "repo-maintainer", Name: "daily", NextDueAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	enq, seen := recordingEnqueuer()
	if err := runHeartbeatScanOnce(ctx, paths, store, enq, now); err != nil {
		t.Fatalf("runHeartbeatScanOnce: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected a single coalesced run, got %d", len(*seen))
	}
	state, _, err := store.GetHeartbeatState(ctx, "repo-maintainer", "daily")
	if err != nil {
		t.Fatalf("GetHeartbeatState: %v", err)
	}
	if !state.NextDueAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("next_due not re-anchored to now: %+v", state)
	}
}
