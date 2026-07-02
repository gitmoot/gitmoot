package cli

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// liveABSampler returns the sampling draw used by maybeRunLiveAB to decide
// whether a single foreground ask is intercepted. It is a var seam (defaults to
// math/rand.Float64, in [0,1)) so tests can force a hit (return 0) or a miss
// (return 1) deterministically. It is consulted ONLY after every cheap gate
// (rate>0, managed, action==ask, above floor) already passed, so an off-path ask
// never even draws — keeping the hot path byte-identical.
var liveABSampler = func() float64 { return rand.Float64() }

// liveABInteractive reports whether the current ask is a genuine interactive TTY
// session (both stdin AND stdout are character devices). It is a var seam so
// tests can force interactive/non-interactive. The interceptor is gated on it so
// a piped / scripted / redirected ask is NEVER intercepted: the A/B block can
// only be presented (and the pick read) on a real terminal, never blocking an
// unattended ask on fmt.Scanln nor polluting a redirected stdout. Combined with
// the request.JSONOutput gate this keeps both `--json` and any non-tty ask
// byte-identical to a plain single Mailbox.Run.
var liveABInteractive = func() bool {
	in, errIn := os.Stdin.Stat()
	out, errOut := os.Stdout.Stat()
	return errIn == nil && errOut == nil &&
		in.Mode()&os.ModeCharDevice != 0 && out.Mode()&os.ModeCharDevice != 0
}

// liveABPolicyLoader is a var seam over LoadSkillOptABPolicy so tests can supply
// a policy without writing a config file. It returns the off-by-default policy
// (rate 0) on any error, which keeps the interceptor fail-safe: a malformed or
// unreadable config can never turn interception ON, only leave it OFF.
var liveABPolicyLoader = func(home string) config.SkillOptABPolicy {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return config.DefaultSkillOptABPolicy()
	}
	policy, err := config.LoadSkillOptABPolicy(paths)
	if err != nil {
		return config.DefaultSkillOptABPolicy()
	}
	return policy
}

// liveABEventKind is the job event recorded whenever a sampled interception
// degrades to the champion-only answer (no challenger, no pick, or a challenger
// Deliver error) — the fail-safe marker the test asserts.
const liveABEventKind = "live_ab_skipped"

// maybeRunLiveAB optionally forks the single foreground `agent ask` Mailbox.Run
// into a champion-vs-challenger A/B (#482) and routes the human pick through
// #473's recordSkillOptABPick + bandit update. It returns handled=false (a pure
// no-op, NO side effects, NO Deliver) whenever the feature is off or ungated, so
// the caller falls through to the existing single Mailbox.Run unchanged —
// byte-identical behavior when live_ab_sample_rate is 0 (the default).
//
// When it DOES intercept it returns handled=true and is responsible for
// delivering the champion answer to the user (via the same Mailbox.Run path, so
// the job/result the user sees is identical to a normal ask) plus a SECOND,
// strictly-serialized challenger Deliver under the SAME runtime-session lock the
// caller already holds — never acquiring a second lock. It is FAIL-SAFE: once the
// champion has answered, any subsequent error (challenger Deliver failure, no
// pick available, record failure) is swallowed, logged as a live_ab_skipped job
// event, and reported as handled=true with a nil error so the user always gets
// the champion answer and the primary ask is never degraded.
func maybeRunLiveAB(ctx context.Context, store *db.Store, request localAgentDispatchRequest, agent db.Agent, job db.Job, adapter workflow.DeliveryAdapter, managed bool) (bool, error) {
	// --- Cheap gates first (no I/O beyond the policy load); any miss is a no-op. ---
	if strings.TrimSpace(request.Action) != "ask" {
		return false, nil
	}
	if request.Background {
		return false, nil
	}
	if !managed {
		// Shell / unmanaged / bespoke traffic stays byte-identical.
		return false, nil
	}
	// Interactive-only gate (#482): the A/B presentation prints to stdout and reads
	// a pick from stdin, so it can only fire on a genuine TTY. A `--json` ask or any
	// piped / redirected / scripted (e.g. fully-unattended) ask is a pure no-op here
	// — it never presents the "[live A/B] ..." block (which would corrupt the JSON
	// stream) and never runs the second challenger Deliver, falling through to the
	// single Mailbox.Run with byte-identical output. This is a cheap gate placed
	// before any I/O so a non-interactive ask never even loads the policy.
	if request.JSONOutput || !liveABInteractive() {
		return false, nil
	}
	policy := liveABPolicyLoader(request.Home)
	if policy.LiveABSampleRate <= 0 {
		return false, nil
	}
	templateID, _ := db.SplitAgentTemplateReference(strings.TrimSpace(agent.TemplateID))
	if templateID == "" {
		return false, nil
	}

	// --- Resolve champion + challenger versions; no challenger ⇒ nothing to A/B. ---
	champion, challenger, ok, err := resolveLiveABVariants(ctx, store, templateID)
	if err != nil {
		// A resolution error must never break the ask: treat as not-intercepted.
		return false, nil
	}
	if !ok {
		return false, nil
	}

	// --- Traffic floor: the champion arm's bandit pull count must clear the floor.
	// Reuses #473's bandit pull count as the honest traffic signal so low-traffic /
	// bespoke agents (e.g. researcher) never auto-A/B, exactly as the issue demands.
	arm, _, err := store.GetBanditArm(ctx, templateID, champion.version.ID)
	if err != nil {
		return false, nil
	}
	if arm.Pulls < policy.BanditMinSamples {
		return false, nil
	}

	// --- Sampling die: only a ~rate fraction is intercepted. ---
	if liveABSampler() >= policy.LiveABSampleRate {
		return false, nil
	}

	// === Past this point we ARE intercepting (handled=true regardless of outcome). ===
	// 1) Champion: the canonical answer the user receives, via the normal
	//    Mailbox.Run path so the job/result are identical to a plain ask.
	championResult, runErr := (workflow.Mailbox{Store: store}).Run(ctx, job.ID, runtimeAgent(agent), adapter)
	if runErr != nil {
		// The primary ask itself failed — surface it exactly as the non-intercepted
		// path would (the caller propagates the same error).
		return true, runErr
	}
	championAnswer := strings.TrimSpace(championResult.Summary)

	// 2) Challenger: a SECOND Deliver, strictly AFTER the champion returned, under
	//    the one already-held runtime-session lock. From here everything is
	//    fail-safe: the champion already answered, so any error degrades to a
	//    champion-only ask + a live_ab_skipped event.
	if err := runLiveABChallenger(ctx, store, request, agent, job, templateID, champion, challenger, championAnswer); err != nil {
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: liveABEventKind, Message: err.Error()})
	}
	return true, nil
}

