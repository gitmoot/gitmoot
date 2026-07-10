package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// skillOptABSource is the contract source tag on every Mode B (#473) row: the
// eval_run metadata feedback_source, the two eval_review_options, and the
// RankedFeedbackEvent. It keeps the pairwise A/B preferences trivially separable
// from Mode A's auto-trace rows (source=auto-trace) and from human gold.
const (
	skillOptABSource          = "skillopt-ab"
	skillOptABFeedbackSource  = "preference_ab"
	skillOptABRunIDPrefix     = "skillopt-ab:"
	skillOptABItemID          = "ab"
	skillOptABChampionLabel   = "champion"
	skillOptABChallengerLabel = "challenger"
)

// skillOptABJudgeSource / skillOptABJudgeReviewer tag the off-by-default
// cross-family LLM-judge auto-pairwise row (#483). They are DISTINCT from the
// human row's source/reviewer (skillopt-ab / human) so the judge's
// RankedFeedbackEvent COEXISTS with the human row under the UNIQUE
// (run_id,item_id,reviewer,source,source_url) conflict key instead of
// overwriting it — separable evidence, weighted BELOW the human pick by the
// source tag in the downstream export/optimizer.
//
// MEASURE-THE-JUDGE (#344/#345): the A/B judge is judge-derived, cross-family,
// and weighted-low here; it is NOT calibrated against human gold in this slice.
// It is judge-tagged + weighted-below-human + evidence-only (it NEVER touches
// the promotion Beta-Bernoulli bandit) until a judge↔human agreement capture
// per task-kind lands (#344/#345) and the optimizer can trust it more. Mirrors
// internal/skillopt/cross_family_review.go's reviewReviewerID MEASURE-THE-JUDGE
// note.
const (
	skillOptABJudgeSource   = "skillopt-ab-judge"
	skillOptABJudgeReviewer = "skillopt-ab-judge"
)

// skillOptABJurorSource / skillOptABJurorReviewerPrefix tag the PER-JUROR detail
// rows of a cross-family judge JURY (#349). The AGGREGATED jury verdict reuses the
// canonical skillopt-ab-judge reviewer/source above (so a jury strictly upgrades
// the single-judge evidence under the SAME tag — a downstream consumer that reads
// the judge row gets the better, aggregated verdict for free). Each individual
// juror's pick is recorded SEPARATELY under this DISTINCT source (reviewer
// skillopt-ab-juror:<family>) so the per-juror transparency rows coexist with the
// aggregate without being double-counted as extra judge votes. Like the single
// judge, NONE of these ever touch the promotion bandit (evidence-only).
const (
	skillOptABJurorSource         = "skillopt-ab-juror"
	skillOptABJurorReviewerPrefix = "skillopt-ab-juror:"
	// skillOptABJuryItemID is a DEDICATED eval_review_item id that carries ONLY the
	// jury aggregation metadata (disagreement flag, tallies, per-juror picks). It is
	// distinct from the shared "ab" item so the human/single-judge path's idempotent
	// ensureSkillOptABRunRows (which re-upserts the "ab" item without jury metadata)
	// can never clobber it. The per-juror ranked rows still reference the shared "ab"
	// item (with the real options); this item is metadata-only.
	skillOptABJuryItemID = skillOptABItemID + "-jury"
)

// skillOptABVariant is one resolved A/B arm: the version being served plus its
// human-readable role label (champion|challenger).
type skillOptABVariant struct {
	version db.AgentTemplateVersion
	label   string
}

// skillOptABDelivery is the result of running one variant through the runtime.
type skillOptABDelivery struct {
	label  string
	answer string
}

// skillOptABDeliverFunc is the seam tests override to inject deterministic
// answers without a live runtime (a foreground ask would also strand a
// runtime-session lock, so the two real calls MUST be serialized — see
// deliverSkillOptABVariant). The default builds a runtime adapter and calls
// Deliver once per variant.
type skillOptABDeliverFunc func(ctx context.Context, agent runtime.Agent, prompt string) (string, error)

// skillOptABDeliver is the package-level delivery seam (defaults to the real
// adapter path; overridden in tests). It is a var, not a const, exactly so the
// CLI A/B test can supply a fake adapter.
var skillOptABDeliver skillOptABDeliverFunc = realSkillOptABDeliver

// skillOptABJudgeDeliver is the SEPARATE delivery seam for the off-by-default
// cross-family LLM-judge call (#483). It defaults to the SAME real adapter path
// as skillOptABDeliver (realSkillOptABDeliver builds the adapter from the passed
// runtime.Agent's Runtime, so a judge Agent carrying a DIFFERENT family runs the
// judge on that family), and is its own var purely so tests inject a
// deterministic judge independent of the variant seam. The judge call is run
// SERIALIZED after the two variant deliveries (a third sequential Deliver) so
// per-runtime session locks never overlap — the same serialize-the-Deliver-calls
// discipline realSkillOptABDeliver documents (a foreground ask strands a 30-min
// runtime-session lock if it overlaps another on the same family).
var skillOptABJudgeDeliver skillOptABDeliverFunc = realSkillOptABDeliver

// skillOptABJudgeAuthedRuntimes is the injectable probe for which runtime
// families are authed/available, used to let PickCrossFamilyReviewer materialize
// an ephemeral cross-family judge leg only on a family that can actually run. It
// defaults to the real daemonAuthedRuntimes probe (which runs live Health checks)
// and is overridden in tests so unit tests stay deterministic and offline. It is
// a var of the reviewAuthedRuntimes type so it shares the cross-family review
// injection seam.
var skillOptABJudgeAuthedRuntimes = func(repoScope string) reviewAuthedRuntimes {
	return daemonAuthedRuntimes(repoScope)
}

// realSkillOptABDeliver resolves the agent's runtime adapter and delivers a
// single one-shot prompt, returning the answer text. Each call is independent
// and synchronous; runSkillOptAB invokes it twice in series so the second call
// only starts after the first returns — never concurrently — which keeps any
// per-runtime session usage cleanly serialized.
//
// Session isolation (#482): an EMPTY agent.RuntimeRef is the "forked/throwaway
// session" signal. The live-AB challenger sets it so its Deliver never resumes
// the agent's live session (codex always `exec resume`s in Deliver; claude/kimi
// resume a pinned ref), which would otherwise inject the challenger turn into the
// user's real conversation and poison the next genuine ask. On an empty ref we
// mint a fresh throwaway session via adapter.Start instead — a brand-new
// conversation the challenger answers on, leaving the agent's live session (and
// its resume_hint) untouched. A non-empty ref (the manual `skillopt ab` path)
// keeps its existing resume behavior unchanged.
func realSkillOptABDeliver(ctx context.Context, agent runtime.Agent, prompt string) (string, error) {
	// Adapter Dir must be a real filesystem checkout, NOT the "owner/repo"
	// RepoScope form. Callers that resolved the repo to its registered
	// db.Repo.CheckoutPath set agent.WorkingDir; prefer it so real (non-stubbed)
	// deliveries chdir into an existing directory. When unset we fall back to
	// RepoScope, preserving the legacy behavior of callers that never resolve a
	// checkout (the manual `skillopt ab`/#483 judge paths, which are test-stubbed).
	adapterDir := strings.TrimSpace(agent.WorkingDir)
	if adapterDir == "" {
		adapterDir = agent.RepoScope
	}
	adapter, err := runtimeStartAdapterFor(newRuntimeFactory(), agent.Runtime, adapterDir)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(agent.RuntimeRef) == "" {
		// Forked/throwaway session: Start opens a fresh conversation and answers the
		// prompt in one shot, never touching the agent's live session.
		started, startErr := adapter.Start(ctx, runtime.StartRequest{Agent: agent, Prompt: prompt})
		if startErr != nil {
			return "", startErr
		}
		return strings.TrimSpace(started.Raw), nil
	}
	result, err := adapter.Deliver(ctx, agent, runtime.Job{AgentName: agent.Name, Action: "ask", Prompt: prompt, Model: agent.Model, Effort: agent.Effort})
	if err != nil {
		return "", err
	}
	answer := strings.TrimSpace(result.Summary)
	if answer == "" {
		answer = strings.TrimSpace(result.Raw)
	}
	return answer, nil
}

