package cli

import (
	"context"
	"fmt"
	"testing"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
)

// diamondSpecYAML is a fan-out/fan-in pipeline whose declared (spec) stage order —
// zfetch, ascore, bdedupe, publish — is deliberately NOT alphabetical, so a stage
// list in spec order is provably different from the store's ORDER BY stage_id
// (ascore, bdedupe, publish, zfetch). One cmd carries angle brackets/ampersand to
// prove nothing rewrites the stored command.
const diamondSpecYAML = `name: listing-refresh
repo: jerryfane/noted
stages:
  - id: zfetch
    cmd: ./scripts/fetch.sh
  - id: ascore
    cmd: ./scripts/score.sh --filter "p<95> && q>1"
    needs: [zfetch]
  - id: bdedupe
    cmd: ./scripts/dedupe.sh
    needs: [zfetch]
  - id: publish
    cmd: ./scripts/publish.sh
    needs: [ascore, bdedupe]
`

func openPipelineTestStore(t *testing.T, home string) *db.Store {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func seedTestPipeline(t *testing.T, store *db.Store, p db.Pipeline) {
	t.Helper()
	if p.SpecHash == "" {
		p.SpecHash = pipeline.Hash([]byte(p.SpecYAML))
	}
	if err := store.CreateOrUpdatePipeline(context.Background(), p); err != nil {
		t.Fatalf("CreateOrUpdatePipeline %s: %v", p.Name, err)
	}
}

func seedTestRun(t *testing.T, store *db.Store, run db.PipelineRun, stages []db.PipelineRunStage) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreatePipelineRun(ctx, run); err != nil {
		t.Fatalf("CreatePipelineRun %s: %v", run.ID, err)
	}
	for _, stage := range stages {
		stage.RunID = run.ID
		if err := store.CreatePipelineRunStage(ctx, stage); err != nil {
			t.Fatalf("CreatePipelineRunStage %s/%s: %v", run.ID, stage.StageID, err)
		}
	}
}

// seedDiamondBlockedRun seeds the listing-refresh pipeline plus one blocked run of
// the diamond: zfetch+bdedupe succeeded, ascore BLOCKED with persisted needs, and
// publish SKIPPED. specHash lets a caller force a spec-hash mismatch to exercise
// the stage_id-order fallback; pass "" to use the matching (real) hash.
func seedDiamondBlockedRun(t *testing.T, home, runID, specHash string) {
	t.Helper()
	store := openPipelineTestStore(t, home)
	defer store.Close()

	realHash := pipeline.Hash([]byte(diamondSpecYAML))
	seedTestPipeline(t, store, db.Pipeline{
		Name: "listing-refresh", Repo: "jerryfane/noted", SpecYAML: diamondSpecYAML,
		SpecHash: realHash, Enabled: true,
	})

	runHash := realHash
	if specHash != "" {
		runHash = specHash
	}
	started := time.UnixMilli(1_751_000_000_000).UTC()
	finished := time.UnixMilli(1_751_000_090_000).UTC()
	blockedNeeds := []string{"set R2 token: gitmoot config set r2.token"}
	seedTestRun(t, store,
		db.PipelineRun{
			ID: runID, Pipeline: "listing-refresh", Trigger: "manual", SpecHash: runHash,
			State: pipeline.RunBlocked, HaltStage: "ascore",
			HaltReason: "scoring model needs the R2 token",
			NeedsJSON:  marshalPipelineNeeds(blockedNeeds),
			StartedAt:  started, FinishedAt: finished,
		},
		[]db.PipelineRunStage{
			{StageID: "zfetch", State: pipeline.StageSucceeded, JobID: "job-zfetch", StartedAt: started, FinishedAt: started.Add(10 * time.Second)},
			{StageID: "bdedupe", State: pipeline.StageSucceeded, JobID: "job-bdedupe", StartedAt: started, FinishedAt: started.Add(20 * time.Second)},
			{StageID: "ascore", State: pipeline.StageBlocked, JobID: "job-ascore", Attempt: 1,
				NeedsJSON: marshalPipelineNeeds(blockedNeeds), Summary: "scoring blocked: needs R2 <token> & creds"},
			{StageID: "publish", State: pipeline.StageSkipped},
		})
}

// TestWebDataSourcePipelinesEmpty pins the empty-store contract: a non-nil, empty
// slice (never nil), so the API layer's nil->[] coercion has nothing to do.
func TestWebDataSourcePipelinesEmpty(t *testing.T) {
	home := dashboardTestHome(t)
	ds := &webDataSource{home: home}

	pipelines, err := ds.Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}
	if pipelines == nil {
		t.Fatalf("Pipelines() = nil, want non-nil empty slice")
	}
	if len(pipelines) != 0 {
		t.Fatalf("Pipelines() len = %d, want 0: %+v", len(pipelines), pipelines)
	}
}