// runLiveABChallenger delivers the challenger variant, presents both answers,
// captures the human pick, and records it through the #473 path. Every failure
// returns an error the fail-safe caller logs as live_ab_skipped; the champion
// answer is already delivered, so no error here ever degrades the user's ask.
func runLiveABChallenger(ctx context.Context, store *db.Store, request localAgentDispatchRequest, agent db.Agent, job db.Job, templateID string, champion, challenger skillOptABVariant, championAnswer string) error {
	paths, err := pathsFromFlag(request.Home)
	if err != nil {
		return fmt.Errorf("live_ab resolve paths: %w", err)
	}

	// CRITICAL (#482, goal Risk #4): the challenger Deliver MUST run on a
	// forked/throwaway session, NEVER the agent's live RuntimeRef. Reusing
	// agent.RuntimeRef would resume the user's real session a SECOND time (codex
	// always `exec resume`s in Deliver; claude/kimi resume a pinned ref), injecting
	// the challenger's "Template instructions:\n..." turn into the user's ongoing
	// conversation and poisoning the resume_hint — so the NEXT genuine ask resumes a
	// contaminated thread. An EMPTY RuntimeRef is the forked-session signal:
	// realSkillOptABDeliver mints a fresh throwaway session via adapter.Start, so the
	// agent's live session is never touched.
	abAgent := runtime.Agent{
		Name:           agent.Name,
		Role:           firstNonEmpty(agent.Role, "ask"),
		Runtime:        agent.Runtime,
		RuntimeRef:     "", // forked/throwaway session — never the agent's live ref
		RepoScope:      agent.RepoScope,
		TemplateID:     agent.TemplateID,
		Capabilities:   agent.Capabilities,
		AutonomyPolicy: firstNonEmpty(agent.AutonomyPolicy, runtime.AutonomyPolicyReadOnly),
		Model:          agent.Model,
	}
	prompt := strings.TrimSpace(request.Instructions)

	// The challenger Deliver runs strictly after the champion's Mailbox.Run
	// returned (serialized under the single held lock) via the shared #473 seam.
	challengerDelivery, err := deliverSkillOptABVariant(ctx, abAgent, prompt, challenger)
	if err != nil {
		return fmt.Errorf("live_ab challenger deliver: %w", err)
	}
	championDelivery := skillOptABDelivery{label: skillOptABChampionLabel, answer: championAnswer}

	// Present both answers and capture the pick. A missing pick (non-interactive
	// session) is a clean fail-safe skip — the champion answer already stands.
	winnerLabel, loserLabel, ok := presentLiveABAndCapturePick(prompt, championDelivery.answer, challengerDelivery.answer)
	if !ok {
		return fmt.Errorf("live_ab no pick captured")
	}

	// Route through the EXACT #473 record path: same RankedFeedbackEvent shape,
	// source tag, run-id prefix, and per-pick SourceURL as a manual `skillopt ab`.
	// Each intercepted ask is its OWN comparison (its own live prompt, its own
	// challenger answer, the champion resolved right now), so it mints its own
	// comparison token — the live path is exactly the many-picks-per-challenger
	// shape that makes per-comparison joining in the #344 harness mandatory.
	runID := skillOptABRunIDPrefix + challenger.version.ID
	pickSourceURL := skillOptABPickSourceURL(challenger.version.ID, skillOptABComparisonToken())
	if err := recordSkillOptABPick(ctx, store, paths, runID, pickSourceURL, templateID, champion, challenger, championDelivery, challengerDelivery, winnerLabel, loserLabel, prompt); err != nil {
		return fmt.Errorf("live_ab record pick: %w", err)
	}

	// Update both bandit arms (winner +win, loser +loss) — the SAME posterior
	// update as manual A/B. MANUAL PROMOTION is preserved: this writes feedback +
	// the posterior only; it never promotes or rolls back a version.
	championWin := winnerLabel == skillOptABChampionLabel
	if _, err := store.IncrementBanditArm(ctx, templateID, champion.version.ID, championWin); err != nil {
		return fmt.Errorf("live_ab update champion arm: %w", err)
	}
	if _, err := store.IncrementBanditArm(ctx, templateID, challenger.version.ID, !championWin); err != nil {
		return fmt.Errorf("live_ab update challenger arm: %w", err)
	}
	return nil
}

