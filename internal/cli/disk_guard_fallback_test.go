package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// fallbackFixture is diskGuardRemoteFixture with the review-only threshold set
// above the dispatch floor, a codex reviewer, and disk measured at freeGiB.
func fallbackFixture(t *testing.T, freeGiB uint64) (context.Context, *db.Store, jobWorker) {
	t.Helper()
	ctx, store, worker := diskGuardRemoteFixture(t, true, 4)
	paths, err := worker.configPaths()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A second [disk_guard] section would be a new table; the loader reads
	// key = value pairs under the most recent header, so re-open it.
	fmt.Fprintf(f, "\n[disk_guard]\nremote_reviews_below_bytes = %d\n", uint64(30)<<30)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "rev-codex", Runtime: "codex"}); err != nil {
		t.Fatal(err)
	}
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 100 << 30, FreeBytes: freeGiB << 30}, nil
	})
	return ctx, store, worker
}

func messagesTo(t *testing.T, ctx context.Context, store *db.Store, role string) []db.Message {
	t.Helper()
	msgs, err := store.ListMessages(ctx, role, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func eventKinds(t *testing.T, ctx context.Context, store *db.Store, jobID string) []string {
	t.Helper()
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// TestReviewThresholdMovesReviewsRemoteButKeepsOtherJobsLocal: with free space
// between the review threshold and the dispatch floor, the review goes remote
// and other jobs still dispatch here; above the threshold nothing moves.
func TestReviewThresholdMovesReviewsRemoteButKeepsOtherJobsLocal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		freeGiB    uint64
		wantRemote bool
	}{
		{name: "below review threshold, above floor", freeGiB: 20, wantRemote: true},
		{name: "above review threshold", freeGiB: 40, wantRemote: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, store, worker := fallbackFixture(t, tc.freeGiB)
			queueDiskGuardJob(t, ctx, store, "review", "rev-omp", "review", `,"model":"anthropic/claude-sonnet-5-5"`)
			queueDiskGuardJob(t, ctx, store, "implement", "rev-omp", "implement", "")
			got := pendingIDs(t, ctx, worker)
			slices.Sort(got)
			if !slices.Equal(got, []string{"implement", "review"}) {
				t.Fatalf("pending = %v, want both: the implement job runs here, the review runs (remote or local)", got)
			}
			if gotRemote := storedBackend(t, ctx, store, "review") == "remote"; gotRemote != tc.wantRemote {
				t.Fatalf("review stored remote=%v, want %v", gotRemote, tc.wantRemote)
			}
			if b := storedBackend(t, ctx, store, "implement"); b == "remote" {
				t.Fatalf("a non-review job was moved to remote")
			}
		})
	}
}

// TestCodexReviewMovesToOmpWithTheSameModelAndAlertsOnce: the codex review
// runs remotely as omp with the identical model, an event records the switch,
// and the requester gets exactly one message across repeated daemon passes.
func TestCodexReviewMovesToOmpWithTheSameModelAndAlertsOnce(t *testing.T) {
	ctx, store, worker := fallbackFixture(t, 20)
	queueDiskGuardJob(t, ctx, store, "codex-review", "rev-codex", "review", `,"model":"gpt-6-astra","review_requester":"usmle-app"`)
	for pass := 0; pass < 3; pass++ {
		pendingIDs(t, ctx, worker)
	}
	p := storedPayload(t, ctx, store, "codex-review")
	if b, _ := p.ExecBackendOverride(); b != "remote" {
		t.Fatalf("codex review backend %q, want remote", b)
	}
	if p.RuntimeOverride != "omp" || p.Model != "openai-codex/gpt-6-astra" {
		t.Fatalf("runtime=%q model=%q, want omp with the same model openai-codex/gpt-6-astra", p.RuntimeOverride, p.Model)
	}
	if !slices.Contains(eventKinds(t, ctx, store, "codex-review"), diskGuardRuntimeChangedEventKind) {
		t.Fatalf("no %s event", diskGuardRuntimeChangedEventKind)
	}
	msgs := messagesTo(t, ctx, store, "usmle-app")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "codex -> omp") || !strings.Contains(msgs[0].Body, "codex-review") {
		t.Fatalf("messages to requester = %+v, want exactly one naming the job and the runtime change", msgs)
	}
}

