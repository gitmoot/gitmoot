package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/skillopt"
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

// realSkillOptABDeliver resolves the agent's runtime adapter and delivers a
// single one-shot prompt, returning the answer text. Each call is independent
// and synchronous; runSkillOptAB invokes it twice in series so the second call
// only starts after the first returns — never concurrently — which keeps any
// per-runtime session usage cleanly serialized.
func realSkillOptABDeliver(ctx context.Context, agent runtime.Agent, prompt string) (string, error) {
	adapter, err := runtimeStartAdapterFor(newRuntimeFactory(), agent.Runtime, agent.RepoScope)
	if err != nil {
		return "", err
	}
	result, err := adapter.Deliver(ctx, agent, runtime.Job{AgentName: agent.Name, Action: "ask", Prompt: prompt, Model: agent.Model})
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
	// Separate the leading positionals (agent, prompt) from flags. flag.Parse stops
	// at the first non-flag, so collect positionals manually to allow them anywhere.
	positionals := []string{}
	rest := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			// flags that take a value consume the next token unless --flag=value.
			if !strings.Contains(arg, "=") && (arg == "--home" || arg == "--challenger" || arg == "--pick" || arg == "--seed") && i+1 < len(args) {
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
	return skillOptABOptions{
		home:       strings.TrimSpace(*home),
		agent:      strings.TrimSpace(positionals[0]),
		prompt:     strings.TrimSpace(positionals[1]),
		challenger: strings.TrimSpace(*challenger),
		pick:       pickValue,
		seed:       *seed,
		seedSet:    seedSet,
	}, true
}

func printSkillOptABUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt ab <agent> \"<prompt>\" [--challenger <versionId>] [--pick a|b] [--seed N] [--home path]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Champion-challenger A/B (#473 Mode B): serves the prompt through the agent's")
	fmt.Fprintln(w, "current promoted template version (champion) AND a pending candidate version")
	fmt.Fprintln(w, "(challenger), shuffles the two answers as Option A/B, records the human pick as a")
	fmt.Fprintln(w, "pairwise RankedFeedbackEvent (source=skillopt-ab), updates the Beta-Bernoulli")
	fmt.Fprintln(w, "bandit, and prints P(challenger>champion). Manual only; no pending candidate is a")
	fmt.Fprintln(w, "clean no-op.")
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
	rng := rand.New(rand.NewSource(options.seed))
	swap := rng.Intn(2) == 1
	optionA, optionB := championDelivery, challengerDelivery
	if swap {
		optionA, optionB = challengerDelivery, championDelivery
	}

	fmt.Fprintf(stdout, "Prompt: %s\n\n", options.prompt)
	fmt.Fprintf(stdout, "Option A:\n%s\n\n", optionA.answer)
	fmt.Fprintf(stdout, "Option B:\n%s\n\n", optionB.answer)

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

	runID := skillOptABRunIDPrefix + challenger.version.ID
	if err := recordSkillOptABPick(ctx, store, paths, runID, templateID, champion, challenger, championDelivery, challengerDelivery, winnerLabel, loserLabel, options.prompt); err != nil {
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
func recordSkillOptABPick(ctx context.Context, store *db.Store, paths config.Paths, runID, templateID string, champion, challenger skillOptABVariant, championDelivery, challengerDelivery skillOptABDelivery, winnerLabel, loserLabel, prompt string) error {
	blobStore := artifact.NewStore(paths.ArtifactBlobs)

	metadataJSON, err := json.Marshal(map[string]any{
		"feedback_source": skillOptABFeedbackSource,
		"source":          skillOptABSource,
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
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{RunID: runID, ItemID: skillOptABItemID, Title: prompt}); err != nil {
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
		evalArtifact, err := prepareReviewItemContentArtifact(blobStore, runID, skillOptABItemID, opt.label, []byte(answerOrPlaceholder(opt.delivery.answer)), "text/plain", "text")
		if err != nil {
			return err
		}
		if err := store.UpsertEvalArtifact(ctx, evalArtifact); err != nil {
			return err
		}
		if err := store.UpsertEvalReviewOption(ctx, db.EvalReviewOption{
			RunID:      runID,
			ItemID:     skillOptABItemID,
			Label:      opt.label,
			ArtifactID: evalArtifact.ID,
			Role:       opt.label,
		}); err != nil {
			return err
		}
	}

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
		ItemID:        skillOptABItemID,
		RankingJSON:   string(ranking),
		TieGroupsJSON: string(tieGroups),
		Winner:        winnerLabel,
		Reviewer:      "human",
		Source:        skillOptABSource,
	})
}

// answerOrPlaceholder guarantees a non-empty artifact body (the blob path rejects
// empty content), substituting a marker when a variant returned no text.
func answerOrPlaceholder(answer string) string {
	if strings.TrimSpace(answer) == "" {
		return "(no answer)"
	}
	return answer
}