type skillOptABOptions struct {
	home       string
	agent      string
	prompt     string
	challenger string
	pick       string
	seed       int64
	seedSet    bool
	// judge turns the off-by-default cross-family LLM-judge auto-pairwise path ON
	// for this invocation (#483); the [skillopt].mode_b_judge_enabled config knob is
	// the equivalent persistent admission gate. When neither is set the command is
	// byte-identical to #473 (no judge selected, delivered, or recorded).
	judge bool
	// judgeOnly skips soliciting the human pick and records ONLY the judge row
	// (mirrors the --pick non-interactive escape). It implies --judge.
	judgeOnly bool
	// jurySize turns the cross-family judge into a JURY of this many DISTINCT-family
	// judges (#349); >= 2 implies --judge. 0 (the default) defers to the
	// [skillopt].mode_b_jury_size config knob; an explicit flag overrides it. < 2
	// (after resolution) is the single-judge path (byte-identical).
	jurySize int
}

func runSkillOptAB(args []string, stdout, stderr io.Writer) int {
	options, ok := parseSkillOptABOptions(args, stderr)
	if !ok {
		if containsHelpFlag(args) {
			return 0
		}
		return 2
	}

	exit := 0
	err := withStoreAndPaths(options.home, func(paths config.Paths, store *db.Store) error {
		code := runSkillOptABWithStore(context.Background(), store, paths, options, stdout, stderr)
		exit = code
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: %v\n", err)
		return 1
	}
	return exit
}

func parseSkillOptABOptions(args []string, stderr io.Writer) (skillOptABOptions, bool) {
	if len(args) == 0 || containsHelpFlag(args) {
		printSkillOptABUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "skillopt ab requires an agent and a prompt")
		}
		return skillOptABOptions{}, false
	}
	fs := flag.NewFlagSet("skillopt ab", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	challenger := fs.String("challenger", "", "challenger version id (defaults to the sole pending candidate)")
	pick := fs.String("pick", "", "non-interactive pick: a or b (the shuffled option labels)")
	seed := fs.Int64("seed", 0, "deterministic seed for the label shuffle and the P(>) Monte Carlo")
	judge := fs.Bool("judge", false, "also have a cross-family LLM judge pick A/B and record a separate skillopt-ab-judge row (#483; off by default)")
	judgeOnly := fs.Bool("judge-only", false, "record ONLY the cross-family judge row, skipping the human pick prompt (implies --judge)")
	jurySize := fs.Int("jury-size", 0, "run a cross-family judge JURY of N distinct-family judges instead of one (#349; >=2 implies --judge; 0 defers to [skillopt].mode_b_jury_size)")
	// Separate the leading positionals (agent, prompt) from flags. flag.Parse stops
	// at the first non-flag, so collect positionals manually to allow them anywhere.
	positionals := []string{}
	rest := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			// flags that take a value consume the next token unless --flag=value.
			if !strings.Contains(arg, "=") && (arg == "--home" || arg == "--challenger" || arg == "--pick" || arg == "--seed" || arg == "--jury-size") && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	if err := fs.Parse(rest); err != nil {
		return skillOptABOptions{}, false
	}
	if len(positionals) != 2 {
		fmt.Fprintln(stderr, "skillopt ab requires exactly one agent and one prompt")
		printSkillOptABUsage(stderr)
		return skillOptABOptions{}, false
	}
	seedSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedSet = true
		}
	})
	pickValue := strings.ToLower(strings.TrimSpace(*pick))
	if pickValue != "" && pickValue != "a" && pickValue != "b" {
		fmt.Fprintln(stderr, "skillopt ab --pick must be a or b")
		return skillOptABOptions{}, false
	}
	// --judge-only implies --judge; --jury-size >= 2 implies --judge (the jury runs
	// inside the judge seam): recording any judge/jury row requires the judge path
	// to be admitted.
	judgeOn := *judge || *judgeOnly || *jurySize >= 2
	return skillOptABOptions{
		home:       strings.TrimSpace(*home),
		agent:      strings.TrimSpace(positionals[0]),
		prompt:     strings.TrimSpace(positionals[1]),
		challenger: strings.TrimSpace(*challenger),
		pick:       pickValue,
		seed:       *seed,
		seedSet:    seedSet,
		judge:      judgeOn,
		judgeOnly:  *judgeOnly,
		jurySize:   *jurySize,
	}, true
}

func printSkillOptABUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt ab <agent> \"<prompt>\" [--challenger <versionId>] [--pick a|b] [--seed N] [--judge] [--judge-only] [--home path]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Champion-challenger A/B (#473 Mode B): serves the prompt through the agent's")
	fmt.Fprintln(w, "current promoted template version (champion) AND a pending candidate version")
	fmt.Fprintln(w, "(challenger), shuffles the two answers as Option A/B, records the human pick as a")
	fmt.Fprintln(w, "pairwise RankedFeedbackEvent (source=skillopt-ab), updates the Beta-Bernoulli")
	fmt.Fprintln(w, "bandit, and prints P(challenger>champion). Manual only; no pending candidate is a")
	fmt.Fprintln(w, "clean no-op.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "--judge (or [skillopt].mode_b_judge_enabled, off by default) ALSO has a")
	fmt.Fprintln(w, "CROSS-FAMILY LLM judge pick A/B from the same shuffled options and records a")
	fmt.Fprintln(w, "SEPARATE skillopt-ab-judge RankedFeedbackEvent that coexists with (and weights")
	fmt.Fprintln(w, "below) the human row. The judge NEVER touches the promotion bandit and is")
	fmt.Fprintln(w, "skipped when no different runtime family is available. --judge-only records only")
	fmt.Fprintln(w, "the judge row (skips the human pick prompt).")
}

