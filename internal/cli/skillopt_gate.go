package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// gateReplayEnvRunner is the minimal subprocess capability the deterministic replay
// driver needs: run a `sh -c` command with extra env. subprocess.ExecRunner
// satisfies it; a test injects a deterministic fake so the E2E never shells out.
type gateReplayEnvRunner interface {
	RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error)
}

// gateReplayRunner is swappable so tests drive the gate through the real candidate
// plumbing with a deterministic fake runner (no live subprocess). Production uses
// subprocess.ExecRunner.
var gateReplayRunner gateReplayEnvRunner = subprocess.ExecRunner{}

func runSkillOptGate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptGateUsage(stdout)
		return 0
	}
	switch args[0] {
	case "run":
		return runSkillOptGateRun(args[1:], stdout, stderr)
	case "history":
		return runSkillOptGateHistory(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt gate command %q\n\n", args[0])
		printSkillOptGateUsage(stderr)
		return 2
	}
}

func printSkillOptGateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt gate run --candidate <version-id> [--corpus <path>] [--replay-command <cmd>] [--config path] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt gate history --candidate <version-id> [--home path] [--json]")
}

// skillOptGateRunReport is the machine-readable result of one `gate run` (#627).
type skillOptGateRunReport struct {
	CandidateVersionID string                   `json:"candidate_version_id"`
	ChampionVersionID  string                   `json:"champion_version_id"`
	TemplateID         string                   `json:"template_id"`
	CorpusPath         string                   `json:"corpus_path"`
	CorpusVersion      int                      `json:"corpus_version"`
	CorpusItems        int                      `json:"corpus_items"`
	Accepted           bool                     `json:"accepted"`
	Attempts           int                      `json:"attempts"`
	ChampionMean       float64                  `json:"champion_mean"`
	CandidateMean      float64                  `json:"candidate_mean"`
	Reason             string                   `json:"reason"`
	Deltas             []skillopt.GateItemDelta `json:"deltas"`
	GateRunID          string                   `json:"gate_run_id"`
}

func runSkillOptGateRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt gate run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	candidateID := fs.String("candidate", "", "candidate template version id to gate (required)")
	corpusPath := fs.String("corpus", "", "path to the fixed replay corpus file (overrides [skillopt].gate_corpus)")
	replayCommand := fs.String("replay-command", "", "deterministic replay driver command (overrides the corpus/config default)")
	configPath := fs.String("config", "", "config file to load [skillopt] defaults from")
	jsonOut := fs.Bool("json", false, "emit the gate report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(*candidateID) == "" {
		fmt.Fprintln(stderr, "skillopt gate run: --candidate is required")
		printSkillOptGateUsage(stderr)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt gate run does not accept positional arguments")
		return 2
	}

	policy := skillOptGatePolicy(*home, *configPath)
	resolvedCorpus := firstNonEmpty(strings.TrimSpace(*corpusPath), policy.GateCorpusPath)
	if resolvedCorpus == "" {
		fmt.Fprintln(stderr, "skillopt gate run: no corpus (pass --corpus or set [skillopt].gate_corpus)")
		return 2
	}
	corpus, err := skillopt.LoadGateCorpus(resolvedCorpus)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt gate run: %v\n", err)
		return 1
	}
	replayCmd := firstNonEmpty(strings.TrimSpace(*replayCommand), strings.TrimSpace(corpus.ReplayCommand), policy.GateReplayCommand)
	if replayCmd == "" {
		fmt.Fprintln(stderr, "skillopt gate run: no replay command (pass --replay-command, set the corpus replay_command, or [skillopt].gate_replay_command)")
		return 2
	}

	var report skillOptGateRunReport
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		candidate, err := store.GetAgentTemplateVersionByID(ctx, strings.TrimSpace(*candidateID))
		if err != nil {
			return fmt.Errorf("resolve candidate %q: %w", strings.TrimSpace(*candidateID), err)
		}
		champion, hasChampion, err := store.GetCurrentAgentTemplateVersion(ctx, candidate.TemplateID)
		if err != nil {
			return fmt.Errorf("resolve champion for template %q: %w", candidate.TemplateID, err)
		}
		if !hasChampion {
			return fmt.Errorf("template %q has no current champion version to compare against; the gate needs a baseline", candidate.TemplateID)
		}

		replay := gateReplayDriver(corpus, replayCmd)
		// Score the champion ONCE on the same fixed corpus (it does not change across
		// gate attempts).
		championScores, _, err := replay(ctx, champion.Content)
		if err != nil {
			return fmt.Errorf("replay champion: %w", err)
		}
		// The CLI `gate run` is a SINGLE evaluation (attempt 1, no optimizer wired):
		// the one-retry-with-optimizer protocol is the train-workflow integration. A
		// nil optimize func makes RunGateProtocol take exactly attempt 1 and reject on
		// failure — the same code path, so the CLI and the workflow share the gate.
		outcome, err := skillopt.RunGateProtocol(ctx, championScores, candidate.Content, replay, nil)
		if err != nil {
			return err
		}

		report = buildGateRunReport(candidate, champion, corpus, resolvedCorpus, outcome)
		gateRun := gateRunRecord(candidate, champion, corpus, resolvedCorpus, outcome)
		report.GateRunID = gateRun.ID
		if err := store.InsertSkillOptGateRun(ctx, gateRun); err != nil {
			return fmt.Errorf("persist gate run: %w", err)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt gate run: %v\n", err)
		return 1
	}

	if *jsonOut {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			fmt.Fprintf(stderr, "skillopt gate run: encode json: %v\n", err)
			return 1
		}
	} else {
		printGateRunReport(stdout, report)
	}
	// A rejected gate is NOT a CLI error (the run completed and was persisted), but it
	// exits non-zero so a script can branch on the verdict.
	if !report.Accepted {
		return 1
	}
	return 0
}