// TestWebDataSourcePipelines pins the list mapping: name order, schedule-state
// field mapping (including the two time.Time -> epoch-ms conversions), the Recent
// cap at 10 newest-first, and the Duration = finished-started rule.
func TestWebDataSourcePipelines(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	ctx := context.Background()

	// Pipeline A: "aaa-many" — 12 runs, to prove the Recent cap (10) + newest-first.
	seedTestPipeline(t, store, db.Pipeline{
		Name: "aaa-many", Repo: "acme/api", SpecYAML: diamondSpecYAML, Enabled: true, Interval: "168h",
	})
	base := time.UnixMilli(1_751_100_000_000).UTC()
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("prun-many-%02d", i)
		seedTestRun(t, store, db.PipelineRun{
			ID: id, Pipeline: "aaa-many", Trigger: "schedule", State: pipeline.RunSucceeded,
			StartedAt: base.Add(time.Duration(i) * time.Minute), FinishedAt: base.Add(time.Duration(i)*time.Minute + 30*time.Second),
		}, nil)
	}

	// Pipeline B: "nightly-deploy" — schedule state + two runs (one in-flight).
	seedTestPipeline(t, store, db.Pipeline{
		Name: "nightly-deploy", Repo: "acme/webapp", SpecYAML: diamondSpecYAML, Enabled: true,
		Interval: "24h", Jitter: "15m",
	})
	lastRunAt := time.UnixMilli(1_751_000_000_000).UTC()
	nextDueAt := time.UnixMilli(1_751_025_000_000).UTC()
	if err := store.UpdatePipelineScheduleState(ctx, db.PipelineScheduleState{
		Name: "nightly-deploy", LastRunAt: lastRunAt, NextDueAt: nextDueAt,
		LastRunID: "prun-nd-0001", LastStatus: pipeline.RunSucceeded,
	}); err != nil {
		t.Fatalf("UpdatePipelineScheduleState: %v", err)
	}
	ndStarted := time.UnixMilli(1_750_999_000_000).UTC()
	ndFinished := time.UnixMilli(1_750_999_060_000).UTC() // +60s => Duration 60000
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-nd-0001", Pipeline: "nightly-deploy", Trigger: "schedule", State: pipeline.RunSucceeded,
		StartedAt: ndStarted, FinishedAt: ndFinished,
	}, nil)
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-nd-0002", Pipeline: "nightly-deploy", Trigger: "manual", State: pipeline.RunRunning,
		StartedAt: ndStarted.Add(time.Hour), // newest, still running => no finish
	}, nil)
	store.Close()

	ds := &webDataSource{home: home}
	pipelines, err := ds.Pipelines(ctx)
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}
	if len(pipelines) != 2 {
		t.Fatalf("len(pipelines) = %d, want 2: %+v", len(pipelines), pipelines)
	}
	// ORDER BY name.
	if pipelines[0].Name != "aaa-many" || pipelines[1].Name != "nightly-deploy" {
		t.Fatalf("pipeline order = %s,%s, want aaa-many,nightly-deploy", pipelines[0].Name, pipelines[1].Name)
	}

	many := pipelines[0]
	if len(many.Recent) != 10 {
		t.Fatalf("aaa-many Recent len = %d, want 10 (cap): %+v", len(many.Recent), many.Recent)
	}
	// Newest-first: prun-many-11 down to prun-many-02 (00, 01 dropped by the cap).
	if many.Recent[0].ID != "prun-many-11" || many.Recent[9].ID != "prun-many-02" {
		t.Fatalf("aaa-many Recent bounds = %s..%s, want prun-many-11..prun-many-02", many.Recent[0].ID, many.Recent[9].ID)
	}
	if many.StageCount != 4 {
		t.Fatalf("aaa-many StageCount = %d, want 4 (diamond spec)", many.StageCount)
	}

	nd := pipelines[1]
	if nd.Repo != "acme/webapp" || !nd.Enabled || nd.Interval != "24h" || nd.Jitter != "15m" {
		t.Fatalf("nightly-deploy meta = %+v, want repo acme/webapp enabled 24h/15m", nd)
	}
	if nd.LastRunID != "prun-nd-0001" || nd.LastStatus != pipeline.RunSucceeded {
		t.Fatalf("nightly-deploy last = %s/%s, want prun-nd-0001/succeeded", nd.LastRunID, nd.LastStatus)
	}
	if nd.LastRunAt != lastRunAt.UnixMilli() || nd.NextDueAt != nextDueAt.UnixMilli() {
		t.Fatalf("nightly-deploy times = last %d next %d, want %d / %d", nd.LastRunAt, nd.NextDueAt, lastRunAt.UnixMilli(), nextDueAt.UnixMilli())
	}
	if len(nd.Recent) != 2 {
		t.Fatalf("nightly-deploy Recent len = %d, want 2", len(nd.Recent))
	}
	// Newest-first: the in-flight run leads; it has no finish so Duration is 0.
	inflight := nd.Recent[0]
	if inflight.ID != "prun-nd-0002" || inflight.State != pipeline.RunRunning || inflight.Duration != 0 {
		t.Fatalf("nightly-deploy newest = %+v, want prun-nd-0002 running duration 0", inflight)
	}
	done := nd.Recent[1]
	if done.ID != "prun-nd-0001" || done.Duration != 60_000 {
		t.Fatalf("nightly-deploy done run = %+v, want prun-nd-0001 duration 60000", done)
	}
}