func runSkillOptABWithStore(ctx context.Context, store *db.Store, paths config.Paths, options skillOptABOptions, stdout, stderr io.Writer) int {
	agent, err := store.GetAgent(ctx, options.agent)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: resolve agent %q: %v\n", options.agent, err)
		return 1
	}
	templateID, _ := db.SplitAgentTemplateReference(strings.TrimSpace(agent.TemplateID))
	if templateID == "" {
		fmt.Fprintf(stderr, "skillopt ab: agent %q has no template; nothing to A/B\n", options.agent)
		return 1
	}

	champion, challenger, ok, code := resolveSkillOptABVariants(ctx, store, templateID, options, stdout, stderr)
	if !ok {
		return code
	}

	runtimeAgent := runtime.Agent{
		Name:           agent.Name,
		Role:           firstNonEmpty(agent.Role, "ask"),
		Runtime:        agent.Runtime,
		RuntimeRef:     agent.RuntimeRef,
		RepoScope:      agent.RepoScope,
		TemplateID:     agent.TemplateID,
		Capabilities:   agent.Capabilities,
		AutonomyPolicy: firstNonEmpty(agent.AutonomyPolicy, runtime.AutonomyPolicyReadOnly),
		Model:          agent.Model,
		Effort:         agent.Effort,
	}

	// Deliver BOTH variants, SERIALIZED (the second call only after the first
	// returns) so any per-runtime session usage never overlaps and locks release
	// cleanly between calls.
	championDelivery, err := deliverSkillOptABVariant(ctx, runtimeAgent, options.prompt, champion)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: deliver champion: %v\n", err)
		return 1
	}
	challengerDelivery, err := deliverSkillOptABVariant(ctx, runtimeAgent, options.prompt, challenger)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: deliver challenger: %v\n", err)
		return 1
	}

	// Label-shuffle: map Option A/B to champion/challenger via a recorded mapping so
	// the human cannot infer which is the challenger, yet the stored event maps the
	// pick back to the CORRECT role (an off-by-one here would silently invert every
	// preference). optionA/optionB are the deliveries behind the presented labels.
	//
	// Seed selection mirrors skillOptABProbSeed: when --seed is set the shuffle is
	// deterministic (so the seeded tests pin the A=champion/A=challenger mapping);
	// when it is NOT set we MUST seed nondeterministically. rand.NewSource(0).Intn(2)
	// is a constant 0, so a fixed default-0 seed would make swap=false on EVERY
	// unseeded run — Option B would always be the challenger, reintroducing exactly
	// the position/identity bias the shuffle exists to remove.
	rng := rand.New(rand.NewSource(skillOptABShuffleSeed(options)))
	swap := rng.Intn(2) == 1
	optionA, optionB := championDelivery, challengerDelivery
	if swap {
		optionA, optionB = challengerDelivery, championDelivery
	}

	fmt.Fprintf(stdout, "Prompt: %s\n\n", options.prompt)
	fmt.Fprintf(stdout, "Option A:\n%s\n\n", optionA.answer)
	fmt.Fprintf(stdout, "Option B:\n%s\n\n", optionB.answer)

	// runID keeps the version-id prefix so every pick stays trivially filterable as
	// Mode B for THIS challenger. The judge row (when enabled) reuses this SAME run
	// and item so it coexists with the human row instead of forming a parallel run.
	runID := skillOptABRunIDPrefix + challenger.version.ID

	// comparisonToken identifies THIS invocation's single comparison. (run_id,
	// item_id) alone is the CHALLENGER, not the comparison — run_id is
	// "skillopt-ab:<challengerVersion>" and item_id is always "ab", so every
	// repeated A/B of one challenger (each with its own prompt, freshly
	// regenerated answers, its own shuffle, and the champion resolved at
	// invocation time) would collide into one bucket. The token is embedded in
	// the SourceURL of EVERY row this invocation records (human pick, judge
	// aggregate, per-juror details) so the #344 agreement harness can join judge
	// vs human PER COMPARISON instead of pooling verdicts made on different
	// comparisons (cross-item contamination).
	comparisonToken := skillOptABComparisonToken()

	// OFF-BY-DEFAULT cross-family LLM-judge auto-pairwise (#483). When neither the
	// --judge flag nor [skillopt].mode_b_judge_enabled is set, judgeEnabled is false
	// and this whole branch is skipped — no cross-family reviewer is selected, no
	// third Deliver happens, no judge row is written: byte-identical to #473. The
	// judge delivery is the THIRD SERIALIZED Deliver (after both variants, before any
	// interactive human pick) so per-runtime session locks never overlap; the judge
	// sees the SAME shuffled Option A/B as the human (it never learns champion vs
	// challenger), and its pick maps back through the SAME optionA/optionB mapping.
	if skillOptABJudgeEnabled(paths, options) {
		runSkillOptABJudge(ctx, store, paths, runtimeAgent, agent, runID, comparisonToken, templateID, champion, challenger, optionA, optionB, options, stdout, stderr)
	}

	// --judge-only records ONLY the judge row above and skips the human pick + bandit
	// update entirely (mirrors the --pick non-interactive escape). The human path is
	// untouched in every other case.
	if options.judgeOnly {
		return 0
	}

	pick, ok := resolveSkillOptABPick(options.pick, stdout, stderr)
	if !ok {
		return 2
	}

	// Map the presented pick (a|b) back to the role label via the shuffle mapping.
	pickedDelivery := optionA
	if pick == "b" {
		pickedDelivery = optionB
	}
	winnerLabel := pickedDelivery.label
	loserLabel := skillOptABChampionLabel
	if winnerLabel == skillOptABChampionLabel {
		loserLabel = skillOptABChallengerLabel
	}

	// pickSourceURL is a per-pick token that makes each recorded preference a
	// DISTINCT contract row: the ranked_feedback_events conflict key is
	// (run_id, item_id, reviewer, source, source_url), and the first four are
	// identical across repeated A/Bs of the same challenger, so without a unique
	// source_url an ON CONFLICT DO UPDATE would overwrite every prior pick and only
	// the last preference would survive as evidence. It carries the SHARED
	// per-invocation comparison token so the human row and this invocation's
	// judge/juror rows join as ONE comparison in the #344 harness.
	pickSourceURL := skillOptABPickSourceURL(challenger.version.ID, comparisonToken)
	if err := recordSkillOptABPick(ctx, store, paths, runID, pickSourceURL, templateID, champion, challenger, championDelivery, challengerDelivery, winnerLabel, loserLabel, options.prompt); err != nil {
		fmt.Fprintf(stderr, "skillopt ab: record pick: %v\n", err)
		return 1
	}

	// Update both bandit arms in the same logical step: winner +win, loser +loss.
	championWin := winnerLabel == skillOptABChampionLabel
	if _, err := store.IncrementBanditArm(ctx, templateID, champion.version.ID, championWin); err != nil {
		fmt.Fprintf(stderr, "skillopt ab: update champion arm: %v\n", err)
		return 1
	}
	challengerArm, err := store.IncrementBanditArm(ctx, templateID, challenger.version.ID, !championWin)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: update challenger arm: %v\n", err)
		return 1
	}
	championArm, _, err := store.GetBanditArm(ctx, templateID, champion.version.ID)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: read champion arm: %v\n", err)
		return 1
	}

	prob := skillopt.ProbChallengerBeats(
		skillopt.BetaParams{Alpha: championArm.Alpha, Beta: championArm.Beta},
		skillopt.BetaParams{Alpha: challengerArm.Alpha, Beta: challengerArm.Beta},
		rand.New(rand.NewSource(skillOptABProbSeed(options))),
		skillopt.DefaultProbDraws,
	)

	fmt.Fprintf(stdout, "Recorded pick: %s (Option %s)\n", winnerLabel, strings.ToUpper(pick))
	fmt.Fprintf(stdout, "Champion %s: Beta(%.0f, %.0f)\n", champion.version.ID, championArm.Alpha, championArm.Beta)
	fmt.Fprintf(stdout, "Challenger %s: Beta(%.0f, %.0f)\n", challenger.version.ID, challengerArm.Alpha, challengerArm.Beta)
	fmt.Fprintf(stdout, "P(challenger>champion): %s\n", skillopt.ConfidenceSummary(prob, challengerArm.Pulls))
	return 0
}