// resolveLiveABVariants resolves the champion (current promoted version) and the
// challenger (the sole pending candidate). ok=false (with nil error) means there
// is nothing to A/B — no current version, or zero/multiple pending candidates —
// and the caller falls through to a normal single ask.
func resolveLiveABVariants(ctx context.Context, store *db.Store, templateID string) (skillOptABVariant, skillOptABVariant, bool, error) {
	template, err := store.GetAgentTemplate(ctx, templateID)
	if err != nil {
		return skillOptABVariant{}, skillOptABVariant{}, false, err
	}
	championID := strings.TrimSpace(template.VersionID)
	if championID == "" {
		return skillOptABVariant{}, skillOptABVariant{}, false, nil
	}
	championVersion, err := store.GetAgentTemplateVersionByID(ctx, championID)
	if err != nil {
		return skillOptABVariant{}, skillOptABVariant{}, false, err
	}
	pending, err := store.ListPendingAgentTemplateVersions(ctx, templateID)
	if err != nil {
		return skillOptABVariant{}, skillOptABVariant{}, false, err
	}
	// Auto-interception only fires with a single unambiguous challenger; zero or
	// multiple pending candidates is a clean no-op (the operator can disambiguate
	// via the manual `skillopt ab --challenger`).
	if len(pending) != 1 {
		return skillOptABVariant{}, skillOptABVariant{}, false, nil
	}
	return skillOptABVariant{version: championVersion, label: skillOptABChampionLabel},
		skillOptABVariant{version: pending[0], label: skillOptABChallengerLabel},
		true, nil
}

// liveABPresenter is the presentation+pick seam (overridden in tests). It is
// shown both answers and returns the human pick as a role label
// (champion|challenger) plus ok=false when no pick could be captured (e.g. a
// non-interactive session), which the fail-safe caller treats as a clean skip.
var liveABPresenter = defaultLiveABPresenter

func presentLiveABAndCapturePick(prompt, championAnswer, challengerAnswer string) (winnerLabel, loserLabel string, ok bool) {
	return liveABPresenter(prompt, championAnswer, challengerAnswer)
}

// defaultLiveABPresenter prints both answers (shuffled as Option A/B) to stdout
// and reads one pick line via the shared readSkillOptABLine seam. A missing pick
// is the common non-interactive case and returns ok=false (fail-safe skip). When
// a valid pick arrives it is mapped back through the shuffle to the correct role.
func defaultLiveABPresenter(prompt, championAnswer, challengerAnswer string) (string, string, bool) {
	// Shuffle so the human cannot infer which answer is the challenger; map the
	// pick back to the role via the recorded swap.
	swap := liveABShuffle()
	optionAAnswer, optionAIsChampion := championAnswer, true
	optionBAnswer := challengerAnswer
	if swap {
		optionAAnswer, optionBAnswer = challengerAnswer, championAnswer
		optionAIsChampion = false
	}

	fmt.Printf("\n[live A/B] Two answers for: %s\n", prompt)
	fmt.Printf("Option A:\n%s\n\n", optionAAnswer)
	fmt.Printf("Option B:\n%s\n\n", optionBAnswer)
	fmt.Print("Which is better? [a/b] (enter to skip): ")

	line, gotLine := readSkillOptABLine()
	if !gotLine {
		return "", "", false
	}
	pick := strings.ToLower(strings.TrimSpace(line))
	if pick != "a" && pick != "b" {
		return "", "", false
	}
	pickedChampion := (pick == "a") == optionAIsChampion
	if pickedChampion {
		return skillOptABChampionLabel, skillOptABChallengerLabel, true
	}
	return skillOptABChallengerLabel, skillOptABChampionLabel, true
}

// liveABShuffle is a seam over the one coin flip that decides the A/B label
// shuffle; tests pin it. The default uses the same package math/rand source as
// liveABSampler so an unseeded run is non-deterministic (no fixed-seed bias).
var liveABShuffle = func() bool { return rand.Intn(2) == 1 }