// TestWebDataSourcePipelineRunBlockedDiamond pins the run detail: stages come back
// in spec (topological) order — NOT the store's alphabetical stage_id order — with
// spec-derived cmd + dependency deps merged, the blocked stage carrying its
// persisted needs, the skipped stage present, and the run-level halt/needs mapped.
func TestWebDataSourcePipelineRunBlockedDiamond(t *testing.T) {
	home := dashboardTestHome(t)
	seedDiamondBlockedRun(t, home, "prun-diamond-0001", "")

	ds := &webDataSource{home: home}
	run, err := ds.PipelineRun(context.Background(), "prun-diamond-0001")
	if err != nil {
		t.Fatalf("PipelineRun: %v", err)
	}

	if run.State != pipeline.RunBlocked || run.HaltStage != "ascore" {
		t.Fatalf("run state/halt = %s/%s, want blocked/ascore", run.State, run.HaltStage)
	}
	if run.HaltReason != "scoring model needs the R2 token" {
		t.Fatalf("run HaltReason = %q", run.HaltReason)
	}
	if run.Repo != "jerryfane/noted" {
		t.Fatalf("run Repo = %q, want jerryfane/noted", run.Repo)
	}
	if len(run.Needs) != 1 || run.Needs[0] != "set R2 token: gitmoot config set r2.token" {
		t.Fatalf("run-level Needs = %+v, want the R2-token need", run.Needs)
	}

	if len(run.Stages) != 4 {
		t.Fatalf("len(Stages) = %d, want 4: %+v", len(run.Stages), run.Stages)
	}
	// SPEC order wins over alphabetical: index 0 is zfetch, not ascore.
	gotOrder := []string{run.Stages[0].ID, run.Stages[1].ID, run.Stages[2].ID, run.Stages[3].ID}
	wantOrder := []string{"zfetch", "ascore", "bdedupe", "publish"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("stage order = %v, want %v (spec order, not stage_id)", gotOrder, wantOrder)
		}
	}

	byID := map[string]dashboard.PipelineStage{}
	for _, s := range run.Stages {
		byID[s.ID] = s
	}
	zfetch := byID["zfetch"]
	if zfetch.Cmd != "./scripts/fetch.sh" || len(zfetch.Deps) != 0 {
		t.Fatalf("zfetch = %+v, want fetch cmd + no deps (root)", zfetch)
	}
	ascore := byID["ascore"]
	if ascore.State != pipeline.StageBlocked || ascore.Attempt != 1 {
		t.Fatalf("ascore state/attempt = %s/%d, want blocked/1", ascore.State, ascore.Attempt)
	}
	if ascore.Cmd != `./scripts/score.sh --filter "p<95> && q>1"` {
		t.Fatalf("ascore Cmd = %q, want the verbatim (escapable) filter cmd", ascore.Cmd)
	}
	if len(ascore.Deps) != 1 || ascore.Deps[0] != "zfetch" {
		t.Fatalf("ascore Deps = %+v, want [zfetch]", ascore.Deps)
	}
	if len(ascore.Needs) != 1 || ascore.Needs[0] != "set R2 token: gitmoot config set r2.token" {
		t.Fatalf("ascore Needs = %+v, want the persisted blocked need", ascore.Needs)
	}
	publish := byID["publish"]
	if publish.State != pipeline.StageSkipped || publish.JobID != "" {
		t.Fatalf("publish = %+v, want skipped with no job", publish)
	}
	if len(publish.Deps) != 2 || publish.Deps[0] != "ascore" || publish.Deps[1] != "bdedupe" {
		t.Fatalf("publish Deps = %+v, want [ascore bdedupe]", publish.Deps)
	}
}

// TestWebDataSourcePipelineRunNotFound pins the unknown-id sentinel: !ok from the
// store maps to dashboard.ErrPipelineRunNotFound (the API layer serves 404), NOT an
// empty 200.
func TestWebDataSourcePipelineRunNotFound(t *testing.T) {
	home := dashboardTestHome(t)
	ds := &webDataSource{home: home}

	_, err := ds.PipelineRun(context.Background(), "prun-does-not-exist")
	if err != dashboard.ErrPipelineRunNotFound {
		t.Fatalf("PipelineRun(unknown) err = %v, want dashboard.ErrPipelineRunNotFound", err)
	}
}