// skillOptABShuffleSeed derives the label-shuffle seed: the explicit --seed when
// set (so the seeded shuffle tests pin the A/B->role mapping deterministically),
// else a nondeterministic time-based fallback so repeated unseeded interactive
// runs do NOT all reuse seed 0. rand.NewSource(0).Intn(2) is a constant 0, so a
// pinned-0 default would make swap=false on every default run — Option B would
// always be the challenger, defeating the anti-bias guarantee. This mirrors
// skillOptABProbSeed's !seedSet fallback so the two seams stay consistent.
func skillOptABShuffleSeed(options skillOptABOptions) int64 {
	if options.seedSet {
		return options.seed
	}
	return time.Now().UnixNano()
}

// skillOptABProbSeed derives the P(>) Monte Carlo seed: the explicit --seed when
// set, else a deterministic-enough fallback so repeated runs without a seed do
// not all reuse 0 (which would bias toward the same rng stream). The shuffle uses
// the raw seed; the prob estimate offsets it so the two streams are independent.
func skillOptABProbSeed(options skillOptABOptions) int64 {
	if options.seedSet {
		return options.seed ^ 0x6a09e667f3bcc908
	}
	return time.Now().UnixNano()
}

// resolveSkillOptABVariants resolves the champion (current promoted version) and
// challenger (the --challenger version, or the sole pending candidate). It
// returns ok=false with a clean exit code: 0 (no pending challenger — the honest
// no-op) or 1 (a real error). The champion must be a promoted current version.
func resolveSkillOptABVariants(ctx context.Context, store *db.Store, templateID string, options skillOptABOptions, stdout, stderr io.Writer) (skillOptABVariant, skillOptABVariant, bool, int) {
	template, err := store.GetAgentTemplate(ctx, templateID)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: resolve template %q: %v\n", templateID, err)
		return skillOptABVariant{}, skillOptABVariant{}, false, 1
	}
	championID := strings.TrimSpace(template.VersionID)
	if championID == "" {
		fmt.Fprintf(stderr, "skillopt ab: template %q has no current promoted version\n", templateID)
		return skillOptABVariant{}, skillOptABVariant{}, false, 1
	}
	championVersion, err := store.GetAgentTemplateVersionByID(ctx, championID)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: resolve champion version: %v\n", err)
		return skillOptABVariant{}, skillOptABVariant{}, false, 1
	}

	pending, err := store.ListPendingAgentTemplateVersions(ctx, templateID)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt ab: list pending candidates: %v\n", err)
		return skillOptABVariant{}, skillOptABVariant{}, false, 1
	}
	var challengerVersion db.AgentTemplateVersion
	switch {
	case options.challenger != "":
		found := false
		for _, v := range pending {
			if v.ID == options.challenger {
				challengerVersion = v
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(stderr, "skillopt ab: challenger %q is not a pending candidate of template %q\n", options.challenger, templateID)
			return skillOptABVariant{}, skillOptABVariant{}, false, 1
		}
	case len(pending) == 0:
		fmt.Fprintln(stdout, "nothing to A/B: no pending challenger; promote a candidate or run skillopt train to produce one")
		return skillOptABVariant{}, skillOptABVariant{}, false, 0
	case len(pending) == 1:
		challengerVersion = pending[0]
	default:
		fmt.Fprintf(stderr, "skillopt ab: %d pending candidates; pass --challenger <versionId> to choose one\n", len(pending))
		return skillOptABVariant{}, skillOptABVariant{}, false, 1
	}

	return skillOptABVariant{version: championVersion, label: skillOptABChampionLabel},
		skillOptABVariant{version: challengerVersion, label: skillOptABChallengerLabel},
		true, 0
}

// deliverSkillOptABVariant builds the per-variant prompt (the variant's own
// template instructions prepended to the user prompt) and delivers it through
// the seam.
func deliverSkillOptABVariant(ctx context.Context, agent runtime.Agent, userPrompt string, variant skillOptABVariant) (skillOptABDelivery, error) {
	prompt := skillOptABVariantPrompt(variant.version, userPrompt)
	answer, err := skillOptABDeliver(ctx, agent, prompt)
	if err != nil {
		return skillOptABDelivery{}, err
	}
	return skillOptABDelivery{label: variant.label, answer: strings.TrimSpace(answer)}, nil
}

func skillOptABVariantPrompt(version db.AgentTemplateVersion, userPrompt string) string {
	var b strings.Builder
	instructions := strings.TrimRight(agenttemplate.InstructionsForContent(version.Content), "\n")
	if instructions != "" {
		b.WriteString("Template instructions:\n")
		b.WriteString(instructions)
		b.WriteString("\n\n")
	}
	b.WriteString(userPrompt)
	return b.String()
}

// resolveSkillOptABPick returns the human pick (a|b). With --pick it is
// non-interactive; otherwise it reads one line from stdin.
func resolveSkillOptABPick(flagPick string, stdout io.Writer, stderr io.Writer) (string, bool) {
	if flagPick == "a" || flagPick == "b" {
		return flagPick, true
	}
	fmt.Fprint(stdout, "Which is better? [a/b]: ")
	line, ok := readSkillOptABLine()
	if !ok {
		fmt.Fprintln(stderr, "skillopt ab: no pick provided")
		return "", false
	}
	pick := strings.ToLower(strings.TrimSpace(line))
	if pick != "a" && pick != "b" {
		fmt.Fprintln(stderr, "skillopt ab: pick must be a or b")
		return "", false
	}
	return pick, true
}

// readSkillOptABLine is a thin seam over reading one interactive line, overridden
// in tests; the default reads from os.Stdin.
var readSkillOptABLine = defaultReadSkillOptABLine

func defaultReadSkillOptABLine() (string, bool) {
	var line string
	if _, err := fmt.Scanln(&line); err != nil {
		return "", false
	}
	return line, true
}

// recordSkillOptABPick persists the pairwise contract rows for one A/B pick: a
// 2-option eval_run (OptionsCount=2, metadata feedback_source=preference_ab), two
// eval_review_options (champion/challenger) backed by the answer artifacts (via
// the existing blob path), one eval_review_item, and ONE RankedFeedbackEvent
// (ranking=[winner,loser], source=skillopt-ab) that passes
// store.validateRankedFeedbackEventOptions.
func recordSkillOptABPick(ctx context.Context, store *db.Store, paths config.Paths, runID, pickSourceURL, templateID string, champion, challenger skillOptABVariant, championDelivery, challengerDelivery skillOptABDelivery, winnerLabel, loserLabel, prompt string) error {
	if err := ensureSkillOptABRunRows(ctx, store, paths, runID, skillOptABItemID, templateID, champion, challenger, championDelivery, challengerDelivery, prompt, skillOptABSource, skillOptABFeedbackSource); err != nil {
		return err
	}
	return upsertSkillOptABRankedEvent(ctx, store, runID, skillOptABItemID, winnerLabel, loserLabel, "human", skillOptABSource, pickSourceURL, "")
}

