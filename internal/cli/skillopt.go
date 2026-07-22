package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

var newSkillOptGitHubClient = func() github.Client {
	return github.NewClient("")
}

func runSkillOpt(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "export":
		return runSkillOptExport(args[1:], stdout, stderr)
	case "import":
		return runSkillOptImport(args[1:], stdout, stderr)
	case "review":
		return runSkillOptReview(args[1:], stdout, stderr)
	case "candidate":
		return runSkillOptCandidate(args[1:], stdout, stderr)
	case "feedback":
		return runSkillOptFeedback(args[1:], stdout, stderr)
	case "judge-report":
		return runSkillOptJudgeReport(args[1:], stdout, stderr)
	case "judge":
		return runSkillOptJudge(args[1:], stdout, stderr)
	case "train":
		return runSkillOptTrain(args[1:], stdout, stderr)
	case "ab":
		return runSkillOptAB(args[1:], stdout, stderr)
	case "pairwise":
		return runSkillOptPairwise(args[1:], stdout, stderr)
	case "rubric":
		return runSkillOptRubric(args[1:], stdout, stderr)
	case "gate":
		return runSkillOptGate(args[1:], stdout, stderr)
	case "binary":
		return runSkillOptBinary(args[1:], stdout, stderr)
	case "synth":
		return runSkillOptSynth(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func printSkillOptUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt export --run <run-id> [--output package.json]")
	fmt.Fprintln(w, "  gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]")
	fmt.Fprintln(w, "  gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id> [--mode validate|explore|refine|distill] [--options N]")
	fmt.Fprintln(w, "  gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md [--title text]")
	fmt.Fprintln(w, "  gitmoot skillopt review item add --run <run-id> --item <item-id> --option a=option-a.md --option b=option-b.md [...] [--title text]")
	fmt.Fprintln(w, "  gitmoot skillopt review status --run <run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate list [--template id]")
	fmt.Fprintln(w, "  gitmoot skillopt candidate show <version-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate promote <version-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate reject <version-id> [--reason text]")
	fmt.Fprintln(w, "  gitmoot skillopt gate run --candidate <version-id> [--corpus path] [--replay-command cmd] [--config path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt gate history --candidate <version-id> [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt binary run --set <file> --run <run-id> --source <file> [--deterministic] [--reviewer runtime] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt binary show --run <run-id> [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt synth --template <id> --repo owner/repo --weak <agent> --strong <agent> [--judge <agent>] [--challenger <agent>] [--max-items N] [--max-rounds-per-item M] [--gap F] [--out dir] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt synth list [--status pending_human_approval|approved|rejected] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt synth approve <item-id> [--home path]")
	fmt.Fprintln(w, "  gitmoot skillopt synth reject <item-id> [--home path]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)")
	fmt.Fprintln(w, "  gitmoot skillopt ab <agent> \"<prompt>\" [--challenger <versionId>] [--pick a|b] [--seed N] [--judge] [--judge-only] [--home path]")
	fmt.Fprintln(w, "  gitmoot skillopt pairwise import <packet-dir> [--packet path] [--secret-map path] [--picks path] [--reviewer name] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt rubric induce --template <id> [--out <dir>] [--holdout 0.2] [--min-events N] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt judge-report [--template id]")
	fmt.Fprintln(w, "  gitmoot skillopt judge agreement [--template <id>] [--home <h>] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt judge promote --template <id> --task-kind <kind> --file <pkg.json> [--home <h>] [--yes] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt train init --name <name> --template <id> --review-repo owner/repo --artifact-kind kind --preview kind (--request text|--request-file path)")
	fmt.Fprintln(w, "  gitmoot skillopt train init templates --json")
	fmt.Fprintln(w, "  gitmoot skillopt train start --config .gitmoot/skillopt/<name>/config.toml [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file path [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train status --session <id>")
	fmt.Fprintln(w, "  gitmoot skillopt train run [--config path | --session <id>] [--plain]")
	fmt.Fprintln(w, "  gitmoot skillopt train continue --session <id> [--backend codex] [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--model name] [--optimizer-model name] [--target-model name] [--optimizer-backend name] [--target-backend name] [--evaluator-id id] [--evaluator-model name] [--evaluator-backend name] [--skill-update-mode mode] [--num-epochs N] [--batch-size N] [--optimizer-views N] [--retry-optimizer-views auto|inherit|N] [--gate hard|soft|mixed] [--out-root path] [--timeout duration] [--dry-run] [--rerun-optimizer] [--export-only] [--promote version|--reject version --reason text] [--start-next]")
	fmt.Fprintln(w, "  gitmoot skillopt train recover --session <id> [--out-root path] [--generation [--abort | --advance-state]] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt train stop --session <id> --reason <text>")
}

func runSkillOptTrain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptTrainUsage(stdout)
		return 0
	}
	switch args[0] {
	case "init":
		return runSkillOptTrainInit(args[1:], stdout, stderr)
	case "start":
		return runSkillOptTrainStart(args[1:], stdout, stderr)
	case "status":
		return runSkillOptTrainStatus(args[1:], stdout, stderr)
	case "run":
		return runSkillOptTrainRun(args[1:], stdout, stderr)
	case "continue":
		return runSkillOptTrainContinue(args[1:], stdout, stderr)
	case "recover":
		return runSkillOptTrainRecover(args[1:], stdout, stderr)
	case "stop":
		return runSkillOptTrainStop(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt train command %q\n\n", args[0])
		printSkillOptTrainUsage(stderr)
		return 2
	}
}

func printSkillOptTrainUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt train init --name <name> --template <id> --review-repo owner/repo --artifact-kind kind --preview kind (--request text|--request-file path)")
	fmt.Fprintln(w, "  gitmoot skillopt train init templates --json")
	fmt.Fprintln(w, "  gitmoot skillopt train start --config .gitmoot/skillopt/<name>/config.toml [--session <id>] [--items-file path] [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file path [--session <id>] [--workspace-repo owner/repo] [--preview-repo owner/repo] [--preview-mode none|optional|required] [--preview-renderer none|vue-vite] [--preview-publisher none|github-pages] [--preview-route-template template] [--request-file path] [--task-kind kind] [--mode explore|refine|distill|validate] [--exploration-level high|medium|low] [--options N] [--min-items N] [--preferred-gate hard|soft|hard_then_soft] [--dry-run] [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train status --session <id>")
	fmt.Fprintln(w, "  gitmoot skillopt train run [--config path | --session <id>] [--plain]")
	fmt.Fprintln(w, "  gitmoot skillopt train continue --session <id> [--backend codex] [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--model name] [--optimizer-model name] [--target-model name] [--optimizer-backend name] [--target-backend name] [--evaluator-id id] [--evaluator-model name] [--evaluator-backend name] [--skill-update-mode mode] [--num-epochs N] [--batch-size N] [--optimizer-views N] [--retry-optimizer-views auto|inherit|N] [--gate hard|soft|mixed] [--out-root path] [--timeout duration] [--dry-run] [--rerun-optimizer] [--export-only] [--promote version|--reject version --reason text] [--start-next]")
	fmt.Fprintln(w, "  gitmoot skillopt train recover --session <id> [--out-root path] [--generation [--abort | --advance-state]] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt train stop --session <id> --reason <text>")
}

type skillOptTrainContinueRequest struct {
	Home              string
	SessionID         string
	GeneratorAgent    string
	GeneratorType     string
	GenerationLockTTL time.Duration
	Optimizer         skillOptTrainOptimizerRequest
	PromoteCandidate  string
	RejectCandidate   string
	DecisionReason    string
	StartNext         bool
	// Progress receives human-facing notices emitted while a continue step runs,
	// such as announcing a long-lived optimizer launch. It is nil for automated
	// callers (the review watcher) that have no attached terminal.
	Progress io.Writer
	// GenerationLockExtend, when set, is called after each generated option to
	// push the generation lock TTL forward so long runs do not outlive it.
	GenerationLockExtend func() error
}

type skillOptTrainOptimizerRequest struct {
	SkillOptBin                  string
	Backend                      string
	Model                        string
	OptimizerModel               string
	TargetModel                  string
	OptimizerBackend             string
	TargetBackend                string
	EvaluatorID                  string
	EvaluatorModel               string
	EvaluatorBackend             string
	SkillUpdateMode              string
	NumEpochs                    int
	BatchSize                    int
	OptimizerViews               int
	OptimizerViewsSet            bool
	RetryOptimizerViews          string
	RetryOptimizerViewsSet       bool
	NoopRetryBudget              int
	NoopRetryBudgetSet           bool
	GateRejectRetryBudget        int
	GateRejectRetryBudgetSet     bool
	WrongArtifactRetryBudget     int
	WrongArtifactRetryBudgetSet  bool
	TargetArtifactRetryBudget    int
	TargetArtifactRetryBudgetSet bool
	HardFailureRetryBudget       int
	HardFailureRetryBudgetSet    bool
	FeedbackDirectMode           string
	FinalEval                    bool
	FinalEvalSet                 bool
	Gate                         string
	OutRoot                      string
	Timeout                      string
	DryRun                       bool
	RerunOptimizer               bool
	ExportOnly                   bool
	OptimizerLockState           string
}

