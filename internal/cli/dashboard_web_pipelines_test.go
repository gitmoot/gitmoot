package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

// diamondSpecYAML is a fan-out/fan-in pipeline whose declared (spec) stage order —
// zfetch, ascore, bdedupe, publish — is deliberately NOT alphabetical, so a stage
// list in spec order is provably different from the store's ORDER BY stage_id
// (ascore, bdedupe, publish, zfetch). One cmd carries angle brackets/ampersand to
// prove nothing rewrites the stored command.
const diamondSpecYAML = `name: listing-refresh
repo: jerryfane/noted
description: Keeps the listing index fresh for readers.
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

func TestWebDataSourcePipelinesPipelineTriggerMode(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	upstreamSpec := "name: upstream\nstages: [{id: run, cmd: echo}]\n"
	downstreamSpec := "name: downstream\nrepo: owner/downstream\ntrigger: {kind: pipeline, pipeline: upstream}\nstages: [{id: run, cmd: echo}]\n"
	seedTestPipeline(t, store, db.Pipeline{Name: "upstream", SpecYAML: upstreamSpec})
	seedTestPipeline(t, store, db.Pipeline{Name: "downstream", Repo: "owner/downstream", SpecYAML: downstreamSpec, Enabled: true})
	store.Close()

	ds := &webDataSource{home: home}
	rows, err := ds.Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "downstream" || rows[0].Mode != "after: upstream" {
		t.Fatalf("pipeline trigger dashboard mode = %+v", rows)
	}
	store = openPipelineTestStore(t, home)
	if removed, err := store.DeletePipeline(context.Background(), "upstream"); err != nil || !removed {
		t.Fatalf("remove upstream: removed=%v err=%v", removed, err)
	}
	store.Close()
	rows, err = ds.Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines after upstream removal: %v", err)
	}
	if len(rows) != 1 || rows[0].Mode != "after: upstream (upstream missing)" {
		t.Fatalf("missing-upstream dashboard mode = %+v", rows)
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
	// Persisted needs_json wins over the strict spec gate. This blocked row carries
	// a human-action need rather than a stage id, so it safely produces no edge.
	if ascore.Deps == nil || len(ascore.Deps) != 0 {
		t.Fatalf("ascore Deps = %+v, want non-nil empty persisted override", ascore.Deps)
	}
	if len(ascore.Needs) != 1 || ascore.Needs[0] != "set R2 token: gitmoot config set r2.token" {
		t.Fatalf("ascore Needs = %+v, want the persisted blocked need", ascore.Needs)
	}
	bdedupe := byID["bdedupe"]
	if !slices.Equal(bdedupe.Deps, []string{"zfetch"}) {
		t.Fatalf("bdedupe Deps = %+v, want strict-hash spec deps [zfetch]", bdedupe.Deps)
	}
	publish := byID["publish"]
	if publish.State != pipeline.StageSkipped || publish.JobID != "" {
		t.Fatalf("publish = %+v, want skipped with no job", publish)
	}
	if len(publish.Deps) != 2 || publish.Deps[0] != "ascore" || publish.Deps[1] != "bdedupe" {
		t.Fatalf("publish Deps = %+v, want [ascore bdedupe]", publish.Deps)
	}
}

// TestWebDataSourcePipelineRunStoredDeps pins persisted-dependency precedence over
// both spec gates: matching-hash and same-stage-set structural fallback. Stored
// order is retained; roots and malformed values become non-nil empty slices.
func TestWebDataSourcePipelineRunStoredDeps(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	realHash := pipeline.Hash([]byte(diamondSpecYAML))
	seedTestPipeline(t, store, db.Pipeline{
		Name: "listing-refresh", Repo: "jerryfane/noted", SpecYAML: diamondSpecYAML, SpecHash: realHash,
	})
	rows := []db.PipelineRunStage{
		{StageID: "zfetch", State: pipeline.StageSucceeded},
		{StageID: "ascore", State: pipeline.StagePending, NeedsJSON: `["bdedupe"]`},
		{StageID: "bdedupe", State: pipeline.StagePending, NeedsJSON: `not-json`},
		{StageID: "publish", State: pipeline.StagePending, NeedsJSON: `["zfetch","ascore"]`},
	}
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-stored-deps-strict", Pipeline: "listing-refresh", State: pipeline.RunRunning, SpecHash: realHash,
	}, rows)
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-stored-deps-structural", Pipeline: "listing-refresh", State: pipeline.RunRunning, SpecHash: "sha256-stale",
	}, rows)
	store.Close()

	for _, runID := range []string{"prun-stored-deps-strict", "prun-stored-deps-structural"} {
		run, err := (&webDataSource{home: home}).PipelineRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("PipelineRun(%s): %v", runID, err)
		}
		byID := map[string]dashboard.PipelineStage{}
		for _, stage := range run.Stages {
			byID[stage.ID] = stage
		}
		if got := byID["ascore"].Deps; !slices.Equal(got, []string{"bdedupe"}) {
			t.Fatalf("%s ascore Deps = %v, want persisted override [bdedupe]", runID, got)
		}
		if got := byID["publish"].Deps; !slices.Equal(got, []string{"zfetch", "ascore"}) {
			t.Fatalf("%s publish Deps = %v, want persisted order [zfetch ascore]", runID, got)
		}
		for _, id := range []string{"zfetch", "bdedupe"} {
			if got := byID[id].Deps; got == nil || len(got) != 0 {
				t.Fatalf("%s %s Deps = %#v, want non-nil empty persisted result", runID, id, got)
			}
		}
		encoded, err := json.Marshal(byID["zfetch"].Deps)
		if err != nil {
			t.Fatalf("marshal root Deps: %v", err)
		}
		if string(encoded) != "[]" {
			t.Fatalf("%s root Deps JSON = %s, want []", runID, encoded)
		}
	}
}

// TestWebDataSourcePipelineRunProgress pins the #822 bridge contract: one bulk
// latest-event read maps valid progress onto running stages only. Missing and
// malformed events fail open, and terminal stages never retain stale progress.
func TestWebDataSourcePipelineRunProgress(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	started := time.UnixMilli(1_751_000_000_000).UTC()
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-progress-0001", Pipeline: "progress-test", Trigger: "manual",
		State: pipeline.RunRunning, StartedAt: started,
	}, []db.PipelineRunStage{
		{StageID: "mapped", State: pipeline.StageRunning, JobID: "job-progress-mapped"},
		{StageID: "absent", State: pipeline.StageRunning, JobID: "job-progress-absent"},
		{StageID: "malformed", State: pipeline.StageRunning, JobID: "job-progress-malformed"},
		{StageID: "terminal", State: pipeline.StageSucceeded, JobID: "job-progress-terminal"},
	})
	ctx := context.Background()
	for _, event := range []db.JobEvent{
		{JobID: "job-progress-mapped", Kind: "progress", Message: `{"elapsed":"1m0s","activity":"compiled package 42/100"}`},
		{JobID: "job-progress-malformed", Kind: "progress", Message: `{"elapsed":`},
		{JobID: "job-progress-terminal", Kind: "progress", Message: `{"elapsed":"2m0s","activity":"stale terminal output"}`},
	} {
		if err := store.AddJobEvent(ctx, event); err != nil {
			store.Close()
			t.Fatalf("AddJobEvent(%s): %v", event.JobID, err)
		}
	}
	mappedEvent, ok, err := store.GetLatestJobEventByKind(ctx, "job-progress-mapped", "progress")
	if err != nil || !ok {
		store.Close()
		t.Fatalf("GetLatestJobEventByKind(mapped): ok=%v err=%v", ok, err)
	}
	wantProgressAt := parseJobTimeMillis(mappedEvent.CreatedAt)
	store.Close()

	run, err := (&webDataSource{home: home}).PipelineRun(ctx, "prun-progress-0001")
	if err != nil {
		t.Fatalf("PipelineRun: %v", err)
	}
	byID := map[string]dashboard.PipelineStage{}
	for _, stage := range run.Stages {
		byID[stage.ID] = stage
	}

	t.Run("maps running progress activity and event millis", func(t *testing.T) {
		got := byID["mapped"]
		if got.ProgressActivity != "compiled package 42/100" || got.ProgressAt != wantProgressAt || got.ProgressAt == 0 {
			t.Fatalf("mapped progress = %q/%d, want activity/%d", got.ProgressActivity, got.ProgressAt, wantProgressAt)
		}
	})
	t.Run("absent event stays zero", func(t *testing.T) {
		got := byID["absent"]
		if got.ProgressActivity != "" || got.ProgressAt != 0 {
			t.Fatalf("absent progress = %q/%d, want zero values", got.ProgressActivity, got.ProgressAt)
		}
	})
	t.Run("malformed event stays zero without error", func(t *testing.T) {
		got := byID["malformed"]
		if got.ProgressActivity != "" || got.ProgressAt != 0 {
			t.Fatalf("malformed progress = %q/%d, want zero values", got.ProgressActivity, got.ProgressAt)
		}
	})
	t.Run("terminal stage never carries progress", func(t *testing.T) {
		got := byID["terminal"]
		if got.ProgressActivity != "" || got.ProgressAt != 0 {
			t.Fatalf("terminal progress = %q/%d, want zero values", got.ProgressActivity, got.ProgressAt)
		}
	})
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

// TestWebDataSourcePipelineRunSpecHashMismatchSameStageSetFallback pins the
// metadata-only re-add case: a hash mismatch keeps store ordering and suppresses
// metadata, but an exact stage-ID set restores dependency edges for layout.
func TestWebDataSourcePipelineRunSpecHashMismatchSameStageSetFallback(t *testing.T) {
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
	// The strict hash gate still suppresses all display metadata.
	for _, s := range run.Stages {
		if s.Cmd != "" || s.Kind != "" || s.AgentRuntime != "" || s.Retry != 0 {
			t.Fatalf("stage %s carried spec metadata on hash mismatch: %+v", s.ID, s)
		}
	}
	byID := map[string]dashboard.PipelineStage{}
	for _, stage := range run.Stages {
		byID[stage.ID] = stage
	}
	if !slices.Equal(byID["bdedupe"].Deps, []string{"zfetch"}) {
		t.Fatalf("bdedupe structural Deps = %v, want [zfetch]", byID["bdedupe"].Deps)
	}
	if !slices.Equal(byID["publish"].Deps, []string{"ascore", "bdedupe"}) {
		t.Fatalf("publish structural Deps = %v, want [ascore bdedupe]", byID["publish"].Deps)
	}
	if byID["zfetch"].Deps == nil || len(byID["zfetch"].Deps) != 0 {
		t.Fatalf("zfetch structural Deps = %#v, want non-nil empty root", byID["zfetch"].Deps)
	}
	// The blocked row's present needs_json wins over structural fallback and is
	// filtered to no edge because its human-action text is not a stage id.
	if byID["ascore"].Deps == nil || len(byID["ascore"].Deps) != 0 {
		t.Fatalf("ascore persisted Deps = %#v, want non-nil empty", byID["ascore"].Deps)
	}
	// The blocked stage's PERSISTED needs still surface (they live on the row, not the spec).
	ascore := byID["ascore"]
	if len(ascore.Needs) != 1 {
		t.Fatalf("ascore Needs on fallback = %+v, want the persisted need retained", ascore.Needs)
	}
	// Repo is a pipeline-level attribute, so it is still resolved from the current row.
	if run.Repo != "jerryfane/noted" {
		t.Fatalf("run Repo on fallback = %q, want jerryfane/noted", run.Repo)
	}
}

// TestWebDataSourcePipelineRunSpecHashMismatchDifferentStageSetFallback pins the
// honest fallback: when stage membership changed, an old run gets neither current
// metadata nor current dependency edges.
func TestWebDataSourcePipelineRunSpecHashMismatchDifferentStageSetFallback(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{
		Name: "listing-refresh", Repo: "jerryfane/noted", SpecYAML: diamondSpecYAML,
	})
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-diamond-different-set", Pipeline: "listing-refresh", State: pipeline.RunRunning,
		SpecHash: "sha256-stale-mismatch",
	}, []db.PipelineRunStage{
		{StageID: "zfetch", State: pipeline.StageSucceeded},
		{StageID: "ascore", State: pipeline.StagePending},
		{StageID: "publish", State: pipeline.StagePending},
	})
	store.Close()

	run, err := (&webDataSource{home: home}).PipelineRun(context.Background(), "prun-diamond-different-set")
	if err != nil {
		t.Fatalf("PipelineRun: %v", err)
	}
	for _, stage := range run.Stages {
		if stage.Deps == nil || len(stage.Deps) != 0 {
			t.Fatalf("different-set stage %s Deps = %#v, want non-nil empty", stage.ID, stage.Deps)
		}
		if stage.Cmd != "" || stage.Kind != "" || stage.AgentRuntime != "" || stage.Retry != 0 {
			t.Fatalf("different-set stage %s carried spec metadata: %+v", stage.ID, stage)
		}
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

// retrySpecYAML declares a linear pipeline whose second stage carries a retry
// budget, so the spec's Retry field is provably surfaced onto both the declared
// DAG (PipelineDetail) and a run's merged stage (PipelineRun).
const retrySpecYAML = `name: bench-suite
repo: acme/bench
stages:
  - id: build
    cmd: ./build.sh
  - id: test
    cmd: ./test.sh
    needs: [build]
    retry: 2
`

// diamondStageRows returns the four diamond stage rows all in one state, in a
// deliberately non-spec, non-alphabetical order (so a passing spec-order assertion
// is not an accident of insertion order). RunID is filled in by seedTestRun.
func diamondStageRows(state string) []db.PipelineRunStage {
	return []db.PipelineRunStage{
		{StageID: "publish", State: state},
		{StageID: "zfetch", State: state},
		{StageID: "bdedupe", State: state},
		{StageID: "ascore", State: state},
	}
}

// TestWebDataSourcePipelineDetailNeverRun pins the declared-DAG preview for a
// pipeline that has never run: Declared is the spec DAG in spec order (every stage
// pending, with cmd/deps merged) and Runs is a non-nil empty slice.
func TestWebDataSourcePipelineDetailNeverRun(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{
		Name: "listing-refresh", Repo: "jerryfane/noted", SpecYAML: diamondSpecYAML, Enabled: true, Interval: "24h",
	})
	store.Close()

	ds := &webDataSource{home: home}
	detail, err := ds.PipelineDetail(context.Background(), "listing-refresh")
	if err != nil {
		t.Fatalf("PipelineDetail: %v", err)
	}
	if detail.Name != "listing-refresh" {
		t.Fatalf("detail.Name = %q, want listing-refresh", detail.Name)
	}
	if detail.Description != "Keeps the listing index fresh for readers." {
		t.Fatalf("detail.Description = %q, want raw spec description", detail.Description)
	}
	if detail.Runs == nil || len(detail.Runs) != 0 {
		t.Fatalf("detail.Runs = %+v, want non-nil empty slice (never run)", detail.Runs)
	}
	if detail.Declared == nil || len(detail.Declared) != 4 {
		t.Fatalf("detail.Declared len = %d, want 4 non-nil: %+v", len(detail.Declared), detail.Declared)
	}
	// Declared is the spec (topological) order, NOT alphabetical stage_id order.
	wantOrder := []string{"zfetch", "ascore", "bdedupe", "publish"}
	for i, want := range wantOrder {
		if detail.Declared[i].ID != want {
			t.Fatalf("declared order = %v, want spec order %v",
				[]string{detail.Declared[0].ID, detail.Declared[1].ID, detail.Declared[2].ID, detail.Declared[3].ID}, wantOrder)
		}
		if detail.Declared[i].State != pipeline.StagePending {
			t.Fatalf("declared %s state = %q, want pending", want, detail.Declared[i].State)
		}
	}
	byID := map[string]dashboard.PipelineStage{}
	for _, s := range detail.Declared {
		byID[s.ID] = s
	}
	if byID["zfetch"].Cmd != "./scripts/fetch.sh" || len(byID["zfetch"].Deps) != 0 {
		t.Fatalf("declared zfetch = %+v, want fetch cmd + no deps", byID["zfetch"])
	}
	if byID["ascore"].Cmd != `./scripts/score.sh --filter "p<95> && q>1"` {
		t.Fatalf("declared ascore Cmd = %q, want the verbatim filter cmd", byID["ascore"].Cmd)
	}
	if len(byID["ascore"].Deps) != 1 || byID["ascore"].Deps[0] != "zfetch" {
		t.Fatalf("declared ascore Deps = %+v, want [zfetch]", byID["ascore"].Deps)
	}
	if len(byID["publish"].Deps) != 2 || byID["publish"].Deps[0] != "ascore" || byID["publish"].Deps[1] != "bdedupe" {
		t.Fatalf("declared publish Deps = %+v, want [ascore bdedupe]", byID["publish"].Deps)
	}
	// #873: the declared preview is self-describing with zero runs.
	if byID["zfetch"].Kind != "shell" {
		t.Fatalf("declared zfetch Kind = %q, want shell", byID["zfetch"].Kind)
	}
}

func TestWebDataSourcePipelineDetailKeysNamesOnlyAndLiveDrift(t *testing.T) {
	const (
		ownSentinel     = "own-secret-value-974"
		sharedSentinel  = "shared-secret-value-974"
		defaultSentinel = "inline-default-value-974"
	)
	home := dashboardTestHome(t)
	envFile := writePipelineEnvFile(t, t.TempDir(), "OWN_TWO="+ownSentinel+"\nOWN_ONE="+ownSentinel+"\n", 0o600)
	writeDefaultKeychain(t, home, "SHARED_TOKEN="+sharedSentinel+"\n")
	raw := fmt.Sprintf(`name: keys-view
env_file: %q
env:
  DEFAULT_TOKEN: %q
stages:
  - id: deliver
    cmd: echo deliver
    env_keys: [OWN_*, SHARED_TOKEN, DEFAULT_TOKEN]
  - id: harvest
    cmd: echo harvest
  - id: inspect
    agent: scout
    action: ask
    prompt: inspect
`, envFile, defaultSentinel)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{Name: "keys-view", SpecYAML: raw})
	if _, err := store.AddKeychainKey(context.Background(), "SHARED_TOKEN", db.KeychainModeInjected); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantKeychainKey(context.Background(), db.KeychainConsumerPipeline, "keys-view", "SHARED_TOKEN"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	ds := &webDataSource{home: home}
	detail, err := ds.PipelineDetail(context.Background(), "keys-view")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Keys.EnvFile != (dashboard.PipelineEnvFileStatus{Path: envFile, Status: pipelineEnvFileStatusOK}) {
		t.Fatalf("envFile = %+v", detail.Keys.EnvFile)
	}
	if len(detail.Keys.Stages) != 3 {
		t.Fatalf("stages = %+v", detail.Keys.Stages)
	}
	wantKeys := []dashboard.PipelineKeyEntry{
		{Name: "OWN_ONE", Source: pipelineKeySourceOwn, Mode: db.KeychainModeInjected},
		{Name: "OWN_TWO", Source: pipelineKeySourceOwn, Mode: db.KeychainModeInjected},
		{Name: "SHARED_TOKEN", Source: pipelineKeySourceShared, Mode: db.KeychainModeInjected},
		{Name: "DEFAULT_TOKEN", Source: pipelineKeySourceDefault, Mode: db.KeychainModeInjected},
	}
	if got := detail.Keys.Stages[0]; got.ID != "deliver" || got.Kind != "shell" || !reflect.DeepEqual(got.Keys, wantKeys) || len(got.UnresolvedSelectors) != 0 {
		t.Fatalf("deliver keys = %+v", got)
	}
	for _, stage := range detail.Keys.Stages[1:] {
		if stage.Keys == nil || stage.UnresolvedSelectors == nil || len(stage.Keys) != 0 || len(stage.UnresolvedSelectors) != 0 {
			t.Fatalf("empty stage arrays = %+v", stage)
		}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{ownSentinel, sharedSentinel, defaultSentinel} {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("PipelineDetail leaked secret/default value %q: %s", sentinel, encoded)
		}
	}

	if err := os.Remove(envFile); err != nil {
		t.Fatal(err)
	}
	drifted, err := ds.PipelineDetail(context.Background(), "keys-view")
	if err != nil {
		t.Fatal(err)
	}
	if drifted.Keys.EnvFile.Status != pipelineEnvFileStatusMissing {
		t.Fatalf("drifted envFile = %+v", drifted.Keys.EnvFile)
	}
	if got, want := drifted.Keys.Stages[0].Keys, wantKeys[2:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("drifted keys = %#v, want %#v", got, want)
	}
	if got, want := drifted.Keys.Stages[0].UnresolvedSelectors, []string{"OWN_*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("drifted unresolved = %#v, want %#v", got, want)
	}
}

func TestDashboardPipelineDetailKeysHTTPShape(t *testing.T) {
	home := dashboardTestHome(t)
	envFile := writePipelineEnvFile(t, t.TempDir(), "TOKEN=http-secret-value-974\n", 0o600)
	raw := fmt.Sprintf("name: http-keys\nenv_file: %q\nstages:\n  - {id: deliver, cmd: echo, env_keys: [TOKEN]}\n  - {id: idle, cmd: echo}\n", envFile)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{Name: "http-keys", SpecYAML: raw})
	store.Close()

	server := httptest.NewServer(dashboard.Serve(&webDataSource{home: home}))
	defer server.Close()
	resp, err := http.Get(server.URL + "/api/pipelines/http-keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"envFile"`) || !strings.Contains(string(body), `"unresolvedSelectors"`) || strings.Contains(string(body), "http-secret-value-974") {
		t.Fatalf("unexpected wire shape or value leak: %s", body)
	}
	var detail dashboard.PipelineDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Keys.Stages == nil || len(detail.Keys.Stages) != 2 || detail.Keys.Stages[0].Keys == nil || detail.Keys.Stages[1].Keys == nil || detail.Keys.Stages[1].UnresolvedSelectors == nil {
		t.Fatalf("non-null array contract failed: %+v", detail.Keys)
	}
}