// ensureSkillOptABRunRows idempotently upserts the shared pairwise scaffolding for
// one A/B run: the 2-option eval_run (OptionsCount=2, metadata
// feedback_source=preference_ab), the eval_review_item, and the two
// eval_review_options (champion/challenger) backed by the answer artifacts via the
// blob path. Both the human-record path AND the judge-record path call it, so the
// judge row is self-sufficient when --judge-only skips the human record, and the
// rows are byte-identical when both run (every write here is an idempotent upsert
// keyed by (run_id,item_id,label)).
func ensureSkillOptABRunRows(ctx context.Context, store *db.Store, paths config.Paths, runID, itemID, templateID string, champion, challenger skillOptABVariant, championDelivery, challengerDelivery skillOptABDelivery, prompt, source, feedbackSource string) error {
	blobStore := artifact.NewStore(paths.ArtifactBlobs)

	metadataJSON, err := json.Marshal(map[string]any{
		"feedback_source": feedbackSource,
		"source":          source,
		"champion":        champion.version.ID,
		"challenger":      challenger.version.ID,
	})
	if err != nil {
		return err
	}
	run := db.EvalRun{
		ID:                runID,
		TemplateID:        templateID,
		TemplateVersionID: challenger.version.ID,
		State:             "complete",
		Mode:              db.EvalRunModeValidate,
		OptionsCount:      2,
		MetadataJSON:      string(metadataJSON),
	}
	if err := store.UpsertEvalRun(ctx, run); err != nil {
		return err
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{RunID: runID, ItemID: itemID, Title: prompt}); err != nil {
		return err
	}

	// Store each answer as an eval_artifact via the blob path, then register the
	// option pointing at it.
	for _, opt := range []struct {
		label    string
		delivery skillOptABDelivery
	}{
		{label: skillOptABChampionLabel, delivery: championDelivery},
		{label: skillOptABChallengerLabel, delivery: challengerDelivery},
	} {
		evalArtifact, err := prepareReviewItemContentArtifact(blobStore, runID, itemID, opt.label, []byte(answerOrPlaceholder(opt.delivery.answer)), "text/plain", "text")
		if err != nil {
			return err
		}
		if err := store.UpsertEvalArtifact(ctx, evalArtifact); err != nil {
			return err
		}
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
			RunID:      runID,
			ItemID:     itemID,
			Label:      opt.label,
			ArtifactID: evalArtifact.ID,
			Role:       opt.label,
		}); err != nil {
			return err
		}
	}
	return nil
}

// upsertSkillOptABRankedEvent writes ONE pairwise RankedFeedbackEvent
// (ranking=[winner,loser], tie groups [[winner],[loser]]) for the given
// reviewer/source/source_url over the existing run's two options. The
// (run_id,item_id,reviewer,source,source_url) conflict key is what keeps the human
// row (reviewer=human,source=skillopt-ab) and the judge row
// (reviewer=skillopt-ab-judge,source=skillopt-ab-judge) as SEPARATE coexisting
// rows on the same run/item, and what makes repeated picks each persist via a
// distinct per-pick source_url. reasoning is stored verbatim (the judge/juror
// paths pass the machine-readable position blob from
// skillOptABJudgePickPositionJSON; the human paths pass "").
func upsertSkillOptABRankedEvent(ctx context.Context, store *db.Store, runID, itemID, winnerLabel, loserLabel, reviewer, source, sourceURL, reasoning string) error {
	ranking, err := json.Marshal([]string{winnerLabel, loserLabel})
	if err != nil {
		return err
	}
	tieGroups, err := json.Marshal([][]string{{winnerLabel}, {loserLabel}})
	if err != nil {
		return err
	}
	return store.UpsertRankedFeedbackEvent(ctx, db.RankedFeedbackEvent{
		RunID:         runID,
		ItemID:        itemID,
		RankingJSON:   string(ranking),
		TieGroupsJSON: string(tieGroups),
		Winner:        winnerLabel,
		Reasoning:     reasoning,
		Reviewer:      reviewer,
		Source:        source,
		SourceURL:     sourceURL,
	})
}

// skillOptABJudgePickPositionJSON encodes the judge's RAW presented-position
// pick (a|b) as a tiny machine-readable JSON blob carried on the judge/juror
// RankedFeedbackEvent.Reasoning field. This is the MINIMAL ADDITIVE persistence
// the position-bias audit needs (#344, "Reliability without Validity"): the
// stored Winner is the UNBLINDED role (champion|challenger), so without this
// blob the presented position is unrecoverable and |P(pick=a) - 0.5| cannot be
// measured. Reasoning is an existing opaque passthrough (no schema change), it
// was previously always empty on judge/juror rows, and judge rows are
// source-tagged + weighted below human downstream, so filling it changes no
// existing flow.
func skillOptABJudgePickPositionJSON(pick string) string {
	data, err := json.Marshal(map[string]string{"judge_pick_position": pick})
	if err != nil {
		return ""
	}
	return string(data)
}

// skillOptABJudgePickPosition decodes the position blob written by
// skillOptABJudgePickPositionJSON. ok is false for rows that predate the
// position capture (empty/free-text reasoning) or carry anything other than a
// valid a|b position, so the audit silently skips unmeasurable rows instead of
// fabricating positions.
func skillOptABJudgePickPosition(reasoning string) (string, bool) {
	reasoning = strings.TrimSpace(reasoning)
	if reasoning == "" {
		return "", false
	}
	var decoded struct {
		JudgePickPosition string `json:"judge_pick_position"`
	}
	if err := json.Unmarshal([]byte(reasoning), &decoded); err != nil {
		return "", false
	}
	pick := strings.ToLower(strings.TrimSpace(decoded.JudgePickPosition))
	if pick != "a" && pick != "b" {
		return "", false
	}
	return pick, true
}

// skillOptABComparisonMarker is the SourceURL fragment marker that carries the
// per-invocation comparison token. It is deliberately distinct from the legacy
// "#<nano>-<seq>" suffix (which was minted PER ROW, so the human row and the
// judge row of the same comparison never shared it): a SourceURL without this
// marker is a legacy row the #344 agreement harness must treat as unmeasurable
// rather than pool by challenger.
const skillOptABComparisonMarker = "#cmp:"

// skillOptABComparisonToken mints ONE token per `skillopt ab` invocation — i.e.
// per COMPARISON: one prompt, one pair of freshly generated answers, one
// shuffle, one champion resolved at that instant. Every row the invocation
// records (human pick, judge aggregate, per-juror details) embeds this SAME
// token in its SourceURL, which is what lets the agreement harness join judge
// verdicts against human verdicts of the SAME comparison. The
// nanosecond+counter shape also keeps each invocation's conflict keys distinct
// (the uniqueness role the old per-row suffix played).
func skillOptABComparisonToken() string {
	seq := atomic.AddUint64(&skillOptABPickSeq, 1)
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), seq)
}

// skillOptABComparisonTokenFromSourceURL recovers the per-invocation comparison
// token from a recorded SourceURL. ok=false for legacy rows written before the
// token existed (their "#<nano>-<seq>" suffix carries no marker), so the
// agreement harness can exclude them loudly instead of fabricating a join.
func skillOptABComparisonTokenFromSourceURL(sourceURL string) (string, bool) {
	index := strings.LastIndex(sourceURL, skillOptABComparisonMarker)
	if index < 0 {
		return "", false
	}
	token := strings.TrimSpace(sourceURL[index+len(skillOptABComparisonMarker):])
	if token == "" {
		return "", false
	}
	return token, true
}

// skillOptABPickSourceURL mints the human pick's SourceURL for the Mode B
// RankedFeedbackEvent. The run id, item id, reviewer, and source are constant
// across repeated A/Bs of one challenger, so the embedded per-invocation
// comparison token is what differentiates each pick's conflict key (a tight
// loop of picks never collides on a single row) AND what joins this pick with
// the same invocation's judge/juror rows in the #344 agreement harness.
func skillOptABPickSourceURL(versionID, comparisonToken string) string {
	return fmt.Sprintf("%s%s%s%s", skillOptABRunIDPrefix, versionID, skillOptABComparisonMarker, comparisonToken)
}

// skillOptABPickSeq is a process-local monotonic counter that guarantees two
// comparison tokens minted within the same nanosecond are still distinct.
var skillOptABPickSeq uint64