func writeSkillOptMarkdownFence(builder *strings.Builder, content string) {
	fence := "```"
	for strings.Contains(content, fence) {
		fence += "`"
	}
	builder.WriteString(fence)
	builder.WriteString("text\n")
	builder.WriteString(strings.TrimRight(content, "\n"))
	builder.WriteString("\n")
	builder.WriteString(fence)
	builder.WriteString("\n")
}

func runSkillOptTrainStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	reason := fs.String("reason", "", "reason for abandoning the train session")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train stop does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "skillopt train stop requires --session and --reason")
		return 2
	}
	var stopped db.SkillOptTrainIteration
	if err := withStore(*home, func(store *db.Store) error {
		iteration, err := stopSkillOptTrainSession(context.Background(), store, strings.TrimSpace(*sessionID), strings.TrimSpace(*reason))
		if err != nil {
			return err
		}
		stopped = iteration
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train stop: %v\n", err)
		return 1
	}
	writeLine(stdout, "stopped train session %s", strings.TrimSpace(*sessionID))
	writeLine(stdout, "iteration: %s", stopped.ID)
	writeLine(stdout, "reason: %s", strings.TrimSpace(*reason))
	return 0
}

// stopSkillOptTrainSession abandons a train session with a reason — the shared
// body behind `train stop` and the dashboard's stop action.
func stopSkillOptTrainSession(ctx context.Context, store *db.Store, sessionID string, reason string) (db.SkillOptTrainIteration, error) {
	if sessionID == "" || strings.TrimSpace(reason) == "" {
		return db.SkillOptTrainIteration{}, errors.New("a session id and a reason are required")
	}
	session, err := store.GetSkillOptTrainSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.SkillOptTrainIteration{}, fmt.Errorf("train session %s not found", sessionID)
		}
		return db.SkillOptTrainIteration{}, err
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(ctx, session.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.SkillOptTrainIteration{}, fmt.Errorf("train session %s has no iteration to stop", session.ID)
		}
		return db.SkillOptTrainIteration{}, err
	}
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateRunAbandoned); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	session.State = skillopt.TrainStateRunAbandoned
	session.MetadataJSON = skillOptTrainDecisionMetadata(session.MetadataJSON, reason)
	iteration.State = skillopt.TrainStateRunAbandoned
	iteration.DecisionReason = strings.TrimSpace(reason)
	iteration.MetadataJSON = skillOptTrainDecisionMetadata(iteration.MetadataJSON, reason)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	return iteration, nil
}

// deleteSkillOptTrainSession removes a train session and its history, and
// returns the GitHub repos gitmoot recorded as created for it. The records
// deliberately survive the cascade so a caller can offer their cleanup; any
// future delete surface must list them BEFORE the delete or they orphan.
func deleteSkillOptTrainSession(ctx context.Context, store *db.Store, sessionID string) ([]string, error) {
	records, err := store.ListCreatedReposForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if err := store.DeleteSkillOptTrainSession(ctx, sessionID); err != nil {
		return nil, err
	}
	// Remove the per-option target agents this session scaffolded so they don't
	// accumulate on the Agents page / agent list (best-effort — internal plumbing).
	removeSkillOptTrainTargetAgents(ctx, store, sessionID)
	repos := make([]string, 0, len(records))
	for _, record := range records {
		repos = append(repos, record.Repo)
	}
	return repos, nil
}

// removeSkillOptTrainTargetAgents deletes the `skillopt-target-<run>-…` agents a
// training session scaffolded (one per generated option). The session id is
// embedded in each name, so it only touches this session's plumbing. Best-effort.
func removeSkillOptTrainTargetAgents(ctx context.Context, store *db.Store, sessionID string) {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return
	}
	marker := skillOptSafeAgentName(strings.TrimSpace(sessionID))
	for _, agent := range agents {
		if strings.HasPrefix(agent.Name, "skillopt-target-") && marker != "" && strings.Contains(agent.Name, marker) {
			_, _ = store.RemoveAgent(ctx, agent.Name)
		}
	}
}

// cleanupCreatedTrainRepo deletes a gitmoot-created GitHub repo and then its
// created_repos record — in that order, so a failed GitHub delete keeps the
// repo on offer for a retry.
func cleanupCreatedTrainRepo(ctx context.Context, store *db.Store, repo string) error {
	parsed, err := daemon.ParseRepository(repo)
	if err != nil {
		return err
	}
	if err := newSkillOptGitHubClient().DeleteRepository(ctx, parsed); err != nil {
		return err
	}
	return store.DeleteCreatedRepoRecord(ctx, repo)
}

