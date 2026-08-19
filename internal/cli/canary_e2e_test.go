package cli

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// canaryE2EFixture installs a planner template (v1 = current champion) and drives
// the REAL runCandidateNotify in canary mode so v2 becomes a live `canary` row
// BEHIND the still-current champion — exactly the prod promote path
// (candidate_notify.go), not a hand-written CanaryPromote. It also registers an
// agent bound to the template so the routing leg can resolve through the genuine
// Mailbox.Enqueue path. It returns the store, the champion + canary version ids,
// their distinct resolved commits (the routing discriminator), and the agent name.
func canaryE2EFixture(t *testing.T) (store *db.Store, championID, canaryID, championCommit, canaryCommit, agentName string) {
	t.Helper()
	ctx := context.Background()
	store, version, candidate, _ := candidateNotifyFixture(t)

	champion, err := store.GetAgentTemplate(ctx, version.TemplateID)
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}

	// Drive the REAL prod promote path in canary mode: a guardrails-pass candidate
	// goes to the `canary` state (NOT current) behind the champion, stamping
	// canary_started_at. minSamples here is the candidate-promote floor (1, satisfied
	// by the single real-CI feedback event); the daemon regression floor is set
	// separately on the harvester below.
	autoPromoteCandidateScore(&candidate, 0.96)
	policy := autoPromotePolicy(1, 0.9)
	policy.AutoPromoteCanary = true
	policy.AutoPromoteCanarySample = floatPtrCLI(0.5)
	if err := runCandidateNotify(ctx, store, &recordingSink{}, policy, candidate, version, []db.FeedbackEvent{realCIFeedbackEvent()}, false, nil, 0, "", 0, 0); err != nil {
		t.Fatalf("runCandidateNotify (canary promote): %v", err)
	}

	// The candidate is now a canary; the champion is unchanged and still current.
	canary, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("get canary version: %v", err)
	}
	if canary.State != "canary" {
		t.Fatalf("candidate state = %q, want canary", canary.State)
	}
	afterChampion, err := store.GetAgentTemplate(ctx, version.TemplateID)
	if err != nil {
		t.Fatalf("GetAgentTemplate after promote: %v", err)
	}
	if afterChampion.VersionID != champion.VersionID {
		t.Fatalf("champion moved under canary: %q -> %q", champion.VersionID, afterChampion.VersionID)
	}
	if canary.ResolvedCommit == "" || champion.ResolvedCommit == "" || canary.ResolvedCommit == champion.ResolvedCommit {
		t.Fatalf("champion/canary commits not distinct: champion=%q canary=%q", champion.ResolvedCommit, canary.ResolvedCommit)
	}

	// An agent bound to the template, so Mailbox.Enqueue resolves the template
	// reference through templateSnapshot -> routeCanary (the prod routing seam).
	agentName = "planner-bot"
	if err := store.UpsertAgent(ctx, db.Agent{Name: agentName, Runtime: "codex", TemplateID: version.TemplateID}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	return store, champion.VersionID, canary.ID, champion.ResolvedCommit, canary.ResolvedCommit, agentName
}

// enqueueImplement runs ONE real Mailbox.Enqueue for an implement job and returns
// the persisted, parsed payload. The payload's TemplateResolvedCommit is whatever
// the routing seam resolved — champion or canary — so the caller can both assert
// the route AND feed the SAME payload back into the harvester, connecting the
// routing and attribution seams through the prod job record.
func enqueueImplement(t *testing.T, mb workflow.Mailbox, agentName string, pr int, id string) (db.Job, workflow.JobPayload) {
	t.Helper()
	job, err := mb.Enqueue(context.Background(), workflow.JobRequest{
		ID:          id,
		Agent:       agentName,
		Action:      "implement",
		Repo:        "o/r",
		PullRequest: pr,
	})
	if err != nil {
		t.Fatalf("Enqueue %s: %v", id, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload %s: %v", id, err)
	}
	return job, payload
}

// scriptedDraws returns a deterministic CanaryRand that yields the given draws in
// order, failing the test if drawn more times than scripted. Each Enqueue that
// reaches an active canary draws exactly once.
func scriptedDraws(t *testing.T, draws ...float64) func() float64 {
	t.Helper()
	var mu sync.Mutex
	i := 0
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(draws) {
			t.Fatalf("canary rng drawn more than the %d scripted times", len(draws))
		}
		d := draws[i]
		i++
		return d
	}
}

