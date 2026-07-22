package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/memory"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func harvestTestCandidate(t *testing.T, mutate func(*workflow.JobPayload), candidateMutate func(*db.MemoryHarvestCandidate)) db.MemoryHarvestCandidate {
	t.Helper()
	payload := workflow.JobPayload{
		Repo: "owner/repo", Sender: "local",
		Result: &workflow.AgentResult{Decision: "approved", Summary: strings.Repeat("durable repository detail ", 10)},
	}
	if mutate != nil {
		mutate(&payload)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	candidate := db.MemoryHarvestCandidate{
		JobID: "job", Agent: "worker", JobType: "ask", State: string(workflow.JobSucceeded),
		Payload: string(raw), ResultHash: strings.Repeat("a", 64),
	}
	if candidateMutate != nil {
		candidateMutate(&candidate)
	}
	return candidate
}

func TestMemoryHarvestZeroCostPreFilters(t *testing.T) {
	tests := []struct {
		name string
		p    func(*workflow.JobPayload)
		c    func(*db.MemoryHarvestCandidate)
		want string
	}{
		{name: "missing result", p: func(p *workflow.JobPayload) { p.Result = nil }, want: "missing payload result"},
		{name: "cancelled", c: func(c *db.MemoryHarvestCandidate) { c.State = string(workflow.JobCancelled) }, want: "cancelled job"},
		{name: "skipped decision", p: func(p *workflow.JobPayload) { p.Result.Decision = "skipped" }, want: "skipped decision"},
		{name: "produce", c: func(c *db.MemoryHarvestCandidate) { c.JobType = "produce" }, want: "produce job"},
		{name: "heartbeat", p: func(p *workflow.JobPayload) { p.Sender = "heartbeat" }, want: "heartbeat job"},
		{name: "pipeline runner", c: func(c *db.MemoryHarvestCandidate) { c.AgentRole = "pipeline-runner" }, want: "hidden pipeline runner"},
		{name: "finalize", p: func(p *workflow.JobPayload) { p.DelegationFinalize = true }, want: "delegation-finalize coordinator"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, got := memoryHarvestPayload(harvestTestCandidate(t, tc.p, tc.c))
			if got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		p    func(*workflow.JobPayload)
	}{
		{name: "pipeline sender agent stage", p: func(p *workflow.JobPayload) { p.Sender = "pipeline" }},
		{name: "ephemeral child", p: func(p *workflow.JobPayload) { p.Ephemeral = &workflow.EphemeralSpec{Runtime: "codex"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, reason := memoryHarvestPayload(harvestTestCandidate(t, tc.p, nil)); reason != "" {
				t.Fatalf("primary target was filtered: %s", reason)
			}
		})
	}
}

func TestParseMemoryHarvestReplyStrict(t *testing.T) {
	good, err := parseMemoryHarvestReply(`{"candidates":[{"content":"  durable   fact  "}]}`, 2)
	if err != nil || len(good.Candidates) != 1 || good.Candidates[0].Content != "durable fact" {
		t.Fatalf("good reply = %+v err=%v", good, err)
	}
	tooLong := strings.Repeat("x", memoryHarvestCandidateRunes+1)
	tests := []string{
		`not json`,
		`{"candidates":[],"scope":"repo"}`,
		`{"candidates":[]} {"candidates":[]}`,
		`{"candidates":[{"content":"ok","key":"chosen-by-model"}]}`,
		`{"candidates":[{"content":""}]}`,
		`{"candidates":[{"content":"` + tooLong + `"}]}`,
		`{"candidates":[{"content":"one"},{"content":"two"},{"content":"three"}]}`,
	}
	for _, raw := range tests {
		if _, err := parseMemoryHarvestReply(raw, 2); err == nil {
			t.Fatalf("expected strict parser rejection for %q", raw)
		}
	}
}

func TestProjectMemoryHarvestResultOnlySummaryAndFindingsWithinCap(t *testing.T) {
	result := workflow.AgentResult{
		Summary:     "durable summary",
		Findings:    []json.RawMessage{json.RawMessage(`"durable finding"`)},
		ChangesMade: []string{"must not be projected"},
		TestsRun:    []string{"must not be projected"},
		Needs:       []string{"must not be projected"},
	}
	projection, hasFindings := projectMemoryHarvestResult(result)
	if !hasFindings || !strings.Contains(projection, "durable summary") || !strings.Contains(projection, "durable finding") {
		t.Fatalf("projection len=%d hasFindings=%v", len(projection), hasFindings)
	}
	for _, excluded := range []string{"changes_made", "tests_run", "needs", "must not be projected"} {
		if strings.Contains(projection, excluded) {
			t.Fatalf("projection leaked %q: %s", excluded, projection)
		}
	}
	large, _ := projectMemoryHarvestResult(workflow.AgentResult{Summary: strings.Repeat("s", memoryHarvestProjectionCap+100)})
	if len(large) > memoryHarvestProjectionCap {
		t.Fatalf("projection len=%d exceeds cap=%d", len(large), memoryHarvestProjectionCap)
	}
}

func TestHarvestObservationDedupAndMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "harvest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	existing := "Activepieces strips version prefixes during import."
	if _, err := store.InsertMemoryObservation(ctx, db.MemoryObservation{
		Owner: db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: "someone"}, Repo: "owner/repo", Scope: memory.ScopeRepo,
		Key: "existing", Content: existing,
	}); err != nil {
		t.Fatal(err)
	}
	candidate := harvestTestCandidate(t, func(p *workflow.JobPayload) { p.OriginalAgent = "persistent-author" }, nil)
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(candidate.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	observations, err := harvestObservations(ctx, store, candidate, payload, []string{
		existing, "  SQLite   uses a pure-Go modernc driver in this repository. ", "SQLite uses a pure-Go modernc driver in this repository.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Fatalf("dedup observations = %+v", observations)
	}
	got := observations[0]
	if got.Owner.Kind != memory.OwnerKindShared || got.Owner.Ref != memory.SharedOwnerRef || got.AuthorRef != "persistent-author" ||
		got.Repo != "owner/repo" || got.Scope != memory.ScopeRepo || got.TrustMark != memory.TrustLow ||
		got.Provenance != "harvest:"+candidate.ResultHash || got.SourceJob != candidate.JobID ||
		got.Key != "harvest-sqlite-uses-a-pure-go-modernc-driver-"+memory.ContentHash(got.Content)[:16] {
		t.Fatalf("harvest metadata = %+v", got)
	}
}

func harvestObservationForContent(t *testing.T, content string) db.MemoryObservation {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "harvest-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	candidate := harvestTestCandidate(t, nil, nil)
	var payload workflow.JobPayload
	if err := json.Unmarshal([]byte(candidate.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	observations, err := harvestObservations(context.Background(), store, candidate, payload, []string{content})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Fatalf("observations = %+v, want one", observations)
	}
	return observations[0]
}

func TestHarvestSlugReadable(t *testing.T) {
	got := harvestObservationForContent(t, "Deployments use canaries before production traffic moves. Rollback remains automatic.")
	want := "harvest-deployments-use-canaries-before-production-traffic-" + memory.ContentHash(got.Content)[:16]
	if got.Key != want {
		t.Fatalf("key = %q, want %q", got.Key, want)
	}
}

func TestHarvestSlugStableForSameContent(t *testing.T) {
	content := "Deployments use canaries before production traffic moves."
	first := harvestObservationForContent(t, content)
	second := harvestObservationForContent(t, content)
	if first.Key != second.Key {
		t.Fatalf("same content keys differ: %q vs %q", first.Key, second.Key)
	}
}

func TestHarvestSlugDistinctForDistinctContent(t *testing.T) {
	first := harvestObservationForContent(t, "Deployments use canaries. First detail follows.")
	second := harvestObservationForContent(t, "Deployments use canaries! Second detail follows.")
	firstPrefix := strings.TrimSuffix(first.Key, memory.ContentHash(first.Content)[:16])
	secondPrefix := strings.TrimSuffix(second.Key, memory.ContentHash(second.Content)[:16])
	if firstPrefix != secondPrefix {
		t.Fatalf("title slugs differ: %q vs %q", firstPrefix, secondPrefix)
	}
	if first.Key == second.Key {
		t.Fatalf("distinct content produced the same key: %q", first.Key)
	}
}

func TestHarvestSlugDegenerateContent(t *testing.T) {
	got := harvestObservationForContent(t, "```go\nfmt.Println(\"hello\")\n```")
	want := "harvest-untitled-" + memory.ContentHash(got.Content)[:16]
	if got.Key != want {
		t.Fatalf("key = %q, want %q", got.Key, want)
	}
}

func TestAutoConfirmEligibleProvenanceFailClosed(t *testing.T) {
	for _, provenance := range []string{"ingest:notes.md", "chat:thread#1", "workflow:release#2"} {
		if !autoConfirmEligibleProvenance(provenance) {
			t.Fatalf("allowlisted provenance rejected: %q", provenance)
		}
	}
	for _, provenance := range []string{"", "harvest:abc", "distill:job", "future:source", "x-ingest:notes"} {
		if autoConfirmEligibleProvenance(provenance) {
			t.Fatalf("unlisted provenance accepted: %q", provenance)
		}
	}
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "autoconfirm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	confirmed, skipped, err := autoConfirmObservationIfEnabled(ctx, store, db.MemoryObservation{
		Owner: db.MemoryOwner{Kind: memory.OwnerKindShared, Ref: memory.SharedOwnerRef}, AuthorRef: "worker",
		Repo: "owner/repo", Scope: memory.ScopeRepo, Key: "harvest-x", Content: "durable fact", Provenance: "harvest:abc",
	}, true)
	if err != nil || confirmed || skipped {
		t.Fatalf("harvest auto-confirm = confirmed:%v skipped:%v err:%v", confirmed, skipped, err)
	}
	rows, err := store.ListConfirmedMemories(ctx, "", "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("confirmed rows = %+v err=%v", rows, err)
	}
}

func newMemoryHarvestSweepStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "harvest-sweep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	initialized, err := store.InitializeMemoryHarvestState(context.Background())
	if err != nil || !initialized {
		t.Fatalf("initialize high-water = %v err=%v", initialized, err)
	}
	return store
}