func TestWebDataSourcePipelineDetailKeysFailOpenSpec(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{Name: "broken-keys", SpecYAML: "name: [unterminated"})
	store.Close()

	detail, err := (&webDataSource{home: home}).PipelineDetail(context.Background(), "broken-keys")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Declared == nil || detail.Runs == nil || detail.Keys.Stages == nil {
		t.Fatalf("fail-open slices must be non-nil: %+v", detail)
	}
	if detail.Keys.EnvFile.Status != pipelineEnvFileStatusNone || len(detail.Keys.Stages) != 0 {
		t.Fatalf("fail-open keys = %+v", detail.Keys)
	}
}

// TestWebDataSourcePipelineDetailDeclaredAgentKind pins that a never-run AGENT
// stage carries its kind and (when the agent is registered) runtime, so the
// dashboard's declared DAG can badge it (#873).
func TestWebDataSourcePipelineDetailDeclaredAgentKind(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	specYAML := "name: mailflow\nrepo: owner/repo\nstages:\n  - {id: triage, agent: helper, action: ask, prompt: read it}\n  - {id: run, cmd: echo, needs: [triage]}\n"
	seedTestPipeline(t, store, db.Pipeline{Name: "mailflow", Repo: "owner/repo", SpecYAML: specYAML, Enabled: true})
	if err := store.UpsertAgent(context.Background(), db.Agent{Name: "helper", Runtime: "codex", RepoScope: "owner/repo"}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	store.Close()

	detail, err := (&webDataSource{home: home}).PipelineDetail(context.Background(), "mailflow")
	if err != nil {
		t.Fatalf("PipelineDetail: %v", err)
	}
	byID := map[string]dashboard.PipelineStage{}
	for _, s := range detail.Declared {
		byID[s.ID] = s
	}
	if byID["triage"].Kind != "agent_ask" || byID["triage"].AgentRuntime != "codex" {
		t.Fatalf("declared triage = %+v, want agent_ask/codex", byID["triage"])
	}
	if byID["run"].Kind != "shell" || byID["run"].AgentRuntime != "" {
		t.Fatalf("declared run = %+v, want shell with no runtime", byID["run"])
	}
}

// TestWebDataSourcePipelineDetailHistory pins the run-history projection: runs come
// back newest-first, each carrying non-nil per-stage marks in spec order, with
// run-level state/trigger/duration mapped.
func TestWebDataSourcePipelineDetailHistory(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	realHash := pipeline.Hash([]byte(diamondSpecYAML))
	seedTestPipeline(t, store, db.Pipeline{
		Name: "listing-refresh", Repo: "jerryfane/noted", SpecYAML: diamondSpecYAML, SpecHash: realHash, Enabled: true,
	})
	base := time.UnixMilli(1_751_300_000_000).UTC()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("prun-hist-%02d", i)
		started := base.Add(time.Duration(i) * time.Minute)
		seedTestRun(t, store, db.PipelineRun{
			ID: id, Pipeline: "listing-refresh", Trigger: "schedule", SpecHash: realHash,
			State: pipeline.RunSucceeded, StartedAt: started, FinishedAt: started.Add(30 * time.Second),
		}, diamondStageRows(pipeline.StageSucceeded))
	}
	store.Close()

	ds := &webDataSource{home: home}
	detail, err := ds.PipelineDetail(context.Background(), "listing-refresh")
	if err != nil {
		t.Fatalf("PipelineDetail: %v", err)
	}
	if len(detail.Runs) != 3 {
		t.Fatalf("len(Runs) = %d, want 3: %+v", len(detail.Runs), detail.Runs)
	}
	// Newest-first (started_at DESC): hist-02, hist-01, hist-00.
	wantRunOrder := []string{"prun-hist-02", "prun-hist-01", "prun-hist-00"}
	for i, want := range wantRunOrder {
		if detail.Runs[i].ID != want {
			t.Fatalf("run order = %v, want newest-first %v",
				[]string{detail.Runs[0].ID, detail.Runs[1].ID, detail.Runs[2].ID}, wantRunOrder)
		}
	}
	newest := detail.Runs[0]
	if newest.Trigger != "schedule" || newest.State != pipeline.RunSucceeded || newest.Duration != 30_000 {
		t.Fatalf("newest run = %+v, want schedule/succeeded/30000ms", newest)
	}
	// Marks are non-nil and in spec order (NOT alphabetical stage_id order).
	if newest.Stages == nil || len(newest.Stages) != 4 {
		t.Fatalf("newest run marks = %+v, want 4 non-nil marks", newest.Stages)
	}
	wantMarkOrder := []string{"zfetch", "ascore", "bdedupe", "publish"}
	for i, want := range wantMarkOrder {
		if newest.Stages[i].ID != want {
			t.Fatalf("mark order = %v, want spec order %v",
				[]string{newest.Stages[0].ID, newest.Stages[1].ID, newest.Stages[2].ID, newest.Stages[3].ID}, wantMarkOrder)
		}
		if newest.Stages[i].State != pipeline.StageSucceeded {
			t.Fatalf("mark %s state = %q, want succeeded", want, newest.Stages[i].State)
		}
	}
}