// answerOrPlaceholder guarantees a non-empty artifact body (the blob path rejects
// empty content), substituting a marker when a variant returned no text.
func answerOrPlaceholder(answer string) string {
	if strings.TrimSpace(answer) == "" {
		return "(no answer)"
	}
	return answer
}

// skillOptABJudgeEnabled is the SINGLE admission gate for the off-by-default
// cross-family LLM-judge auto-pairwise path (#483): the per-invocation --judge /
// --judge-only flag OR the persistent [skillopt].mode_b_judge_enabled config knob.
// When neither is set it returns false and the judge branch is skipped entirely —
// byte-identical to #473. A config read error fails safe to OFF (the flag still
// admits it) so a malformed config never turns the judge on by accident.
func skillOptABJudgeEnabled(paths config.Paths, options skillOptABOptions) bool {
	if options.judge {
		return true
	}
	policy, err := config.LoadSkillOptPolicy(paths)
	if err != nil {
		return false
	}
	// A configured jury (mode_b_jury_size >= 2) admits the judge seam too: the jury
	// runs INSIDE it, so enabling the jury alone is enough to turn it on (#349).
	return policy.ModeBJudgeEnabled || policy.ModeBJurySize >= 2
}

// skillOptABJuryConfig resolves the effective jury parameters (#349): the
// per-invocation --jury-size flag wins when set (>0), else the persistent
// [skillopt].mode_b_jury_* config knobs. A size < 2 means the single-judge path
// (the jury is off / byte-identical). A config read error fails safe to size 0
// (single judge) so a malformed config never silently fans out judges.
func skillOptABJuryConfig(paths config.Paths, options skillOptABOptions) skillopt.JuryConfig {
	cfg := skillopt.JuryConfig{}
	policy, err := config.LoadSkillOptPolicy(paths)
	if err == nil {
		cfg.VetoDimensions = policy.ModeBJuryVetoDimensions
		cfg.VetoFloor = policy.ModeBJuryVetoFloor
		cfg.DisagreementTau = policy.ModeBJuryDisagreementTau
	}
	return cfg
}

// skillOptABJurySize resolves the effective jury size: the --jury-size flag when
// set (>0), else [skillopt].mode_b_jury_size, else 0 (single judge). A config read
// error fails safe to the single-judge path.
func skillOptABJurySize(paths config.Paths, options skillOptABOptions) int {
	if options.jurySize > 0 {
		return options.jurySize
	}
	policy, err := config.LoadSkillOptPolicy(paths)
	if err != nil {
		return 0
	}
	return policy.ModeBJurySize
}

// runSkillOptABJudge runs the off-by-default cross-family LLM-judge auto-pairwise
// leg (#483) best-effort: it selects a CROSS-FAMILY judge (reusing #470's
// PickCrossFamilyReviewer + the SelfFamily refinement), SKIPS on no-cross-family /
// not-authed (never a silent same-family self-judgment), delivers the SAME shuffled
// Option A/B text the human sees through the SERIALIZED third Deliver seam, parses
// the judge's {"pick":"a"|"b"}, maps that pick back through the SAME optionA/optionB
// mapping to the correct champion/challenger role, and records ONE separate
// skillopt-ab-judge RankedFeedbackEvent that coexists with the human row. It NEVER
// touches the promotion bandit and NEVER fails the command: every error path logs a
// best-effort note and returns, leaving the human path intact (fail-safe).
func runSkillOptABJudge(ctx context.Context, store *db.Store, paths config.Paths, runtimeAgent runtime.Agent, agent db.Agent, runID, comparisonToken, templateID string, champion, challenger skillOptABVariant, optionA, optionB skillOptABDelivery, options skillOptABOptions, stdout, stderr io.Writer) {
	// authedRuntimes probes which families can actually run, so an ephemeral judge
	// leg is only materialized on an authed DIFFERENT family. The probe is injectable
	// so unit tests stay deterministic/offline.
	authed := map[string]bool{}
	if probe := skillOptABJudgeAuthedRuntimes(agent.RepoScope); probe != nil {
		authed = probe(ctx)
	}

	// JURY (#349): when a jury of >= 2 distinct families is configured AND >= 2
	// distinct families are actually available, run the cross-family judge JURY and
	// return. With the jury OFF (size < 2) or fewer than 2 distinct families
	// available, fall through to the EXISTING single cross-family judge path below —
	// byte-identical to #483. The authed probe is computed once above and shared.
	if size := skillOptABJurySize(paths, options); size >= 2 {
		jury, err := workflow.PickCrossFamilyJury(ctx, store, agent.Runtime, agent.RepoScope, authed, size)
		if err != nil {
			log.Printf("skillopt ab jury: select cross-family jury: %v", err)
			return
		}
		if len(jury) >= 2 {
			runSkillOptABJudgeJury(ctx, store, paths, runID, comparisonToken, templateID, champion, challenger, optionA, optionB, options, jury, stdout)
			return
		}
		// < 2 distinct families: graceful degradation to the single-judge path.
		fmt.Fprintf(stdout, "skillopt ab: only %d distinct cross-family judge(s) available; falling back to a single judge\n", len(jury))
	}

	reviewer, ok, err := workflow.PickCrossFamilyReviewer(ctx, store, agent.Runtime, agent.RepoScope, authed)
	if err != nil {
		log.Printf("skillopt ab judge: select cross-family judge: %v", err)
		return
	}
	// CROSS-FAMILY ONLY: SelfFamily (no different family available) or !ok (no
	// review-capable runtime authed at all) means SKIP — never a same-family
	// self-preference judgment of one's own template family. This is stricter than
	// #470's same-family-with-warning, which is correct for an A/B judge of one's own
	// family.
	if !ok || reviewer.SelfFamily {
		fmt.Fprintln(stdout, "skillopt ab: no cross-family judge available; skipping judge (cross-family only)")
		return
	}

	judgeAgent := skillOptABJudgeAgent(reviewer)
	prompt := buildSkillOptABJudgePrompt(options.prompt, optionA.answer, optionB.answer)
	raw, err := skillOptABJudgeDeliver(ctx, judgeAgent, prompt)
	if err != nil {
		log.Printf("skillopt ab judge: deliver judge prompt: %v", err)
		return
	}
	pick, parsed := parseSkillOptABJudgePick(raw)
	if !parsed {
		// Fail-safe: unparseable/empty/tie judge output drops the judge result — no
		// row, no error escalation — exactly like ParseReviewRubric's HasScore=false
		// "don't fabricate" stance. The human path is unaffected.
		fmt.Fprintln(stdout, "skillopt ab: judge returned no usable pick; skipping judge row")
		return
	}

	// Map the judge's a/b pick back to the role label via the SAME shuffle mapping
	// the human pick uses (the judge saw the SAME debiased Option A/B), so an
	// off-by-one here cannot silently invert the judged preference.
	pickedDelivery := optionA
	if pick == "b" {
		pickedDelivery = optionB
	}
	winnerLabel := pickedDelivery.label
	loserLabel := skillOptABChampionLabel
	if winnerLabel == skillOptABChampionLabel {
		loserLabel = skillOptABChallengerLabel
	}

	// Ensure the shared run/item/options exist (idempotent) so the judge row is
	// self-sufficient even under --judge-only where the human record path never runs.
	if err := ensureSkillOptABRunRows(ctx, store, paths, runID, skillOptABItemID, templateID, champion, challenger, optionAToDelivery(optionA, optionB, champion), optionAToDelivery(optionA, optionB, challenger), options.prompt, skillOptABSource, skillOptABFeedbackSource); err != nil {
		log.Printf("skillopt ab judge: ensure run rows: %v", err)
		return
	}
	judgeSourceURL := skillOptABJudgeSourceURL(challenger.version.ID, comparisonToken)
	// The reasoning blob records the judge's RAW presented-position pick so the
	// position-bias audit (`skillopt judge agreement`, #344) can measure
	// |P(pick=a) - 0.5| — the stored Winner alone is the unblinded role and
	// cannot recover the position.
	if err := upsertSkillOptABRankedEvent(ctx, store, runID, skillOptABItemID, winnerLabel, loserLabel, skillOptABJudgeReviewer, skillOptABJudgeSource, judgeSourceURL, skillOptABJudgePickPositionJSON(pick)); err != nil {
		log.Printf("skillopt ab judge: record judge pick: %v", err)
		return
	}

	// Evidence-only, judge-tagged, weighted-below-human, NOT calibrated against human
	// gold in this slice (MEASURE-THE-JUDGE, #344/#345). The judge NEVER increments
	// the promotion Beta-Bernoulli bandit — promotion stays manual + human-driven.
	fmt.Fprintf(stdout, "Judge (%s, cross-family): %s (Option %s) — recorded as %s (evidence-only, does not move the promotion bandit)\n",
		reviewer.Runtime, winnerLabel, strings.ToUpper(pick), skillOptABJudgeSource)
}