// gateReplayDriver builds the deterministic replay function: for each corpus item it
// writes the template content to a temp file and runs the replay command via
// `sh -c` with the item's prompt/expected/id in the environment, parsing the
// command's stdout into a GateReplayResult scored by the EXISTING deterministic
// metrics (#627). There is NO live LLM in the gate — the command is the
// deterministic map, so two runs over the same corpus + template yield identical
// scores.
func gateReplayDriver(corpus skillopt.GateCorpus, replayCommand string) skillopt.GateReplayFunc {
	return func(ctx context.Context, templateContent string) ([]skillopt.GateItemScore, skillopt.GateReplayLog, error) {
		tmpFile, err := os.CreateTemp("", "gitmoot-gate-template-*.md")
		if err != nil {
			return nil, skillopt.GateReplayLog{}, fmt.Errorf("stage template: %w", err)
		}
		templatePath := tmpFile.Name()
		defer os.Remove(templatePath)
		if _, err := tmpFile.WriteString(templateContent); err != nil {
			tmpFile.Close()
			return nil, skillopt.GateReplayLog{}, fmt.Errorf("write template: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			return nil, skillopt.GateReplayLog{}, fmt.Errorf("close template: %w", err)
		}

		scores := make([]skillopt.GateItemScore, 0, len(corpus.Items))
		results := make([]skillopt.GateReplayResult, 0, len(corpus.Items))
		for _, item := range corpus.Items {
			env := []string{
				"GITMOOT_GATE_TEMPLATE_FILE=" + templatePath,
				"GITMOOT_GATE_ITEM_ID=" + item.ID,
				"GITMOOT_GATE_PROMPT=" + item.Prompt,
				"GITMOOT_GATE_EXPECTED=" + item.Expected,
			}
			res, runErr := gateReplayRunner.RunEnv(ctx, filepath.Dir(templatePath), env, "sh", "-c", replayCommand)
			if runErr != nil {
				detail := strings.TrimSpace(res.Stderr)
				if detail == "" {
					detail = strings.TrimSpace(res.Stdout)
				}
				if detail != "" {
					return nil, skillopt.GateReplayLog{}, fmt.Errorf("replay corpus item %q: %s: %w", item.ID, detail, runErr)
				}
				return nil, skillopt.GateReplayLog{}, fmt.Errorf("replay corpus item %q: %w", item.ID, runErr)
			}
			result, parseErr := parseGateReplayResult(item.ID, res.Stdout)
			if parseErr != nil {
				return nil, skillopt.GateReplayLog{}, fmt.Errorf("replay corpus item %q: %w", item.ID, parseErr)
			}
			results = append(results, result)
			score, has := result.Score()
			scores = append(scores, skillopt.GateItemScore{ItemID: item.ID, Score: score, HasScore: has})
		}
		return scores, skillopt.GateReplayLog{Results: results}, nil
	}
}

// parseGateReplayResult decodes the replay command's stdout into a per-item result,
// stamping the corpus item id so a driver that omits it still aligns. A blank stdout
// is a hard error (the command produced no verdict).
func parseGateReplayResult(itemID string, stdout string) (skillopt.GateReplayResult, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return skillopt.GateReplayResult{}, errors.New("replay command produced no output")
	}
	var result skillopt.GateReplayResult
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return skillopt.GateReplayResult{}, fmt.Errorf("decode replay result: %w", err)
	}
	if strings.TrimSpace(result.ItemID) == "" {
		result.ItemID = itemID
	}
	return result, nil
}

func buildGateRunReport(candidate, champion db.AgentTemplateVersion, corpus skillopt.GateCorpus, corpusPath string, outcome skillopt.GateProtocolOutcome) skillOptGateRunReport {
	report := skillOptGateRunReport{
		CandidateVersionID: candidate.ID,
		ChampionVersionID:  champion.ID,
		TemplateID:         candidate.TemplateID,
		CorpusPath:         corpusPath,
		CorpusVersion:      corpus.Version,
		CorpusItems:        len(corpus.Items),
		Accepted:           outcome.Accepted,
		Attempts:           len(outcome.Attempts),
		Reason:             outcome.Reason,
	}
	if len(outcome.Attempts) > 0 {
		last := outcome.Attempts[len(outcome.Attempts)-1]
		report.ChampionMean = last.Verdict.ChampionMean
		report.CandidateMean = last.Verdict.CandidateMean
		report.Deltas = last.Verdict.Deltas
	}
	return report
}