func metadataStringSlice(metadata map[string]any, key string) []string {
	value, ok := metadata[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				values = append(values, text)
			}
		}
		return values
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{strings.TrimSpace(typed)}
	default:
		return nil
	}
}

func skillOptTrainOptimizerDefaultsMetadata(request skillOptTrainOptimizerRequest) map[string]any {
	metadata := map[string]any{}
	if value := strings.TrimSpace(request.Backend); value != "" {
		metadata["backend"] = value
	}
	if value := strings.TrimSpace(request.OptimizerBackend); value != "" {
		metadata["optimizer_backend"] = value
	}
	if value := strings.TrimSpace(request.TargetBackend); value != "" {
		metadata["target_backend"] = value
	}
	if value := strings.TrimSpace(request.OptimizerModel); value != "" {
		metadata["optimizer_model"] = value
	}
	if value := strings.TrimSpace(request.TargetModel); value != "" {
		metadata["target_model"] = value
	}
	if value := strings.TrimSpace(request.EvaluatorID); value != "" {
		metadata["evaluator_id"] = value
	}
	if value := strings.TrimSpace(request.EvaluatorBackend); value != "" {
		metadata["evaluator_backend"] = value
	}
	if value := strings.TrimSpace(request.SkillUpdateMode); value != "" {
		metadata["skill_update_mode"] = value
	}
	if request.OptimizerViewsSet {
		metadata["optimizer_views"] = request.OptimizerViews
	}
	if request.RetryOptimizerViewsSet {
		metadata["retry_optimizer_views"] = strings.TrimSpace(request.RetryOptimizerViews)
	}
	if request.NoopRetryBudgetSet {
		metadata["noop_retry_budget"] = request.NoopRetryBudget
	}
	if request.GateRejectRetryBudgetSet {
		metadata["gate_reject_retry_budget"] = request.GateRejectRetryBudget
	}
	if request.WrongArtifactRetryBudgetSet {
		metadata["wrong_artifact_retry_budget"] = request.WrongArtifactRetryBudget
	}
	if request.TargetArtifactRetryBudgetSet {
		metadata["target_artifact_retry_budget"] = request.TargetArtifactRetryBudget
	}
	if request.HardFailureRetryBudgetSet {
		metadata["hard_failure_retry_budget"] = request.HardFailureRetryBudget
	}
	if request.FinalEval {
		metadata["final_eval"] = true
	}
	return metadata
}

func metadataInt(metadata map[string]any, key string) (int, bool) {
	value := metadataFloatPtr(metadata, key)
	if value == nil {
		return 0, false
	}
	return int(*value), true
}

func skillOptMetadataString(metadataJSON string, path ...string) string {
	var current any
	if err := json.Unmarshal([]byte(metadataJSON), &current); err != nil {
		return ""
	}
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	if value, ok := current.(string); ok {
		return value
	}
	return ""
}

func mergeSkillOptTrainMetadata(existing string, key string, value map[string]any) string {
	metadata := decodedSkillOptMetadata(existing)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[key] = value
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return existing
	}
	return string(encoded)
}

func decodedSkillOptMetadata(value string) map[string]any {
	var metadata map[string]any
	if strings.TrimSpace(value) != "" {
		_ = json.Unmarshal([]byte(value), &metadata)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	return metadata
}

func decodedSkillOptMetadataValue(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func metadataSlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			values = append(values, item)
		}
		return values
	case []map[string]string:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			metadata := make(map[string]any, len(item))
			for key, value := range item {
				metadata[key] = value
			}
			values = append(values, metadata)
		}
		return values
	default:
		return nil
	}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func normalizeSkillOptMetadataJSON(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return "", fmt.Errorf("metadata-json: %w", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("metadata-json: %w", err)
	}
	return string(encoded), nil
}

func scoreText(score *float64) string {
	if score == nil {
		return "-"
	}
	return fmt.Sprintf("%.4g", *score)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if before, _, ok := strings.Cut(value, "\n"); ok {
		return strings.TrimSpace(before)
	}
	return value
}

func emptyText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func indentJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return value
	}
	encoded, err := json.MarshalIndent(decoded, "  ", "  ")
	if err != nil {
		return value
	}
	return string(encoded)
}

func withSkillOptStore(home string, fn func(config.Paths, *db.Store) error) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	if err := config.Initialize(paths); err != nil {
		return err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	return fn(paths, store)
}