// skillOptABJuror is one surviving jury member's parsed verdict: its resolved
// family, the RAW presented position it picked (a|b — persisted for the #344
// position-bias audit), the role it picked (champion|challenger), and whether
// that pick means the challenger won (the boolean the pure aggregator votes on).
type skillOptABJuror struct {
	family         string
	pick           string
	winnerLabel    string
	challengerWins bool
}

// runSkillOptABJudgeJury runs the off-by-default cross-family judge JURY (#349)
// best-effort: it delivers the SAME blind shuffled Option A/B to EACH distinct-family
// juror (SERIALIZED — one Deliver at a time so per-runtime session locks never
// overlap), DROPS any juror that errors or returns an unparseable pick (fail-safe —
// the jury proceeds with the rest), aggregates the survivors' picks via the PURE
// skillopt.EvaluateJury (majority vote + disagreement flag), and records the
// aggregated verdict (under the canonical skillopt-ab-judge tag) PLUS one per-juror
// detail row (under the distinct skillopt-ab-juror source). It NEVER touches the
// promotion bandit and NEVER fails the command (every error path logs/prints and
// returns, leaving the human path intact). Candidate origin is blinded to every
// juror exactly as the single judge blinds it. When NO juror survives it writes no
// row (same as the single judge's unparseable drop).
func runSkillOptABJudgeJury(ctx context.Context, store *db.Store, paths config.Paths, runID, comparisonToken, templateID string, champion, challenger skillOptABVariant, optionA, optionB skillOptABDelivery, options skillOptABOptions, jury []workflow.CrossFamilyReviewer, stdout io.Writer) {
	prompt := buildSkillOptABJudgePrompt(options.prompt, optionA.answer, optionB.answer)

	var jurors []skillOptABJuror
	for _, reviewer := range jury {
		judgeAgent := skillOptABJudgeAgent(reviewer)
		raw, err := skillOptABJudgeDeliver(ctx, judgeAgent, prompt)
		if err != nil {
			// Fail-safe: a juror that errors is DROPPED; the jury proceeds with the rest.
			log.Printf("skillopt ab jury: juror %s deliver: %v", reviewer.Runtime, err)
			continue
		}
		pick, parsed := parseSkillOptABJudgePick(raw)
		if !parsed {
			log.Printf("skillopt ab jury: juror %s returned no usable pick; dropping", reviewer.Runtime)
			continue
		}
		// Map the juror's a/b pick back to the role via the SAME shuffle mapping the
		// human/single-judge use (the juror saw the SAME blind Option A/B).
		pickedDelivery := optionA
		if pick == "b" {
			pickedDelivery = optionB
		}
		jurors = append(jurors, skillOptABJuror{
			family:         reviewer.Runtime,
			pick:           pick,
			winnerLabel:    pickedDelivery.label,
			challengerWins: pickedDelivery.label == skillOptABChallengerLabel,
		})
	}

	if len(jurors) == 0 {
		fmt.Fprintln(stdout, "skillopt ab: no jury member returned a usable pick; skipping jury rows")
		return
	}

	// Pure aggregation: majority vote on "challenger wins" + disagreement flag. The
	// pairwise A/B path carries no rubric dimensions, so the median/veto outputs are
	// inert here; the same aggregator serves a future promotion-boundary rubric jury.
	verdicts := make([]skillopt.JuryJudgeVerdict, 0, len(jurors))
	for _, j := range jurors {
		verdicts = append(verdicts, skillopt.JuryJudgeVerdict{Decision: j.challengerWins})
	}
	decision := skillopt.EvaluateJury(skillOptABJuryConfig(paths, options), verdicts)

	// Ensure the shared run/item/options exist (idempotent), then stamp the jury
	// aggregation onto the eval_review_item metadata (existing MetadataJSON — no new
	// contract field) so the disagreement flag + tallies ride existing eval metadata.
	if err := ensureSkillOptABRunRows(ctx, store, paths, runID, skillOptABItemID, templateID, champion, challenger, optionAToDelivery(optionA, optionB, champion), optionAToDelivery(optionA, optionB, challenger), options.prompt, skillOptABSource, skillOptABFeedbackSource); err != nil {
		log.Printf("skillopt ab jury: ensure run rows: %v", err)
		return
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:        runID,
		ItemID:       skillOptABJuryItemID,
		Title:        "jury aggregation: " + options.prompt,
		MetadataJSON: skillOptABJuryMetadata(decision, jurors),
	}); err != nil {
		log.Printf("skillopt ab jury: stamp jury metadata: %v", err)
		return
	}

	// Per-juror transparency rows (distinct skillopt-ab-juror source so they coexist
	// with the aggregate without being double-counted as judge votes).
	for _, j := range jurors {
		loserLabel := skillOptABChampionLabel
		if j.winnerLabel == skillOptABChampionLabel {
			loserLabel = skillOptABChallengerLabel
		}
		reviewer := skillOptABJurorReviewerPrefix + j.family
		sourceURL := skillOptABJurorSourceURL(challenger.version.ID, j.family, comparisonToken)
		// Each juror row carries its RAW presented-position pick for the #344
		// position-bias audit, exactly like the single-judge row.
		if err := upsertSkillOptABRankedEvent(ctx, store, runID, skillOptABItemID, j.winnerLabel, loserLabel, reviewer, skillOptABJurorSource, sourceURL, skillOptABJudgePickPositionJSON(j.pick)); err != nil {
			log.Printf("skillopt ab jury: record juror %s row: %v", j.family, err)
			return
		}
	}

	// Aggregated verdict under the CANONICAL judge tag (a jury upgrades the single
	// judge's evidence in place). Majority "challenger wins" => challenger is the
	// winner; a tie/veto resolves to champion (fail-safe baseline).
	aggWinner := skillOptABChampionLabel
	aggLoser := skillOptABChallengerLabel
	if decision.Decision {
		aggWinner, aggLoser = skillOptABChallengerLabel, skillOptABChampionLabel
	}
	// The jury AGGREGATE verdict is a majority over jurors, not one positional
	// pick, so it carries no position blob (reasoning "") — only the per-juror
	// rows above are position-auditable.
	if err := upsertSkillOptABRankedEvent(ctx, store, runID, skillOptABItemID, aggWinner, aggLoser, skillOptABJudgeReviewer, skillOptABJudgeSource, skillOptABJudgeSourceURL(challenger.version.ID, comparisonToken), ""); err != nil {
		log.Printf("skillopt ab jury: record aggregate verdict: %v", err)
		return
	}

	fmt.Fprintf(stdout, "Jury (%d cross-family judges): %s wins %d:%d — recorded as %s (evidence-only, does not move the promotion bandit)\n",
		len(jurors), aggWinner, decision.Approve, decision.Reject, skillOptABJudgeSource)
	if decision.Disagreement {
		// Route to a human (and feed #345): a split jury is the load-this-to-a-human
		// signal the issue mandates.
		fmt.Fprintf(stdout, "Jury DISAGREEMENT (%s): routing to human review (recorded for judge<->human calibration, #345)\n", decision.DisagreementReason)
	}
}

