package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

// candidateNotifyFixture installs a base "planner" template, imports a candidate,
// and returns the store, the just-pending version, and the candidate package — the
// shared setup for the #471 notify/auto-promote tests.
func candidateNotifyFixture(t *testing.T) (*db.Store, db.AgentTemplateVersion, skillopt.CandidatePackage, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with stronger guidance.")
	version, err := skillopt.ImportCandidatePackageWithOptions(context.Background(), store, candidate, skillopt.CandidateImportOptions{SourcePath: "candidate.json"})
	if err != nil {
		t.Fatalf("ImportCandidatePackageWithOptions returned error: %v", err)
	}
	if version.State != "pending" {
		t.Fatalf("imported version state = %q, want pending", version.State)
	}
	return store, version, candidate, paths.Home
}

// TestRunCandidateNotifyEmitsAwaitingExactlyOnce proves the shared helper emits
// EXACTLY ONE candidate.awaiting_promotion carrying the version id (JobID) and
// template id (RootID), and — with auto_promote off (the default) — NO
// candidate.auto_promoted and no promotion.
func TestRunCandidateNotifyEmitsAwaitingExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)
	sink := &recordingSink{}

	if err := runCandidateNotify(ctx, store, sink, config.DefaultSkillOptPolicy(), candidate, version, nil, false, nil, 0, "", 0, 0); err != nil {
		t.Fatalf("runCandidateNotify returned error: %v", err)
	}

	awaiting := sink.byType(events.EventCandidateAwaitingPromotion)
	if len(awaiting) != 1 {
		t.Fatalf("awaiting_promotion emits = %d, want 1", len(awaiting))
	}
	if awaiting[0].JobID != version.ID || awaiting[0].RootID != version.TemplateID {
		t.Fatalf("awaiting event ids = %q/%q, want %q/%q", awaiting[0].JobID, awaiting[0].RootID, version.ID, version.TemplateID)
	}
	if awaiting[0].Status != "awaiting_promotion" {
		t.Fatalf("awaiting status = %q", awaiting[0].Status)
	}
	if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 0 {
		t.Fatalf("auto_promoted emits = %d, want 0 (default policy off)", got)
	}
	if sink.count() != 1 {
		t.Fatalf("total emits = %d, want 1 (no double-emit)", sink.count())
	}
	// The version stays pending (manual default).
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("version state = %q, want pending (no promotion)", after.State)
	}
}

// TestRunCandidateNotifyNilSinkIsNoOp proves the helper is byte-identical when the
// event stream is OFF: a nil sink emits nothing and the pending row is unchanged.
func TestRunCandidateNotifyNilSinkIsNoOp(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)

	if err := runCandidateNotify(ctx, store, nil, config.DefaultSkillOptPolicy(), candidate, version, nil, false, nil, 0, "", 0, 0); err != nil {
		t.Fatalf("runCandidateNotify with nil sink returned error: %v", err)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("version state = %q, want pending", after.State)
	}
}

// TestRunSkillOptImportOffByDefaultByteIdentical proves the full `skillopt import`
// path with [events] AND [skillopt].auto_promote unset writes ONLY the pending
// version (no promotion, no current change) — the manual, byte-identical default.
func TestRunSkillOptImportOffByDefaultByteIdentical(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(ctx, cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	before, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	candidate := cliSkillOptCandidatePackage(t, "planner", before.VersionID, "Plan with stronger guidance.")
	store.Close()

	candidatePath := filepath.Join(t.TempDir(), "candidate.json")
	writeCandidateJSON(t, candidatePath, candidate)

	var stdout, stderr bytes.Buffer
	if code := runSkillOptImport([]string{"--home", home, "--file", candidatePath}, &stdout, &stderr); code != 0 {
		t.Fatalf("runSkillOptImport exit = %d, stderr: %s", code, stderr.String())
	}

	store2, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("reopen store returned error: %v", err)
	}
	defer store2.Close()
	after, err := store2.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate after import returned error: %v", err)
	}
	if after.VersionID != before.VersionID || after.Content != before.Content {
		t.Fatalf("current template changed by import: before=%q after=%q", before.VersionID, after.VersionID)
	}
	pending, err := store2.ListPendingAgentTemplateVersions(ctx, "planner")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
	}
	if len(pending) != 1 || pending[0].State != "pending" {
		t.Fatalf("expected exactly one pending candidate, got %+v", pending)
	}
}