func seedMemoryHarvestSweepJob(t *testing.T, store *db.Store, id string) {
	t.Helper()
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Sender: "local",
		Result: &workflow.AgentResult{Decision: "approved", Summary: strings.Repeat("durable repository behavior "+id+" ", 8)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID: id, Agent: "worker", Type: "ask", State: string(workflow.JobSucceeded), Payload: string(payload),
	}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func withMemoryHarvestClassifier(t *testing.T, deliver memoryHarvestDeliverFunc) {
	t.Helper()
	previous := memoryHarvestLLMDeliver
	memoryHarvestLLMDeliver = deliver
	t.Cleanup(func() { memoryHarvestLLMDeliver = previous })
}

func TestMemoryHarvestSweepCapsClassifierCalls(t *testing.T) {
	ctx := context.Background()
	store := newMemoryHarvestSweepStore(t)
	for i := 0; i < 8; i++ {
		seedMemoryHarvestSweepJob(t, store, fmt.Sprintf("cap-job-%02d", i))
	}
	settings := config.DefaultMemorySettings()
	settings.HarvestEnabled = true
	settings.HarvestMaxJobsPerSweep = 5
	calls := 0
	withMemoryHarvestClassifier(t, func(context.Context, runtime.Agent, string) (string, error) {
		calls++
		return fmt.Sprintf(`{"candidates":[{"content":"Repository import normalization fact number %d remains stable across runs."}]}`, calls), nil
	})

	result, err := sweepMemoryHarvest(ctx, "", store, settings, io.Discard)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if calls != 5 || result.ClassifierCall != 5 || result.Classified != 5 {
		t.Fatalf("calls=%d result=%+v, want exactly five classifiers", calls, result)
	}
}

func TestMemoryHarvestSweepTurnsAbandonedStartedReceiptUncertain(t *testing.T) {
	ctx := context.Background()
	store := newMemoryHarvestSweepStore(t)
	seedMemoryHarvestSweepJob(t, store, "crash-job")
	rows, err := store.ListMemoryHarvestCandidates(ctx, time.Now().Add(-memoryHarvestReceiptLease), 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("candidates=%+v err=%v", rows, err)
	}
	old := time.Now().UTC().Add(-memoryHarvestReceiptLease - time.Minute)
	if claimed, err := store.ClaimMemoryHarvestRun(ctx, rows[0].JobID, rows[0].ResultHash, old, old.Add(-memoryHarvestReceiptLease)); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if started, err := store.StartMemoryHarvestRun(ctx, rows[0].JobID, rows[0].ResultHash, old); err != nil || !started {
		t.Fatalf("start=%v err=%v", started, err)
	}
	calls := 0
	withMemoryHarvestClassifier(t, func(context.Context, runtime.Agent, string) (string, error) {
		calls++
		return `{"candidates":[]}`, nil
	})
	settings := config.DefaultMemorySettings()
	settings.HarvestEnabled = true

	result, err := sweepMemoryHarvest(ctx, "", store, settings, io.Discard)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	run, ok, err := store.GetMemoryHarvestRun(ctx, rows[0].JobID, rows[0].ResultHash)
	if err != nil || !ok || run.State != db.MemoryHarvestUncertain || result.Uncertain != 1 || calls != 0 {
		t.Fatalf("run=%+v ok=%v result=%+v calls=%d err=%v", run, ok, result, calls, err)
	}
}

func TestMemoryHarvestSweepHonorsGlobalDisabled(t *testing.T) {
	ctx := context.Background()
	store := newMemoryHarvestSweepStore(t)
	seedMemoryHarvestSweepJob(t, store, "disabled-job")
	rows, err := store.ListMemoryHarvestCandidates(ctx, time.Now().Add(-memoryHarvestReceiptLease), 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("candidates=%+v err=%v", rows, err)
	}
	calls := 0
	withMemoryHarvestClassifier(t, func(context.Context, runtime.Agent, string) (string, error) {
		calls++
		return `{"candidates":[]}`, nil
	})
	settings := config.DefaultMemorySettings()
	settings.HarvestEnabled = true
	settings.Disabled = true

	result, err := sweepMemoryHarvest(ctx, "", store, settings, io.Discard)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_, exists, err := store.GetMemoryHarvestRun(ctx, rows[0].JobID, rows[0].ResultHash)
	if err != nil || exists || calls != 0 || result != (memoryHarvestSweepResult{}) {
		t.Fatalf("exists=%v calls=%d result=%+v err=%v", exists, calls, result, err)
	}
}

func TestDaemonMemoryHarvestLineOffAndUncertain(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if got := daemonMemoryHarvestLine(paths, home); got != "" {
		t.Fatalf("off status line=%q, want absent", got)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if claimed, err := store.ClaimMemoryHarvestRun(context.Background(), "status-job", "status-hash", now, now.Add(-time.Minute)); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if started, err := store.StartMemoryHarvestRun(context.Background(), "status-job", "status-hash", now); err != nil || !started {
		t.Fatalf("start=%v err=%v", started, err)
	}
	if err := store.MarkMemoryHarvestUncertain(context.Background(), "status-job", "status-hash", "unknown outcome", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := daemonMemoryHarvestLine(paths, home); !strings.Contains(got, "uncertain receipts: 1") {
		t.Fatalf("uncertain status line=%q, want present", got)
	}
}