// skillOptABJuryMetadata serializes the jury aggregation onto the eval_review_item
// metadata (#349) — existing MetadataJSON, NO new contract field — so the
// disagreement flag, the vote tally, and the per-juror picks ride the existing eval
// metadata the export/optimizer (and #345 capture) already read.
func skillOptABJuryMetadata(decision skillopt.JuryDecision, jurors []skillOptABJuror) string {
	picks := make([]map[string]any, 0, len(jurors))
	for _, j := range jurors {
		picks = append(picks, map[string]any{"family": j.family, "winner": j.winnerLabel})
	}
	meta := map[string]any{
		"jury":              true,
		"jury_size":         len(jurors),
		"jury_approve":      decision.Approve,
		"jury_reject":       decision.Reject,
		"jury_disagreement": decision.Disagreement,
		"jury_jurors":       picks,
	}
	if decision.Disagreement {
		meta["jury_disagreement_reason"] = decision.DisagreementReason
	}
	if decision.Vetoed {
		meta["jury_vetoed"] = true
		meta["jury_veto_reason"] = decision.VetoReason
	}
	raw, _ := json.Marshal(meta)
	return string(raw)
}

// skillOptABJurorSourceURL mints a per-juror SourceURL embedding the juror
// family so each juror's detail row has a distinct (run,item,reviewer,source,url)
// conflict key and repeated jury runs each persist without colliding. Like the
// human and judge URLs it carries the invocation's SHARED comparison token so
// every juror verdict joins the same single comparison in the #344 harness.
func skillOptABJurorSourceURL(versionID, family, comparisonToken string) string {
	return fmt.Sprintf("%s%s:juror:%s%s%s", skillOptABRunIDPrefix, versionID, family, skillOptABComparisonMarker, comparisonToken)
}

// optionAToDelivery recovers the champion/challenger delivery from the shuffled
// optionA/optionB pair by matching the role label, so the shared run-row scaffold
// always registers the champion option with the champion answer and the challenger
// option with the challenger answer regardless of the shuffle.
func optionAToDelivery(optionA, optionB skillOptABDelivery, variant skillOptABVariant) skillOptABDelivery {
	if optionA.label == variant.label {
		return optionA
	}
	return optionB
}

// skillOptABJudgeAgent synthesizes the read-only runtime.Agent the judge runs as.
// Its Runtime is the SELECTED CROSS-FAMILY reviewer family (a DIFFERENT family than
// the agent under test), so realSkillOptABDeliver builds the adapter on that family
// and the judgment is never a self-preference judgment of one's own template family.
// A registered reviewer agent supplies its runtime ref/model; an ephemeral judge is
// a synthetic read-only ask worker on the chosen family.
func skillOptABJudgeAgent(reviewer workflow.CrossFamilyReviewer) runtime.Agent {
	return runtime.Agent{
		Name:           "gitmoot-ab-judge-" + reviewer.Runtime,
		Role:           "ask",
		Runtime:        reviewer.Runtime,
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}
}

// buildSkillOptABJudgePrompt assembles the cross-family judge prompt from the SAME
// shuffled Option A/Option B text the human sees (it NEVER learns which is champion
// vs challenger — anti-position/identity bias), asking for a small
// gitmoot_result-style JSON pick. The judge must return ONLY {"pick":"a"} or
// {"pick":"b"}; anything else is dropped fail-safe by parseSkillOptABJudgePick.
func buildSkillOptABJudgePrompt(userPrompt, optionA, optionB string) string {
	var b strings.Builder
	b.WriteString("You are an impartial cross-family judge comparing two answers to the SAME prompt. ")
	b.WriteString("You do NOT know which answer is the established version and which is the candidate — judge ONLY on quality. ")
	b.WriteString("This is a SOFT, advisory signal; it never promotes or blocks anything.\n\n")
	b.WriteString("## Prompt\n")
	b.WriteString(strings.TrimSpace(userPrompt) + "\n\n")
	b.WriteString("## Option A\n")
	b.WriteString(strings.TrimSpace(optionA) + "\n\n")
	b.WriteString("## Option B\n")
	b.WriteString(strings.TrimSpace(optionB) + "\n\n")
	b.WriteString("Decide which option is the better answer. Return ONLY a JSON object of the form ")
	b.WriteString(`{"pick":"a"} or {"pick":"b"} (no ties, no prose). Do NOT modify any files (read-only).`)
	b.WriteString("\n")
	return b.String()
}

// parseSkillOptABJudgePick extracts the judge's a/b pick from its raw output using
// the existing brace-balanced jsonCandidates scan (reused from the cross-family
// review parser), tolerating a result wrapped in surrounding prose. It accepts a
// top-level {"pick":...} or a gitmoot_result-nested pick. ok=false on
// empty/unparseable/ambiguous output (the fail-safe drop): a non-a/non-b value, no
// JSON candidate, or no pick field at all all yield ("", false), so a garbled judge
// never fabricates a preference.
func parseSkillOptABJudgePick(raw string) (string, bool) {
	for _, candidate := range jsonCandidates(raw) {
		var envelope struct {
			Pick          string `json:"pick"`
			GitmootResult struct {
				Pick     string `json:"pick"`
				Metadata struct {
					Pick string `json:"pick"`
				} `json:"metadata"`
			} `json:"gitmoot_result"`
		}
		if err := json.Unmarshal([]byte(candidate), &envelope); err != nil {
			continue
		}
		pick := firstNonEmpty(
			strings.ToLower(strings.TrimSpace(envelope.Pick)),
			strings.ToLower(strings.TrimSpace(envelope.GitmootResult.Pick)),
			strings.ToLower(strings.TrimSpace(envelope.GitmootResult.Metadata.Pick)),
		)
		if pick == "a" || pick == "b" {
			return pick, true
		}
	}
	return "", false
}

// skillOptABJudgeSourceURL mints the judge's SourceURL, mirroring
// skillOptABPickSourceURL but with a distinct :judge segment so the judge row's
// (run_id,item_id,reviewer,source,source_url) conflict key never collides with a
// human pick's and repeated judge runs each persist. It embeds the SAME
// per-invocation comparison token as the human pick it was made alongside, which
// is the join key the #344 agreement harness pairs the two verdicts on.
func skillOptABJudgeSourceURL(versionID, comparisonToken string) string {
	return fmt.Sprintf("%s%s:judge%s%s", skillOptABRunIDPrefix, versionID, skillOptABComparisonMarker, comparisonToken)
}