// TestWebDataSourcePipelineDetailMarksOrdering pins the mark-ordering semantics
// shared with the run-detail path: spec order when the run's SpecHash matches the
// current spec, and the store's stage_id fallback order on a hash mismatch.
func TestWebDataSourcePipelineDetailMarksOrdering(t *testing.T) {
	t.Run("spec order on hash match", func(t *testing.T) {
		home := dashboardTestHome(t)
		seedDiamondBlockedRun(t, home, "prun-detail-match", "")

		ds := &webDataSource{home: home}
		detail, err := ds.PipelineDetail(context.Background(), "listing-refresh")
		if err != nil {
			t.Fatalf("PipelineDetail: %v", err)
		}
		if len(detail.Runs) != 1 {
			t.Fatalf("len(Runs) = %d, want 1", len(detail.Runs))
		}
		marks := detail.Runs[0].Stages
		want := []string{"zfetch", "ascore", "bdedupe", "publish"}
		for i, w := range want {
			if marks[i].ID != w {
				t.Fatalf("match marks order = %+v, want spec order %v", marks, want)
			}
		}
		// The blocked stage's outcome comes through on its mark.
		byID := map[string]string{}
		for _, m := range marks {
			byID[m.ID] = m.State
		}
		if byID["ascore"] != pipeline.StageBlocked || byID["publish"] != pipeline.StageSkipped {
			t.Fatalf("mark states = %+v, want ascore blocked + publish skipped", byID)
		}
	})

	t.Run("stage_id fallback on hash mismatch", func(t *testing.T) {
		home := dashboardTestHome(t)
		seedDiamondBlockedRun(t, home, "prun-detail-stale", "sha256-stale-mismatch")

		ds := &webDataSource{home: home}
		detail, err := ds.PipelineDetail(context.Background(), "listing-refresh")
		if err != nil {
			t.Fatalf("PipelineDetail: %v", err)
		}
		if len(detail.Runs) != 1 {
			t.Fatalf("len(Runs) = %d, want 1", len(detail.Runs))
		}
		marks := detail.Runs[0].Stages
		// stage_id (alphabetical) order: ascore, bdedupe, publish, zfetch.
		want := []string{"ascore", "bdedupe", "publish", "zfetch"}
		for i, w := range want {
			if marks[i].ID != w {
				t.Fatalf("fallback marks order = %+v, want stage_id order %v", marks, want)
			}
		}
	})

	t.Run("mixed hashes in one history order per run", func(t *testing.T) {
		// One history containing BOTH a current-spec run and a stale-spec run:
		// the ordering decision must be made per run, not hoisted — the matching
		// run keeps spec order while the stale one falls back to stage_id order
		// in the same PipelineDetail response.
		home := dashboardTestHome(t)
		seedDiamondBlockedRun(t, home, "prun-mixed-1-match", "")
		seedDiamondBlockedRun(t, home, "prun-mixed-2-stale", "sha256-stale-mismatch")

		ds := &webDataSource{home: home}
		detail, err := ds.PipelineDetail(context.Background(), "listing-refresh")
		if err != nil {
			t.Fatalf("PipelineDetail: %v", err)
		}
		if len(detail.Runs) != 2 {
			t.Fatalf("len(Runs) = %d, want 2", len(detail.Runs))
		}
		// Identical StartedAt (the seed helper's fixed instant) → ID-desc tie-break.
		if detail.Runs[0].ID != "prun-mixed-2-stale" || detail.Runs[1].ID != "prun-mixed-1-match" {
			t.Fatalf("run order = %s, %s; want stale (ID desc) then match", detail.Runs[0].ID, detail.Runs[1].ID)
		}
		ids := func(marks []dashboard.PipelineStageMark) []string {
			out := make([]string, len(marks))
			for i, m := range marks {
				out[i] = m.ID
			}
			return out
		}
		wantStale := []string{"ascore", "bdedupe", "publish", "zfetch"} // stage_id fallback
		wantMatch := []string{"zfetch", "ascore", "bdedupe", "publish"} // spec order
		if got := ids(detail.Runs[0].Stages); !slices.Equal(got, wantStale) {
			t.Fatalf("stale run marks = %v, want stage_id order %v", got, wantStale)
		}
		if got := ids(detail.Runs[1].Stages); !slices.Equal(got, wantMatch) {
			t.Fatalf("matching run marks = %v, want spec order %v", got, wantMatch)
		}
	})
}