// TestNotifyAndMaybeAutoPromoteFlushesPerInvocationSink proves the HIGH #471 fix
// end to end: the SHORT-LIVED CLI path (notifyAndMaybeAutoPromoteCandidate) builds
// its OWN per-invocation webhook sink and FLUSHES it before returning, so the
// candidate.awaiting_promotion POST lands even though the "process" (here, the
// helper) returns immediately after. Without the deferred flush the queued event
// would be destroyed when the drain goroutine never gets to run.
func TestNotifyAndMaybeAutoPromoteFlushesPerInvocationSink(t *testing.T) {
	ctx := context.Background()
	store, version, candidate, home := candidateNotifyFixture(t)

	var delivered atomic.Int64
	got := make(chan events.Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev events.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		delivered.Add(1)
		got <- ev
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Configure [events] for this home so buildDaemonEventSink yields a real webhook
	// sink pointed at the test server. The fixture returns the already-resolved
	// <home>/.gitmoot ROOT, so the config.toml lives directly under it.
	cfgPath := filepath.Join(home, config.ConfigName)
	if err := os.WriteFile(cfgPath, []byte("[events]\nwebhook_url = \""+srv.URL+"\"\n"), 0o644); err != nil {
		t.Fatalf("write events config: %v", err)
	}

	if err := notifyAndMaybeAutoPromoteCandidate(ctx, store, home, candidate, version, ""); err != nil {
		t.Fatalf("notifyAndMaybeAutoPromoteCandidate returned error: %v", err)
	}

	// The flush must have delivered the awaiting_promotion event by the time the
	// helper returned — no post-return sleep needed.
	if n := delivered.Load(); n != 1 {
		t.Fatalf("delivered = %d immediately after return, want 1 (the per-invocation sink must flush before exit)", n)
	}
	select {
	case ev := <-got:
		if ev.Type != events.EventCandidateAwaitingPromotion {
			t.Fatalf("event type = %q, want candidate.awaiting_promotion", ev.Type)
		}
		if ev.JobID != version.ID || ev.RootID != version.TemplateID {
			t.Fatalf("event ids = %q/%q, want %q/%q", ev.JobID, ev.RootID, version.ID, version.TemplateID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected the delivered awaiting_promotion event")
	}
}

// TestResolveBanditConfidenceNoArmIsNil proves the notify seam contributes
// nothing when the candidate has no bandit arm: (nil, 0, "") so the optional
// confidence guardrail stays a no-op and the awaiting detail is unchanged.
func TestResolveBanditConfidenceNoArmIsNil(t *testing.T) {
	ctx := context.Background()
	store, version, _, _ := candidateNotifyFixture(t)
	conf, samples, summary, wins, losses := resolveBanditConfidence(ctx, store, version)
	if conf != nil || samples != 0 || summary != "" || wins != 0 || losses != 0 {
		t.Fatalf("no-arm confidence = (%v, %d, %q, %d, %d), want (nil, 0, \"\", 0, 0)", conf, samples, summary, wins, losses)
	}
}

// TestResolveBanditConfidenceComputesFromArms proves the seam computes a
// confidence when the candidate (challenger) arm has pulls: a challenger far
// ahead of the champion yields a high P(>) and a summary naming the challenger
// pull count. The champion arm is keyed by the template's CURRENT version id,
// which resolveBanditConfidence resolves via GetAgentTemplate.
func TestResolveBanditConfidenceComputesFromArms(t *testing.T) {
	ctx := context.Background()
	store, version, _, _ := candidateNotifyFixture(t)

	champ, err := store.GetAgentTemplate(ctx, version.TemplateID)
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	if err := store.UpsertBanditArm(ctx, db.BanditArm{TemplateID: version.TemplateID, TemplateVersionID: champ.VersionID, Alpha: 2, Beta: 40, Pulls: 40}); err != nil {
		t.Fatalf("seed champion arm: %v", err)
	}
	if err := store.UpsertBanditArm(ctx, db.BanditArm{TemplateID: version.TemplateID, TemplateVersionID: version.ID, Alpha: 40, Beta: 2, Pulls: 40}); err != nil {
		t.Fatalf("seed challenger arm: %v", err)
	}

	conf, samples, summary, wins, losses := resolveBanditConfidence(ctx, store, version)
	if conf == nil {
		t.Fatal("expected a non-nil confidence when the challenger arm has pulls")
	}
	if *conf < 0.95 {
		t.Fatalf("P(>) = %.4f, want > 0.95 for a dominant challenger", *conf)
	}
	if samples != 40 {
		t.Fatalf("samples = %d, want 40 (challenger pulls)", samples)
	}
	if !strings.Contains(summary, "over 40 samples") {
		t.Fatalf("summary = %q, want it to name 40 samples", summary)
	}
	// The challenger arm Alpha=40, Beta=2 decodes to 39 candidate wins / 1 loss
	// (wins = Alpha-1, losses = Beta-1) — the PACE (#687) discordant-pair stream.
	if wins != 39 || losses != 1 {
		t.Fatalf("win/loss = (%d, %d), want (39, 1) from Alpha=40, Beta=2", wins, losses)
	}
}

func writeCandidateJSON(t *testing.T, path string, candidate skillopt.CandidatePackage) {
	t.Helper()
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
}