func gateRunRecord(candidate, champion db.AgentTemplateVersion, corpus skillopt.GateCorpus, corpusPath string, outcome skillopt.GateProtocolOutcome) db.SkillOptGateRun {
	var championMean, candidateMean float64
	var deltasJSON string
	if len(outcome.Attempts) > 0 {
		last := outcome.Attempts[len(outcome.Attempts)-1]
		championMean = last.Verdict.ChampionMean
		candidateMean = last.Verdict.CandidateMean
		if raw, err := json.Marshal(last.Verdict.Deltas); err == nil {
			deltasJSON = string(raw)
		}
	}
	return db.SkillOptGateRun{
		ID:                 newGateRunID(),
		TemplateID:         candidate.TemplateID,
		CandidateVersionID: candidate.ID,
		ChampionVersionID:  champion.ID,
		CorpusPath:         corpusPath,
		CorpusVersion:      corpus.Version,
		CorpusItems:        len(corpus.Items),
		Attempts:           len(outcome.Attempts),
		Accepted:           outcome.Accepted,
		ChampionMean:       championMean,
		CandidateMean:      candidateMean,
		Reason:             outcome.Reason,
		DeltasJSON:         deltasJSON,
	}
}

func printGateRunReport(w io.Writer, report skillOptGateRunReport) {
	verdict := "REJECTED"
	if report.Accepted {
		verdict = "ACCEPTED"
	}
	fmt.Fprintf(w, "gate %s (attempts: %d)\n", verdict, report.Attempts)
	fmt.Fprintf(w, "candidate: %s   champion: %s\n", report.CandidateVersionID, report.ChampionVersionID)
	fmt.Fprintf(w, "corpus: %s (v%d, %d items)\n", report.CorpusPath, report.CorpusVersion, report.CorpusItems)
	fmt.Fprintf(w, "champion mean: %.4f   candidate mean: %.4f\n", report.ChampionMean, report.CandidateMean)
	fmt.Fprintf(w, "reason: %s\n", report.Reason)
	if len(report.Deltas) > 0 {
		fmt.Fprintf(w, "%-24s %-10s %-10s %-10s\n", "ITEM", "CHAMPION", "CANDIDATE", "DELTA")
		for _, d := range report.Deltas {
			marker := ""
			if d.Regressed {
				marker = "  (regressed)"
			}
			fmt.Fprintf(w, "%-24s %-10.4f %-10.4f %+-10.4f%s\n", d.ItemID, d.ChampionScore, d.CandidateScore, d.Delta, marker)
		}
	}
}

func runSkillOptGateHistory(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt gate history", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	candidateID := fs.String("candidate", "", "candidate template version id (required)")
	jsonOut := fs.Bool("json", false, "emit the history as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(*candidateID) == "" {
		fmt.Fprintln(stderr, "skillopt gate history: --candidate is required")
		return 2
	}
	var runs []db.SkillOptGateRun
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		runs, err = store.ListSkillOptGateRuns(context.Background(), strings.TrimSpace(*candidateID))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt gate history: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := json.NewEncoder(stdout).Encode(runs); err != nil {
			fmt.Fprintf(stderr, "skillopt gate history: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	if len(runs) == 0 {
		writeLine(stdout, "no gate runs for %s", strings.TrimSpace(*candidateID))
		return 0
	}
	fmt.Fprintf(stdout, "%-20s %-9s %-8s %-9s %-9s %s\n", "GATE-RUN", "VERDICT", "ATTEMPTS", "CHAMP", "CAND", "CREATED")
	for _, run := range runs {
		verdict := "reject"
		if run.Accepted {
			verdict = "accept"
		}
		fmt.Fprintf(stdout, "%-20s %-9s %-8d %-9.4f %-9.4f %s\n", run.ID, verdict, run.Attempts, run.ChampionMean, run.CandidateMean, run.CreatedAt)
	}
	return 0
}

// skillOptGatePolicy loads the [skillopt] policy for gate defaults, preferring an
// explicit --config path and falling back to the home's config. It is fail-soft: a
// load error yields the default policy (empty gate defaults), so the CLI still runs
// when the operator passes every value explicitly.
func skillOptGatePolicy(home, configPath string) config.SkillOptPolicy {
	if trimmed := strings.TrimSpace(configPath); trimmed != "" {
		if policy, err := config.LoadSkillOptPolicy(config.Paths{ConfigFile: trimmed}); err == nil {
			return policy
		}
		return config.DefaultSkillOptPolicy()
	}
	if policy, err := loadSkillOptPolicy(home); err == nil {
		return policy
	}
	return config.DefaultSkillOptPolicy()
}

// newGateRunID mints a fresh, sortable-ish gate-run id (RFC3339Nano timestamp plus
// a random suffix so concurrent runs never collide). It is a var so tests can make
// it deterministic.
var newGateRunID = func() string {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "gate-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return fmt.Sprintf("gate-%s-%s", time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(suffix))
}