// TestWebDataSourcePipelineDetailRetry pins that the spec's per-stage retry budget
// propagates to both the declared DAG (PipelineDetail) and a run's merged stage
// (PipelineRun) under the hash gate, and is absent on a hash mismatch.
func TestWebDataSourcePipelineDetailRetry(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	realHash := pipeline.Hash([]byte(retrySpecYAML))
	seedTestPipeline(t, store, db.Pipeline{
		Name: "bench-suite", Repo: "acme/bench", SpecYAML: retrySpecYAML, SpecHash: realHash, Enabled: false,
	})
	started := time.UnixMilli(1_751_400_000_000).UTC()
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-bench-0001", Pipeline: "bench-suite", Trigger: "manual", SpecHash: realHash,
		State: pipeline.RunSucceeded, StartedAt: started, FinishedAt: started.Add(time.Minute),
	}, []db.PipelineRunStage{
		{StageID: "build", State: pipeline.StageSucceeded, JobID: "job-build"},
		{StageID: "test", State: pipeline.StageSucceeded, JobID: "job-test", Attempt: 2},
	})
	store.Close()

	ds := &webDataSource{home: home}

	// Declared DAG carries the retry budget.
	detail, err := ds.PipelineDetail(context.Background(), "bench-suite")
	if err != nil {
		t.Fatalf("PipelineDetail: %v", err)
	}
	declaredByID := map[string]dashboard.PipelineStage{}
	for _, s := range detail.Declared {
		declaredByID[s.ID] = s
	}
	if declaredByID["test"].Retry != 2 {
		t.Fatalf("declared test Retry = %d, want 2", declaredByID["test"].Retry)
	}
	if declaredByID["build"].Retry != 0 {
		t.Fatalf("declared build Retry = %d, want 0 (no retry set)", declaredByID["build"].Retry)
	}

	// Run detail merges the same retry budget (hash matches).
	run, err := ds.PipelineRun(context.Background(), "prun-bench-0001")
	if err != nil {
		t.Fatalf("PipelineRun: %v", err)
	}
	runByID := map[string]dashboard.PipelineStage{}
	for _, s := range run.Stages {
		runByID[s.ID] = s
	}
	if runByID["test"].Retry != 2 || runByID["test"].Attempt != 2 {
		t.Fatalf("run test = %+v, want retry 2 / attempt 2", runByID["test"])
	}
}