// TestReviewThatCannotRunRemotelyWaitsAndAlertsWithReason: an unknown model
// is never replaced; the review waits here and the requester is told why.
func TestReviewThatCannotRunRemotelyWaitsAndAlertsWithReason(t *testing.T) {
	ctx, store, worker := fallbackFixture(t, 20)
	queueDiskGuardJob(t, ctx, store, "codex-nomodel", "rev-codex", "review", `,"review_requester":"usmle-app"`)
	queueDiskGuardJob(t, ctx, store, "noexec", "rev-omp", "review", `,"model":"anthropic/claude-sonnet-5-5","review_no_execution":true,"acting_org_role":"gitmoot"`)
	for pass := 0; pass < 2; pass++ {
		if got := pendingIDs(t, ctx, worker); len(got) != 0 {
			t.Fatalf("pending = %v, want both reviews held off this host", got)
		}
	}
	for id, role := range map[string]string{"codex-nomodel": "usmle-app", "noexec": "gitmoot"} {
		p := storedPayload(t, ctx, store, id)
		if b, _ := p.ExecBackendOverride(); b == "remote" || p.RuntimeOverride != "" {
			t.Fatalf("%s was changed (backend %q runtime %q); it must wait unchanged", id, b, p.RuntimeOverride)
		}
		msgs := messagesTo(t, ctx, store, role)
		if len(msgs) != 1 || !strings.Contains(msgs[0].Body, id) || !strings.Contains(msgs[0].Body, "cannot run on the remote backend") {
			t.Fatalf("messages to %s = %+v, want one hold alert naming %s", role, msgs, id)
		}
	}
}

// TestUndoneRemoteRouteRestoresTheReviewsOwnRuntimeAndModel: the codex -> omp
// switch is only for the remote run. When E2B refuses the run, the review goes
// back to waiting locally exactly as requested, and the requester is told once.
func TestUndoneRemoteRouteRestoresTheReviewsOwnRuntimeAndModel(t *testing.T) {
	ctx, store, worker := fallbackFixture(t, 20)
	queueDiskGuardJob(t, ctx, store, "codex-review", "rev-codex", "review", `,"model":"gpt-6-astra","review_requester":"usmle-app"`)
	pendingIDs(t, ctx, worker)
	if p := storedPayload(t, ctx, store, "codex-review"); p.RuntimeOverride != "omp" {
		t.Fatalf("precondition: review not switched to omp (runtime %q)", p.RuntimeOverride)
	}
	job, err := store.GetJob(ctx, "codex-review")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		worker.undoDiskGuardRoute(ctx, job, workflow.JobFailed, false, errors.New("e2b refused the sandbox"))
	}
	p := storedPayload(t, ctx, store, "codex-review")
	if b, _ := p.ExecBackendOverride(); b == "remote" {
		t.Fatalf("still routed remote after undo")
	}
	if p.RuntimeOverride != "" || p.Model != "gpt-6-astra" || p.DiskGuardRuntimeSwitched {
		t.Fatalf("after undo runtime=%q model=%q switched=%v, want the review's own (no override, gpt-6-astra)", p.RuntimeOverride, p.Model, p.DiskGuardRuntimeSwitched)
	}
	undo := 0
	for _, m := range messagesTo(t, ctx, store, "usmle-app") {
		if strings.Contains(m.Body, "did not start") {
			undo++
		}
	}
	if undo != 1 {
		t.Fatalf("undo alerts = %d, want exactly 1", undo)
	}
}

// TestBandHeldReviewIsNeverSilent (#2373 review, P3): a review in the review
// band that will not move remotely (here it was requested --exec-backend
// local) is held with a durable event and exactly one alert naming why.
func TestBandHeldReviewIsNeverSilent(t *testing.T) {
	ctx, store, worker := fallbackFixture(t, 20)
	queueDiskGuardJob(t, ctx, store, "local-review", "rev-omp", "review", `,"model":"anthropic/claude-sonnet-5-5","exec_backend":"local","review_requester":"usmle-app"`)
	for pass := 0; pass < 2; pass++ {
		if got := pendingIDs(t, ctx, worker); len(got) != 0 {
			t.Fatalf("pending = %v, want the review held in the band", got)
		}
	}
	if !slices.Contains(eventKinds(t, ctx, store, "local-review"), diskGuardReviewHeldEventKind) {
		t.Fatalf("no %s event for the held review", diskGuardReviewHeldEventKind)
	}
	msgs := messagesTo(t, ctx, store, "usmle-app")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "--exec-backend local") {
		t.Fatalf("messages = %+v, want one alert naming --exec-backend local", msgs)
	}
}