// regressingChangesOutcome is a verifiable NEGATIVE outcome (choice "b", score 0)
// the inner #465 harvester attributes to the routed version's auto-trace run.
func regressingChangesOutcome(pr int) workflow.Outcome {
	return workflow.Outcome{Kind: workflow.OutcomeChangesRequested, Repo: "o/r", PullRequest: pr, FixRounds: 3}
}

// healthyMergeOutcome is a verifiable STRONG-POSITIVE merge (real external CI via
// the stub status reader, choice "a", score 1.0).
func healthyMergeOutcome(pr int) workflow.Outcome {
	return workflow.Outcome{Kind: workflow.OutcomeMerged, Repo: "o/r", PullRequest: pr, HeadSHA: "headsha"}
}

// passingCIStatusReader is a CombinedStatusReader stub reporting a single passing
// EXTERNAL (non-gitmoot/) commit status, so the inner harvester's no-CI guard
// scores a merge as a strong real-CI positive without any network.
type passingCIStatusReader struct{}

func (passingCIStatusReader) GetCombinedStatus(_ context.Context, _ github.Repository, _ string) (github.CombinedStatus, error) {
	return github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci/build", State: "success"}}}, nil
}

func (passingCIStatusReader) ListPullRequestChecks(_ context.Context, _ github.Repository, _ int64) ([]github.PullRequestCheck, error) {
	return nil, nil
}

