package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// liveABChampionAdapter is the champion-side DeliveryAdapter (the one
// Mailbox.Run drives). It records its Deliver calls and returns a valid
// gitmoot_result whose summary is the champion answer.
type liveABChampionAdapter struct {
	mu      sync.Mutex
	calls   int
	summary string
	// onDeliver, when set, records the champion call into a shared order slice so a
	// test can prove the champion runs strictly before the challenger.
	onDeliver func()
}

func (a *liveABChampionAdapter) Deliver(_ context.Context, _ runtime.Agent, _ runtime.Job) (runtime.Result, error) {
	a.mu.Lock()
	a.calls++
	cb := a.onDeliver
	a.mu.Unlock()
	if cb != nil {
		cb()
	}
	out := `{"gitmoot_result":{"decision":"approved","summary":"` + a.summary + `","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	return runtime.Result{Summary: out, Raw: out}, nil
}

func (a *liveABChampionAdapter) deliverCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// liveABFixture installs a "planner" template (its current version is the
// champion), a single pending challenger version, registers a managed agent bound
// to the template, seeds the champion bandit arm to a chosen pull count, and
// enqueues a foreground ask job. It returns the home, store, the request, the
// db.Agent, the enqueued job, and the challenger version id.
func liveABFixture(t *testing.T, championPulls int) (string, *db.Store, localAgentDispatchRequest, db.Agent, db.Job, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	if err := store.UpsertAgentTemplate(ctx, cliSkillOptTemplate("planner", "Champion guidance.")); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	champ, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	challengerTmpl := cliSkillOptTemplate("planner", "Challenger guidance, stronger and more actionable.")
	challengerVersion, err := store.AddPendingAgentTemplateVersion(ctx, challengerTmpl)
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}

	agent := db.Agent{
		Name:           "planner-bot",
		Role:           "ask",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     "last",
		TemplateID:     "planner",
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}
	if err := store.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	// Seed the champion arm above/below the floor by recording championPulls
	// pairwise outcomes against it.
	for i := 0; i < championPulls; i++ {
		if _, err := store.IncrementBanditArm(ctx, "planner", champ.VersionID, i%2 == 0); err != nil {
			t.Fatalf("IncrementBanditArm: %v", err)
		}
	}

	request := localAgentDispatchRequest{
		Agent:        "planner-bot",
		Action:       "ask",
		Instructions: "Plan the migration.",
		Home:         home,
	}
	job, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
		ID:           "ask-planner-bot",
		Agent:        "planner-bot",
		Action:       "ask",
		Repo:         "jerryfane/gitmoot",
		Instructions: "Plan the migration.",
		Sender:       "local",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return home, store, request, agent, job, challengerVersion.ID
}

// withLiveABPolicy overrides the policy loader seam for the test's duration.
func withLiveABPolicy(t *testing.T, rate float64, floor int) {
	t.Helper()
	prev := liveABPolicyLoader
	liveABPolicyLoader = func(string) config.SkillOptABPolicy {
		return config.SkillOptABPolicy{LiveABSampleRate: rate, BanditMinSamples: floor}
	}
	t.Cleanup(func() { liveABPolicyLoader = prev })
}

// withLiveABSampler pins the sampling draw (0 = always hit, 1 = always miss).
func withLiveABSampler(t *testing.T, value float64) {
	t.Helper()
	prev := liveABSampler
	liveABSampler = func() float64 { return value }
	t.Cleanup(func() { liveABSampler = prev })
}

// withLiveABInteractive pins the interactive-TTY gate. The test binary's stdio is
// not a TTY, so the interactive interception path (#482) must be forced on for the
// "intercepts" cases and can be forced off to assert the non-interactive no-op.
func withLiveABInteractive(t *testing.T, interactive bool) {
	t.Helper()
	prev := liveABInteractive
	liveABInteractive = func() bool { return interactive }
	t.Cleanup(func() { liveABInteractive = prev })
}

// withLiveABChallengerDeliver pins the challenger Deliver seam (shared with the
// CLI A/B) and records its calls. It records, in order, every champion+challenger
// invocation so the test can prove serialization.
func withLiveABChallengerDeliver(t *testing.T, calls *[]string, challengerAnswer string, challengerErr error) {
	t.Helper()
	prev := skillOptABDeliver
	skillOptABDeliver = func(_ context.Context, _ runtime.Agent, prompt string) (string, error) {
		*calls = append(*calls, "challenger")
		if challengerErr != nil {
			return "", challengerErr
		}
		return challengerAnswer, nil
	}
	t.Cleanup(func() { skillOptABDeliver = prev })
}

// withLiveABPresenter pins the presenter to deterministically pick a role.
func withLiveABPresenter(t *testing.T, winner, loser string, ok bool) {
	t.Helper()
	prev := liveABPresenter
	liveABPresenter = func(_, _, _ string) (string, string, bool) {
		return winner, loser, ok
	}
	t.Cleanup(func() { liveABPresenter = prev })
}

// TestMaybeRunLiveABOffByDefaultIsNoop is the byte-identical proof: with rate 0
// (the default, no [skillopt] section) maybeRunLiveAB returns handled=false and
// performs NO Deliver, writes NO RankedFeedbackEvent, and updates NO bandit arm,
// so the caller's single Mailbox.Run runs unchanged.
func TestMaybeRunLiveABOffByDefaultIsNoop(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 100)
	withLiveABPolicy(t, 0.0, 1) // rate 0 = off, even above the floor
	withLiveABSampler(t, 0.0)   // would hit if rate>0
	var challengerCalls []string
	withLiveABChallengerDeliver(t, &challengerCalls, "Challenger answer.", nil)
	champ := &liveABChampionAdapter{summary: "Champion answer."}

	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if handled {
		t.Fatal("rate 0 must return handled=false (no-op)")
	}
	if champ.deliverCount() != 0 {
		t.Fatalf("champion Deliver calls = %d, want 0 (interceptor must not run anything when off)", champ.deliverCount())
	}
	if len(challengerCalls) != 0 {
		t.Fatalf("challenger Deliver calls = %d, want 0", len(challengerCalls))
	}
	assertNoLiveABRecord(t, store, challengerID)
}

// TestMaybeRunLiveABUnmanagedIsNoop: an unmanaged agent (managed=false) is never
// intercepted regardless of rate.
func TestMaybeRunLiveABUnmanagedIsNoop(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 100)
	withLiveABPolicy(t, 1.0, 1)
	withLiveABSampler(t, 0.0)
	champ := &liveABChampionAdapter{summary: "Champion answer."}

	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, false /* unmanaged */)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if handled {
		t.Fatal("unmanaged agent must return handled=false")
	}
	if champ.deliverCount() != 0 {
		t.Fatalf("champion Deliver calls = %d, want 0", champ.deliverCount())
	}
	assertNoLiveABRecord(t, store, challengerID)
}

// TestMaybeRunLiveABBelowFloorIsNoop: a managed agent below bandit_min_samples is
// NEVER auto-A/B'd even at rate 1.0 — the low-traffic guarantee.
func TestMaybeRunLiveABBelowFloorIsNoop(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 5) // 5 pulls
	withLiveABPolicy(t, 1.0, 30)                                       // floor 30
	withLiveABSampler(t, 0.0)
	withLiveABInteractive(t, true) // isolate: prove the FLOOR gate, not the tty gate
	champ := &liveABChampionAdapter{summary: "Champion answer."}

	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if handled {
		t.Fatal("below the floor must return handled=false")
	}
	if champ.deliverCount() != 0 {
		t.Fatalf("champion Deliver calls = %d, want 0", champ.deliverCount())
	}
	assertNoLiveABRecord(t, store, challengerID)
}

// TestMaybeRunLiveABSamplingMissIsNoop: above the floor and managed, a sampling
// MISS (draw >= rate) returns handled=false and runs nothing.
func TestMaybeRunLiveABSamplingMissIsNoop(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 100)
	withLiveABPolicy(t, 0.25, 1)
	withLiveABSampler(t, 1.0)      // miss
	withLiveABInteractive(t, true) // isolate: prove the SAMPLING gate, not the tty gate
	champ := &liveABChampionAdapter{summary: "Champion answer."}

	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if handled {
		t.Fatal("a sampling miss must return handled=false")
	}
	if champ.deliverCount() != 0 {
		t.Fatalf("champion Deliver calls = %d, want 0", champ.deliverCount())
	}
	assertNoLiveABRecord(t, store, challengerID)
}

// TestMaybeRunLiveABInterceptsAndRecords: sampled + above the floor + managed runs
// the champion via Mailbox.Run AND a serialized challenger Deliver, then records
// exactly one RankedFeedbackEvent (source=skillopt-ab) and updates BOTH bandit
// arms — the same path as a manual `skillopt ab` pick.
func TestMaybeRunLiveABInterceptsAndRecords(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 50)
	withLiveABPolicy(t, 1.0, 30)
	withLiveABSampler(t, 0.0) // hit
	withLiveABInteractive(t, true)
	var calls []string
	withLiveABChallengerDeliver(t, &calls, "Challenger answer.", nil)
	withLiveABPresenter(t, skillOptABChallengerLabel, skillOptABChampionLabel, true)
	ctx := context.Background()

	champBefore, _, err := store.GetBanditArm(ctx, "planner", championVersionID(t, store))
	if err != nil {
		t.Fatalf("GetBanditArm champion before: %v", err)
	}

	champ := &liveABChampionAdapter{summary: "Champion answer."}
	handled, err := maybeRunLiveAB(ctx, store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if !handled {
		t.Fatal("sampled+above-floor must return handled=true")
	}
	// Champion ran exactly once via Mailbox.Run.
	if champ.deliverCount() != 1 {
		t.Fatalf("champion Deliver calls = %d, want exactly 1", champ.deliverCount())
	}
	// Challenger ran exactly once.
	if len(calls) != 1 || calls[0] != "challenger" {
		t.Fatalf("challenger calls = %v, want exactly one challenger Deliver", calls)
	}

	// Exactly one RankedFeedbackEvent, source=skillopt-ab, challenger won.
	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ranked feedback events = %d, want exactly 1", len(events))
	}
	if events[0].Source != skillOptABSource {
		t.Fatalf("event source = %q, want %q", events[0].Source, skillOptABSource)
	}
	if events[0].Winner != skillOptABChallengerLabel {
		t.Fatalf("winner = %q, want challenger", events[0].Winner)
	}

	// Both arms updated: champion lost (+1 pull, +1 beta), challenger won.
	champAfter, _, err := store.GetBanditArm(ctx, "planner", championVersionID(t, store))
	if err != nil {
		t.Fatalf("GetBanditArm champion after: %v", err)
	}
	if champAfter.Pulls != champBefore.Pulls+1 {
		t.Fatalf("champion pulls = %d, want %d (one loss recorded)", champAfter.Pulls, champBefore.Pulls+1)
	}
	challengerArm, ok, err := store.GetBanditArm(ctx, "planner", challengerID)
	if err != nil || !ok {
		t.Fatalf("GetBanditArm challenger: ok=%v err=%v", ok, err)
	}
	if challengerArm.Pulls != 1 {
		t.Fatalf("challenger pulls = %d, want 1 (one win recorded)", challengerArm.Pulls)
	}
}

// TestMaybeRunLiveABSerializesChampionBeforeChallenger proves the two Deliver
// calls run strictly in series (champion first, challenger second) — they share
// the single runtime-session lock the caller already holds, so the interceptor
// never acquires a second lock and the order is deterministic. The interceptor
// itself must never reference acquireRuntimeSessionLock.
func TestMaybeRunLiveABSerializesChampionBeforeChallenger(t *testing.T) {
	_, store, request, agent, job, _ := liveABFixture(t, 50)
	withLiveABPolicy(t, 1.0, 30)
	withLiveABSampler(t, 0.0)
	withLiveABInteractive(t, true)
	var order []string
	// Challenger seam appends "challenger".
	withLiveABChallengerDeliver(t, &order, "Challenger answer.", nil)
	withLiveABPresenter(t, skillOptABChampionLabel, skillOptABChallengerLabel, true)

	champ := &liveABChampionAdapter{summary: "Champion answer."}
	champ.onDeliver = func() { order = append(order, "champion") }

	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if len(order) != 2 || order[0] != "champion" || order[1] != "challenger" {
		t.Fatalf("Deliver order = %v, want [champion challenger] (serialized, champion first)", order)
	}
}

// TestMaybeRunLiveABNoSecondLockAcquisition is the lock-reuse guard: the
// interceptor source must not call acquireRuntimeSessionLock — it runs strictly
// under the lock dispatchLocalAgentJob already holds, so a second acquisition
// would self-deadlock with `session is busy`.
func TestMaybeRunLiveABNoSecondLockAcquisition(t *testing.T) {
	src, err := os.ReadFile("agent_dispatch_live_ab.go")
	if err != nil {
		t.Fatalf("read interceptor source: %v", err)
	}
	if strings.Contains(string(src), "acquireRuntimeSessionLock") {
		t.Fatal("the live-AB interceptor must not acquire a second runtime-session lock")
	}
}

// TestMaybeRunLiveABChallengerErrorFailSafe: when the challenger Deliver errors,
// the champion answer still ran (handled=true, nil error), NO RankedFeedbackEvent
// is written, NO bandit arm is updated, and a live_ab_skipped job event is logged.
func TestMaybeRunLiveABChallengerErrorFailSafe(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 50)
	withLiveABPolicy(t, 1.0, 30)
	withLiveABSampler(t, 0.0)
	withLiveABInteractive(t, true)
	var calls []string
	withLiveABChallengerDeliver(t, &calls, "", errors.New("challenger runtime exploded"))
	withLiveABPresenter(t, skillOptABChallengerLabel, skillOptABChampionLabel, true)
	ctx := context.Background()

	champBefore, _, err := store.GetBanditArm(ctx, "planner", championVersionID(t, store))
	if err != nil {
		t.Fatalf("GetBanditArm before: %v", err)
	}

	champ := &liveABChampionAdapter{summary: "Champion answer."}
	handled, err := maybeRunLiveAB(ctx, store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB must be fail-safe (nil error), got %v", err)
	}
	if !handled {
		t.Fatal("an intercepted ask with a challenger error is still handled=true (champion answered)")
	}
	// The champion answer was delivered to the user.
	if champ.deliverCount() != 1 {
		t.Fatalf("champion Deliver calls = %d, want 1 (the user always gets the champion answer)", champ.deliverCount())
	}
	// No A/B record, no bandit update.
	assertNoLiveABRecord(t, store, challengerID)
	champAfter, _, err := store.GetBanditArm(ctx, "planner", championVersionID(t, store))
	if err != nil {
		t.Fatalf("GetBanditArm after: %v", err)
	}
	if champAfter.Pulls != champBefore.Pulls {
		t.Fatalf("champion pulls = %d, want unchanged %d (no update on fail-safe)", champAfter.Pulls, champBefore.Pulls)
	}
	// A live_ab_skipped job event was recorded.
	assertLiveABSkippedEvent(t, store, job.ID)
}

// TestMaybeRunLiveABNoPickFailSafe: when the presenter captures no pick (non-
// interactive), the champion answer still stands, no record is written, and a
// live_ab_skipped event is logged.
func TestMaybeRunLiveABNoPickFailSafe(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 50)
	withLiveABPolicy(t, 1.0, 30)
	withLiveABSampler(t, 0.0)
	withLiveABInteractive(t, true)
	var calls []string
	withLiveABChallengerDeliver(t, &calls, "Challenger answer.", nil)
	withLiveABPresenter(t, "", "", false) // no pick captured
	ctx := context.Background()

	champ := &liveABChampionAdapter{summary: "Champion answer."}
	handled, err := maybeRunLiveAB(ctx, store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if !handled {
		t.Fatal("handled must be true (champion ran)")
	}
	if champ.deliverCount() != 1 {
		t.Fatalf("champion Deliver calls = %d, want 1", champ.deliverCount())
	}
	assertNoLiveABRecord(t, store, challengerID)
	assertLiveABSkippedEvent(t, store, job.ID)
}

// championVersionID reads the planner template's current promoted version id.
func championVersionID(t *testing.T, store *db.Store) string {
	t.Helper()
	tmpl, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	return tmpl.VersionID
}

func assertNoLiveABRecord(t *testing.T, store *db.Store, challengerID string) {
	t.Helper()
	runID := skillOptABRunIDPrefix + challengerID
	events, err := store.ListRankedFeedbackEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ranked feedback events = %d, want 0 (no A/B record)", len(events))
	}
}

func assertLiveABSkippedEvent(t *testing.T, store *db.Store, jobID string) {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == liveABEventKind {
			return
		}
	}
	t.Fatalf("expected a %q job event, got %d events", liveABEventKind, len(events))
}

// TestDefaultLiveABPresenterMapsPickThroughShuffle proves the default presenter
// maps a pick back to the correct role under both shuffle orientations, so a
// captured preference is never silently inverted.
func TestDefaultLiveABPresenterMapsPickThroughShuffle(t *testing.T) {
	prevShuffle := liveABShuffle
	prevLine := readSkillOptABLine
	t.Cleanup(func() {
		liveABShuffle = prevShuffle
		readSkillOptABLine = prevLine
	})

	// No swap: Option A = champion. Pick "a" -> champion wins.
	liveABShuffle = func() bool { return false }
	readSkillOptABLine = func() (string, bool) { return "a", true }
	winner, loser, ok := defaultLiveABPresenter("q", "champ", "chal")
	if !ok || winner != skillOptABChampionLabel || loser != skillOptABChallengerLabel {
		t.Fatalf("no-swap pick a: winner=%q loser=%q ok=%v, want champion wins", winner, loser, ok)
	}

	// Swap: Option A = challenger. Pick "a" -> challenger wins.
	liveABShuffle = func() bool { return true }
	readSkillOptABLine = func() (string, bool) { return "a", true }
	winner, loser, ok = defaultLiveABPresenter("q", "champ", "chal")
	if !ok || winner != skillOptABChallengerLabel || loser != skillOptABChampionLabel {
		t.Fatalf("swap pick a: winner=%q loser=%q ok=%v, want challenger wins", winner, loser, ok)
	}

	// No pick line -> ok=false (fail-safe skip).
	readSkillOptABLine = func() (string, bool) { return "", false }
	if _, _, ok := defaultLiveABPresenter("q", "champ", "chal"); ok {
		t.Fatal("missing pick line must return ok=false")
	}
}

// TestMaybeRunLiveABChallengerUsesForkedSession is the session-isolation guard
// (#482, goal Risk #4): the challenger Deliver must run on a FORKED/throwaway
// session, never the agent's live RuntimeRef. The fixture pins the agent to the
// live ref "last"; this asserts the runtime.Agent handed to the challenger deliver
// seam carries an EMPTY ref, so realSkillOptABDeliver mints a fresh session via
// adapter.Start instead of resuming (and poisoning) the user's real conversation.
func TestMaybeRunLiveABChallengerUsesForkedSession(t *testing.T) {
	_, store, request, agent, job, _ := liveABFixture(t, 50)
	if agent.RuntimeRef != "last" {
		t.Fatalf("fixture precondition: agent.RuntimeRef = %q, want the live ref %q", agent.RuntimeRef, "last")
	}
	withLiveABPolicy(t, 1.0, 30)
	withLiveABSampler(t, 0.0)
	withLiveABInteractive(t, true)

	// Capture the runtime.Agent the challenger deliver seam actually receives.
	prevDeliver := skillOptABDeliver
	var challengerRef string
	var sawChallenger bool
	skillOptABDeliver = func(_ context.Context, a runtime.Agent, _ string) (string, error) {
		sawChallenger = true
		challengerRef = a.RuntimeRef
		return "Challenger answer.", nil
	}
	t.Cleanup(func() { skillOptABDeliver = prevDeliver })
	withLiveABPresenter(t, skillOptABChampionLabel, skillOptABChallengerLabel, true)

	champ := &liveABChampionAdapter{summary: "Champion answer."}
	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true (sampled + above floor + interactive)")
	}
	if !sawChallenger {
		t.Fatal("challenger deliver was never invoked")
	}
	if challengerRef != "" {
		t.Fatalf("challenger RuntimeRef = %q, want \"\" (forked/throwaway session, NEVER the agent's live ref %q)", challengerRef, agent.RuntimeRef)
	}
}

// TestMaybeRunLiveABJSONOutputIsNoop is the byte-clean-JSON guard (#482): an
// intercepted `agent ask --json` (JSONOutput=true) is a pure no-op — handled=false
// with NO champion-via-intercept Deliver, NO challenger Deliver, and NO A/B record
// — so runAgentAsk's writeJSON emits ONLY the JSON object, never prefixed by the
// "[live A/B] ..." presentation block that would break a scripted consumer.
func TestMaybeRunLiveABJSONOutputIsNoop(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 100)
	request.JSONOutput = true // `agent ask --json`
	withLiveABPolicy(t, 1.0, 1)
	withLiveABSampler(t, 0.0)       // would hit
	withLiveABInteractive(t, true)  // even on a TTY, --json must stay byte-clean
	var challengerCalls []string
	withLiveABChallengerDeliver(t, &challengerCalls, "Challenger answer.", nil)
	champ := &liveABChampionAdapter{summary: "Champion answer."}

	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if handled {
		t.Fatal("--json ask must return handled=false (no interception, byte-clean JSON)")
	}
	if champ.deliverCount() != 0 {
		t.Fatalf("champion Deliver calls = %d, want 0 (caller runs the single Mailbox.Run)", champ.deliverCount())
	}
	if len(challengerCalls) != 0 {
		t.Fatalf("challenger Deliver calls = %d, want 0 on --json", len(challengerCalls))
	}
	assertNoLiveABRecord(t, store, challengerID)
}

// TestMaybeRunLiveABNonInteractiveIsNoop is the no-blocking-pick guard (#482): a
// non-interactive (piped / scripted / redirected) ask is a pure no-op — it never
// runs the second Deliver and never reaches the blocking fmt.Scanln pick, so a
// fully-unattended ask returns the champion answer immediately and is never hung
// waiting for an A/B pick it cannot supply.
func TestMaybeRunLiveABNonInteractiveIsNoop(t *testing.T) {
	_, store, request, agent, job, challengerID := liveABFixture(t, 100)
	withLiveABPolicy(t, 1.0, 1)
	withLiveABSampler(t, 0.0)        // would hit
	withLiveABInteractive(t, false)  // piped / non-tty session
	var challengerCalls []string
	withLiveABChallengerDeliver(t, &challengerCalls, "Challenger answer.", nil)
	champ := &liveABChampionAdapter{summary: "Champion answer."}

	handled, err := maybeRunLiveAB(context.Background(), store, request, agent, job, champ, true)
	if err != nil {
		t.Fatalf("maybeRunLiveAB: %v", err)
	}
	if handled {
		t.Fatal("a non-interactive ask must return handled=false (no blocking pick)")
	}
	if champ.deliverCount() != 0 {
		t.Fatalf("champion Deliver calls = %d, want 0", champ.deliverCount())
	}
	if len(challengerCalls) != 0 {
		t.Fatalf("challenger Deliver calls = %d, want 0 when non-interactive", len(challengerCalls))
	}
	assertNoLiveABRecord(t, store, challengerID)
}

// TestRealSkillOptABDeliverForkedSessionUsesStart proves the forked-session
// contract at the deliver layer: an EMPTY agent.RuntimeRef routes through
// adapter.Start (a fresh throwaway conversation), NOT adapter.Deliver (which would
// resume the agent's live session). This is the mechanism that protects the user's
// live session for the live-AB challenger.
func TestRealSkillOptABDeliverForkedSessionUsesStart(t *testing.T) {
	prevFactory := newRuntimeFactory
	prevAdapterFor := runtimeStartAdapterFor
	t.Cleanup(func() {
		newRuntimeFactory = prevFactory
		runtimeStartAdapterFor = prevAdapterFor
	})
	fake := &forkSessionFakeAdapter{startAnswer: "fresh-session answer"}
	runtimeStartAdapterFor = func(runtime.Factory, string, string) (runtime.Adapter, error) {
		return fake, nil
	}

	answer, err := realSkillOptABDeliver(context.Background(), runtime.Agent{
		Name:       "planner-bot",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: "", // forked/throwaway session signal
	}, "challenger prompt")
	if err != nil {
		t.Fatalf("realSkillOptABDeliver: %v", err)
	}
	if fake.startCalls != 1 {
		t.Fatalf("adapter.Start calls = %d, want exactly 1 (forked session)", fake.startCalls)
	}
	if fake.deliverCalls != 0 {
		t.Fatalf("adapter.Deliver calls = %d, want 0 (an empty ref must NEVER resume the live session)", fake.deliverCalls)
	}
	if answer != "fresh-session answer" {
		t.Fatalf("answer = %q, want the Start raw output", answer)
	}

	// Control: a NON-empty ref keeps the legacy resume (Deliver) path.
	fake.startCalls, fake.deliverCalls = 0, 0
	fake.deliverAnswer = "resumed answer"
	answer, err = realSkillOptABDeliver(context.Background(), runtime.Agent{
		Name:       "planner-bot",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: "last",
	}, "champion prompt")
	if err != nil {
		t.Fatalf("realSkillOptABDeliver (resume): %v", err)
	}
	if fake.deliverCalls != 1 || fake.startCalls != 0 {
		t.Fatalf("non-empty ref: Start=%d Deliver=%d, want Start=0 Deliver=1 (resume preserved)", fake.startCalls, fake.deliverCalls)
	}
	if answer != "resumed answer" {
		t.Fatalf("answer = %q, want the Deliver summary", answer)
	}
}

// forkSessionFakeAdapter records whether Start (fresh session) or Deliver (resume)
// was used, so the forked-session contract can be asserted without a live runtime.
type forkSessionFakeAdapter struct {
	startCalls    int
	deliverCalls  int
	startAnswer   string
	deliverAnswer string
}

func (a *forkSessionFakeAdapter) Name() string { return runtime.CodexRuntime }
func (a *forkSessionFakeAdapter) Start(_ context.Context, _ runtime.StartRequest) (runtime.StartResult, error) {
	a.startCalls++
	return runtime.StartResult{RuntimeRef: "fresh-throwaway", Raw: a.startAnswer}, nil
}
func (a *forkSessionFakeAdapter) Validate(_ context.Context, _ runtime.Agent) error { return nil }
func (a *forkSessionFakeAdapter) Deliver(_ context.Context, _ runtime.Agent, _ runtime.Job) (runtime.Result, error) {
	a.deliverCalls++
	return runtime.Result{Summary: a.deliverAnswer, Raw: a.deliverAnswer}, nil
}
func (a *forkSessionFakeAdapter) Health(_ context.Context, _ runtime.Agent) error { return nil }
func (a *forkSessionFakeAdapter) Capabilities(_ context.Context) ([]string, error) {
	return []string{"ask"}, nil
}