// TestWebDataSourcePipelineRunRetryAbsentOnHashMismatch pins the other half of
// the retry hash gate: a run whose SpecHash no longer matches the pipeline's
// stored spec gets no spec-merged metadata — Retry and Cmd stay empty. The exact
// stage-ID set still permits the structural Deps fallback used for layout.
func TestWebDataSourcePipelineRunRetryAbsentOnHashMismatch(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{
		Name: "bench-suite", Repo: "acme/bench", SpecYAML: retrySpecYAML, Enabled: false,
	})
	started := time.UnixMilli(1_751_400_000_000).UTC()
	seedTestRun(t, store, db.PipelineRun{
		ID: "prun-bench-stale", Pipeline: "bench-suite", Trigger: "manual", SpecHash: "sha256-stale-mismatch",
		State: pipeline.RunSucceeded, StartedAt: started, FinishedAt: started.Add(time.Minute),
	}, []db.PipelineRunStage{
		{StageID: "build", State: pipeline.StageSucceeded, JobID: "job-build"},
		{StageID: "test", State: pipeline.StageSucceeded, JobID: "job-test", Attempt: 2},
	})
	store.Close()

	ds := &webDataSource{home: home}
	run, err := ds.PipelineRun(context.Background(), "prun-bench-stale")
	if err != nil {
		t.Fatalf("PipelineRun: %v", err)
	}
	byID := map[string]dashboard.PipelineStage{}
	for _, s := range run.Stages {
		byID[s.ID] = s
		if s.Retry != 0 || s.Cmd != "" || s.Kind != "" || s.AgentRuntime != "" {
			t.Fatalf("stale-spec stage %s carries spec metadata: %+v", s.ID, s)
		}
	}
	if byID["build"].Deps == nil || len(byID["build"].Deps) != 0 {
		t.Fatalf("stale build Deps = %#v, want non-nil empty root", byID["build"].Deps)
	}
	if !slices.Equal(byID["test"].Deps, []string{"build"}) {
		t.Fatalf("stale test Deps = %v, want structural fallback [build]", byID["test"].Deps)
	}
	// The run-level data is untouched by the gate.
	if run.Stages[0].Attempt+run.Stages[1].Attempt != 2 {
		t.Fatalf("store-side stage fields must survive: %+v", run.Stages)
	}
}