// TestWebDataSourcePipelineRunSpecHashMismatchFallback pins the fallback: when the
// run's SpecHash does not match the stored pipeline's spec, stages keep the store's
// stage_id order and carry NO spec-derived cmd/deps (the run's snapshot no longer
// corresponds to the current spec) — mirroring orderPipelineRunStages' fallback.
func TestWebDataSourcePipelineRunSpecHashMismatchFallback(t *testing.T) {
	home := dashboardTestHome(t)
	seedDiamondBlockedRun(t, home, "prun-diamond-stale", "sha256-stale-mismatch")

	ds := &webDataSource{home: home}
	run, err := ds.PipelineRun(context.Background(), "prun-diamond-stale")
	if err != nil {
		t.Fatalf("PipelineRun: %v", err)
	}

	if len(run.Stages) != 4 {
		t.Fatalf("len(Stages) = %d, want 4", len(run.Stages))
	}
	// stage_id (alphabetical) order: ascore, bdedupe, publish, zfetch.
	wantOrder := []string{"ascore", "bdedupe", "publish", "zfetch"}
	for i, want := range wantOrder {
		if run.Stages[i].ID != want {
			t.Fatalf("fallback stage order = %v, want stage_id order %v",
				[]string{run.Stages[0].ID, run.Stages[1].ID, run.Stages[2].ID, run.Stages[3].ID}, wantOrder)
		}
	}
	// No spec applied => no merged cmd/deps on any stage.
	for _, s := range run.Stages {
		if s.Cmd != "" || len(s.Deps) != 0 {
			t.Fatalf("stage %s carried spec fields on hash mismatch: %+v", s.ID, s)
		}
	}
	// The blocked stage's PERSISTED needs still surface (they live on the row, not the spec).
	var ascore dashboard.PipelineStage
	for _, s := range run.Stages {
		if s.ID == "ascore" {
			ascore = s
		}
	}
	if len(ascore.Needs) != 1 {
		t.Fatalf("ascore Needs on fallback = %+v, want the persisted need retained", ascore.Needs)
	}
	// Repo is a pipeline-level attribute, so it is still resolved from the current row.
	if run.Repo != "jerryfane/noted" {
		t.Fatalf("run Repo on fallback = %q, want jerryfane/noted", run.Repo)
	}
}

// TestWebDataSourcePipelinesDeterministic pins byte-stable output (the UI polls with
// a change-signature skip): two calls produce identical %+v.
func TestWebDataSourcePipelinesDeterministic(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{Name: "beta", Repo: "acme/b", SpecYAML: diamondSpecYAML, Enabled: false, Interval: "12h"})
	seedTestPipeline(t, store, db.Pipeline{Name: "alpha", Repo: "acme/a", SpecYAML: diamondSpecYAML, Enabled: true, Interval: "24h"})
	base := time.UnixMilli(1_751_200_000_000).UTC()
	for i := 0; i < 3; i++ {
		seedTestRun(t, store, db.PipelineRun{
			ID: fmt.Sprintf("prun-alpha-%d", i), Pipeline: "alpha", State: pipeline.RunSucceeded,
			StartedAt: base.Add(time.Duration(i) * time.Minute),
		}, nil)
	}
	store.Close()

	ds := &webDataSource{home: home}
	first, err := ds.Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines first: %v", err)
	}
	second, err := ds.Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines second: %v", err)
	}
	if fmt.Sprintf("%+v", first) != fmt.Sprintf("%+v", second) {
		t.Fatalf("Pipelines not deterministic:\n first=%+v\nsecond=%+v", first, second)
	}
}

// TestWebDataSourcePipelineRunDeterministic pins byte-stable run detail across calls.
func TestWebDataSourcePipelineRunDeterministic(t *testing.T) {
	home := dashboardTestHome(t)
	seedDiamondBlockedRun(t, home, "prun-diamond-det", "")

	ds := &webDataSource{home: home}
	first, err := ds.PipelineRun(context.Background(), "prun-diamond-det")
	if err != nil {
		t.Fatalf("PipelineRun first: %v", err)
	}
	second, err := ds.PipelineRun(context.Background(), "prun-diamond-det")
	if err != nil {
		t.Fatalf("PipelineRun second: %v", err)
	}
	if fmt.Sprintf("%+v", first) != fmt.Sprintf("%+v", second) {
		t.Fatalf("PipelineRun not deterministic:\n first=%+v\nsecond=%+v", first, second)
	}
}