// TestCanaryLifecycleE2E drives the #484 canary lifecycle END TO END with NO LLM,
// no network and deterministic routing, exercising the two integration seams the
// existing unit tests bypass:
//
//	(1) Mailbox.Enqueue -> routeCanary actually SENDING sampled traffic to the
//	    canary version's snapshot, and
//	(2) the canaryRegressionHarvester.Harvest() decorator wrapping the REAL #465
//	    inner harvester, which ATTRIBUTES a canary-routed job's outcome to the
//	    CANARY version's OWN auto-trace run (skillopt.AutoTraceRunID(canaryID)) via
//	    the same UpsertFeedbackEvent path prod uses — NOT hand-seeded.
//
// Each subtest builds a fresh fixture (a rollback rejects its canary). The champion
// baseline IS seeded (the lifecycle's pre-existing evidence), but the CANARY run is
// populated ONLY by Harvest's attribution — which is what makes the regression
// window see any canary outcomes at all. The champion baseline is seeded AFTER the
// canary starts so its events fall inside the canary_started_at window the
// comparator bounds to.
func TestCanaryLifecycleE2E(t *testing.T) {
	t.Run("routes_sampled_traffic_then_rolls_back_a_regressing_canary", func(t *testing.T) {
		ctx := context.Background()
		store, championID, canaryID, championCommit, canaryCommit, agentName := canaryE2EFixture(t)

		// Champion baseline: strong real-CI positives, seeded AFTER the canary started.
		seedAutoTraceFeedback(t, store, "planner", championID, "a", 3)

		// ROUTING LEG: sample=0.5. Draws below the sample resolve the CANARY snapshot;
		// draws at/above resolve the CHAMPION. Proves routing genuinely splits traffic.
		mb := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("canary routing enqueue test"))
		mb.CanaryEnabled = true
		mb.CanaryRand = scriptedDraws(t, 0.10, 0.20, 0.30, 0.80)
		canaryJobs := routeAndAssertSplit(t, mb, agentName, "rollback", championCommit, canaryCommit, []routeCase{
			{pr: 101, canary: true}, {pr: 102, canary: true}, {pr: 103, canary: true}, {pr: 104, canary: false},
		})
		if len(canaryJobs) != 3 {
			t.Fatalf("canary-routed jobs = %d, want 3", len(canaryJobs))
		}

		// ATTRIBUTION + ROLLBACK LEG: the REAL harvester (inner #465 harvester wrapped
		// by the #484 regression decorator). Feed the routed canary payloads through
		// Harvest() with regressing outcomes. Harvest's inner leg attributes each
		// outcome to AutoTraceRunID(canaryID); its decorator then evaluates the window.
		// The canary run is NEVER hand-seeded.
		sink := &recordingSink{}
		h := &canaryRegressionHarvester{
			inner:      skillopt.NewOutcomeHarvester(store, nil),
			store:      store,
			sink:       sink,
			minSamples: floatPtrCLIInt(3),
		}
		for i, payload := range canaryJobs {
			job := db.Job{ID: fmt.Sprintf("rollback-job-%d", i), Agent: agentName, Type: "implement"}
			if err := h.Harvest(ctx, job, payload, regressingChangesOutcome(200+i)); err != nil {
				t.Fatalf("Harvest #%d: %v", i, err)
			}
		}

		// The harvester attributed the outcomes to the canary's OWN run — proof the
		// routing->attribution chain is wired (an empty run would mean routing never
		// reached the canary version, and the window would hold instead of rolling back).
		assertCanaryFeedbackCount(t, store, canaryID, 3)

		// ROLLBACK: candidate.rolled_back emitted EXACTLY ONCE, the canary is rejected,
		// and the prior champion is STILL the live current version (never displaced).
		if got := len(sink.byType(events.EventCandidateRolledBack)); got != 1 {
			t.Fatalf("rolled_back emits = %d, want exactly 1", got)
		}
		if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 0 {
			t.Fatalf("auto_promoted emits = %d, want 0 (regressing canary)", got)
		}
		rejected, err := store.GetAgentTemplateVersionByID(ctx, canaryID)
		if err != nil {
			t.Fatalf("get canary after rollback: %v", err)
		}
		if rejected.State != "rejected" {
			t.Fatalf("canary state = %q, want rejected", rejected.State)
		}
		tmpl, err := store.GetAgentTemplate(ctx, "planner")
		if err != nil {
			t.Fatalf("GetAgentTemplate after rollback: %v", err)
		}
		if tmpl.VersionID != championID {
			t.Fatalf("current version = %q, want champion %q (champion must stay live)", tmpl.VersionID, championID)
		}
	})

	t.Run("routes_sampled_traffic_then_graduates_a_healthy_canary", func(t *testing.T) {
		ctx := context.Background()
		store, championID, canaryID, championCommit, canaryCommit, agentName := canaryE2EFixture(t)

		// Champion baseline: strong real-CI positives within the window.
		seedAutoTraceFeedback(t, store, "planner", championID, "a", 3)

		mb := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("canary routing enqueue test"))
		mb.CanaryEnabled = true
		mb.CanaryRand = scriptedDraws(t, 0.05, 0.15, 0.25, 0.95)
		canaryJobs := routeAndAssertSplit(t, mb, agentName, "graduate", championCommit, canaryCommit, []routeCase{
			{pr: 301, canary: true}, {pr: 302, canary: true}, {pr: 303, canary: true}, {pr: 304, canary: false},
		})

		// Healthy canary: strong real-CI merges (parity with the champion baseline).
		sink := &recordingSink{}
		h := &canaryRegressionHarvester{
			inner:      skillopt.NewOutcomeHarvester(store, passingCIStatusReader{}),
			store:      store,
			sink:       sink,
			minSamples: floatPtrCLIInt(3),
		}
		for i, payload := range canaryJobs {
			job := db.Job{ID: fmt.Sprintf("graduate-job-%d", i), Agent: agentName, Type: "implement"}
			if err := h.Harvest(ctx, job, payload, healthyMergeOutcome(400+i)); err != nil {
				t.Fatalf("Harvest #%d: %v", i, err)
			}
		}

		assertCanaryFeedbackCount(t, store, canaryID, 3)

		// GRADUATE: candidate.auto_promoted exactly once, canary becomes current.
		if got := len(sink.byType(events.EventCandidateAutoPromoted)); got != 1 {
			t.Fatalf("auto_promoted emits = %d, want exactly 1 (healthy canary)", got)
		}
		if got := len(sink.byType(events.EventCandidateRolledBack)); got != 0 {
			t.Fatalf("rolled_back emits = %d, want 0 (healthy canary)", got)
		}
		graduated, err := store.GetAgentTemplateVersionByID(ctx, canaryID)
		if err != nil {
			t.Fatalf("get canary after graduate: %v", err)
		}
		if graduated.State != "current" {
			t.Fatalf("canary state = %q, want current (graduated)", graduated.State)
		}
		tmpl, err := store.GetAgentTemplate(ctx, "planner")
		if err != nil {
			t.Fatalf("GetAgentTemplate after graduate: %v", err)
		}
		if tmpl.VersionID != canaryID {
			t.Fatalf("current version = %q, want graduated canary %q", tmpl.VersionID, canaryID)
		}
	})

	// wired_through_daemon_config_assembly closes the one uncovered link the other
	// subtests bypass by hand-wiring: the daemon's config->component glue. Instead of
	// setting Mailbox.CanaryEnabled directly and hand-building a
	// &canaryRegressionHarvester{...}, it writes a real [skillopt] canary config into
	// a fixture home and drives the SAME rollback through canaryRoutingEnabled(home)
	// (the routing gate, daemon.go/engine.go) and daemonOutcomeHarvesterWithCanary(
	// store, gh, home) (the harvester assembly, daemon.go). A regression in that glue
	// — forgetting to wrap (bare base harvester), wiring the wrong minSamples, or a
	// gate that returns false when the canary policy is on — would now fail here.
	t.Run("wired_through_daemon_config_assembly", func(t *testing.T) {
		ctx := context.Background()
		store, championID, canaryID, championCommit, canaryCommit, agentName := canaryE2EFixture(t)
		seedAutoTraceFeedback(t, store, "planner", championID, "a", 3)

		// A real, fully-configured canary [skillopt] policy. min_samples=3 is the
		// daemon regression floor the harvester must read off the policy; sample=0.5
		// is the routing discriminator the Mailbox must read off the SAME policy.
		home := writeHarvestConfig(t, "[skillopt]\n"+
			"auto_trace_enabled = true\n"+
			"auto_promote = true\n"+
			"auto_promote_canary = true\n"+
			"auto_promote_canary_sample = 0.5\n"+
			"auto_promote_min_samples = 3\n")

		// GATE: the routing seam turns on only because the config configures it. (A
		// gate that ignored the canary policy — daemon.go:4029/engine.go:287 — would
		// build every Mailbox with CanaryEnabled=false and the canary would never see
		// traffic.) Cross-check the OFF case so the gate is not a constant-true.
		if !canaryRoutingEnabled(home) {
			t.Fatal("canaryRoutingEnabled = false with a configured canary policy, want true")
		}
		offHome := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = true\n")
		if canaryRoutingEnabled(offHome) {
			t.Fatal("canaryRoutingEnabled = true with canary OFF, want false")
		}

		// ASSEMBLY: the harvester must be the #484 wrapper (not the bare #465 base)
		// and must carry the policy's min_samples. A forgotten wrap or wrong floor
		// (canary_daemon.go:25 / daemon.go:4000) ships silently otherwise.
		built := daemonOutcomeHarvesterWithCanary(store, github.NoopClient{}, home)
		wrapper, ok := built.(*canaryRegressionHarvester)
		if !ok {
			t.Fatalf("daemonOutcomeHarvesterWithCanary returned %T, want *canaryRegressionHarvester (must wrap the base)", built)
		}
		if wrapper.minSamples == nil || *wrapper.minSamples != 3 {
			t.Fatalf("wrapper minSamples = %v, want the configured 3", wrapper.minSamples)
		}
		// With canary OFF the assembly must return the BARE base, never the wrapper.
		if offBuilt := daemonOutcomeHarvesterWithCanary(store, github.NoopClient{}, offHome); offBuilt == nil {
			t.Fatal("daemonOutcomeHarvesterWithCanary = nil with auto_trace on, want the bare base")
		} else if _, isWrapper := offBuilt.(*canaryRegressionHarvester); isWrapper {
			t.Fatal("daemonOutcomeHarvesterWithCanary wrapped the base with canary OFF, want the bare base")
		}

		// ROUTING through the config-gated flag, then ROLLBACK through the assembled
		// harvester. The harvester's internal sink is the daemon's (nil with no
		// [events] config), so the rollback is asserted on store-observable state
		// rather than emitted events — the event assertions live in the hand-wired
		// rollback subtest above.
		mb := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("canary routing enqueue test"))
		mb.CanaryEnabled = canaryRoutingEnabled(home)
		mb.CanaryRand = scriptedDraws(t, 0.10, 0.20, 0.30, 0.80)
		canaryJobs := routeAndAssertSplit(t, mb, agentName, "wired", championCommit, canaryCommit, []routeCase{
			{pr: 701, canary: true}, {pr: 702, canary: true}, {pr: 703, canary: true}, {pr: 704, canary: false},
		})
		if len(canaryJobs) != 3 {
			t.Fatalf("canary-routed jobs = %d, want 3", len(canaryJobs))
		}
		for i, payload := range canaryJobs {
			job := db.Job{ID: fmt.Sprintf("wired-job-%d", i), Agent: agentName, Type: "implement"}
			if err := built.Harvest(ctx, job, payload, regressingChangesOutcome(800+i)); err != nil {
				t.Fatalf("Harvest #%d: %v", i, err)
			}
		}
		assertCanaryFeedbackCount(t, store, canaryID, 3)

		rejected, err := store.GetAgentTemplateVersionByID(ctx, canaryID)
		if err != nil {
			t.Fatalf("get canary after rollback: %v", err)
		}
		if rejected.State != "rejected" {
			t.Fatalf("canary state = %q, want rejected (assembled harvester must roll back)", rejected.State)
		}
		tmpl, err := store.GetAgentTemplate(ctx, "planner")
		if err != nil {
			t.Fatalf("GetAgentTemplate after rollback: %v", err)
		}
		if tmpl.VersionID != championID {
			t.Fatalf("current version = %q, want champion %q (champion must stay live)", tmpl.VersionID, championID)
		}
	})

	// rolled_back_emitted_once_under_concurrent_harvest exercises the dedup contract:
	// the daemon runs jobs in a parallel worker pool, so two same-template harvests
	// can race to roll back the same canary. RejectAgentTemplateVersion is idempotent
	// (changed=false on the second), and the decorator gates the emit on changed, so
	// rolled_back must fire EXACTLY ONCE even when two Harvest() calls race to the
	// rollback. Still no hand-seeding: the canary samples come only from Harvest.
	//
	// ROOT-CAUSE NOTE (so this stays green under -race in CI): db.Open sets
	// SetMaxOpenConns(1), so the two reject transactions are SERIALIZED at the DB —
	// the first commits state='rejected' (changed=true, one emit) and the second
	// reads the already-rejected row (changed=false, no emit). A true double-reject
	// is therefore impossible, which is why this is reliable under the detector. The
	// two-goroutine race below exercises the real evaluate->emit path under -race;
	// the deterministic re-reject assertion at the end pins the underlying
	// idempotency contract WITHOUT depending on goroutine scheduling.
	t.Run("rolled_back_emitted_once_under_concurrent_harvest", func(t *testing.T) {
		ctx := context.Background()
		store, championID, canaryID, _, canaryCommit, agentName := canaryE2EFixture(t)
		seedAutoTraceFeedback(t, store, "planner", championID, "a", 3)

		mb := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("canary routing enqueue test"))
		mb.CanaryEnabled = true
		mb.CanaryRand = scriptedDraws(t, 0.0, 0.0, 0.0, 0.0)
		var canaryJobs []workflow.JobPayload
		for i := 0; i < 4; i++ {
			_, payload := enqueueImplement(t, mb, agentName, 500+i, fmt.Sprintf("race-job-%d", i))
			if payload.TemplateResolvedCommit != canaryCommit {
				t.Fatalf("race draw #%d did not route to canary: commit %q", i, payload.TemplateResolvedCommit)
			}
			canaryJobs = append(canaryJobs, payload)
		}

		sink := &recordingSink{}
		h := &canaryRegressionHarvester{
			inner:      skillopt.NewOutcomeHarvester(store, nil),
			store:      store,
			sink:       sink,
			minSamples: floatPtrCLIInt(3),
		}
		// Two regressing canary outcomes below min_samples: hold, no emit.
		for i := 0; i < 2; i++ {
			job := db.Job{ID: fmt.Sprintf("race-job-%d", i), Agent: agentName, Type: "implement"}
			if err := h.Harvest(ctx, job, canaryJobs[i], regressingChangesOutcome(600+i)); err != nil {
				t.Fatalf("pre-race Harvest #%d: %v", i, err)
			}
		}
		if got := len(sink.byType(events.EventCandidateRolledBack)); got != 0 {
			t.Fatalf("rolled_back emits = %d before min_samples, want 0", got)
		}

		// Two RACING harvests cross the min_samples floor concurrently; both evaluate
		// and attempt rollback. Exactly one must emit.
		var wg sync.WaitGroup
		for i := 2; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				job := db.Job{ID: fmt.Sprintf("race-job-%d", i), Agent: agentName, Type: "implement"}
				_ = h.Harvest(ctx, job, canaryJobs[i], regressingChangesOutcome(600+i))
			}(i)
		}
		wg.Wait()

		if got := len(sink.byType(events.EventCandidateRolledBack)); got != 1 {
			t.Fatalf("rolled_back emits = %d under concurrent harvest, want exactly 1 (dedup)", got)
		}
		rejected, err := store.GetAgentTemplateVersionByID(ctx, canaryID)
		if err != nil {
			t.Fatalf("get canary after race: %v", err)
		}
		if rejected.State != "rejected" {
			t.Fatalf("canary state = %q, want rejected", rejected.State)
		}

		// DETERMINISTIC dedup pin (scheduling-independent): the exactly-once contract
		// rests entirely on RejectAgentTemplateVersion being idempotent — a SECOND
		// reject of the now-rejected canary must report changed=false, which is what
		// the decorator's `if !changed { return }` gate keys off to suppress the
		// duplicate emit. Assert that primitive directly so a regression that made
		// re-reject report changed=true (re-firing rolled_back) is caught even if the
		// goroutine race above happens not to overlap on a given run.
		reRejected, changed, err := store.RejectAgentTemplateVersion(ctx, canaryID, "dedup probe")
		if err != nil {
			t.Fatalf("re-reject canary: %v", err)
		}
		if changed {
			t.Fatal("re-reject of an already-rejected canary reported changed=true, want false (idempotent dedup)")
		}
		if reRejected.State != "rejected" {
			t.Fatalf("re-reject state = %q, want rejected", reRejected.State)
		}
	})
}