// TestWebDataSourcePipelineDetailNotFound pins the unknown-name sentinel: a missing
// pipeline maps to dashboard.ErrPipelineNotFound (the API layer serves 404), not an
// empty 200.
func TestWebDataSourcePipelineDetailNotFound(t *testing.T) {
	home := dashboardTestHome(t)
	ds := &webDataSource{home: home}

	_, err := ds.PipelineDetail(context.Background(), "no-such-pipeline")
	if err != dashboard.ErrPipelineNotFound {
		t.Fatalf("PipelineDetail(unknown) err = %v, want dashboard.ErrPipelineNotFound", err)
	}
}

// TestWebDataSourcePipelineDetailDeterministic pins byte-stable detail output across
// calls (the UI polls with a change-signature skip).
func TestWebDataSourcePipelineDetailDeterministic(t *testing.T) {
	home := dashboardTestHome(t)
	store := openPipelineTestStore(t, home)
	seedTestPipeline(t, store, db.Pipeline{
		Name: "listing-refresh", Repo: "jerryfane/noted", SpecYAML: diamondSpecYAML, Enabled: true,
	})
	base := time.UnixMilli(1_751_500_000_000).UTC()
	for i := 0; i < 3; i++ {
		started := base.Add(time.Duration(i) * time.Minute)
		seedTestRun(t, store, db.PipelineRun{
			ID: fmt.Sprintf("prun-det-%02d", i), Pipeline: "listing-refresh", Trigger: "schedule",
			State: pipeline.RunSucceeded, StartedAt: started, FinishedAt: started.Add(30 * time.Second),
		}, diamondStageRows(pipeline.StageSucceeded))
	}
	store.Close()

	ds := &webDataSource{home: home}
	first, err := ds.PipelineDetail(context.Background(), "listing-refresh")
	if err != nil {
		t.Fatalf("PipelineDetail first: %v", err)
	}
	second, err := ds.PipelineDetail(context.Background(), "listing-refresh")
	if err != nil {
		t.Fatalf("PipelineDetail second: %v", err)
	}
	if fmt.Sprintf("%+v", first) != fmt.Sprintf("%+v", second) {
		t.Fatalf("PipelineDetail not deterministic:\n first=%+v\nsecond=%+v", first, second)
	}
}