// routeCase is one scripted resolution: the PR number and whether the scripted draw
// should route it to the canary.
type routeCase struct {
	pr     int
	canary bool
}

// routeAndAssertSplit enqueues one implement job per case through the real Mailbox,
// asserts each resolved to the canary or champion commit as scripted, and returns
// the canary-routed payloads for the attribution leg.
func routeAndAssertSplit(t *testing.T, mb workflow.Mailbox, agentName, prefix, championCommit, canaryCommit string, cases []routeCase) []workflow.JobPayload {
	t.Helper()
	var canaryJobs []workflow.JobPayload
	for i, c := range cases {
		_, payload := enqueueImplement(t, mb, agentName, c.pr, fmt.Sprintf("%s-job-%d", prefix, i))
		if c.canary {
			if payload.TemplateResolvedCommit != canaryCommit {
				t.Fatalf("case #%d (below sample) resolved commit %q, want CANARY %q", i, payload.TemplateResolvedCommit, canaryCommit)
			}
			canaryJobs = append(canaryJobs, payload)
		} else if payload.TemplateResolvedCommit != championCommit {
			t.Fatalf("case #%d (>= sample) resolved commit %q, want CHAMPION %q", i, payload.TemplateResolvedCommit, championCommit)
		}
	}
	return canaryJobs
}

// assertCanaryFeedbackCount asserts the canary version's auto-trace run holds
// exactly want feedback events — i.e. Harvest's attribution populated the canary's
// OWN run (not the champion's).
func assertCanaryFeedbackCount(t *testing.T, store *db.Store, canaryID string, want int) {
	t.Helper()
	feedback, err := store.ListFeedbackEvents(context.Background(), skillopt.AutoTraceRunID(canaryID))
	if err != nil {
		t.Fatalf("ListFeedbackEvents(canary): %v", err)
	}
	if len(feedback) != want {
		t.Fatalf("canary auto-trace feedback = %d, want %d (Harvest must attribute the routed outcomes)", len(feedback), want)
	}
}
