package cli

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/feedback"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"gopkg.in/yaml.v3"
)

var newSkillOptGitHubClient = func() github.Client {
	return github.NewClient("")
}

var skillOptTrainOptimizerRunner subprocess.Runner = subprocess.ExecRunner{}

var skillOptTrainPreviewRunner subprocess.Runner = subprocess.ExecRunner{}

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
	case "train":
		return runSkillOptTrain(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)")
	fmt.Fprintln(w, "  gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file path [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train status --session <id>")
	fmt.Fprintln(w, "  gitmoot skillopt train continue --session <id> [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--model name] [--optimizer-model name] [--target-model name] [--optimizer-backend name] [--target-backend name] [--evaluator-id id] [--evaluator-model name] [--evaluator-backend name] [--skill-update-mode mode] [--num-epochs N] [--batch-size N] [--gate hard|soft|mixed] [--out-root path] [--timeout duration] [--dry-run] [--rerun-optimizer] [--promote version|--reject version --reason text] [--start-next]")
	fmt.Fprintln(w, "  gitmoot skillopt train stop --session <id> --reason <text>")
}

func runSkillOptTrain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptTrainUsage(stdout)
		return 0
	}
	switch args[0] {
	case "start":
		return runSkillOptTrainStart(args[1:], stdout, stderr)
	case "status":
		return runSkillOptTrainStatus(args[1:], stdout, stderr)
	case "continue":
		return runSkillOptTrainContinue(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file path [--session <id>] [--workspace-repo owner/repo] [--preview-repo owner/repo] [--preview-mode none|optional|required] [--preview-renderer none|vue-vite] [--preview-publisher none|github-pages] [--preview-route-template template] [--request-file path] [--task-kind kind] [--mode explore|refine|distill|validate] [--exploration-level high|medium|low] [--options N] [--min-items N] [--preferred-gate hard|soft|hard_then_soft] [--dry-run] [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train status --session <id>")
	fmt.Fprintln(w, "  gitmoot skillopt train continue --session <id> [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--model name] [--optimizer-model name] [--target-model name] [--optimizer-backend name] [--target-backend name] [--evaluator-id id] [--evaluator-model name] [--evaluator-backend name] [--skill-update-mode mode] [--num-epochs N] [--batch-size N] [--gate hard|soft|mixed] [--out-root path] [--timeout duration] [--dry-run] [--rerun-optimizer] [--promote version|--reject version --reason text] [--start-next]")
	fmt.Fprintln(w, "  gitmoot skillopt train stop --session <id> --reason <text>")
}

func runSkillOptTrainStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "agent template id or version to train")
	repoFlag := fs.String("repo", "", "target repository in owner/repo form")
	sessionID := fs.String("session", "", "train session id")
	workspaceRepoFlag := fs.String("workspace-repo", "", "workspace repository in owner/repo form")
	previewRepoFlag := fs.String("preview-repo", "", "preview repository in owner/repo form")
	previewMode := fs.String("preview-mode", "", "preview mode: none, optional, or required")
	previewRenderer := fs.String("preview-renderer", "", "preview renderer: none or vue-vite")
	previewPublisher := fs.String("preview-publisher", "", "preview publisher: none or github-pages")
	previewRouteTemplate := fs.String("preview-route-template", "", "preview route template for published options")
	requestText := fs.String("request", "", "human training request")
	requestFile := fs.String("request-file", "", "file containing the human training request")
	taskKind := fs.String("task-kind", "custom", "task kind: correctness, ux, design, writing, data, or custom")
	mode := fs.String("mode", db.EvalRunModeExplore, "train mode: explore, refine, distill, or validate")
	explorationLevel := fs.String("exploration-level", "", "exploration level: high, medium, or low")
	optionsCount := fs.Int("options", 0, "expected number of review options")
	itemsFile := fs.String("items-file", "", "YAML or JSON file describing training review items")
	minItems := fs.Int("min-items", 2, "minimum number of training review items")
	preferredGate := fs.String("preferred-gate", "", "evaluation gate: hard, soft, or hard_then_soft")
	dryRun := fs.Bool("dry-run", false, "print inferred session state without writing")
	yes := fs.Bool("yes", false, "confirm creation without an interactive prompt")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train start does not accept positional arguments")
		return 2
	}
	request, err := readSkillOptTrainRequest(*requestText, *requestFile)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*templateID) == "" || strings.TrimSpace(*repoFlag) == "" || strings.TrimSpace(request) == "" {
		fmt.Fprintln(stderr, "skillopt train start requires --template, --repo, and --request or --request-file")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	workspaceRepo, err := parseOptionalSkillOptTrainRepo("workspace-repo", *workspaceRepoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	previewRepo, err := parseOptionalSkillOptTrainRepo("preview-repo", *previewRepoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	policy, err := skillopt.BuildTrainPreviewPolicy(repo.FullName(), previewRepo, *previewMode, *previewRenderer, *previewPublisher, *previewRouteTemplate)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	normalizedTaskKind, err := normalizeSkillOptTrainTaskKind(*taskKind)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	normalizedMode, normalizedExploration, err := normalizeSkillOptTrainMode(*mode, *explorationLevel)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	if *optionsCount < 0 || *optionsCount == 1 {
		fmt.Fprintln(stderr, "skillopt train start: --options must be zero or at least 2")
		return 2
	}
	effectiveOptionsCount := effectiveSkillOptOptionsCount(normalizedMode, *optionsCount)
	items, itemWarnings, err := readSkillOptTrainItems(*itemsFile)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	if *minItems < 2 {
		fmt.Fprintln(stderr, "skillopt train start: --min-items must be at least 2")
		return 2
	}
	if len(items) < *minItems {
		fmt.Fprintf(stderr, "skillopt train start: --items-file must contain at least %d items, got %d\n", *minItems, len(items))
		return 2
	}
	normalizedGate, err := normalizeSkillOptPreferredGate(*preferredGate, normalizedTaskKind)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	itemWarnings = append(itemWarnings, detectSkillOptTrainItemWarnings(items)...)
	itemWarnings = append(itemWarnings, detectSkillOptTrainPreviewWarnings(policy)...)
	var plan skillOptTrainStartPlan
	openStore := withStore
	if *dryRun || !*yes {
		openStore = withReadOnlyStore
	}
	if err := openStore(*home, func(store *db.Store) error {
		template, err := loadInstalledTemplate(context.Background(), store, *templateID)
		if err != nil {
			return err
		}
		plan = buildSkillOptTrainStartPlan(template, repo.FullName(), workspaceRepo, policy, strings.TrimSpace(*sessionID), request, normalizedTaskKind, normalizedMode, normalizedExploration, effectiveOptionsCount, normalizedGate, items, itemWarnings)
		if *dryRun {
			return nil
		}
		if !*yes {
			return nil
		}
		if _, err := store.GetSkillOptTrainSession(context.Background(), plan.Session.ID); err == nil {
			return fmt.Errorf("train session %s already exists; use a different --session or inspect it with gitmoot skillopt train status --session %s", plan.Session.ID, plan.Session.ID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := store.GetEvalRun(context.Background(), plan.EvalRun.ID); err == nil {
			return fmt.Errorf("eval run %s already exists; use a different --session or inspect it with gitmoot skillopt review status --run %s", plan.EvalRun.ID, plan.EvalRun.ID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := store.UpsertSkillOptTrainSession(context.Background(), plan.Session); err != nil {
			return err
		}
		if err := store.UpsertSkillOptTrainIteration(context.Background(), plan.Iteration); err != nil {
			return err
		}
		if err := store.UpsertEvalRun(context.Background(), plan.EvalRun); err != nil {
			return err
		}
		for _, item := range plan.Items {
			if err := store.UpsertEvalReviewItem(context.Background(), item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 1
	}
	printSkillOptTrainStartPlan(stdout, plan)
	if *dryRun {
		writeLine(stdout, "dry_run: true")
		return 0
	}
	if !*yes {
		writeLine(stdout, "confirmation_required: true")
		writeLine(stdout, "confirm_command: %s", skillOptTrainConfirmCommand(args, plan.Session.ID))
		return 2
	}
	writeLine(stdout, "created train session %s", plan.Session.ID)
	return 0
}

type skillOptTrainStartPlan struct {
	Session   db.SkillOptTrainSession
	Iteration db.SkillOptTrainIteration
	EvalRun   db.EvalRun
	Items     []db.EvalReviewItem
	Warnings  []string
	Summary   skillopt.TrainStatusSummary
}

func buildSkillOptTrainStartPlan(template db.AgentTemplate, repo string, workspaceRepo string, previewPolicy skillopt.TrainPreviewPolicy, sessionID string, request string, taskKind string, mode string, explorationLevel string, optionsCount int, preferredGate string, itemPlans []skillOptTrainItemPlan, warnings []string) skillOptTrainStartPlan {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = generatedSkillOptTrainSessionID(template.ID)
	}
	state := skillopt.TrainStateRequestConfirmed
	if workspaceRepo != "" {
		state = skillopt.TrainStateItemsReady
	}
	metadata := skillOptTrainStartMetadata(request, mode, explorationLevel, optionsCount, preferredGate, itemPlans, warnings, previewPolicy)
	session := db.SkillOptTrainSession{
		ID:                sessionID,
		TemplateID:        template.ID,
		TemplateVersionID: template.VersionID,
		TargetRepo:        repo,
		WorkspaceRepo:     workspaceRepo,
		PreviewRepo:       previewPolicy.Repo,
		RequestSummary:    firstLine(request),
		TaskKind:          taskKind,
		State:             state,
		MetadataJSON:      metadata,
	}
	iteration := db.SkillOptTrainIteration{
		ID:                    sessionID + "-001",
		SessionID:             sessionID,
		EvalRunID:             sessionID + "-review-001",
		BaseTemplateVersionID: template.VersionID,
		Mode:                  mode,
		ExplorationLevel:      explorationLevel,
		State:                 state,
		MetadataJSON:          metadata,
	}
	run := db.EvalRun{
		ID:                iteration.EvalRunID,
		TemplateID:        template.ID,
		TemplateVersionID: template.VersionID,
		TargetRepo:        repo,
		State:             "review",
		Mode:              mode,
		ExplorationLevel:  explorationLevel,
		OptionsCount:      optionsCount,
		MetadataJSON:      metadata,
	}
	items := make([]db.EvalReviewItem, 0, len(itemPlans))
	for _, item := range itemPlans {
		items = append(items, db.EvalReviewItem{
			RunID:        run.ID,
			ItemID:       item.ItemID,
			Title:        item.Title,
			MetadataJSON: skillOptTrainItemMetadata(item),
		})
	}
	summary := skillopt.BuildTrainStatusSummary(session, &iteration, skillopt.TrainStatusCounts{})
	return skillOptTrainStartPlan{Session: session, Iteration: iteration, EvalRun: run, Items: items, Warnings: warnings, Summary: summary}
}

func printSkillOptTrainStartPlan(stdout io.Writer, plan skillOptTrainStartPlan) {
	writeLine(stdout, "session: %s", plan.Session.ID)
	writeLine(stdout, "template: %s", plan.Session.TemplateID)
	writeLine(stdout, "template_version: %s", plan.Session.TemplateVersionID)
	writeLine(stdout, "repo: %s", plan.Session.TargetRepo)
	writeLine(stdout, "workspace_repo: %s", emptyText(plan.Session.WorkspaceRepo))
	writeLine(stdout, "preview_repo: %s", emptyText(plan.Session.PreviewRepo))
	writeLine(stdout, "preview_mode: %s", plan.Summary.PreviewPolicy.Mode)
	writeLine(stdout, "preview_renderer: %s", plan.Summary.PreviewPolicy.Renderer)
	writeLine(stdout, "preview_publisher: %s", plan.Summary.PreviewPolicy.Publisher)
	writeLine(stdout, "preview_route_template: %s", emptyText(plan.Summary.PreviewPolicy.RouteTemplate))
	writeLine(stdout, "expected_review_repo: %s", emptyText(plan.Summary.PreviewPolicy.ExpectedReviewRepo))
	writeLine(stdout, "task_kind: %s", plan.Session.TaskKind)
	writeLine(stdout, "request_summary: %s", plan.Session.RequestSummary)
	writeLine(stdout, "iteration: %s", plan.Iteration.ID)
	writeLine(stdout, "eval_run: %s", plan.Iteration.EvalRunID)
	writeLine(stdout, "mode: %s", plan.Iteration.Mode)
	writeLine(stdout, "exploration_level: %s", plan.Iteration.ExplorationLevel)
	writeLine(stdout, "preferred_gate: %s", skillOptMetadataString(plan.EvalRun.MetadataJSON, "evaluation", "preferred_gate"))
	writeLine(stdout, "items: %d", len(plan.Items))
	for _, warning := range plan.Warnings {
		writeLine(stdout, "warning: %s", warning)
	}
	writeLine(stdout, "current_phase: %s", plan.Summary.CurrentPhase)
	writeLine(stdout, "blocked_step: %s", plan.Summary.BlockedStep)
	writeLine(stdout, "next_action: %s", plan.Summary.NextAction)
}

func runSkillOptTrainStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train status does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(stderr, "skillopt train status requires --session")
		return 2
	}
	var session db.SkillOptTrainSession
	var iteration *db.SkillOptTrainIteration
	var counts skillopt.TrainStatusCounts
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		session, iteration, counts, err = loadSkillOptTrainStatus(context.Background(), store, *sessionID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train status: %v\n", err)
		return 1
	}
	printSkillOptTrainStatus(stdout, skillopt.BuildTrainStatusSummary(session, iteration, counts), counts)
	return 0
}

func runSkillOptTrainContinue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train continue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	generatorAgent := fs.String("generator-agent", "", "existing agent to use for option generation")
	generatorType := fs.String("generator-type", "", "managed agent type to use for option generation; defaults to skillopt-generator")
	skillOptBin := fs.String("skillopt-bin", "", "gitmoot-skillopt executable path; defaults to gitmoot-skillopt on PATH")
	model := fs.String("model", "", "model name to pass to both optimizer and target when specific model flags are omitted")
	optimizerModel := fs.String("optimizer-model", "", "optimizer model name")
	targetModel := fs.String("target-model", "", "target model name")
	optimizerBackend := fs.String("optimizer-backend", "", "optimizer backend")
	targetBackend := fs.String("target-backend", "", "target backend")
	evaluatorID := fs.String("evaluator-id", "", "evaluator id, such as landing_page_v1")
	evaluatorModel := fs.String("evaluator-model", "", "evaluator model name")
	evaluatorBackend := fs.String("evaluator-backend", "", "evaluator backend")
	skillUpdateMode := fs.String("skill-update-mode", "", "SkillOpt update mode")
	numEpochs := fs.Int("num-epochs", 0, "optimizer epoch count")
	batchSize := fs.Int("batch-size", 0, "optimizer batch size")
	gate := fs.String("gate", "", "optimizer gate metric: hard, soft, or mixed")
	outRoot := fs.String("out-root", "", "optimizer output directory")
	timeout := fs.String("timeout", "", "optimizer timeout duration")
	dryRun := fs.Bool("dry-run", false, "ask gitmoot-skillopt to avoid model calls while still producing a candidate package")
	rerunOptimizer := fs.Bool("rerun-optimizer", false, "rerun gitmoot-skillopt after optimizer completion instead of retrying the existing candidate import")
	promote := fs.String("promote", "", "candidate version to promote after candidate review")
	reject := fs.String("reject", "", "candidate version to reject after candidate review")
	reason := fs.String("reason", "", "decision reason required with --reject")
	startNext := fs.Bool("start-next", false, "start the next train iteration after a promote or reject decision")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train continue does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(stderr, "skillopt train continue requires --session")
		return 2
	}
	if *numEpochs < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --num-epochs must be zero or greater")
		return 2
	}
	if *batchSize < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --batch-size must be zero or greater")
		return 2
	}
	var output skillOptTrainContinueOutput
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		output, err = continueSkillOptTrain(context.Background(), paths, store, skillOptTrainContinueRequest{
			Home:           *home,
			SessionID:      *sessionID,
			GeneratorAgent: *generatorAgent,
			GeneratorType:  *generatorType,
			Optimizer: skillOptTrainOptimizerRequest{
				SkillOptBin:      *skillOptBin,
				Model:            *model,
				OptimizerModel:   *optimizerModel,
				TargetModel:      *targetModel,
				OptimizerBackend: *optimizerBackend,
				TargetBackend:    *targetBackend,
				EvaluatorID:      *evaluatorID,
				EvaluatorModel:   *evaluatorModel,
				EvaluatorBackend: *evaluatorBackend,
				SkillUpdateMode:  *skillUpdateMode,
				NumEpochs:        *numEpochs,
				BatchSize:        *batchSize,
				Gate:             *gate,
				OutRoot:          *outRoot,
				Timeout:          *timeout,
				DryRun:           *dryRun,
				RerunOptimizer:   *rerunOptimizer,
			},
			PromoteCandidate: *promote,
			RejectCandidate:  *reject,
			DecisionReason:   *reason,
			StartNext:        *startNext,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train continue: %v\n", err)
		return 1
	}
	printSkillOptTrainStatus(stdout, output.Summary, output.Counts)
	if output.Summary.CurrentPhase == skillopt.TrainStateRunAbandoned {
		fmt.Fprintln(stderr, "skillopt train continue: train session is abandoned")
		return 1
	}
	writeLine(stdout, "continue_ready: %t", output.ContinueReady)
	for _, line := range output.Lines {
		writeLine(stdout, "%s", line)
	}
	return 0
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
}

type skillOptTrainOptimizerRequest struct {
	SkillOptBin      string
	Model            string
	OptimizerModel   string
	TargetModel      string
	OptimizerBackend string
	TargetBackend    string
	EvaluatorID      string
	EvaluatorModel   string
	EvaluatorBackend string
	SkillUpdateMode  string
	NumEpochs        int
	BatchSize        int
	Gate             string
	OutRoot          string
	Timeout          string
	DryRun           bool
	RerunOptimizer   bool
}

type skillOptTrainContinueOutput struct {
	Summary       skillopt.TrainStatusSummary
	Counts        skillopt.TrainStatusCounts
	ContinueReady bool
	Lines         []string
}

func continueSkillOptTrain(ctx context.Context, paths config.Paths, store *db.Store, request skillOptTrainContinueRequest) (skillOptTrainContinueOutput, error) {
	session, iteration, counts, err := loadSkillOptTrainStatus(ctx, store, request.SessionID)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	summary := skillopt.BuildTrainStatusSummary(session, iteration, counts)
	output := skillOptTrainContinueOutput{Summary: summary, Counts: counts}
	if summary.CurrentPhase == skillopt.TrainStateRunAbandoned {
		return output, nil
	}
	if iteration == nil {
		output.Lines = []string{"next: train session has no iteration to continue"}
		return output, nil
	}
	if strings.TrimSpace(request.PromoteCandidate) != "" && strings.TrimSpace(request.RejectCandidate) != "" {
		return skillOptTrainContinueOutput{}, errors.New("train continue accepts only one of --promote or --reject")
	}
	if skillOptTrainDecisionRequested(request) &&
		summary.CurrentPhase != skillopt.TrainStateCandidateReviewPublished &&
		summary.CurrentPhase != skillopt.TrainStateCandidateCreated &&
		summary.CurrentPhase != skillopt.TrainStateCandidatePromoted &&
		summary.CurrentPhase != skillopt.TrainStateCandidateRejected {
		return skillOptTrainContinueOutput{}, fmt.Errorf("candidate decisions require train iteration at %s; current phase is %s", skillopt.TrainStateCandidateReviewPublished, summary.CurrentPhase)
	}
	if request.StartNext &&
		summary.CurrentPhase != skillopt.TrainStateCandidateReviewPublished &&
		summary.CurrentPhase != skillopt.TrainStateCandidateCreated &&
		summary.CurrentPhase != skillopt.TrainStateOptimizerCompletedNoCandidate &&
		summary.CurrentPhase != skillopt.TrainStateCandidatePromoted &&
		summary.CurrentPhase != skillopt.TrainStateCandidateRejected {
		return skillOptTrainContinueOutput{}, fmt.Errorf("--start-next requires a promoted candidate, rejected candidate, or no-candidate optimizer result; current phase is %s", summary.CurrentPhase)
	}
	switch summary.CurrentPhase {
	case skillopt.TrainStateItemsReady:
		generationLockTTL, err := estimateSkillOptTrainGenerationLockTTL(ctx, store, request, *iteration)
		if err != nil {
			return output, err
		}
		request.GenerationLockTTL = generationLockTTL
		releaseGenerationLock, _, err := acquireSkillOptTrainGenerationLock(ctx, store, session.ID, iteration.ID, generationLockTTL)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseGenerationLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase != skillopt.TrainStateItemsReady {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		result, err := generateSkillOptTrainOptions(ctx, paths, store, session, *iteration, request)
		if err != nil {
			if metaErr := recordSkillOptTrainGenerationFailure(ctx, store, session, *iteration, request, err); metaErr != nil {
				return skillOptTrainContinueOutput{}, fmt.Errorf("%w; failed to record generation failure: %v", err, metaErr)
			}
			return skillOptTrainContinueOutput{}, err
		}
		session.State = skillopt.TrainStateOptionsGenerated
		iteration.State = skillopt.TrainStateOptionsGenerated
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "generation", result.Metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "generation", result.Metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, *iteration); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		return skillOptTrainContinueOutput{
			Summary:       updatedSummary,
			Counts:        updatedCounts,
			ContinueReady: true,
			Lines: []string{
				fmt.Sprintf("generated_options: %d", result.GeneratedOptions),
				fmt.Sprintf("jobs: %d", len(result.JobIDs)),
				fmt.Sprintf("generator_agent: %s", result.AgentName),
				fmt.Sprintf("generator_runtime: %s", result.Runtime),
				"next: publish the human review packet",
			},
		}, nil
	case skillopt.TrainStateOptionsGenerated:
		releaseReviewLock, _, err := acquireSkillOptTrainReviewLock(ctx, store, session.ID, iteration.ID)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseReviewLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase != skillopt.TrainStateOptionsGenerated {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		result, err := publishSkillOptTrainReview(ctx, paths, store, session, *iteration)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("review: %s", result.URL),
			fmt.Sprintf("review_repo: %s", result.Repo.FullName()),
			fmt.Sprintf("preview_urls: %d", result.PreviewURLs),
			"next: wait for feedback, then run train continue after sync",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateReviewPublished:
		status, err := loadSkillOptReviewStatus(ctx, store, artifact.NewStore(paths.ArtifactBlobs), iteration.EvalRunID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		feedbackCount := len(status.Feedback) + len(status.RankedFeedback)
		if !status.TrainingReady {
			lines := []string{
				fmt.Sprintf("feedback_events: %d", feedbackCount),
				fmt.Sprintf("pairwise_preferences: %d", len(status.PairwisePreferences)),
				fmt.Sprintf("packet_blockers: %d", len(status.PacketBlockers)),
				fmt.Sprintf("training_blockers: %d", len(status.TrainingBlockers)),
			}
			for _, blocker := range status.PacketBlockers {
				lines = append(lines, fmt.Sprintf("packet_blocker: %s", blocker))
			}
			for _, blocker := range status.TrainingBlockers {
				lines = append(lines, fmt.Sprintf("training_blocker: %s", blocker))
			}
			lines = append(lines, "next: sync human feedback from the review surface")
			output.Lines = lines
			return output, nil
		}
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateFeedbackSynced); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		metadata := map[string]any{
			"status":                 "succeeded",
			"source":                 "gitmoot skillopt train continue",
			"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
			"feedback_events":        feedbackCount,
			"ranked_feedback_events": len(status.RankedFeedback),
			"pairwise_preferences":   len(status.PairwisePreferences),
			"recommended_next_mode":  status.Recommendation.RecommendedMode,
			"ranking_stability":      status.Recommendation.RankingStability,
			"recommendation_summary": status.Recommendation.Summary(),
			"training_blocker_count": len(status.TrainingBlockers),
			"review_packet_ready":    status.PacketReady,
			"review_training_ready":  status.TrainingReady,
		}
		session.State = skillopt.TrainStateFeedbackSynced
		iteration.State = skillopt.TrainStateFeedbackSynced
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "feedback_sync", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "feedback_sync", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, *iteration); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("feedback_events: %d", feedbackCount),
			fmt.Sprintf("pairwise_preferences: %d", len(status.PairwisePreferences)),
			fmt.Sprintf("recommended_next_mode: %s", status.Recommendation.RecommendedMode),
			fmt.Sprintf("ranking_stability: %s", status.Recommendation.RankingStability),
			"next: export the training package before running the optimizer",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateFeedbackSynced, skillopt.TrainStateTrainingPackageCreated, skillopt.TrainStateOptimizerCompleted, skillopt.TrainStateOptimizerCompletedNoCandidate:
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase == skillopt.TrainStateOptimizerCompletedNoCandidate {
			if request.StartNext {
				next, err := startNextSkillOptTrainIteration(ctx, store, session, *iteration)
				if err != nil {
					return skillOptTrainContinueOutput{}, err
				}
				updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
				if err != nil {
					return skillOptTrainContinueOutput{}, err
				}
				updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
				lines := []string{
					fmt.Sprintf("started_iteration: %s", next.ID),
					fmt.Sprintf("base_version: %s", next.BaseTemplateVersionID),
					"next: generate review options with train continue",
				}
				return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
			}
			if !request.Optimizer.RerunOptimizer {
				output.Lines = []string{"next: revise feedback and run --start-next, rerun the optimizer with --rerun-optimizer, or stop"}
				return output, nil
			}
		}
		optimizerLockTTL, err := skillOptTrainOptimizerLockTTLForRequest(request.Optimizer)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		releaseOptimizerLock, _, err := acquireSkillOptTrainOptimizerLock(ctx, store, session.ID, iteration.ID, optimizerLockTTL)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseOptimizerLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase != skillopt.TrainStateFeedbackSynced &&
			summary.CurrentPhase != skillopt.TrainStateTrainingPackageCreated &&
			summary.CurrentPhase != skillopt.TrainStateOptimizerCompleted &&
			summary.CurrentPhase != skillopt.TrainStateOptimizerCompletedNoCandidate {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		result, err := continueSkillOptTrainOptimizer(ctx, paths, store, session, *iteration, request.Optimizer)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("training_package: %s", result.TrainingPackagePath),
			fmt.Sprintf("optimizer_out_root: %s", result.OutRoot),
			fmt.Sprintf("candidate_package: %s", result.CandidatePackagePath),
			fmt.Sprintf("artifact_dir: %s", result.ArtifactDir),
			fmt.Sprintf("optimizer_command: %s", shellArgs(append([]string{result.Command}, result.Args...))),
			fmt.Sprintf("optimizer_dry_run: %t", result.DryRun),
		}
		if result.NoCandidateReason != "" {
			lines = append(lines,
				fmt.Sprintf("no_candidate_reason: %s", result.NoCandidateReason),
				fmt.Sprintf("next: %s", result.NoCandidateNextAction),
			)
		} else {
			lines = append(lines,
				fmt.Sprintf("imported_candidate: %s", result.CandidateVersionID),
				"next: publish candidate diff and preview review",
			)
		}
		return skillOptTrainContinueOutput{
			Summary:       updatedSummary,
			Counts:        updatedCounts,
			ContinueReady: true,
			Lines:         lines,
		}, nil
	case skillopt.TrainStateCandidateCreated:
		releaseCandidateReviewLock, _, err := acquireSkillOptTrainCandidateReviewLock(ctx, store, session.ID, iteration.ID)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseCandidateReviewLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if skillOptTrainDecisionRequested(request) && summary.CurrentPhase == skillopt.TrainStateCandidateReviewPublished {
			return continueSkillOptTrainCandidateDecision(ctx, store, session, *iteration, counts, request)
		}
		if summary.CurrentPhase != skillopt.TrainStateCandidateCreated {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		candidateID := strings.TrimSpace(iteration.CandidateVersionID)
		if requestedCandidateID := requestedSkillOptTrainCandidateID(request); requestedCandidateID != "" && requestedCandidateID != candidateID {
			return skillOptTrainContinueOutput{}, fmt.Errorf("candidate %s does not match train iteration candidate %s", requestedCandidateID, candidateID)
		}
		if result, err := syncSkillOptTrainCandidateDecision(ctx, store, session, *iteration, candidateID, requestedSkillOptTrainCandidateDecision(request), strings.TrimSpace(request.DecisionReason)); err != nil || result.Decided {
			if err != nil {
				return skillOptTrainContinueOutput{}, err
			}
			return continueSkillOptTrainAfterCandidateDecision(ctx, store, session.ID, request, result)
		}
		if request.StartNext {
			return skillOptTrainContinueOutput{}, fmt.Errorf("--start-next requires a promoted or rejected candidate; current phase is %s", summary.CurrentPhase)
		}
		if skillOptTrainDecisionRequested(request) {
			if candidateID == "" {
				return skillOptTrainContinueOutput{}, errors.New("train iteration has no candidate version to review")
			}
			if _, recovered, err := recoverSkillOptCandidateReviewPublication(ctx, paths, store, session, *iteration, candidateID); err != nil {
				return skillOptTrainContinueOutput{}, err
			} else if !recovered {
				return skillOptTrainContinueOutput{}, fmt.Errorf("candidate decisions require train iteration at %s; current phase is %s", skillopt.TrainStateCandidateReviewPublished, summary.CurrentPhase)
			}
			updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
			if err != nil {
				return skillOptTrainContinueOutput{}, err
			}
			if updatedIteration == nil {
				return skillOptTrainContinueOutput{}, errors.New("train session has no recovered iteration to decide")
			}
			return continueSkillOptTrainCandidateDecision(ctx, store, updatedSession, *updatedIteration, updatedCounts, request)
		}
		result, err := publishSkillOptTrainCandidateReview(ctx, paths, store, session, *iteration, request.Home)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("candidate_review: %s", result.URL),
			fmt.Sprintf("candidate: %s", result.CandidateVersionID),
			"next: promote with --promote, reject with --reject --reason, or wait for a human decision",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateCandidateReviewPublished:
		return continueSkillOptTrainCandidateDecision(ctx, store, session, *iteration, counts, request)
	case skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		if err := validateTerminalSkillOptTrainDecisionRequest(*iteration, request); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		if !request.StartNext {
			output.Lines = []string{"next: stop or run --start-next"}
			return output, nil
		}
		next, err := startNextSkillOptTrainIteration(ctx, store, session, *iteration)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{}
		if decision := requestedSkillOptTrainCandidateDecision(request); decision != "" {
			lines = append(lines, fmt.Sprintf("%s_candidate: %s", decision, requestedSkillOptTrainCandidateID(request)))
		}
		lines = append(lines,
			fmt.Sprintf("started_iteration: %s", next.ID),
			fmt.Sprintf("base_version: %s", next.BaseTemplateVersionID),
			"next: generate review options with train continue",
		)
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	default:
		output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
		return output, nil
	}
}

func continueSkillOptTrainCandidateDecision(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, counts skillopt.TrainStatusCounts, request skillOptTrainContinueRequest) (skillOptTrainContinueOutput, error) {
	summary := skillopt.BuildTrainStatusSummary(session, &iteration, counts)
	output := skillOptTrainContinueOutput{Summary: summary, Counts: counts}
	result, err := decideSkillOptTrainCandidate(ctx, store, session, iteration, request)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	if !result.Decided {
		if request.StartNext {
			return skillOptTrainContinueOutput{}, fmt.Errorf("--start-next requires a promoted or rejected candidate; current phase is %s", summary.CurrentPhase)
		}
		output.Lines = []string{"next: promote with --promote <candidate-version> or reject with --reject <candidate-version> --reason <text>"}
		return output, nil
	}
	return continueSkillOptTrainAfterCandidateDecision(ctx, store, session.ID, request, result)
}

func continueSkillOptTrainAfterCandidateDecision(ctx context.Context, store *db.Store, sessionID string, request skillOptTrainContinueRequest, result skillOptTrainCandidateDecisionResult) (skillOptTrainContinueOutput, error) {
	updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
	if request.StartNext {
		if updatedIteration == nil {
			return skillOptTrainContinueOutput{}, errors.New("train session has no decided iteration to continue")
		}
		next, err := startNextSkillOptTrainIteration(ctx, store, updatedSession, *updatedIteration)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err = loadSkillOptTrainStatus(ctx, store, sessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary = skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("%s_candidate: %s", result.Decision, result.CandidateVersionID),
			fmt.Sprintf("started_iteration: %s", next.ID),
			fmt.Sprintf("base_version: %s", next.BaseTemplateVersionID),
			"next: generate review options with train continue",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	}
	lines := []string{
		fmt.Sprintf("%s_candidate: %s", result.Decision, result.CandidateVersionID),
		"next: stop or run --start-next",
	}
	return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
}

func validateTerminalSkillOptTrainDecisionRequest(iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) error {
	decision := requestedSkillOptTrainCandidateDecision(request)
	if decision == "" {
		return nil
	}
	candidateID := requestedSkillOptTrainCandidateID(request)
	expected := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID != expected {
		return fmt.Errorf("candidate %s does not match train iteration candidate %s", candidateID, expected)
	}
	currentDecision := ""
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidatePromoted:
		currentDecision = "promoted"
	case skillopt.TrainStateCandidateRejected:
		currentDecision = "rejected"
	}
	if currentDecision != "" && decision != currentDecision {
		return fmt.Errorf("candidate %s is already %s, not %s", candidateID, currentDecision, decision)
	}
	return nil
}

type skillOptTrainOptimizerResult struct {
	TrainingPackagePath   string
	OutRoot               string
	CandidatePackagePath  string
	ArtifactDir           string
	Command               string
	Args                  []string
	DryRun                bool
	CandidateVersionID    string
	NoCandidateReason     string
	NoCandidateNextAction string
}

type skillOptTrainOptimizerPaths struct {
	OutRoot              string
	ArtifactRoot         string
	TrainingPackagePath  string
	CandidatePackagePath string
	ArtifactDir          string
}

type skillOptTrainCandidateReviewResult struct {
	URL                string
	CandidateVersionID string
}

type skillOptTrainCandidateDecisionResult struct {
	Decided            bool
	Decision           string
	CandidateVersionID string
}

func publishSkillOptTrainCandidateReview(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, commandHome string) (skillOptTrainCandidateReviewResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateCandidateReviewPublished); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID == "" {
		return skillOptTrainCandidateReviewResult{}, errors.New("train iteration has no candidate version to review")
	}
	if result, recovered, err := recoverSkillOptCandidateReviewPublication(ctx, paths, store, session, iteration, candidateID); recovered || err != nil {
		return result, err
	}
	refreshedSession, err := store.GetSkillOptTrainSession(ctx, session.ID)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	refreshedIteration, err := store.GetSkillOptTrainIteration(ctx, iteration.ID)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	session = refreshedSession
	iteration = refreshedIteration
	if err := preventDuplicateSkillOptCandidateReviewPublish(session, iteration, candidateID, time.Now().UTC()); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	repo, err := resolveSkillOptTrainCandidateReviewRepo(session, iteration)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	body, err := skillOptTrainCandidateReviewBody(ctx, store, session, iteration, commandHome)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	client := newSkillOptGitHubClient()
	if iteration.PullRequestNumber > 0 && iteration.IssueNumber == 0 {
		iteration.PullRequestRepo = repo.FullName()
	} else {
		iteration.IssueRepo = repo.FullName()
	}
	title := fmt.Sprintf("SkillOpt candidate review: %s", session.ID)
	publishingMetadata := map[string]any{
		"status":              "publishing",
		"candidate_version":   candidateID,
		"issue_repo":          iteration.IssueRepo,
		"issue_number":        iteration.IssueNumber,
		"issue_url":           iteration.IssueURL,
		"pull_request_repo":   iteration.PullRequestRepo,
		"pull_request_number": iteration.PullRequestNumber,
		"pull_request_url":    iteration.PullRequestURL,
		"issue_title":         title,
		"started_at":          time.Now().UTC().Format(time.RFC3339Nano),
		"source":              "gitmoot skillopt train continue",
	}
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, publishingMetadata); err != nil {
		return skillOptTrainCandidateReviewResult{}, fmt.Errorf("write candidate review pre-publish recovery marker: %w", err)
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", publishingMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", publishingMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	postingMetadata := make(map[string]any, len(publishingMetadata)+2)
	for key, value := range publishingMetadata {
		postingMetadata[key] = value
	}
	postingMetadata["status"] = "posting_external"
	postingMetadata["external_post_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, postingMetadata); err != nil {
		if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
			return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
		}
		return skillOptTrainCandidateReviewResult{}, fmt.Errorf("write candidate review external-post recovery marker: %w", err)
	}
	var url string
	if iteration.IssueNumber > 0 {
		comment, err := client.PostIssueComment(ctx, repo, iteration.IssueNumber, body)
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		url = comment.URL
		if strings.TrimSpace(iteration.IssueURL) == "" {
			iteration.IssueURL = skillOptReviewTargetURLFromCommentOrHost(comment.URL, repo, "issues", iteration.IssueNumber)
		}
	} else if iteration.PullRequestNumber > 0 {
		comment, err := client.PostIssueComment(ctx, repo, iteration.PullRequestNumber, body)
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		url = comment.URL
		if strings.TrimSpace(iteration.PullRequestURL) == "" {
			iteration.PullRequestURL = skillOptReviewTargetURLFromCommentOrHost(comment.URL, repo, "pull", iteration.PullRequestNumber)
		}
	} else {
		issue, err := client.CreateIssue(ctx, github.CreateIssueInput{
			Repo:  repo,
			Title: title,
			Body:  body,
		})
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		iteration.IssueNumber = issue.Number
		iteration.IssueURL = issue.URL
		url = issue.URL
	}
	externalMetadata := skillOptCandidateReviewPublicationMetadata(publishingMetadata, iteration, url, "published_external")
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", externalMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", externalMetadata)
	recoveryErr := writeSkillOptCandidateReviewRecovery(paths, session, iteration, externalMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		if recoveryErr != nil {
			return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; candidate review was published at %s but recovery marker write failed: %v", err, url, recoveryErr)
		}
		return skillOptTrainCandidateReviewResult{}, err
	}
	iteration.State = skillopt.TrainStateCandidateReviewPublished
	session.State = skillopt.TrainStateCandidateReviewPublished
	metadata := skillOptCandidateReviewPublicationMetadata(publishingMetadata, iteration, url, "published")
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
	return skillOptTrainCandidateReviewResult{URL: url, CandidateVersionID: candidateID}, nil
}

func recoverSkillOptCandidateReviewPublication(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string) (skillOptTrainCandidateReviewResult, bool, error) {
	sources := []map[string]any{
		decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["candidate_review"]),
		decodedSkillOptMetadataValue(decodedSkillOptMetadata(session.MetadataJSON)["candidate_review"]),
	}
	if review, ok, err := readSkillOptCandidateReviewRecovery(paths, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, true, err
	} else if ok {
		if metadataString(review, "status") == "publishing" && metadataString(review, "external_post_started_at") == "" {
			metadata := make(map[string]any, len(review)+3)
			for key, value := range review {
				metadata[key] = value
			}
			metadata["status"] = "failed"
			metadata["error"] = "candidate review publication interrupted before external post started"
			metadata["failed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
			iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
			if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
				return skillOptTrainCandidateReviewResult{}, true, err
			}
			_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
			return skillOptTrainCandidateReviewResult{}, false, nil
		}
		sources = append(sources, review)
	}
	for _, review := range sources {
		status := metadataString(review, "status")
		if status == "posting_external" {
			target := skillOptCandidateReviewRecoveryTarget(review)
			if target == "" {
				target = "inspect the configured GitHub review surface before retrying"
			}
			return skillOptTrainCandidateReviewResult{}, true, fmt.Errorf("candidate review publication for %s was interrupted after external post started; %s", candidateID, target)
		}
		if status != "published_external" && status != "published" {
			continue
		}
		reviewCandidate := metadataString(review, "candidate_version")
		if reviewCandidate != "" && reviewCandidate != candidateID {
			continue
		}
		url := skillOptCandidateReviewURLFromMetadata(review)
		if url == "" {
			return skillOptTrainCandidateReviewResult{}, true, fmt.Errorf("candidate review publication for %s is marked %s but has no recoverable review URL", candidateID, status)
		}
		applySkillOptCandidateReviewMetadataToIteration(review, &iteration)
		iteration.State = skillopt.TrainStateCandidateReviewPublished
		session.State = skillopt.TrainStateCandidateReviewPublished
		metadata := make(map[string]any, len(review)+3)
		for key, value := range review {
			metadata[key] = value
		}
		metadata["status"] = "published"
		metadata["review_url"] = url
		metadata["recovered_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
		if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
			return skillOptTrainCandidateReviewResult{}, true, err
		}
		_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
		return skillOptTrainCandidateReviewResult{URL: url, CandidateVersionID: candidateID}, true, nil
	}
	return skillOptTrainCandidateReviewResult{}, false, nil
}

func preventDuplicateSkillOptCandidateReviewPublish(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string, now time.Time) error {
	for _, source := range []struct {
		name     string
		metadata string
	}{
		{name: "iteration", metadata: iteration.MetadataJSON},
		{name: "session", metadata: session.MetadataJSON},
	} {
		review := decodedSkillOptMetadataValue(decodedSkillOptMetadata(source.metadata)["candidate_review"])
		status := metadataString(review, "status")
		if status == "publishing" && !skillOptCandidateReviewPublishingFresh(review, now) {
			continue
		}
		if status != "publishing" && status != "published" {
			continue
		}
		reviewCandidate := metadataString(review, "candidate_version")
		if reviewCandidate != "" && reviewCandidate != candidateID {
			continue
		}
		target := skillOptCandidateReviewRecoveryTarget(review)
		if target == "" {
			target = "inspect candidate_review metadata before retrying"
		}
		return fmt.Errorf("candidate review publication for %s is marked %s in %s metadata; %s", candidateID, status, source.name, target)
	}
	return nil
}

func skillOptCandidateReviewPublishingFresh(review map[string]any, now time.Time) bool {
	startedAt := metadataString(review, "started_at")
	if startedAt == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Before(started.Add(skillOptTrainCandidateReviewLockTTL))
}

func writeSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, metadata map[string]any) error {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return errors.New("candidate review recovery path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmpPath, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func readSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (map[string]any, bool, error) {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil, false, nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(content, &metadata); err != nil {
		return nil, true, fmt.Errorf("read candidate review recovery marker %s: %w", path, err)
	}
	return metadata, true, nil
}

func removeSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) error {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func skillOptCandidateReviewRecoveryPath(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) string {
	if strings.TrimSpace(paths.Home) == "" {
		return ""
	}
	name := skillOptCandidateReviewRecoveryName(session.ID, iteration.ID)
	if name == "" {
		return ""
	}
	return filepath.Join(paths.Home, "skillopt", "candidate-reviews", name+".json")
}

func skillOptCandidateReviewRecoveryName(sessionID string, iterationID string) string {
	sessionID = encodeSkillOptCandidateReviewRecoveryToken(sessionID)
	iterationID = encodeSkillOptCandidateReviewRecoveryToken(iterationID)
	if sessionID == "" || iterationID == "" {
		return ""
	}
	return sessionID + "-" + iterationID
}

func encodeSkillOptCandidateReviewRecoveryToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func skillOptCandidateReviewPublicationMetadata(base map[string]any, iteration db.SkillOptTrainIteration, reviewURL string, status string) map[string]any {
	metadata := make(map[string]any, len(base)+9)
	for key, value := range base {
		metadata[key] = value
	}
	metadata["status"] = status
	metadata["issue_repo"] = iteration.IssueRepo
	metadata["issue_number"] = iteration.IssueNumber
	metadata["issue_url"] = iteration.IssueURL
	metadata["pull_request_repo"] = iteration.PullRequestRepo
	metadata["pull_request_number"] = iteration.PullRequestNumber
	metadata["pull_request_url"] = iteration.PullRequestURL
	metadata["review_url"] = reviewURL
	metadata["published_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return metadata
}

func recordFailedSkillOptCandidateReviewPublish(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, publishingMetadata map[string]any, publishErr error) error {
	metadata := make(map[string]any, len(publishingMetadata)+3)
	for key, value := range publishingMetadata {
		metadata[key] = value
	}
	metadata["status"] = "failed"
	metadata["error"] = truncateForMetadata(publishErr.Error())
	metadata["failed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
	return store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration)
}

func applySkillOptCandidateReviewMetadataToIteration(review map[string]any, iteration *db.SkillOptTrainIteration) {
	if value := metadataString(review, "issue_repo"); value != "" {
		iteration.IssueRepo = value
	}
	if value := metadataString(review, "issue_number"); value != "" {
		if number, err := strconv.ParseInt(value, 10, 64); err == nil {
			iteration.IssueNumber = number
		}
	}
	if value := metadataString(review, "issue_url"); value != "" {
		iteration.IssueURL = value
	}
	if value := metadataString(review, "pull_request_repo"); value != "" {
		iteration.PullRequestRepo = value
	}
	if value := metadataString(review, "pull_request_number"); value != "" {
		if number, err := strconv.ParseInt(value, 10, 64); err == nil {
			iteration.PullRequestNumber = number
		}
	}
	if value := metadataString(review, "pull_request_url"); value != "" {
		iteration.PullRequestURL = value
	}
}

func skillOptCandidateReviewURLFromMetadata(review map[string]any) string {
	for _, key := range []string{"review_url", "issue_url", "pull_request_url"} {
		if value := metadataString(review, key); value != "" {
			return value
		}
	}
	repo := metadataString(review, "issue_repo")
	number := metadataString(review, "issue_number")
	if repo != "" && number != "" && number != "0" {
		return "https://github.com/" + repo + "/issues/" + number
	}
	repo = metadataString(review, "pull_request_repo")
	number = metadataString(review, "pull_request_number")
	if repo != "" && number != "" && number != "0" {
		return "https://github.com/" + repo + "/pull/" + number
	}
	return ""
}

func skillOptCandidateReviewRecoveryTarget(review map[string]any) string {
	for _, key := range []string{"review_url", "issue_url", "pull_request_url"} {
		if value := metadataString(review, key); value != "" {
			return "review target: " + value
		}
	}
	repo := metadataString(review, "issue_repo")
	number := metadataString(review, "issue_number")
	if repo != "" && number != "" && number != "0" {
		return "review issue: " + repo + "#" + number
	}
	repo = metadataString(review, "pull_request_repo")
	number = metadataString(review, "pull_request_number")
	if repo != "" && number != "" && number != "0" {
		return "review pull request: " + repo + "#" + number
	}
	if title := metadataString(review, "issue_title"); title != "" {
		return "search for review issue title: " + title
	}
	return ""
}

func resolveSkillOptTrainCandidateReviewRepo(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (github.Repository, error) {
	repoName := strings.TrimSpace(iteration.IssueRepo)
	if iteration.IssueNumber > 0 {
		if repoName == "" {
			repoName = skillOptGitHubIssueURLRepo(iteration.IssueURL)
		}
		if repoName == "" {
			return github.Repository{}, errors.New("candidate review issue repo is required when reusing an existing review issue")
		}
	} else if iteration.PullRequestNumber > 0 {
		repoName = strings.TrimSpace(iteration.PullRequestRepo)
		if repoName == "" {
			repoName = skillOptGitHubPullRequestURLRepo(iteration.PullRequestURL)
		}
		if repoName == "" {
			return github.Repository{}, errors.New("candidate review pull request repo is required when reusing an existing review pull request")
		}
	} else if repoName == "" {
		repoName = strings.TrimSpace(session.WorkspaceRepo)
		if repoName == "" {
			repoName = strings.TrimSpace(session.TargetRepo)
		}
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return github.Repository{}, fmt.Errorf("candidate review repo: %w", err)
	}
	return repo, nil
}

func skillOptGitHubIssueURLRepo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return ""
	}
	repo, err := daemon.ParseRepository(parts[0] + "/" + parts[1])
	if err != nil {
		return ""
	}
	return repo.FullName()
}

func skillOptGitHubPullRequestURLRepo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return ""
	}
	repo, err := daemon.ParseRepository(parts[0] + "/" + parts[1])
	if err != nil {
		return ""
	}
	return repo.FullName()
}

func skillOptReviewTargetURLFromCommentURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return ""
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func skillOptReviewTargetURLFromCommentOrHost(commentURL string, repo github.Repository, kind string, number int64) string {
	if target := skillOptReviewTargetURLFromCommentURL(commentURL); target != "" {
		return target
	}
	parsed, err := neturl.Parse(strings.TrimSpace(commentURL))
	if err != nil || strings.TrimSpace(parsed.Host) == "" {
		return ""
	}
	scheme := strings.TrimSpace(parsed.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	target := neturl.URL{
		Scheme: scheme,
		Host:   parsed.Host,
		Path:   "/" + repo.FullName() + "/" + strings.Trim(kind, "/") + "/" + fmt.Sprint(number),
	}
	return target.String()
}

func skillOptTrainDecisionRequested(request skillOptTrainContinueRequest) bool {
	return strings.TrimSpace(request.PromoteCandidate) != "" || strings.TrimSpace(request.RejectCandidate) != ""
}

func requestedSkillOptTrainCandidateDecision(request skillOptTrainContinueRequest) string {
	if strings.TrimSpace(request.PromoteCandidate) != "" {
		return "promoted"
	}
	if strings.TrimSpace(request.RejectCandidate) != "" {
		return "rejected"
	}
	return ""
}

func requestedSkillOptTrainCandidateID(request skillOptTrainContinueRequest) string {
	if value := strings.TrimSpace(request.PromoteCandidate); value != "" {
		return value
	}
	return strings.TrimSpace(request.RejectCandidate)
}

func skillOptTrainCandidateReviewBody(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, commandHome string) (string, error) {
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	version, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return "", fmt.Errorf("load candidate version %s: %w", candidateID, err)
	}
	candidateID = strings.TrimSpace(version.ID)
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		return "", fmt.Errorf("load candidate review %s: %w", candidateID, err)
	}
	baseRef := strings.TrimSpace(review.BaseVersionID)
	if baseRef == "" {
		baseRef = strings.TrimSpace(iteration.BaseTemplateVersionID)
	}
	var builder strings.Builder
	builder.WriteString("## SkillOpt Candidate Review\n\n")
	fmt.Fprintf(&builder, "Session: `%s`\n", session.ID)
	fmt.Fprintf(&builder, "Iteration: `%s`\n", iteration.ID)
	fmt.Fprintf(&builder, "Template: `%s`\n", session.TemplateID)
	fmt.Fprintf(&builder, "Base: `%s`\n", emptyText(baseRef))
	fmt.Fprintf(&builder, "Candidate: `%s`\n", candidateID)
	if summary := strings.TrimSpace(review.PreferenceSummary); summary != "" {
		fmt.Fprintf(&builder, "\n### Candidate Summary\n%s\n", summary)
	}
	if review.Score != nil {
		fmt.Fprintf(&builder, "\nScore: `%s`\n", scoreText(review.Score))
	}
	if strings.TrimSpace(session.PreviewRepo) != "" {
		fmt.Fprintf(&builder, "\nPreview repo: `%s`\n", session.PreviewRepo)
	}
	if strings.TrimSpace(iteration.PullRequestURL) != "" {
		fmt.Fprintf(&builder, "\nCandidate PR: %s\n", iteration.PullRequestURL)
	}
	if artifactIDs := skillOptCandidateReviewArtifactIDs(review); len(artifactIDs) > 0 {
		builder.WriteString("\n### Artifacts\n")
		for _, artifactID := range artifactIDs {
			fmt.Fprintf(&builder, "- `%s`\n", artifactID)
		}
	}
	if strings.TrimSpace(review.EvalReportJSON) != "" {
		builder.WriteString("\nEval report: stored with the pending candidate review record.\n")
	}
	builder.WriteString("\n### Decision\n")
	usesCustomHome := strings.TrimSpace(commandHome) != ""
	fmt.Fprintf(&builder, "- Promote: `%s`\n", skillOptTrainCandidateDecisionCommand(usesCustomHome, session.ID, "--promote", candidateID, false))
	fmt.Fprintf(&builder, "- Reject: `%s`\n", skillOptTrainCandidateDecisionCommand(usesCustomHome, session.ID, "--reject", candidateID, true))
	fmt.Fprintf(&builder, "- Continue: `%s` after promote/reject completes.\n", skillOptTrainStartNextCommand(usesCustomHome, session.ID))
	return builder.String(), nil
}

func skillOptCandidateReviewArtifactIDs(review db.AgentTemplateCandidateReview) []string {
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(review.DiffArtifactID)
	metadata := decodedSkillOptMetadata(review.SummaryMetadataJSON)
	rawIDs, ok := metadata["artifact_ids"].([]any)
	if !ok {
		return ids
	}
	for _, rawID := range rawIDs {
		add(fmt.Sprint(rawID))
	}
	return ids
}

func skillOptTrainCandidateDecisionCommand(usesCustomHome bool, sessionID, decisionFlag, candidateID string, includeReason bool) string {
	args := []string{"gitmoot", "skillopt", "train", "continue"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID), decisionFlag, strings.TrimSpace(candidateID))
	if includeReason {
		args = append(args, "--reason", "...")
	}
	return shellArgs(args)
}

func skillOptTrainStartNextCommand(usesCustomHome bool, sessionID string) string {
	args := []string{"gitmoot", "skillopt", "train", "continue"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID), "--start-next")
	return shellArgs(args)
}

func decideSkillOptTrainCandidate(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) (skillOptTrainCandidateDecisionResult, error) {
	promote := strings.TrimSpace(request.PromoteCandidate)
	reject := strings.TrimSpace(request.RejectCandidate)
	if promote != "" && reject != "" {
		return skillOptTrainCandidateDecisionResult{}, errors.New("train continue accepts only one of --promote or --reject")
	}
	expected := strings.TrimSpace(iteration.CandidateVersionID)
	if expected == "" {
		return skillOptTrainCandidateDecisionResult{}, errors.New("train iteration has no candidate version")
	}
	candidateID := promote
	decision := ""
	if promote != "" {
		decision = "promoted"
	} else if reject != "" {
		candidateID = reject
		decision = "rejected"
		if strings.TrimSpace(request.DecisionReason) == "" {
			return skillOptTrainCandidateDecisionResult{}, errors.New("train candidate rejection requires --reason")
		}
	}
	if candidateID != "" && candidateID != expected {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("candidate %s does not match train iteration candidate %s", candidateID, expected)
	}
	if decision == "" {
		return syncSkillOptTrainCandidateDecision(ctx, store, session, iteration, expected, "", "")
	}
	if result, err := syncSkillOptTrainCandidateDecision(ctx, store, session, iteration, expected, decision, strings.TrimSpace(request.DecisionReason)); err != nil || result.Decided {
		return result, err
	}
	if err := skillopt.CanTransitionTrainIteration(iteration.State, map[string]string{
		"promoted": skillopt.TrainStateCandidatePromoted,
		"rejected": skillopt.TrainStateCandidateRejected,
	}[decision]); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	if decision == "promoted" {
		session.TemplateVersionID = candidateID
		session.State = skillopt.TrainStateCandidatePromoted
		iteration.State = skillopt.TrainStateCandidatePromoted
	} else {
		session.State = skillopt.TrainStateCandidateRejected
		iteration.State = skillopt.TrainStateCandidateRejected
		iteration.DecisionReason = strings.TrimSpace(request.DecisionReason)
	}
	metadata := map[string]any{
		"decision":          decision,
		"candidate_version": candidateID,
		"reason":            strings.TrimSpace(request.DecisionReason),
		"decided_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"source":            "gitmoot skillopt train continue",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_decision", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_decision", metadata)
	if _, err := store.DecideSkillOptTrainCandidate(ctx, session, iteration, candidateID, decision); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	return skillOptTrainCandidateDecisionResult{Decided: true, Decision: decision, CandidateVersionID: candidateID}, nil
}

func syncSkillOptTrainCandidateDecision(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string, expectedDecision string, fallbackReason string) (skillOptTrainCandidateDecisionResult, error) {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return skillOptTrainCandidateDecisionResult{}, nil
	}
	candidate, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("load candidate version %s: %w", candidateID, err)
	}
	review, reviewErr := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if reviewErr != nil && !errors.Is(reviewErr, sql.ErrNoRows) {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("load candidate review %s: %w", candidateID, reviewErr)
	}
	var decision string
	switch candidate.State {
	case "current":
		decision = "promoted"
	case "rejected":
		decision = "rejected"
	default:
		if reviewErr == nil {
			switch strings.TrimSpace(review.State) {
			case "promoted":
				decision = "promoted"
			case "rejected":
				decision = "rejected"
			}
		}
		if decision == "" {
			return skillOptTrainCandidateDecisionResult{}, nil
		}
	}
	if expectedDecision != "" && expectedDecision != decision {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("candidate %s is already %s, not %s", candidateID, decision, expectedDecision)
	}
	targetState := map[string]string{
		"promoted": skillopt.TrainStateCandidatePromoted,
		"rejected": skillopt.TrainStateCandidateRejected,
	}[decision]
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished:
	default:
		if err := skillopt.CanTransitionTrainIteration(iteration.State, targetState); err != nil {
			return skillOptTrainCandidateDecisionResult{}, err
		}
	}
	reason := strings.TrimSpace(fallbackReason)
	if decision == "rejected" {
		if reviewErr == nil && strings.TrimSpace(review.DecisionReason) != "" {
			reason = strings.TrimSpace(review.DecisionReason)
		}
		if reason == "" {
			return skillOptTrainCandidateDecisionResult{}, errors.New("train candidate rejection requires --reason")
		}
	}
	if decision == "promoted" {
		session.TemplateVersionID = candidateID
		session.State = skillopt.TrainStateCandidatePromoted
		iteration.State = skillopt.TrainStateCandidatePromoted
	} else {
		session.State = skillopt.TrainStateCandidateRejected
		iteration.State = skillopt.TrainStateCandidateRejected
		iteration.DecisionReason = reason
	}
	metadata := map[string]any{
		"decision":          decision,
		"candidate_version": candidateID,
		"reason":            reason,
		"decided_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"source":            "gitmoot skillopt train continue synced candidate state",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_decision", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_decision", metadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	return skillOptTrainCandidateDecisionResult{Decided: true, Decision: decision, CandidateVersionID: candidateID}, nil
}

func startNextSkillOptTrainIteration(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, previous db.SkillOptTrainIteration) (db.SkillOptTrainIteration, error) {
	if err := skillopt.CanStartNextTrainIteration(previous); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	releaseStartNextLock, _, err := acquireSkillOptTrainStartNextLock(ctx, store, session.ID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	defer func() {
		_ = releaseStartNextLock(context.Background())
	}()
	baseVersion := strings.TrimSpace(previous.BaseTemplateVersionID)
	if skillopt.NormalizeTrainState(previous.State) == skillopt.TrainStateCandidatePromoted {
		baseVersion = strings.TrimSpace(previous.CandidateVersionID)
	}
	if baseVersion == "" {
		return db.SkillOptTrainIteration{}, errors.New("next train iteration base version is required")
	}
	previousRun, err := store.GetEvalRun(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("load previous eval run %s: %w", previous.EvalRunID, err)
	}
	iterations, err := store.ListSkillOptTrainIterations(ctx, session.ID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	nextNumber := len(iterations) + 1
	nextID := fmt.Sprintf("%s-%03d", session.ID, nextNumber)
	nextRunID := fmt.Sprintf("%s-review-%03d", session.ID, nextNumber)
	if _, err := store.GetSkillOptTrainIteration(ctx, nextID); err == nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("train iteration %s already exists; inspect it with gitmoot skillopt train status --session %s", nextID, session.ID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.SkillOptTrainIteration{}, err
	}
	if _, err := store.GetEvalRun(ctx, nextRunID); err == nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("eval run %s already exists; inspect it with gitmoot skillopt review status --run %s", nextRunID, nextRunID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.SkillOptTrainIteration{}, err
	}
	metadata := skillOptTrainNextIterationMetadata(session.MetadataJSON, previous.MetadataJSON, map[string]any{
		"id":         previous.ID,
		"state":      previous.State,
		"candidate":  previous.CandidateVersionID,
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	items, err := store.ListEvalReviewItems(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	feedbackEvents, err := store.ListFeedbackEvents(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	rankedFeedbackEvents, err := store.ListRankedFeedbackEvents(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	recommendation := skillopt.RecommendPhaseForItems(previousRun, items, feedbackEvents, rankedFeedbackEvents, pairwisePreferences)
	nextMode := skillOptTrainNextIterationMode(previous.Mode, recommendation.RecommendedMode)
	nextExplorationLevel := strings.TrimSpace(recommendation.ExplorationLevel)
	if nextExplorationLevel == "" {
		nextExplorationLevel = previous.ExplorationLevel
	}
	metadata = mergeSkillOptTrainMetadata(metadata, "phase_recommendation", map[string]any{
		"current_mode":     recommendation.CurrentMode,
		"recommended_mode": recommendation.RecommendedMode,
		"selected_mode":    nextMode,
		"reason":           recommendation.Reason,
	})
	next := db.SkillOptTrainIteration{
		ID:                    nextID,
		SessionID:             session.ID,
		EvalRunID:             nextRunID,
		BaseTemplateVersionID: baseVersion,
		Mode:                  nextMode,
		ExplorationLevel:      nextExplorationLevel,
		State:                 skillopt.TrainStateItemsReady,
		MetadataJSON:          metadata,
	}
	run := db.EvalRun{
		ID:                nextRunID,
		TemplateID:        session.TemplateID,
		TemplateVersionID: baseVersion,
		TargetRepo:        session.TargetRepo,
		State:             "review",
		Mode:              nextMode,
		ExplorationLevel:  nextExplorationLevel,
		OptionsCount:      previousRun.OptionsCount,
		MetadataJSON:      metadata,
	}
	session.TemplateVersionID = baseVersion
	session.State = skillopt.TrainStateItemsReady
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "next_iteration", map[string]any{
		"id":           next.ID,
		"base_version": baseVersion,
		"source":       "gitmoot skillopt train continue",
	})
	nextItems := make([]db.EvalReviewItem, 0, len(items))
	for _, item := range items {
		item.RunID = nextRunID
		item.ID = ""
		item.BaselineArtifactID = ""
		item.CandidateArtifactID = ""
		item.PreviewArtifactID = ""
		item.DiffArtifactID = ""
		nextItems = append(nextItems, item)
	}
	if err := store.UpsertSkillOptTrainNextIteration(ctx, session, next, run, nextItems); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	return next, nil
}

func skillOptTrainNextIterationMetadata(sessionMetadata string, previousMetadata string, previousIteration map[string]any) string {
	metadata := map[string]any{
		"previous_iteration": previousIteration,
	}
	for _, source := range []string{previousMetadata, sessionMetadata} {
		evaluation := decodedSkillOptMetadataValue(decodedSkillOptMetadata(source)["evaluation"])
		if len(evaluation) > 0 {
			metadata["evaluation"] = evaluation
			break
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func skillOptTrainNextIterationMode(previousMode string, recommendedMode string) string {
	switch strings.TrimSpace(recommendedMode) {
	case db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate:
		return strings.TrimSpace(recommendedMode)
	default:
		return strings.TrimSpace(previousMode)
	}
}

func continueSkillOptTrainOptimizer(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerResult, error) {
	if strings.TrimSpace(iteration.EvalRunID) == "" {
		return skillOptTrainOptimizerResult{}, fmt.Errorf("train iteration %s has no eval run id", iteration.ID)
	}
	optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	result := skillOptTrainOptimizerResult{
		TrainingPackagePath:  optimizerPaths.TrainingPackagePath,
		OutRoot:              optimizerPaths.OutRoot,
		CandidatePackagePath: optimizerPaths.CandidatePackagePath,
		ArtifactDir:          optimizerPaths.ArtifactDir,
		DryRun:               request.DryRun,
	}
	state := skillopt.NormalizeTrainState(iteration.State)
	if state == skillopt.TrainStateOptimizerCompleted && request.RerunOptimizer {
		state = skillopt.TrainStateTrainingPackageCreated
	}
	if state == skillopt.TrainStateOptimizerCompletedNoCandidate && request.RerunOptimizer {
		session.State = skillopt.TrainStateTrainingPackageCreated
		iteration.State = skillopt.TrainStateTrainingPackageCreated
		state = skillopt.TrainStateTrainingPackageCreated
	}
	if state == skillopt.TrainStateFeedbackSynced {
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateTrainingPackageCreated); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		exportMetadata, err := exportSkillOptTrainPackage(ctx, store, iteration, optimizerPaths)
		if err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		session.State = skillopt.TrainStateTrainingPackageCreated
		iteration.State = skillopt.TrainStateTrainingPackageCreated
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", exportMetadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", exportMetadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		state = skillopt.TrainStateTrainingPackageCreated
	}
	if state == skillopt.TrainStateTrainingPackageCreated {
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptimizerCompleted); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		command, args, err := buildSkillOptTrainOptimizerCommand(iteration, request, optimizerPaths)
		if err != nil {
			if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, command, args, subprocess.Result{}, err); metaErr != nil {
				return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
			}
			return skillOptTrainOptimizerResult{}, err
		}
		result.Command = command
		result.Args = args
		runResult, err := runSkillOptTrainOptimizer(ctx, optimizerPaths, request, command, args)
		if err != nil {
			if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, command, args, runResult, err); metaErr != nil {
				return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
			}
			return skillOptTrainOptimizerResult{}, err
		}
		metadata := skillOptTrainOptimizerMetadata(request, optimizerPaths, command, args, runResult, "succeeded", nil)
		session.State = skillopt.TrainStateOptimizerCompleted
		iteration.State = skillopt.TrainStateOptimizerCompleted
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		state = skillopt.TrainStateOptimizerCompleted
	}
	if state == skillopt.TrainStateOptimizerCompleted {
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateCandidateCreated); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		version, err := importSkillOptTrainCandidate(ctx, paths, store, session, iteration, optimizerPaths)
		if err != nil {
			if errors.Is(err, skillopt.ErrNoCandidate) {
				reason := skillOptNoCandidateReason(err)
				if metaErr := recordSkillOptTrainNoCandidate(ctx, store, session, iteration, optimizerPaths, reason); metaErr != nil {
					return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record no-candidate result: %v", err, metaErr)
				}
				result.NoCandidateReason = reason
				result.NoCandidateNextAction = skillOptNoCandidateNextAction()
				return result, nil
			}
			if metaErr := recordSkillOptTrainCandidateImportFailure(ctx, store, session, iteration, optimizerPaths, err); metaErr != nil {
				return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record candidate import failure: %v", err, metaErr)
			}
			return skillOptTrainOptimizerResult{}, err
		}
		result.CandidateVersionID = version.ID
		metadata := map[string]any{
			"status":            "succeeded",
			"candidate_version": version.ID,
			"candidate_package": optimizerPaths.CandidatePackagePath,
			"artifact_dir":      optimizerPaths.ArtifactDir,
			"completed_at":      time.Now().UTC().Format(time.RFC3339Nano),
			"source":            "gitmoot skillopt train continue",
		}
		session.State = skillopt.TrainStateCandidateCreated
		iteration.State = skillopt.TrainStateCandidateCreated
		iteration.CandidateVersionID = version.ID
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		return result, nil
	}
	return skillOptTrainOptimizerResult{}, fmt.Errorf("train iteration %s is at %s; expected %s, %s, or %s", iteration.ID, iteration.State, skillopt.TrainStateFeedbackSynced, skillopt.TrainStateTrainingPackageCreated, skillopt.TrainStateOptimizerCompleted)
}

type skillOptTrainGenerationResult struct {
	GeneratedOptions int
	JobIDs           []string
	AgentName        string
	Runtime          string
	Metadata         map[string]any
}

var errSkillOptTrainGenerationBusy = errors.New("skillopt train generation is already running")

var errSkillOptTrainOptimizerBusy = errors.New("skillopt train optimizer is already running")

var errSkillOptTrainCandidateReviewBusy = errors.New("skillopt train candidate review is already publishing")

var errSkillOptTrainReviewBusy = errors.New("skillopt train review is already publishing")

var errSkillOptTrainStartNextBusy = errors.New("skillopt train next iteration is already starting")

const skillOptTrainGenerationLockTTL = 2 * time.Hour

const skillOptTrainGenerationLockBuffer = 10 * time.Minute

const skillOptTrainOptimizerLockTTL = 4 * time.Hour

const skillOptTrainOptimizerLockBuffer = 10 * time.Minute

const skillOptTrainCandidateReviewLockTTL = 30 * time.Minute

const skillOptTrainReviewLockTTL = 30 * time.Minute

const skillOptTrainStartNextLockTTL = 30 * time.Minute

func acquireSkillOptTrainCandidateReviewLock(ctx context.Context, store *db.Store, sessionID string, iterationID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainCandidateReviewLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-candidate-review", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainCandidateReviewLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainCandidateReviewBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainCandidateReviewLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-candidate-review:" + sessionID
	}
	return "skillopt-train-candidate-review:" + sessionID + ":" + iterationID
}

func acquireSkillOptTrainReviewLock(ctx context.Context, store *db.Store, sessionID string, iterationID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainReviewLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-review", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainReviewLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainReviewBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainReviewLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-review:" + sessionID
	}
	return "skillopt-train-review:" + sessionID + ":" + iterationID
}

func acquireSkillOptTrainStartNextLock(ctx context.Context, store *db.Store, sessionID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainStartNextLockKey(sessionID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-start-next", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainStartNextLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainStartNextBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainStartNextLockKey(sessionID string) string {
	return "skillopt-train-start-next:" + strings.TrimSpace(sessionID)
}

func acquireSkillOptTrainOptimizerLock(ctx context.Context, store *db.Store, sessionID string, iterationID string, ttl time.Duration) (func(context.Context) error, bool, error) {
	key := skillOptTrainOptimizerLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if ttl <= 0 {
		ttl = skillOptTrainOptimizerLockTTL
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-optimizer", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainOptimizerBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainOptimizerLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-optimizer:" + sessionID
	}
	return "skillopt-train-optimizer:" + sessionID + ":" + iterationID
}

func skillOptTrainOptimizerLockTTLForRequest(request skillOptTrainOptimizerRequest) (time.Duration, error) {
	timeout := strings.TrimSpace(request.Timeout)
	if timeout == "" {
		return skillOptTrainOptimizerLockTTL, nil
	}
	duration, err := time.ParseDuration(timeout)
	if err != nil {
		return 0, fmt.Errorf("parse optimizer timeout: %w", err)
	}
	if duration <= 0 {
		return 0, errors.New("optimizer timeout must be greater than zero")
	}
	ttl := duration + skillOptTrainOptimizerLockBuffer
	if ttl < skillOptTrainOptimizerLockTTL {
		return skillOptTrainOptimizerLockTTL, nil
	}
	return ttl, nil
}

func acquireSkillOptTrainGenerationLock(ctx context.Context, store *db.Store, sessionID string, iterationID string, ttl time.Duration) (func(context.Context) error, bool, error) {
	key := skillOptTrainGenerationLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if ttl <= 0 {
		ttl = skillOptTrainGenerationLockTTL
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-generation", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainGenerationBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func estimateSkillOptTrainGenerationLockTTL(ctx context.Context, store *db.Store, request skillOptTrainContinueRequest, iteration db.SkillOptTrainIteration) (time.Duration, error) {
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("eval run %s not found", iteration.EvalRunID)
		}
		return 0, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return 0, err
	}
	itemCount := len(items)
	if itemCount <= 0 {
		itemCount = 1
	}
	roles := len(skillOptTrainGenerationRoles(run))
	if roles <= 0 {
		roles = 2
	}
	_, dispatchType, err := skillOptTrainGeneratorSelection(request)
	if err != nil {
		return 0, err
	}
	jobTimeout := skillOptTrainGenerationJobTimeoutHint(request, dispatchType)
	concurrency := skillOptTrainGenerationConcurrencyHint(request, dispatchType)
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > itemCount {
		concurrency = itemCount
	}
	batches := (itemCount + concurrency - 1) / concurrency
	estimated := time.Duration(batches*roles)*jobTimeout + skillOptTrainGenerationLockBuffer
	if estimated < skillOptTrainGenerationLockTTL {
		return skillOptTrainGenerationLockTTL, nil
	}
	return estimated, nil
}

func skillOptTrainGenerationJobTimeoutHint(request skillOptTrainContinueRequest, dispatchType string) time.Duration {
	if strings.TrimSpace(dispatchType) == "" {
		return daemonRunningJobStaleAfter
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return daemonRunningJobStaleAfter
	}
	agentType, ok := types[dispatchType]
	if !ok {
		return daemonRunningJobStaleAfter
	}
	jobTimeout, err := time.ParseDuration(agentType.JobTimeout)
	if err != nil || jobTimeout <= 0 {
		return daemonRunningJobStaleAfter
	}
	return jobTimeout
}

func skillOptTrainGenerationConcurrencyHint(request skillOptTrainContinueRequest, dispatchType string) int {
	if strings.TrimSpace(dispatchType) == "" {
		return 1
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return 1
	}
	agentType, ok := types[dispatchType]
	if !ok || agentType.MaxBackground <= 0 {
		return 1
	}
	return agentType.MaxBackground
}

func skillOptTrainGenerationLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-generation:" + sessionID
	}
	return "skillopt-train-generation:" + sessionID + ":" + iterationID
}

func generateSkillOptTrainOptions(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) (skillOptTrainGenerationResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptionsGenerated); err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s not found", iteration.EvalRunID)
		}
		return skillOptTrainGenerationResult{}, err
	}
	rankedRun := skillOptRunUsesRankedOptions(run)
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	if len(items) == 0 {
		return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s has no review items to generate", run.ID)
	}
	roles := skillOptTrainGenerationRoles(run)
	if len(roles) < 2 {
		return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s expects at least 2 options", run.ID)
	}
	existingGenerated := 0
	completeExistingItems := 0
	for _, item := range items {
		if rankedRun {
			existing, err := store.ListEvalReviewOptions(ctx, run.ID, item.ItemID)
			if err != nil {
				return skillOptTrainGenerationResult{}, err
			}
			if len(existing) > 0 {
				if len(existing) == len(roles) {
					existingGenerated += len(existing)
					completeExistingItems++
					continue
				}
				return skillOptTrainGenerationResult{}, fmt.Errorf("item %s has partial generated options; inspect or clear review options before continuing", item.ItemID)
			}
			continue
		}
		hasBaseline := strings.TrimSpace(item.BaselineArtifactID) != ""
		hasCandidate := strings.TrimSpace(item.CandidateArtifactID) != ""
		if hasBaseline || hasCandidate {
			if hasBaseline && hasCandidate {
				existingGenerated += 2
				completeExistingItems++
				continue
			}
			return skillOptTrainGenerationResult{}, fmt.Errorf("item %s has partial generated A/B artifacts; inspect or clear review item artifacts before continuing", item.ItemID)
		}
	}
	if completeExistingItems > 0 {
		if completeExistingItems == len(items) {
			metadata := map[string]any{
				"status":            "recovered",
				"generated_options": existingGenerated,
				"strategy":          skillOptTrainGenerationStrategy(run),
				"completed_at":      time.Now().UTC().Format(time.RFC3339Nano),
			}
			return skillOptTrainGenerationResult{
				GeneratedOptions: existingGenerated,
				Metadata:         metadata,
			}, nil
		}
		return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s has partial generated items; inspect or clear review artifacts before continuing", run.ID)
	}
	dispatchAgent, dispatchType, err := skillOptTrainGeneratorSelection(request)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	concurrency, err := skillOptTrainGenerationConcurrency(request, dispatchType)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	if err := ensureSkillOptTrainGenerationRepoReady(ctx, store, skillOptTrainGenerationRepo(session)); err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	generatedItems, err := generateSkillOptTrainItemOptions(ctx, store, blobStore, session, iteration, run, items, roles, rankedRun, request, dispatchAgent, dispatchType, concurrency)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	writes := make([]db.EvalReviewGenerationWrite, 0, len(generatedItems))
	generated := 0
	jobIDs := []string{}
	var generatorAgent string
	var generatorRuntime string
	promptRecords := []map[string]any{}
	for _, item := range generatedItems {
		generated += len(item.Artifacts)
		jobIDs = append(jobIDs, item.JobIDs...)
		if generatorAgent == "" {
			generatorAgent = item.AgentName
		}
		if generatorRuntime == "" {
			generatorRuntime = item.Runtime
		}
		promptRecords = append(promptRecords, item.Prompts...)
		writes = append(writes, db.EvalReviewGenerationWrite{
			ItemID:     item.ItemID,
			ReviewItem: item.ReviewItem,
			Artifacts:  item.Artifacts,
			Options:    item.Options,
		})
	}
	if err := store.ReplaceGeneratedEvalReviewArtifacts(ctx, run.ID, writes); err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	metadata := map[string]any{
		"status":            "succeeded",
		"generated_options": generated,
		"jobs":              jobIDs,
		"agent":             generatorAgent,
		"runtime":           generatorRuntime,
		"concurrency":       concurrency,
		"lock_ttl":          request.GenerationLockTTL.String(),
		"strategy":          skillOptTrainGenerationStrategy(run),
		"prompts":           promptRecords,
		"completed_at":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	return skillOptTrainGenerationResult{
		GeneratedOptions: generated,
		JobIDs:           jobIDs,
		AgentName:        generatorAgent,
		Runtime:          generatorRuntime,
		Metadata:         metadata,
	}, nil
}

type skillOptTrainGeneratedItemOptions struct {
	ItemID     string
	ReviewItem *db.EvalReviewItem
	Artifacts  []db.EvalArtifact
	Options    []db.EvalReviewOption
	JobIDs     []string
	AgentName  string
	Runtime    string
	Prompts    []map[string]any
}

func generateSkillOptTrainItemOptions(ctx context.Context, store *db.Store, blobStore artifact.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, items []db.EvalReviewItem, roles []string, rankedRun bool, request skillOptTrainContinueRequest, dispatchAgent string, dispatchType string, concurrency int) ([]skillOptTrainGeneratedItemOptions, error) {
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(items) {
		concurrency = len(items)
	}
	results := make([]skillOptTrainGeneratedItemOptions, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for index, item := range items {
		index := index
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[index] = ctx.Err()
				return
			}
			result, err := generateSkillOptTrainSingleItemOptions(ctx, store, blobStore, session, iteration, run, item, roles, rankedRun, request, dispatchAgent, dispatchType)
			if err != nil {
				errs[index] = err
				return
			}
			results[index] = result
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func generateSkillOptTrainSingleItemOptions(ctx context.Context, store *db.Store, blobStore artifact.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, roles []string, rankedRun bool, request skillOptTrainContinueRequest, dispatchAgent string, dispatchType string) (skillOptTrainGeneratedItemOptions, error) {
	generatedItem := skillOptTrainGeneratedItemOptions{ItemID: item.ItemID}
	replacementOptions := make([]db.EvalReviewOption, 0, len(roles))
	artifactRecords := make([]db.EvalArtifact, 0, len(roles))
	wantsVuePreviewBundle := skillOptTrainWantsVuePreviewBundle(session)
	requiresVuePreviewBundle := skillOptTrainRequiresVuePreviewBundle(session)
	for _, role := range roles {
		prompt := buildSkillOptTrainGenerationPrompt(session, iteration, run, item, role, rankedRun)
		output, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
			RepoFlag:         skillOptTrainGenerationRepo(session),
			Agent:            dispatchAgent,
			Action:           "ask",
			Instructions:     prompt,
			Type:             dispatchType,
			Home:             request.Home,
			AllowManagedSync: dispatchType != "",
		})
		if err != nil {
			return skillOptTrainGeneratedItemOptions{}, fmt.Errorf("generate %s for %s: %w", role, item.ItemID, err)
		}
		if output.Result == nil {
			return skillOptTrainGeneratedItemOptions{}, fmt.Errorf("generate %s for %s: job %s did not return a result", role, item.ItemID, output.JobID)
		}
		if output.Result.Decision != "implemented" {
			return skillOptTrainGeneratedItemOptions{}, fmt.Errorf("generate %s for %s: job %s returned %s, want implemented: %s", role, item.ItemID, output.JobID, output.Result.Decision, output.Result.Summary)
		}
		artifactRole := role
		if rankedRun {
			artifactRole = "option-" + role
		}
		artifactContent := []byte(output.Result.Summary)
		artifactMediaType := "text/markdown"
		artifactDriver := "text"
		var previewBundleMetadata *skillopt.PreviewBundleMetadata
		if wantsVuePreviewBundle {
			bundle, err := skillopt.ParsePreviewBundle([]byte(output.Result.Summary))
			if err != nil {
				if requiresVuePreviewBundle {
					return skillOptTrainGeneratedItemOptions{}, fmt.Errorf("generate %s for %s: preview bundle: %w", role, item.ItemID, err)
				}
			} else {
				artifactContent, err = json.Marshal(bundle)
				if err != nil {
					return skillOptTrainGeneratedItemOptions{}, fmt.Errorf("generate %s for %s: encode preview bundle: %w", role, item.ItemID, err)
				}
				metadata := bundle.Metadata()
				previewBundleMetadata = &metadata
				artifactMediaType = "application/json"
				artifactDriver = skillopt.TrainPreviewRendererVueVite
			}
		}
		artifactRecord, err := prepareReviewItemContentArtifact(blobStore, run.ID, item.ItemID, artifactRole, artifactContent, artifactMediaType, artifactDriver)
		if err != nil {
			return skillOptTrainGeneratedItemOptions{}, err
		}
		artifactRecords = append(artifactRecords, artifactRecord)
		optionMetadata := skillOptTrainGeneratedOptionMetadata(output, prompt, previewBundleMetadata)
		if rankedRun {
			replacementOptions = append(replacementOptions, db.EvalReviewOption{
				RunID:        run.ID,
				ItemID:       item.ItemID,
				Label:        role,
				ArtifactID:   artifactRecord.ID,
				Role:         "option",
				MetadataJSON: optionMetadata,
			})
		} else if role == "baseline" {
			item.BaselineArtifactID = artifactRecord.ID
		} else if role == "candidate" {
			item.CandidateArtifactID = artifactRecord.ID
		}
		generatedItem.JobIDs = append(generatedItem.JobIDs, output.JobID)
		if generatedItem.AgentName == "" {
			generatedItem.AgentName = output.Agent
		}
		if generatedItem.Runtime == "" {
			if agent, err := store.GetAgent(ctx, output.Agent); err == nil {
				generatedItem.Runtime = agent.Runtime
			}
		}
		generatedItem.Prompts = append(generatedItem.Prompts, map[string]any{
			"item_id": item.ItemID,
			"role":    role,
			"job_id":  output.JobID,
			"prompt":  prompt,
		})
	}
	generatedItem.Artifacts = artifactRecords
	generatedItem.Options = replacementOptions
	if !rankedRun {
		generatedItem.ReviewItem = &item
	}
	return generatedItem, nil
}

func skillOptTrainWantsVuePreviewBundle(session db.SkillOptTrainSession) bool {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	return policy.Mode != skillopt.TrainPreviewModeNone && policy.Renderer == skillopt.TrainPreviewRendererVueVite
}

func skillOptTrainRequiresVuePreviewBundle(session db.SkillOptTrainSession) bool {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	return policy.Mode == skillopt.TrainPreviewModeRequired && policy.Renderer == skillopt.TrainPreviewRendererVueVite
}

type skillOptTrainReviewPublishResult struct {
	Repo        github.Repository
	IssueNumber int64
	URL         string
	PreviewURLs int
}

func publishSkillOptTrainReview(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (skillOptTrainReviewPublishResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateReviewPublished); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	if strings.TrimSpace(iteration.EvalRunID) == "" {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("train iteration %s has no eval run id", iteration.ID)
	}
	if recovered, ok, err := recoverSkillOptTrainReviewPublication(ctx, paths, store, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	} else if ok {
		return recovered, nil
	}
	previewURLs, err := publishSkillOptTrainPreviewURLs(ctx, paths, store, session, iteration)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	repo, err := resolveSkillOptFeedbackRepo(ctx, paths, store, run, "")
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	publishingMetadata := map[string]any{
		"status":       "publishing",
		"repo":         repo.FullName(),
		"preview_urls": previewURLs,
		"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, publishingMetadata); err != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("write review pre-publish recovery marker: %w", err)
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", publishingMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", publishingMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	collector := feedback.GitHubCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
	body, err := collector.Body(ctx, store, run.ID)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	client := newSkillOptGitHubClient()
	postingMetadata := make(map[string]any, len(publishingMetadata)+2)
	for key, value := range publishingMetadata {
		postingMetadata[key] = value
	}
	postingMetadata["status"] = "posting_external"
	postingMetadata["external_post_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, postingMetadata); err != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("write review external-post recovery marker: %w", err)
	}
	issue, err := client.CreateIssue(ctx, github.CreateIssueInput{
		Repo:  repo,
		Title: fmt.Sprintf("Gitmoot SkillOpt feedback: %s", strings.TrimSpace(run.ID)),
		Body:  body,
	})
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	published := feedback.GitHubPublishResult{Repo: repo, IssueNumber: issue.Number, URL: issue.URL, Mode: "issue"}
	externalMetadata := map[string]any{
		"status":       "published_external",
		"repo":         published.Repo.FullName(),
		"issue_number": published.IssueNumber,
		"url":          published.URL,
		"preview_urls": previewURLs,
		"published_at": time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	externalMarkerErr := writeSkillOptTrainReviewRecovery(paths, session, iteration, externalMetadata)
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	iteration.IssueRepo = published.Repo.FullName()
	iteration.IssueNumber = published.IssueNumber
	iteration.IssueURL = published.URL
	dbMetadata := make(map[string]any, len(externalMetadata))
	for key, value := range externalMetadata {
		dbMetadata[key] = value
	}
	dbMetadata["status"] = "published"
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", dbMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", dbMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		if externalMarkerErr != nil {
			return skillOptTrainReviewPublishResult{}, fmt.Errorf("%w; review was published at %s but recovery marker write failed: %v", err, published.URL, externalMarkerErr)
		}
		return skillOptTrainReviewPublishResult{}, err
	}
	if externalMarkerErr != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("review was published at %s and recorded in local state but recovery marker write failed: %w", published.URL, externalMarkerErr)
	}
	_ = removeSkillOptTrainReviewRecovery(paths, session, iteration)
	return skillOptTrainReviewPublishResult{Repo: published.Repo, IssueNumber: published.IssueNumber, URL: published.URL, PreviewURLs: previewURLs}, nil
}

func recoverSkillOptTrainReviewPublication(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (skillOptTrainReviewPublishResult, bool, error) {
	review, ok, err := readSkillOptTrainReviewRecovery(paths, session, iteration)
	if err != nil || !ok {
		return skillOptTrainReviewPublishResult{}, ok, err
	}
	status := metadataString(review, "status")
	if status == "posting_external" {
		return skillOptTrainReviewPublishResult{}, true, errors.New("train review publication was interrupted after the external GitHub post started; inspect the review repo before retrying to avoid duplicate issues")
	}
	if status != "published_external" {
		return skillOptTrainReviewPublishResult{}, false, nil
	}
	repo, err := daemon.ParseRepository(metadataString(review, "repo"))
	if err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	issueNumber := int64(metadataNumber(review, "issue_number"))
	url := metadataString(review, "url")
	previewURLs := metadataNumber(review, "preview_urls")
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	iteration.IssueRepo = repo.FullName()
	iteration.IssueNumber = issueNumber
	iteration.IssueURL = url
	metadata := map[string]any{
		"status":       "published",
		"repo":         repo.FullName(),
		"issue_number": issueNumber,
		"url":          url,
		"preview_urls": previewURLs,
		"published_at": time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", metadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	_ = removeSkillOptTrainReviewRecovery(paths, session, iteration)
	return skillOptTrainReviewPublishResult{Repo: repo, IssueNumber: issueNumber, URL: url, PreviewURLs: previewURLs}, true, nil
}

func writeSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, metadata map[string]any) error {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return errors.New("review recovery path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmpPath, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func readSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (map[string]any, bool, error) {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil, false, nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(content, &metadata); err != nil {
		return nil, true, fmt.Errorf("read review recovery marker %s: %w", path, err)
	}
	return metadata, true, nil
}

func removeSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) error {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func skillOptTrainReviewRecoveryPath(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) string {
	if strings.TrimSpace(paths.Home) == "" {
		return ""
	}
	name := skillOptCandidateReviewRecoveryName(session.ID, iteration.ID)
	if name == "" {
		return ""
	}
	return filepath.Join(paths.Home, "skillopt", "reviews", name+".json")
}

func publishSkillOptTrainPreviewURLs(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (int, error) {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	if policy.Mode == skillopt.TrainPreviewModeNone || policy.Renderer == skillopt.TrainPreviewRendererNone || policy.Publisher == skillopt.TrainPreviewPublisherNone {
		return 0, nil
	}
	if policy.Renderer != skillopt.TrainPreviewRendererVueVite || policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, fmt.Errorf("preview renderer %s with publisher %s is not implemented", policy.Renderer, policy.Publisher)
		}
		return 0, nil
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		return 0, err
	}
	options, err := store.ListEvalReviewOptions(ctx, run.ID, "")
	if err != nil {
		return 0, err
	}
	if len(options) == 0 {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, fmt.Errorf("train run %s has no generated options to publish previews", run.ID)
		}
		return 0, nil
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	previewRepo, err := previewRepoRecord(ctx, store, policy)
	if err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, err
		}
		return convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, options)
	}
	if err := requireCleanPreviewRepo(ctx, previewRepo.CheckoutPath); err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, err
		}
		return convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, options)
	}
	publishedCount := 0
	for _, option := range options {
		metadata := optionMetadataMap(option.MetadataJSON)
		if metadataStringValue(metadata, "preview_url") != "" || metadataStringValue(metadata, "url") != "" {
			publishedCount++
			continue
		}
		if metadata["preview_bundle"] == nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s is missing preview bundle metadata", option.ItemID, option.Label)
			}
			continue
		}
		record, err := store.GetEvalArtifact(ctx, option.ArtifactID)
		if err != nil {
			return publishedCount, fmt.Errorf("item %s option %s artifact: %w", option.ItemID, option.Label, err)
		}
		content, err := blobStore.Read(record.Hash)
		if err != nil {
			return publishedCount, fmt.Errorf("item %s option %s preview bundle blob: %w", option.ItemID, option.Label, err)
		}
		bundle, err := skillopt.ParsePreviewBundle(content)
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s preview bundle: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		distDir, cleanup, err := renderVueVitePreviewBundle(ctx, bundle)
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s render preview: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		route, err := skillOptPreviewRoute(policy.RouteTemplate, run.ID, option.ItemID, option.Label)
		if err != nil {
			_ = cleanup()
			return publishedCount, err
		}
		url, err := publishGitHubPagesPreview(ctx, previewRepo, route, distDir)
		_ = cleanup()
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s publish preview: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		metadata["preview_url"] = url
		metadata["preview_route"] = route
		metadata["preview_repo"] = previewRepo.FullName()
		option.MetadataJSON = encodeOptionMetadata(metadata)
		if err := store.UpsertEvalReviewOption(ctx, option); err != nil {
			return publishedCount, err
		}
		publishedCount++
	}
	if policy.Mode == skillopt.TrainPreviewModeRequired && publishedCount != len(options) {
		return publishedCount, fmt.Errorf("required preview run has %d/%d preview URLs", publishedCount, len(options))
	}
	if policy.Mode == skillopt.TrainPreviewModeOptional {
		freshOptions, err := store.ListEvalReviewOptions(ctx, run.ID, "")
		if err != nil {
			return publishedCount, err
		}
		if _, err := convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, freshOptions); err != nil {
			return publishedCount, err
		}
	}
	return publishedCount, nil
}

func convertOptionalPreviewBundlesToFallback(ctx context.Context, store *db.Store, blobStore artifact.Store, options []db.EvalReviewOption) (int, error) {
	for _, option := range options {
		metadata := optionMetadataMap(option.MetadataJSON)
		if metadata["preview_bundle"] == nil || metadataStringValue(metadata, "preview_url") != "" || metadataStringValue(metadata, "url") != "" {
			continue
		}
		record, err := store.GetEvalArtifact(ctx, option.ArtifactID)
		if err != nil {
			return 0, err
		}
		content, err := blobStore.Read(record.Hash)
		if err != nil {
			return 0, err
		}
		bundle, err := skillopt.ParsePreviewBundle(content)
		if err != nil {
			return 0, err
		}
		fallbackContent := []byte(previewBundleInlineFallback(bundle))
		blob, err := blobStore.Put(fallbackContent)
		if err != nil {
			return 0, err
		}
		record.Hash = blob.Hash
		record.MediaType = "text/markdown"
		record.SizeBytes = blob.Size
		record.Driver = "text"
		if err := store.UpsertEvalArtifact(ctx, record); err != nil {
			return 0, err
		}
		delete(metadata, "preview_bundle")
		metadata["preview_fallback"] = "inline"
		metadata["preview_fallback_renderer"] = skillopt.TrainPreviewRendererVueVite
		option.MetadataJSON = encodeOptionMetadata(metadata)
		if err := store.UpsertEvalReviewOption(ctx, option); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func previewBundleInlineFallback(bundle skillopt.PreviewBundle) string {
	var builder strings.Builder
	builder.WriteString("# Vue/Vite preview source\n\n")
	builder.WriteString("A clickable preview was not available, so this option is included as inline Vue/Vite source for review.\n\n")
	for _, file := range bundle.Files {
		fmt.Fprintf(&builder, "## `%s`\n\n", file.Path)
		writeSkillOptMarkdownFence(&builder, file.Content)
		builder.WriteString("\n")
	}
	return builder.String()
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

func previewRepoRecord(ctx context.Context, store *db.Store, policy skillopt.TrainPreviewPolicy) (db.Repo, error) {
	repoName := strings.TrimSpace(policy.Repo)
	if repoName == "" {
		return db.Repo{}, errors.New("preview repo is required")
	}
	repo, err := store.GetRepo(ctx, repoName)
	if err != nil {
		return db.Repo{}, fmt.Errorf("preview repo %s is not registered with a checkout path; run `gitmoot repo add %s --path /path/to/checkout`: %w", repoName, repoName, err)
	}
	if strings.TrimSpace(repo.CheckoutPath) == "" {
		return db.Repo{}, fmt.Errorf("preview repo %s has no checkout path; run `gitmoot repo add %s --path /path/to/checkout`", repoName, repoName)
	}
	if _, err := os.Stat(repo.CheckoutPath); err != nil {
		return db.Repo{}, fmt.Errorf("preview repo %s checkout is not ready: %w", repo.FullName(), err)
	}
	return repo, nil
}

func requireCleanPreviewRepo(ctx context.Context, checkout string) error {
	result, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("check preview repo status: %w", err)
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("preview repo checkout %s is dirty; commit or clean it before publishing previews", checkout)
	}
	return nil
}

func renderVueVitePreviewBundle(ctx context.Context, bundle skillopt.PreviewBundle) (string, func() error, error) {
	workDir, err := os.MkdirTemp("", "gitmoot-vue-preview-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.RemoveAll(workDir) }
	for _, file := range bundle.Files {
		target, err := safeJoinPreviewPath(workDir, file.Path)
		if err != nil {
			_ = cleanup()
			return "", nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = cleanup()
			return "", nil, err
		}
		if err := os.WriteFile(target, []byte(file.Content), 0o644); err != nil {
			_ = cleanup()
			return "", nil, err
		}
	}
	if err := writeTrustedVueViteScaffold(workDir); err != nil {
		_ = cleanup()
		return "", nil, err
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, workDir, "npm", "install", "--ignore-scripts"); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("npm install: %w", err)
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, workDir, "npm", "run", "build"); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("%s: %w", bundle.BuildCommand, err)
	}
	distDir, err := safeJoinPreviewPath(workDir, bundle.DistDir)
	if err != nil {
		_ = cleanup()
		return "", nil, err
	}
	if _, err := os.Stat(filepath.Join(distDir, "index.html")); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("preview build output missing index.html in %s: %w", bundle.DistDir, err)
	}
	return distDir, cleanup, nil
}

func publishGitHubPagesPreview(ctx context.Context, repo db.Repo, route string, distDir string) (string, error) {
	checkout := strings.TrimSpace(repo.CheckoutPath)
	if err := requireCleanPreviewRepo(ctx, checkout); err != nil {
		return "", err
	}
	head, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read preview repo head: %w", err)
	}
	headSHA := strings.TrimSpace(head.Stdout)
	target, err := safeJoinPreviewPath(checkout, route)
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := copyDir(distDir, target); err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return "", err
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "add", "--", route); err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return "", fmt.Errorf("git add preview route: %w", err)
	}
	status, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "status", "--porcelain", "--", route)
	if err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return "", fmt.Errorf("check preview route status: %w", err)
	}
	if strings.TrimSpace(status.Stdout) != "" {
		if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "commit", "-m", "Publish SkillOpt preview "+strings.TrimSuffix(route, "/")); err != nil {
			restorePreviewRoute(ctx, checkout, route, headSHA)
			return "", fmt.Errorf("git commit preview route: %w", err)
		}
		if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "push"); err != nil {
			restorePreviewRoute(ctx, checkout, route, headSHA)
			return "", fmt.Errorf("git push preview route: %w", err)
		}
	}
	return githubPagesURL(repo, route), nil
}

func restorePreviewRoute(ctx context.Context, checkout string, route string, headSHA string) {
	if strings.TrimSpace(headSHA) != "" {
		_, _ = skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "reset", "--hard", headSHA)
	}
	_, _ = skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "clean", "-fd", "--", route)
}

func skillOptPreviewRoute(template string, runID string, itemID string, label string) (string, error) {
	if strings.TrimSpace(template) == "" {
		template = skillopt.DefaultTrainPreviewRouteTemplate
	}
	route := strings.ReplaceAll(template, "{run_id}", previewRouteSlug(runID))
	route = strings.ReplaceAll(route, "{item_id}", previewRouteSlug(itemID))
	route = strings.ReplaceAll(route, "{option_label}", previewRouteSlug(label))
	route = strings.TrimSpace(route)
	if route == "" {
		return "", errors.New("preview route is required")
	}
	route = strings.TrimPrefix(route, "/")
	clean := filepath.ToSlash(filepath.Clean(route))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
		return "", fmt.Errorf("preview route %q is unsafe", route)
	}
	if !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	return clean, nil
}

func safeJoinPreviewPath(root string, relative string) (string, error) {
	root = filepath.Clean(root)
	relative = filepath.FromSlash(strings.TrimSpace(relative))
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("preview path %q must be relative", relative)
	}
	target := filepath.Clean(filepath.Join(root, relative))
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("preview path %q must stay inside %s", relative, root)
	}
	return target, nil
}

func copyDir(source string, target string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		if entry.IsDir() {
			if err := copyDir(sourcePath, targetPath); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(sourcePath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(targetPath, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func githubPagesURL(repo db.Repo, route string) string {
	if strings.EqualFold(repo.Name, repo.Owner+".github.io") {
		return fmt.Sprintf("https://%s.github.io/%s", repo.Owner, strings.TrimLeft(route, "/"))
	}
	return fmt.Sprintf("https://%s.github.io/%s/%s", repo.Owner, repo.Name, strings.TrimLeft(route, "/"))
}

func writeTrustedVueViteScaffold(workDir string) error {
	packageJSON := `{"type":"module","scripts":{"build":"vite build"},"dependencies":{"@vitejs/plugin-vue":"latest","vite":"latest","vue":"latest"}}`
	if err := os.WriteFile(filepath.Join(workDir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		return err
	}
	indexHTML := `<div id="app"></div><script type="module" src="/src/main.js"></script>`
	if err := os.WriteFile(filepath.Join(workDir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		return err
	}
	mainPath := filepath.Join(workDir, "src", "main.js")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o755); err != nil {
		return err
	}
	mainJS := `import { createApp } from 'vue'; import App from './App.vue'; createApp(App).mount('#app');`
	if err := os.WriteFile(mainPath, []byte(mainJS), 0o644); err != nil {
		return err
	}
	viteConfig := "import { defineConfig } from 'vite';\nimport vue from '@vitejs/plugin-vue';\n\nexport default defineConfig({ base: './', plugins: [vue()] });\n"
	return os.WriteFile(filepath.Join(workDir, "vite.config.js"), []byte(viteConfig), 0o644)
}

func previewRouteSlug(value string) string {
	trimmed := strings.TrimSpace(value)
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(trimmed) {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if allowed {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(builder.String(), ".-_")
	if slug == "" {
		slug = "value"
	}
	if slug != trimmed {
		slug = slug + "-" + shortHash(trimmed)
	}
	return slug
}

func optionMetadataMap(metadataJSON string) map[string]any {
	metadata := map[string]any{}
	if strings.TrimSpace(metadataJSON) != "" {
		_ = json.Unmarshal([]byte(metadataJSON), &metadata)
	}
	return metadata
}

func metadataStringValue(metadata map[string]any, key string) string {
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func metadataNumber(metadata map[string]any, key string) int {
	switch value := metadata[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	default:
		return 0
	}
}

func encodeOptionMetadata(metadata map[string]any) string {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
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
		ctx := context.Background()
		session, err := store.GetSkillOptTrainSession(ctx, strings.TrimSpace(*sessionID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("train session %s not found", strings.TrimSpace(*sessionID))
			}
			return err
		}
		iteration, err := store.GetLatestSkillOptTrainIteration(ctx, session.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("train session %s has no iteration to stop", session.ID)
			}
			return err
		}
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateRunAbandoned); err != nil {
			return err
		}
		session.State = skillopt.TrainStateRunAbandoned
		session.MetadataJSON = skillOptTrainDecisionMetadata(session.MetadataJSON, *reason)
		iteration.State = skillopt.TrainStateRunAbandoned
		iteration.DecisionReason = strings.TrimSpace(*reason)
		iteration.MetadataJSON = skillOptTrainDecisionMetadata(iteration.MetadataJSON, *reason)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
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

func loadSkillOptTrainStatus(ctx context.Context, store *db.Store, sessionID string) (db.SkillOptTrainSession, *db.SkillOptTrainIteration, skillopt.TrainStatusCounts, error) {
	session, err := store.GetSkillOptTrainSession(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, fmt.Errorf("train session %s not found", strings.TrimSpace(sessionID))
		}
		return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, err
	}
	latest, err := store.GetLatestSkillOptTrainIteration(ctx, session.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return session, nil, skillopt.TrainStatusCounts{}, nil
		}
		return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, err
	}
	counts, err := loadSkillOptTrainStatusCounts(ctx, store, latest.EvalRunID)
	if err != nil {
		return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, err
	}
	return session, &latest, counts, nil
}

func loadSkillOptTrainStatusCounts(ctx context.Context, store *db.Store, runID string) (skillopt.TrainStatusCounts, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return skillopt.TrainStatusCounts{}, nil
	}
	items, err := store.ListEvalReviewItems(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	feedbackEvents, err := store.ListFeedbackEvents(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	rankedFeedbackEvents, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	return skillopt.TrainStatusCounts{
		ReviewItems:          len(items),
		FeedbackEvents:       len(feedbackEvents),
		RankedFeedbackEvents: len(rankedFeedbackEvents),
		PairwisePreferences:  len(pairwisePreferences),
	}, nil
}

func printSkillOptTrainStatus(stdout io.Writer, summary skillopt.TrainStatusSummary, counts skillopt.TrainStatusCounts) {
	writeLine(stdout, "session: %s", summary.SessionID)
	writeLine(stdout, "iteration: %s", emptyText(summary.IterationID))
	writeLine(stdout, "preview_mode: %s", summary.PreviewPolicy.Mode)
	writeLine(stdout, "preview_renderer: %s", summary.PreviewPolicy.Renderer)
	writeLine(stdout, "preview_publisher: %s", summary.PreviewPolicy.Publisher)
	writeLine(stdout, "preview_repo: %s", emptyText(summary.PreviewPolicy.Repo))
	writeLine(stdout, "preview_route_template: %s", emptyText(summary.PreviewPolicy.RouteTemplate))
	writeLine(stdout, "expected_review_repo: %s", emptyText(summary.PreviewPolicy.ExpectedReviewRepo))
	writeLine(stdout, "current_phase: %s", summary.CurrentPhase)
	writeLine(stdout, "completed_steps: %s", strings.Join(summary.CompletedSteps, ","))
	writeLine(stdout, "blocked_step: %s", emptyText(summary.BlockedStep))
	writeLine(stdout, "next_action: %s", summary.NextAction)
	writeLine(stdout, "issue: %s", emptyText(summary.IssueURL))
	writeLine(stdout, "pull_request: %s", emptyText(summary.PullRequestURL))
	writeLine(stdout, "candidate: %s", emptyText(summary.CandidateVersion))
	writeLine(stdout, "review_items: %d", counts.ReviewItems)
	writeLine(stdout, "feedback: %d", summary.FeedbackCount)
	writeLine(stdout, "pairwise_preferences: %d", counts.PairwisePreferences)
}

func readSkillOptTrainRequest(requestText string, requestFile string) (string, error) {
	requestText = strings.TrimSpace(requestText)
	requestFile = strings.TrimSpace(requestFile)
	if requestText != "" && requestFile != "" {
		return "", errors.New("use only one of --request or --request-file")
	}
	if requestFile == "" {
		return requestText, nil
	}
	content, err := os.ReadFile(requestFile)
	if err != nil {
		return "", fmt.Errorf("read request-file: %w", err)
	}
	return strings.TrimSpace(string(content)), nil
}

type skillOptTrainItemsFile struct {
	Items []skillOptTrainItemPlan `json:"items" yaml:"items"`
}

type skillOptTrainItemPlan struct {
	ItemID         string   `json:"item_id" yaml:"item_id"`
	Title          string   `json:"title" yaml:"title"`
	Brief          string   `json:"brief" yaml:"brief"`
	TargetAudience string   `json:"target_audience" yaml:"target_audience"`
	OutputType     string   `json:"output_type" yaml:"output_type"`
	ArtifactHints  []string `json:"artifact_hints" yaml:"artifact_hints"`
}

func readSkillOptTrainItems(path string) ([]skillOptTrainItemPlan, []string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil, errors.New("skillopt train start requires --items-file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read items-file: %w", err)
	}
	var wrapped skillOptTrainItemsFile
	wrappedErr := yaml.Unmarshal(content, &wrapped)
	items := wrapped.Items
	if wrappedErr != nil && len(items) > 0 {
		return nil, nil, fmt.Errorf("decode items-file: %w", wrappedErr)
	}
	if len(items) == 0 {
		var direct []skillOptTrainItemPlan
		if err := yaml.Unmarshal(content, &direct); err != nil {
			if wrappedErr != nil {
				return nil, nil, fmt.Errorf("decode items-file: %w", wrappedErr)
			}
			return nil, nil, fmt.Errorf("decode items-file: %w", err)
		}
		items = direct
	}
	normalized := make([]skillOptTrainItemPlan, 0, len(items))
	seen := map[string]struct{}{}
	for index, item := range items {
		item.ItemID = strings.TrimSpace(item.ItemID)
		if item.ItemID == "" {
			item.ItemID = fmt.Sprintf("item-%03d", index+1)
		}
		item.Title = strings.TrimSpace(item.Title)
		item.Brief = strings.TrimSpace(item.Brief)
		item.TargetAudience = strings.TrimSpace(item.TargetAudience)
		item.OutputType = strings.TrimSpace(item.OutputType)
		item.ArtifactHints = trimStringSlice(item.ArtifactHints)
		if item.Title == "" {
			return nil, nil, fmt.Errorf("items-file item %s title is required", item.ItemID)
		}
		if item.Brief == "" {
			return nil, nil, fmt.Errorf("items-file item %s brief is required", item.ItemID)
		}
		if item.OutputType == "" {
			return nil, nil, fmt.Errorf("items-file item %s output_type is required", item.ItemID)
		}
		if _, exists := seen[item.ItemID]; exists {
			return nil, nil, fmt.Errorf("items-file item id %q is duplicated", item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		normalized = append(normalized, item)
	}
	return normalized, nil, nil
}

func detectSkillOptTrainItemWarnings(items []skillOptTrainItemPlan) []string {
	warnings := []string{}
	titleCounts := map[string]int{}
	briefCounts := map[string]int{}
	distinctTerms := map[string]struct{}{}
	for _, item := range items {
		titleCounts[strings.ToLower(item.Title)]++
		briefCounts[strings.ToLower(item.Brief)]++
		for _, term := range skillOptTrainItemTerms(item.Title + " " + item.Brief + " " + item.OutputType) {
			distinctTerms[term] = struct{}{}
		}
	}
	for title, count := range titleCounts {
		if title != "" && count > 1 {
			warnings = append(warnings, fmt.Sprintf("duplicate item title %q appears %d times", title, count))
		}
	}
	for brief, count := range briefCounts {
		if brief != "" && count > 1 {
			warnings = append(warnings, fmt.Sprintf("duplicate item brief %q appears %d times", brief, count))
		}
	}
	if len(items) >= 3 && len(distinctTerms) < len(items)*2 {
		warnings = append(warnings, "training items look homogeneous; add more distinct products, audiences, formats, or constraints for stronger feedback")
	}
	return warnings
}

func detectSkillOptTrainPreviewWarnings(policy skillopt.TrainPreviewPolicy) []string {
	if policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages || strings.TrimSpace(policy.Repo) == "" {
		return nil
	}
	return []string{"preview repo must be public or GitHub Pages-enabled before clickable demos can be published"}
}

func skillOptTrainItemTerms(value string) []string {
	parts := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	terms := make([]string, 0, len(parts))
	stop := map[string]struct{}{
		"and": {}, "for": {}, "the": {}, "with": {}, "that": {}, "this": {}, "from": {}, "into": {},
		"page": {}, "build": {}, "create": {}, "make": {}, "write": {}, "design": {}, "output": {},
	}
	for _, part := range parts {
		if len(part) < 4 {
			continue
		}
		if _, skip := stop[part]; skip {
			continue
		}
		terms = append(terms, part)
	}
	return terms
}

func skillOptTrainItemMetadata(item skillOptTrainItemPlan) string {
	metadata := map[string]any{
		"brief":           item.Brief,
		"target_audience": item.TargetAudience,
		"output_type":     item.OutputType,
		"artifact_hints":  item.ArtifactHints,
		"source":          "gitmoot skillopt train start",
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func trimStringSlice(values []string) []string {
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			output = append(output, value)
		}
	}
	return output
}

func parseOptionalSkillOptTrainRepo(name string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	repo, err := daemon.ParseRepository(value)
	if err != nil {
		return "", fmt.Errorf("--%s: %w", name, err)
	}
	return repo.FullName(), nil
}

func normalizeSkillOptTrainTaskKind(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "custom"
	}
	switch value {
	case "correctness", "ux", "design", "writing", "data", "custom":
		return value, nil
	default:
		return "", fmt.Errorf("task kind %q is not supported", value)
	}
}

func normalizeSkillOptTrainMode(mode string, explorationLevel string) (string, string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = db.EvalRunModeExplore
	}
	switch mode {
	case db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate:
	default:
		return "", "", fmt.Errorf("train mode %q is not supported", mode)
	}
	explorationLevel = strings.ToLower(strings.TrimSpace(explorationLevel))
	if explorationLevel == "" {
		switch mode {
		case db.EvalRunModeExplore:
			explorationLevel = db.ExplorationLevelHigh
		case db.EvalRunModeRefine:
			explorationLevel = db.ExplorationLevelMedium
		default:
			explorationLevel = db.ExplorationLevelLow
		}
	}
	switch explorationLevel {
	case db.ExplorationLevelHigh, db.ExplorationLevelMedium, db.ExplorationLevelLow:
		return mode, explorationLevel, nil
	default:
		return "", "", fmt.Errorf("exploration level %q is not supported", explorationLevel)
	}
}

func normalizeSkillOptPreferredGate(value string, taskKind string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		switch taskKind {
		case "correctness", "data":
			value = "hard"
		case "ux", "design", "writing":
			value = "soft"
		default:
			value = "hard_then_soft"
		}
	}
	switch value {
	case "hard", "soft", "hard_then_soft":
		return value, nil
	default:
		return "", fmt.Errorf("preferred gate %q is not supported", value)
	}
}

func effectiveSkillOptOptionsCount(mode string, optionsCount int) int {
	if optionsCount != 0 {
		return optionsCount
	}
	if mode == db.EvalRunModeExplore {
		return 5
	}
	return 2
}

func generatedSkillOptTrainSessionID(templateID string) string {
	base := strings.ToLower(strings.TrimSpace(templateID))
	if base == "" {
		base = "template"
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	now := time.Now().UTC()
	return "train-" + strings.Trim(b.String(), "-_") + "-" + now.Format("20060102-150405") + fmt.Sprintf("-%09d", now.Nanosecond())
}

func skillOptTrainStartMetadata(request string, mode string, explorationLevel string, optionsCount int, preferredGate string, items []skillOptTrainItemPlan, warnings []string, previewPolicy skillopt.TrainPreviewPolicy) string {
	lines := strings.Count(request, "\n") + 1
	words := len(strings.Fields(request))
	previewMetadata, reviewMetadata := previewPolicy.Metadata()
	metadata := map[string]any{
		"request":           request,
		"request_lines":     lines,
		"request_words":     words,
		"request_chars":     len(request),
		"mode":              mode,
		"exploration_level": explorationLevel,
		"options_count":     optionsCount,
		"items_count":       len(items),
		"item_warnings":     warnings,
		"evaluation": map[string]any{
			"preferred_gate": preferredGate,
		},
		"preview": previewMetadata,
		"review":  reviewMetadata,
		"source":  "gitmoot skillopt train start",
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
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

func skillOptTrainDecisionMetadata(existing string, reason string) string {
	var metadata map[string]any
	if strings.TrimSpace(existing) != "" {
		_ = json.Unmarshal([]byte(existing), &metadata)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["decision_reason"] = strings.TrimSpace(reason)
	metadata["decision"] = skillopt.TrainStateRunAbandoned
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return existing
	}
	return string(encoded)
}

func recordSkillOptTrainGenerationFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest, failure error) error {
	metadata := map[string]any{
		"status":       "failed",
		"agent":        strings.TrimSpace(request.GeneratorAgent),
		"agent_type":   strings.TrimSpace(request.GeneratorType),
		"error":        failure.Error(),
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "generation", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "generation", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func resolveSkillOptTrainOptimizerPaths(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerPaths, error) {
	outRoot := strings.TrimSpace(request.OutRoot)
	optimizerMetadata := decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["optimizer"])
	persistedOutRoot := metadataString(optimizerMetadata, "out_root")
	persistedTrainingPackage := metadataString(optimizerMetadata, "training_package")
	persistedCandidateOutput := metadataString(optimizerMetadata, "candidate_output")
	persistedArtifactDir := metadataString(optimizerMetadata, "artifact_dir")
	if persistedTrainingPackage != "" && skillopt.NormalizeTrainState(iteration.State) != skillopt.TrainStateFeedbackSynced {
		if outRoot != "" {
			absRequestedOutRoot, err := filepath.Abs(outRoot)
			if err != nil {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer out-root: %w", err)
			}
			absPersistedOutRoot := persistedOutRoot
			if absPersistedOutRoot == "" {
				absPersistedOutRoot = filepath.Dir(persistedTrainingPackage)
			}
			if absPersistedOutRoot, err = filepath.Abs(absPersistedOutRoot); err != nil {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve persisted optimizer out-root: %w", err)
			}
			if absRequestedOutRoot != absPersistedOutRoot {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("optimizer package already exported at %s; retry with the same --out-root or omit --out-root", persistedTrainingPackage)
			}
		}
		outRoot = persistedOutRoot
		if outRoot == "" {
			outRoot = filepath.Dir(persistedTrainingPackage)
		}
	}
	if outRoot == "" {
		outRoot = persistedOutRoot
	}
	if outRoot == "" {
		outRoot = filepath.Join(paths.Evals, "train", session.ID, iteration.ID, "optimizer")
	}
	absOutRoot, err := filepath.Abs(outRoot)
	if err != nil {
		return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer out-root: %w", err)
	}
	trainingPackagePath := filepath.Join(absOutRoot, "training.json")
	candidatePackagePath := filepath.Join(absOutRoot, "candidate.json")
	artifactDir := filepath.Join(absOutRoot, "artifacts")
	if persistedTrainingPackage != "" && skillopt.NormalizeTrainState(iteration.State) != skillopt.TrainStateFeedbackSynced {
		trainingPackagePath = persistedTrainingPackage
		if persistedCandidateOutput != "" {
			candidatePackagePath = persistedCandidateOutput
		}
		if persistedArtifactDir != "" {
			artifactDir = persistedArtifactDir
		}
	}
	return skillOptTrainOptimizerPaths{
		OutRoot:              absOutRoot,
		ArtifactRoot:         paths.ArtifactBlobs,
		TrainingPackagePath:  trainingPackagePath,
		CandidatePackagePath: candidatePackagePath,
		ArtifactDir:          artifactDir,
	}, nil
}

func exportSkillOptTrainPackage(ctx context.Context, store *db.Store, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths) (map[string]any, error) {
	pkg, err := skillopt.ExportTrainingPackage(ctx, store, iteration.EvalRunID)
	if err != nil {
		return nil, fmt.Errorf("export training package: %w", err)
	}
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode training package: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeSkillOptFile(paths.TrainingPackagePath, encoded); err != nil {
		return nil, fmt.Errorf("write training package: %w", err)
	}
	return map[string]any{
		"status":           "package_created",
		"training_package": paths.TrainingPackagePath,
		"out_root":         paths.OutRoot,
		"artifact_root":    paths.ArtifactRoot,
		"candidate_output": paths.CandidatePackagePath,
		"artifact_dir":     paths.ArtifactDir,
		"created_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"source":           "gitmoot skillopt train continue",
	}, nil
}

func buildSkillOptTrainOptimizerCommand(iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths) (string, []string, error) {
	executable := strings.TrimSpace(request.SkillOptBin)
	if executable == "" {
		executable = "gitmoot-skillopt"
	}
	resolved, err := skillOptTrainOptimizerRunner.LookPath(executable)
	if err != nil {
		return "", nil, fmt.Errorf("find gitmoot-skillopt executable %q: %w", executable, err)
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", nil, fmt.Errorf("resolve gitmoot-skillopt executable %q: %w", resolved, err)
		}
	}
	gate, err := skillOptTrainOptimizerGate(iteration, request)
	if err != nil {
		return "", nil, err
	}
	args := []string{
		"optimize",
		"--training-package", paths.TrainingPackagePath,
		"--artifact-root", paths.ArtifactRoot,
		"--out-root", paths.OutRoot,
		"--candidate-output", paths.CandidatePackagePath,
		"--artifact-dir", paths.ArtifactDir,
		"--gate-metric", gate,
	}
	optimizerModel := strings.TrimSpace(request.OptimizerModel)
	targetModel := strings.TrimSpace(request.TargetModel)
	if model := strings.TrimSpace(request.Model); model != "" {
		if optimizerModel == "" {
			optimizerModel = model
		}
		if targetModel == "" {
			targetModel = model
		}
	}
	if optimizerModel != "" {
		args = append(args, "--optimizer-model", optimizerModel)
	}
	if targetModel != "" {
		args = append(args, "--target-model", targetModel)
	}
	if optimizerBackend := strings.TrimSpace(request.OptimizerBackend); optimizerBackend != "" {
		args = append(args, "--optimizer-backend", optimizerBackend)
	}
	if targetBackend := strings.TrimSpace(request.TargetBackend); targetBackend != "" {
		args = append(args, "--target-backend", targetBackend)
	}
	if evaluatorID := strings.TrimSpace(request.EvaluatorID); evaluatorID != "" {
		args = append(args, "--evaluator-id", evaluatorID)
	}
	if evaluatorModel := strings.TrimSpace(request.EvaluatorModel); evaluatorModel != "" {
		args = append(args, "--evaluator-model", evaluatorModel)
	}
	if evaluatorBackend := strings.TrimSpace(request.EvaluatorBackend); evaluatorBackend != "" {
		args = append(args, "--evaluator-backend", evaluatorBackend)
	}
	if skillUpdateMode := strings.TrimSpace(request.SkillUpdateMode); skillUpdateMode != "" {
		args = append(args, "--skill-update-mode", skillUpdateMode)
	}
	if request.NumEpochs > 0 {
		args = append(args, "--num-epochs", strconv.Itoa(request.NumEpochs))
	}
	if request.BatchSize > 0 {
		args = append(args, "--batch-size", strconv.Itoa(request.BatchSize))
	}
	if request.DryRun {
		args = append(args, "--dry-run")
	}
	return resolved, args, nil
}

func skillOptTrainOptimizerGate(iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (string, error) {
	gate := strings.TrimSpace(strings.ToLower(request.Gate))
	if gate == "" {
		gate = skillOptMetadataString(iteration.MetadataJSON, "evaluation", "preferred_gate")
	}
	gate = strings.TrimSpace(strings.ToLower(gate))
	if gate == "hard_then_soft" {
		gate = "mixed"
	}
	if gate == "" {
		gate = "hard"
	}
	switch gate {
	case "hard", "soft", "mixed":
		return gate, nil
	default:
		return "", fmt.Errorf("optimizer gate %q is not supported; use hard, soft, or mixed", gate)
	}
}

func runSkillOptTrainOptimizer(ctx context.Context, paths skillOptTrainOptimizerPaths, request skillOptTrainOptimizerRequest, command string, args []string) (subprocess.Result, error) {
	timeout := strings.TrimSpace(request.Timeout)
	if timeout != "" {
		duration, err := time.ParseDuration(timeout)
		if err != nil {
			return subprocess.Result{}, fmt.Errorf("parse optimizer timeout: %w", err)
		}
		if duration <= 0 {
			return subprocess.Result{}, errors.New("optimizer timeout must be greater than zero")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	if err := os.MkdirAll(paths.OutRoot, 0o755); err != nil {
		return subprocess.Result{}, fmt.Errorf("create optimizer out-root: %w", err)
	}
	result, err := skillOptTrainOptimizerRunner.Run(ctx, paths.OutRoot, command, args...)
	if err != nil {
		return result, fmt.Errorf("optimizer failed: %w%s", err, subprocessDiagnostics(result))
	}
	return result, nil
}

func subprocessDiagnostics(result subprocess.Result) string {
	stderr := strings.TrimSpace(result.Stderr)
	stdout := strings.TrimSpace(result.Stdout)
	switch {
	case stderr != "" && stdout != "":
		return fmt.Sprintf(" (stderr: %s; stdout: %s)", truncateForMetadata(stderr), truncateForMetadata(stdout))
	case stderr != "":
		return fmt.Sprintf(" (stderr: %s)", truncateForMetadata(stderr))
	case stdout != "":
		return fmt.Sprintf(" (stdout: %s)", truncateForMetadata(stdout))
	default:
		return ""
	}
}

func skillOptTrainOptimizerMetadata(request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string, result subprocess.Result, status string, failure error) map[string]any {
	metadata := map[string]any{
		"status":           status,
		"command":          command,
		"args":             args,
		"training_package": paths.TrainingPackagePath,
		"out_root":         paths.OutRoot,
		"candidate_output": paths.CandidatePackagePath,
		"artifact_dir":     paths.ArtifactDir,
		"dry_run":          request.DryRun,
		"stdout":           truncateForMetadata(result.Stdout),
		"stderr":           truncateForMetadata(result.Stderr),
		"completed_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"source":           "gitmoot skillopt train continue",
	}
	if failure != nil {
		metadata["error"] = failure.Error()
	}
	return metadata
}

func recordSkillOptTrainOptimizerFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string, result subprocess.Result, failure error) error {
	metadata := skillOptTrainOptimizerMetadata(request, paths, command, args, result, "failed", failure)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func recordSkillOptTrainCandidateImportFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, failure error) error {
	metadata := map[string]any{
		"status":            "failed",
		"candidate_package": paths.CandidatePackagePath,
		"artifact_dir":      paths.ArtifactDir,
		"error":             failure.Error(),
		"completed_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"source":            "gitmoot skillopt train continue",
	}
	session.State = skillopt.TrainStateOptimizerCompleted
	iteration.State = skillopt.TrainStateOptimizerCompleted
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func recordSkillOptTrainNoCandidate(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, reason string) error {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptimizerCompletedNoCandidate); err != nil {
		return err
	}
	metadata := map[string]any{
		"status":              "no_candidate",
		"candidate_package":   paths.CandidatePackagePath,
		"artifact_dir":        paths.ArtifactDir,
		"no_candidate_reason": reason,
		"next_action":         skillOptNoCandidateNextAction(),
		"completed_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"source":              "gitmoot skillopt train continue",
	}
	session.State = skillopt.TrainStateOptimizerCompletedNoCandidate
	iteration.State = skillopt.TrainStateOptimizerCompletedNoCandidate
	iteration.CandidateVersionID = ""
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func skillOptNoCandidateReason(err error) string {
	text := strings.TrimSpace(err.Error())
	marker := skillopt.ErrNoCandidate.Error() + ":"
	if index := strings.LastIndex(text, marker); index >= 0 {
		return strings.TrimSpace(text[index+len(marker):])
	}
	return text
}

func skillOptNoCandidateNextAction() string {
	return "do not publish a candidate review; revise feedback and start another iteration, rerun the optimizer with --rerun-optimizer, or stop"
}

func importSkillOptTrainCandidate(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, optimizerPaths skillOptTrainOptimizerPaths) (db.AgentTemplateVersion, error) {
	content, err := os.ReadFile(optimizerPaths.CandidatePackagePath)
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("read optimizer candidate package: %w", err)
	}
	var candidate skillopt.CandidatePackage
	if err := json.Unmarshal(content, &candidate); err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("decode optimizer candidate package: %w", err)
	}
	if err := validateSkillOptTrainCandidatePackage(ctx, store, session, iteration, candidate); err != nil {
		return db.AgentTemplateVersion{}, err
	}
	version, err := skillopt.ImportCandidatePackageWithOptions(ctx, store, candidate, skillopt.CandidateImportOptions{
		SourcePath:  optimizerPaths.CandidatePackagePath,
		ArtifactDir: optimizerPaths.ArtifactDir,
		BlobStore:   artifact.NewStore(paths.ArtifactBlobs),
	})
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("import optimizer candidate package: %w", err)
	}
	return version, nil
}

func validateSkillOptTrainCandidatePackage(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidate skillopt.CandidatePackage) error {
	templateID := strings.TrimSpace(candidate.TemplateID)
	if templateID != strings.TrimSpace(session.TemplateID) {
		return fmt.Errorf("optimizer candidate template_id %q does not match train session template %q", templateID, strings.TrimSpace(session.TemplateID))
	}
	expectedBase := strings.TrimSpace(iteration.BaseTemplateVersionID)
	if expectedBase == "" {
		expectedBase = strings.TrimSpace(session.TemplateVersionID)
	}
	baseRef := strings.TrimSpace(candidate.BaseVersionID)
	if baseRef == "" {
		base, err := store.GetAgentTemplate(ctx, templateID)
		if err != nil {
			return fmt.Errorf("load optimizer candidate current base for %q: %w", templateID, err)
		}
		if base.VersionID != expectedBase {
			return fmt.Errorf("optimizer candidate omitted base_version_id and current base is %q, want active train base %q", base.VersionID, expectedBase)
		}
		return nil
	}
	base, err := store.GetAgentTemplateReference(ctx, baseRef)
	if err != nil {
		return fmt.Errorf("load optimizer candidate base version %q: %w", baseRef, err)
	}
	if base.ID != templateID {
		return fmt.Errorf("optimizer candidate base_version_id %q belongs to template %q, want %q", baseRef, base.ID, templateID)
	}
	if base.VersionID != expectedBase {
		return fmt.Errorf("optimizer candidate base_version_id %q resolved to %q, want active train base %q", baseRef, base.VersionID, expectedBase)
	}
	return nil
}

func truncateForMetadata(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 2000 {
		return value
	}
	return value[:2000] + "...<truncated>"
}

func skillOptTrainGenerationRepo(session db.SkillOptTrainSession) string {
	if repo := strings.TrimSpace(session.WorkspaceRepo); repo != "" {
		return repo
	}
	return strings.TrimSpace(session.TargetRepo)
}

func ensureSkillOptTrainGenerationRepoReady(ctx context.Context, store *db.Store, repoName string) error {
	repoName = strings.TrimSpace(repoName)
	if repoName == "" {
		return errors.New("skillopt train generation repo is required")
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return err
	}
	if existing, err := store.GetRepo(ctx, repo.FullName()); err == nil {
		if strings.TrimSpace(existing.CheckoutPath) == "" {
			return fmt.Errorf("generation repo %s has no checkout path; run `gitmoot repo add %s --path /path/to/checkout` before train continue", repo.FullName(), repo.FullName())
		}
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: existing.CheckoutPath})
		if err != nil {
			return fmt.Errorf("generation repo %s checkout is not ready: %w", repo.FullName(), err)
		}
		record.PollInterval = existing.PollInterval
		return store.UpsertRepo(ctx, record)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
	if err != nil {
		return fmt.Errorf("generation repo %s is not registered with a checkout path; run `gitmoot repo add %s --path /path/to/checkout` before train continue: %w", repo.FullName(), repo.FullName(), err)
	}
	return store.UpsertRepo(ctx, record)
}

func skillOptTrainGeneratorSelection(request skillOptTrainContinueRequest) (string, string, error) {
	agent := strings.TrimSpace(request.GeneratorAgent)
	agentType := strings.TrimSpace(request.GeneratorType)
	if agent != "" && agentType != "" {
		return "", "", errors.New("use only one of --generator-agent or --generator-type")
	}
	if agent != "" {
		return agent, "", nil
	}
	if agentType == "" {
		agentType = "skillopt-generator"
	}
	return agentType, agentType, nil
}

func skillOptTrainGenerationConcurrency(request skillOptTrainContinueRequest, dispatchType string) (int, error) {
	if strings.TrimSpace(dispatchType) == "" {
		return 1, nil
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return 0, err
	}
	agentType, ok := types[dispatchType]
	if !ok {
		return 0, fmt.Errorf("agent %q not found", dispatchType)
	}
	if agentType.MaxBackground <= 0 {
		return 1, nil
	}
	return agentType.MaxBackground, nil
}

func skillOptTrainOptionLabels(count int) []string {
	if count <= 0 {
		count = 2
	}
	labels := make([]string, 0, count)
	for index := 0; index < count; index++ {
		if index < 26 {
			labels = append(labels, string(rune('a'+index)))
			continue
		}
		labels = append(labels, fmt.Sprintf("option-%d", index+1))
	}
	return labels
}

func skillOptTrainGenerationRoles(run db.EvalRun) []string {
	if !skillOptRunUsesRankedOptions(run) {
		return []string{"baseline", "candidate"}
	}
	return skillOptTrainOptionLabels(run.OptionsCount)
}

func buildSkillOptTrainGenerationPrompt(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, rankedRun bool) string {
	itemMetadata := decodedSkillOptMetadata(item.MetadataJSON)
	sessionMetadata := decodedSkillOptMetadata(session.MetadataJSON)
	requestText := metadataString(sessionMetadata, "request")
	if requestText == "" {
		requestText = session.RequestSummary
	}
	var builder strings.Builder
	builder.WriteString("Generate one review option for a Gitmoot SkillOpt training run.\n")
	builder.WriteString("Return the generated artifact content in gitmoot_result.summary with decision implemented. Do not include commentary outside the artifact.\n\n")
	builder.WriteString("Training request:\n")
	builder.WriteString(requestText)
	builder.WriteString("\n\n")
	builder.WriteString("Session: ")
	builder.WriteString(session.ID)
	builder.WriteString("\nIteration: ")
	builder.WriteString(iteration.ID)
	builder.WriteString("\nEval run: ")
	builder.WriteString(run.ID)
	builder.WriteString("\nMode: ")
	builder.WriteString(run.Mode)
	builder.WriteString("\nExploration level: ")
	builder.WriteString(run.ExplorationLevel)
	if rankedRun {
		builder.WriteString("\nOption label: ")
		builder.WriteString(strings.ToUpper(role))
	} else {
		builder.WriteString("\nA/B artifact role: ")
		builder.WriteString(role)
	}
	builder.WriteString("\nItem id: ")
	builder.WriteString(item.ItemID)
	builder.WriteString("\nTitle: ")
	builder.WriteString(item.Title)
	if brief := metadataString(itemMetadata, "brief"); brief != "" {
		builder.WriteString("\nBrief: ")
		builder.WriteString(brief)
	}
	if audience := metadataString(itemMetadata, "target_audience"); audience != "" {
		builder.WriteString("\nTarget audience: ")
		builder.WriteString(audience)
	}
	if outputType := metadataString(itemMetadata, "output_type"); outputType != "" {
		builder.WriteString("\nOutput type: ")
		builder.WriteString(outputType)
	}
	if hints, ok := itemMetadata["artifact_hints"].([]any); ok && len(hints) > 0 {
		builder.WriteString("\nArtifact hints:")
		for _, hint := range hints {
			if text := strings.TrimSpace(fmt.Sprint(hint)); text != "" {
				builder.WriteString("\n- ")
				builder.WriteString(text)
			}
		}
	}
	builder.WriteString("\n\nGeneration rules:\n")
	if rankedRun {
		builder.WriteString("- Make this option meaningfully different from the other labels in layout, content strategy, and visual/interaction direction.\n")
	} else if role == "baseline" {
		builder.WriteString("- Generate the baseline artifact: a solid, conventional answer that satisfies the item brief.\n")
	} else {
		builder.WriteString("- Generate the candidate artifact: a meaningfully different improved answer intended to be compared against the baseline.\n")
	}
	switch run.ExplorationLevel {
	case db.ExplorationLevelHigh:
		builder.WriteString("- Use high exploration: vary the product explanation, proof/content structure, and visual direction substantially.\n")
	case db.ExplorationLevelMedium:
		builder.WriteString("- Use medium exploration: combine promising directions while keeping alternatives visibly different.\n")
	case db.ExplorationLevelLow:
		builder.WriteString("- Use low exploration: make narrow refinements and avoid broad direction changes.\n")
	}
	builder.WriteString("- Keep the artifact self-contained and directly reviewable.\n")
	builder.WriteString("- Preserve the requested output type.\n")
	if skillOptTrainWantsVuePreviewBundle(session) {
		requiresVuePreviewBundle := skillOptTrainRequiresVuePreviewBundle(session)
		builder.WriteString("\nPreview bundle contract:\n")
		if requiresVuePreviewBundle {
			builder.WriteString("- This train session requires a Vue/Vite preview bundle for every generated option.\n")
			builder.WriteString("- Keep gitmoot_result.summary as a string value. The string content must be exactly one serialized JSON object, with no markdown, code fences, or prose.\n")
			builder.WriteString("- Do not set gitmoot_result.summary to a nested object; encode the bundle JSON as the summary string.\n")
		} else {
			builder.WriteString("- This train session is configured for optional Vue/Vite previews. Prefer a Vue/Vite preview bundle so Gitmoot can publish preview URLs; plain text or markdown is accepted only as inline fallback.\n")
			builder.WriteString("- If you return a preview bundle, keep gitmoot_result.summary as a string containing exactly one serialized JSON object, with no markdown, code fences, or prose.\n")
			builder.WriteString("- If you return plain text or markdown fallback, use the normal summary string and do not include the bundle JSON shape.\n")
		}
		builder.WriteString("- Use renderer \"vue-vite\".\n")
		builder.WriteString("- Include build_command exactly \"npm run build\" and dist_dir \"dist\".\n")
		builder.WriteString("- Include files with these required relative paths: package.json, index.html, src/main.js, src/App.vue.\n")
		builder.WriteString("- package.json scripts must include only \"build\": \"vite build\". Do not include dependencies or devDependencies; Gitmoot supplies trusted build dependencies.\n")
		builder.WriteString("- index.html and src/main.js may use a simple Vue mount placeholder; Gitmoot canonicalizes and overwrites them with trusted scaffold files before build.\n")
		builder.WriteString("- src/App.vue must be scriptless template/style Vue only. Do not include script blocks, imports, require, import.meta, @import, or CSS url().\n")
		builder.WriteString("- Each file entry must have path and non-empty content. Use slash-separated relative paths only.\n")
		builder.WriteString("- Do not include local absolute paths, path traversal, secrets, .env files, node_modules, dependency caches, dist, built outputs, vite.config.js, or files outside the required paths.\n")
		builder.WriteString("- The JSON object shape is: renderer string, files array of {path, content}, build_command string, dist_dir string.\n")
	}
	return builder.String()
}

func prepareReviewItemContentArtifact(blobStore artifact.Store, runID string, itemID string, role string, content []byte, mediaType string, driver string) (db.EvalArtifact, error) {
	if len(content) == 0 || strings.TrimSpace(string(content)) == "" {
		return db.EvalArtifact{}, fmt.Errorf("%s content is required", role)
	}
	blob, err := blobStore.Put(content)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("store %s artifact blob: %w", role, err)
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = "text/plain"
	}
	if strings.TrimSpace(driver) == "" {
		driver = "text"
	}
	return db.EvalArtifact{
		ID:        reviewItemArtifactID(runID, itemID, role),
		Hash:      blob.Hash,
		MediaType: mediaType,
		SizeBytes: blob.Size,
		Driver:    driver,
	}, nil
}

func skillOptTrainGeneratedOptionMetadata(output localAgentJobOutput, prompt string, previewBundleMetadata *skillopt.PreviewBundleMetadata) string {
	metadata := map[string]any{
		"source":           "gitmoot skillopt train continue",
		"job_id":           output.JobID,
		"agent":            output.Agent,
		"prompt":           prompt,
		"raw_output_count": output.RawOutputCount,
	}
	if previewBundleMetadata != nil {
		metadata["preview_bundle"] = *previewBundleMetadata
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func skillOptTrainGenerationStrategy(run db.EvalRun) map[string]any {
	return map[string]any{
		"mode":              run.Mode,
		"exploration_level": run.ExplorationLevel,
		"options_count":     run.OptionsCount,
	}
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

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func skillOptTrainConfirmCommand(args []string, sessionID string) string {
	filtered := make([]string, 0, len(args)+4)
	filtered = append(filtered, "gitmoot", "skillopt", "train", "start")
	hasSession := false
	for _, arg := range args {
		if arg == "--dry-run" || arg == "-dry-run" || strings.HasPrefix(arg, "--dry-run=") || strings.HasPrefix(arg, "-dry-run=") || arg == "--yes" || arg == "-yes" || strings.HasPrefix(arg, "--yes=") || strings.HasPrefix(arg, "-yes=") {
			continue
		}
		if arg == "--session" || arg == "-session" || strings.HasPrefix(arg, "--session=") || strings.HasPrefix(arg, "-session=") {
			hasSession = true
		}
		filtered = append(filtered, arg)
	}
	if !hasSession && strings.TrimSpace(sessionID) != "" {
		filtered = append(filtered, "--session", strings.TrimSpace(sessionID))
	}
	filtered = append(filtered, "--yes")
	return shellArgs(filtered)
}

func runSkillOptReview(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "create":
		return runSkillOptReviewCreate(args[1:], stdout, stderr)
	case "item":
		return runSkillOptReviewItem(args[1:], stdout, stderr)
	case "status":
		return runSkillOptReviewStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt review command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptReviewCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "agent template id or version to review")
	repoFlag := fs.String("repo", "", "target repository in owner/repo form")
	runID := fs.String("run", "", "review run id")
	mode := fs.String("mode", "", "review mode: validate, explore, refine, or distill")
	explorationLevel := fs.String("exploration-level", "", "exploration level: high, medium, or low")
	optionsCount := fs.Int("options", 0, "expected number of review options")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review create does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*templateID) == "" || strings.TrimSpace(*repoFlag) == "" || strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt review create requires --template, --repo, and --run")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review create: %v\n", err)
		return 2
	}
	var run db.EvalRun
	if err := withStore(*home, func(store *db.Store) error {
		template, err := loadInstalledTemplate(context.Background(), store, *templateID)
		if err != nil {
			return err
		}
		run = db.EvalRun{
			ID:                strings.TrimSpace(*runID),
			TemplateID:        template.ID,
			TemplateVersionID: template.VersionID,
			TargetRepo:        repo.FullName(),
			State:             "review",
			Mode:              strings.TrimSpace(*mode),
			ExplorationLevel:  strings.TrimSpace(*explorationLevel),
			OptionsCount:      *optionsCount,
			MetadataJSON:      `{"driver":"manual-review"}`,
		}
		return store.UpsertEvalRun(context.Background(), run)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review create: %v\n", err)
		return 1
	}
	writeLine(stdout, "created review %s for %s", run.ID, run.TemplateVersionID)
	return 0
}

func runSkillOptReviewItem(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runSkillOptReviewItemAdd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt review item command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type skillOptOptionSpec struct {
	Label string
	Path  string
}

type preparedSkillOptOption struct {
	Spec     skillOptOptionSpec
	Artifact db.EvalArtifact
	Metadata string
}

func parseSkillOptOptionFlags(values []string) ([]skillOptOptionSpec, error) {
	specs := make([]skillOptOptionSpec, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		label, path, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("--option must use label=path form")
		}
		label = strings.ToLower(strings.TrimSpace(label))
		path = strings.TrimSpace(path)
		if err := validateSkillOptOptionLabel(label); err != nil {
			return nil, err
		}
		if path == "" {
			return nil, fmt.Errorf("option %s path is required", label)
		}
		if _, ok := seen[label]; ok {
			return nil, fmt.Errorf("duplicate option label %q", label)
		}
		seen[label] = struct{}{}
		specs = append(specs, skillOptOptionSpec{Label: label, Path: path})
	}
	return specs, nil
}

func validateSkillOptOptionLabel(label string) error {
	if label == "" {
		return errors.New("option label is required")
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("option label %q must use only letters, digits, dots, dashes, or underscores", label)
		}
	}
	return nil
}

func runSkillOptReviewItemAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review item add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "review run id")
	itemID := fs.String("item", "", "review item id")
	title := fs.String("title", "", "review item title")
	baselinePath := fs.String("baseline", "", "baseline output file")
	candidatePath := fs.String("candidate", "", "candidate output file")
	metadataJSON := fs.String("metadata-json", "", "JSON metadata to attach to the review item")
	mediaType := fs.String("media-type", "", "media type override for stored artifacts")
	driver := fs.String("driver", "text", "artifact driver")
	var optionFlags repeatedStringFlag
	fs.Var(&optionFlags, "option", "N-way option in label=path form; repeat once per option")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review item add does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*itemID) == "" {
		fmt.Fprintln(stderr, "skillopt review item add requires --run and --item")
		return 2
	}
	hasAB := strings.TrimSpace(*baselinePath) != "" || strings.TrimSpace(*candidatePath) != ""
	hasOptions := len(optionFlags) > 0
	if hasAB && hasOptions {
		fmt.Fprintln(stderr, "skillopt review item add accepts either --baseline/--candidate or repeated --option flags, not both")
		return 2
	}
	if !hasOptions && (strings.TrimSpace(*baselinePath) == "" || strings.TrimSpace(*candidatePath) == "") {
		fmt.Fprintln(stderr, "skillopt review item add requires --baseline and --candidate, or repeated --option label=path flags")
		return 2
	}
	optionSpecs, err := parseSkillOptOptionFlags(optionFlags)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 2
	}
	metadata, err := normalizeSkillOptMetadataJSON(*metadataJSON)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 2
	}
	var item db.EvalReviewItem
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		run, err := store.GetEvalRun(ctx, strings.TrimSpace(*runID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("review run %s not found", strings.TrimSpace(*runID))
			}
			return err
		}
		blobStore := artifact.NewStore(paths.ArtifactBlobs)
		rankedRun := skillOptRunUsesRankedOptions(run)
		if hasOptions && !rankedRun {
			return fmt.Errorf("review run %s is validate/A/B mode; use --baseline and --candidate", run.ID)
		}
		if !hasOptions && rankedRun {
			return fmt.Errorf("review run %s is ranked mode; use repeated --option label=path flags", run.ID)
		}
		if hasOptions {
			if run.OptionsCount > 0 && len(optionSpecs) != run.OptionsCount {
				return fmt.Errorf("review run %s expects %d options, got %d", run.ID, run.OptionsCount, len(optionSpecs))
			}
			preparedOptions := make([]preparedSkillOptOption, 0, len(optionSpecs))
			for _, spec := range optionSpecs {
				optionArtifact, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "option-"+spec.Label, spec.Path, *mediaType, *driver)
				if err != nil {
					return err
				}
				optionMetadata, err := reviewOptionMetadataJSON(spec.Path)
				if err != nil {
					return err
				}
				preparedOptions = append(preparedOptions, preparedSkillOptOption{
					Spec:     spec,
					Artifact: optionArtifact,
					Metadata: optionMetadata,
				})
			}
			item = db.EvalReviewItem{
				RunID:        run.ID,
				ItemID:       strings.TrimSpace(*itemID),
				Title:        strings.TrimSpace(*title),
				MetadataJSON: metadata,
			}
			if err := preserveExistingSkillOptReviewItemDetails(ctx, store, &item); err != nil {
				return err
			}
			if err := store.UpsertEvalReviewItem(ctx, item); err != nil {
				return err
			}
			replacementOptions := make([]db.EvalReviewOption, 0, len(preparedOptions))
			for _, prepared := range preparedOptions {
				if err := store.UpsertEvalArtifact(ctx, prepared.Artifact); err != nil {
					return fmt.Errorf("register option %s artifact: %w", prepared.Spec.Label, err)
				}
				replacementOptions = append(replacementOptions, db.EvalReviewOption{
					RunID:        run.ID,
					ItemID:       strings.TrimSpace(*itemID),
					Label:        prepared.Spec.Label,
					ArtifactID:   prepared.Artifact.ID,
					Role:         "option",
					MetadataJSON: prepared.Metadata,
				})
			}
			if err := store.ReplaceEvalReviewOptions(ctx, run.ID, strings.TrimSpace(*itemID), replacementOptions); err != nil {
				return err
			}
			return nil
		}
		baseline, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "baseline", *baselinePath, *mediaType, *driver)
		if err != nil {
			return err
		}
		candidate, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "candidate", *candidatePath, *mediaType, *driver)
		if err != nil {
			return err
		}
		if baseline.ID == candidate.ID {
			return errors.New("baseline and candidate artifact ids must be different")
		}
		if err := store.UpsertEvalArtifact(ctx, baseline); err != nil {
			return fmt.Errorf("register baseline artifact: %w", err)
		}
		if err := store.UpsertEvalArtifact(ctx, candidate); err != nil {
			return fmt.Errorf("register candidate artifact: %w", err)
		}
		item = db.EvalReviewItem{
			RunID:               run.ID,
			ItemID:              strings.TrimSpace(*itemID),
			Title:               strings.TrimSpace(*title),
			BaselineArtifactID:  baseline.ID,
			CandidateArtifactID: candidate.ID,
			MetadataJSON:        metadata,
		}
		if err := preserveExistingSkillOptReviewItemDetails(ctx, store, &item); err != nil {
			return err
		}
		return store.UpsertEvalReviewItem(ctx, item)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 1
	}
	writeLine(stdout, "added review item %s to %s", item.ItemID, item.RunID)
	return 0
}

func runSkillOptReviewStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "review run id")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review status does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt review status requires --run")
		return 2
	}
	var status skillOptReviewStatus
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		status, err = loadSkillOptReviewStatus(context.Background(), store, artifact.NewStore(paths.ArtifactBlobs), *runID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review status: %v\n", err)
		return 1
	}
	itemCount := len(status.Items)
	feedbackCount := len(status.Feedback) + len(status.RankedFeedback)
	fmt.Fprintf(stdout, "run: %s\n", status.Run.ID)
	fmt.Fprintf(stdout, "template: %s\n", status.Run.TemplateID)
	fmt.Fprintf(stdout, "template_version: %s\n", status.Run.TemplateVersionID)
	fmt.Fprintf(stdout, "repo: %s\n", status.Run.TargetRepo)
	fmt.Fprintf(stdout, "state: %s\n", status.Run.State)
	fmt.Fprintf(stdout, "mode: %s\n", status.Recommendation.CurrentMode)
	fmt.Fprintf(stdout, "exploration_level: %s\n", status.Recommendation.ExplorationLevel)
	fmt.Fprintf(stdout, "items: %d\n", itemCount)
	fmt.Fprintf(stdout, "feedback: %d\n", feedbackCount)
	fmt.Fprintf(stdout, "pairwise_preferences: %d\n", len(status.PairwisePreferences))
	fmt.Fprintf(stdout, "ranking_stability: %s\n", status.Recommendation.RankingStability)
	fmt.Fprintf(stdout, "recommended_next_mode: %s\n", status.Recommendation.RecommendedMode)
	fmt.Fprintf(stdout, "recommendation: %s\n", status.Recommendation.Summary())
	fmt.Fprintf(stdout, "packet_blockers: %d\n", len(status.PacketBlockers))
	fmt.Fprintf(stdout, "training_blockers: %d\n", len(status.TrainingBlockers))
	fmt.Fprintf(stdout, "ready_for_packet: %t\n", status.PacketReady)
	fmt.Fprintf(stdout, "ready_for_training: %t\n", status.TrainingReady)
	for _, blocker := range status.PacketBlockers {
		fmt.Fprintf(stdout, "packet_blocker: %s\n", blocker)
	}
	for _, blocker := range status.TrainingBlockers {
		fmt.Fprintf(stdout, "training_blocker: %s\n", blocker)
	}
	return 0
}

func preserveExistingSkillOptReviewItemDetails(ctx context.Context, store *db.Store, item *db.EvalReviewItem) error {
	if store == nil || item == nil {
		return nil
	}
	if strings.TrimSpace(item.Title) != "" && strings.TrimSpace(item.MetadataJSON) != "" {
		return nil
	}
	items, err := store.ListEvalReviewItems(ctx, item.RunID)
	if err != nil {
		return err
	}
	for _, existing := range items {
		if strings.TrimSpace(existing.ItemID) != strings.TrimSpace(item.ItemID) {
			continue
		}
		if strings.TrimSpace(item.Title) == "" {
			item.Title = existing.Title
		}
		if strings.TrimSpace(item.MetadataJSON) == "" {
			item.MetadataJSON = existing.MetadataJSON
		}
		return nil
	}
	return nil
}

type skillOptReviewStatus struct {
	Run                 db.EvalRun
	Items               []db.EvalReviewItem
	Feedback            []db.FeedbackEvent
	RankedFeedback      []db.RankedFeedbackEvent
	PairwisePreferences []db.PairwisePreference
	Recommendation      skillopt.PhaseRecommendation
	PacketBlockers      []string
	TrainingBlockers    []string
	PacketReady         bool
	TrainingReady       bool
}

func loadSkillOptReviewStatus(ctx context.Context, store *db.Store, blobStore artifact.Store, runID string) (skillOptReviewStatus, error) {
	run, err := store.GetEvalRun(ctx, strings.TrimSpace(runID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillOptReviewStatus{}, fmt.Errorf("review run %s not found", strings.TrimSpace(runID))
		}
		return skillOptReviewStatus{}, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	events, err := store.ListFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	rankedEvents, err := store.ListRankedFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	packetBlockers := reviewPacketBlockers(ctx, store, blobStore, run, items)
	trainingBlockers := reviewTrainingBlockers(ctx, store, run, items, events, rankedEvents)
	recommendation := skillopt.RecommendPhaseForItems(run, items, events, rankedEvents, pairwisePreferences)
	return skillOptReviewStatus{
		Run:                 run,
		Items:               items,
		Feedback:            events,
		RankedFeedback:      rankedEvents,
		PairwisePreferences: pairwisePreferences,
		Recommendation:      recommendation,
		PacketBlockers:      packetBlockers,
		TrainingBlockers:    trainingBlockers,
		PacketReady:         len(packetBlockers) == 0,
		TrainingReady:       len(packetBlockers) == 0 && len(trainingBlockers) == 0,
	}, nil
}

func reviewPacketBlockers(ctx context.Context, store *db.Store, blobStore artifact.Store, run db.EvalRun, items []db.EvalReviewItem) []string {
	if len(items) == 0 {
		return []string{"run has no review items"}
	}
	var blockers []string
	validated := map[string]struct{}{}
	for _, item := range items {
		itemID := strings.TrimSpace(item.ItemID)
		if itemID == "" {
			itemID = item.ID
		}
		if skillOptRunUsesRankedOptions(run) {
			options, err := store.ListEvalReviewOptions(ctx, run.ID, item.ItemID)
			if err != nil {
				blockers = append(blockers, fmt.Sprintf("item %s options are not readable: %v", itemID, err))
				continue
			}
			if len(options) == 0 {
				blockers = append(blockers, fmt.Sprintf("item %s has no registered options", itemID))
				continue
			}
			if run.OptionsCount > 0 && len(options) != run.OptionsCount {
				blockers = append(blockers, fmt.Sprintf("item %s has %d options, want %d", itemID, len(options), run.OptionsCount))
				continue
			}
			for _, option := range options {
				blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "option "+option.Label, option.ArtifactID, validated)...)
			}
			continue
		}
		baseline := strings.TrimSpace(item.BaselineArtifactID)
		candidate := strings.TrimSpace(item.CandidateArtifactID)
		if baseline == "" || candidate == "" {
			blockers = append(blockers, fmt.Sprintf("item %s is missing a baseline or candidate artifact", itemID))
			continue
		}
		if baseline == candidate {
			blockers = append(blockers, fmt.Sprintf("item %s uses the same artifact for baseline and candidate", itemID))
			continue
		}
		blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "baseline", baseline, validated)...)
		blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "candidate", candidate, validated)...)
	}
	return blockers
}

func skillOptRunUsesRankedOptions(run db.EvalRun) bool {
	return run.Mode != db.EvalRunModeValidate || run.OptionsCount > 2
}

func reviewTrainingBlockers(ctx context.Context, store *db.Store, run db.EvalRun, items []db.EvalReviewItem, events []db.FeedbackEvent, rankedEvents []db.RankedFeedbackEvent) []string {
	if len(items) == 0 {
		return []string{"run has no review items"}
	}
	var blockers []string
	feedbackByItem := map[string]int{}
	for _, event := range events {
		feedbackByItem[strings.TrimSpace(event.ItemID)]++
	}
	for _, event := range rankedEvents {
		feedbackByItem[strings.TrimSpace(event.ItemID)]++
	}
	for _, item := range items {
		itemID := strings.TrimSpace(item.ItemID)
		if itemID == "" {
			itemID = item.ID
		}
		if feedbackByItem[itemID] == 0 {
			blockers = append(blockers, fmt.Sprintf("item %s has no imported feedback", itemID))
		}
	}
	if _, err := skillopt.ExportTrainingPackage(ctx, store, run.ID); err != nil {
		blockers = append(blockers, fmt.Sprintf("training export failed: %v", err))
	}
	return blockers
}

func validateReviewArtifactBlob(ctx context.Context, store *db.Store, blobStore artifact.Store, itemID string, role string, artifactID string, validated map[string]struct{}) []string {
	if _, ok := validated[artifactID]; ok {
		return nil
	}
	validated[artifactID] = struct{}{}
	record, err := store.GetEvalArtifact(ctx, artifactID)
	if err != nil {
		return []string{fmt.Sprintf("item %s %s artifact %s is not registered: %v", itemID, role, artifactID, err)}
	}
	if _, err := blobStore.Read(record.Hash); err != nil {
		return []string{fmt.Sprintf("item %s %s artifact %s blob is not readable: %v", itemID, role, artifactID, err)}
	}
	return nil
}

func prepareReviewItemArtifact(blobStore artifact.Store, runID string, itemID string, role string, path string, mediaTypeOverride string, driver string) (db.EvalArtifact, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return db.EvalArtifact{}, fmt.Errorf("%s path is required", role)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("read %s file: %w", role, err)
	}
	mediaType, err := reviewArtifactMediaType(path, content, mediaTypeOverride)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("%s file: %w", role, err)
	}
	blob, err := blobStore.Put(content)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("store %s artifact blob: %w", role, err)
	}
	artifactRecord := db.EvalArtifact{
		ID:        reviewItemArtifactID(runID, itemID, role),
		Hash:      blob.Hash,
		MediaType: mediaType,
		SizeBytes: blob.Size,
		Driver:    strings.TrimSpace(driver),
	}
	if artifactRecord.Driver == "" {
		artifactRecord.Driver = "text"
	}
	return artifactRecord, nil
}

func reviewItemArtifactID(runID string, itemID string, role string) string {
	return strings.TrimSpace(runID) + "/" + strings.TrimSpace(itemID) + "/" + strings.TrimSpace(role)
}

func reviewArtifactMediaType(path string, content []byte, override string) (string, error) {
	if mediaType := strings.TrimSpace(override); mediaType != "" {
		return mediaType, nil
	}
	if !utf8.Valid(content) {
		return "", errors.New("binary content requires --media-type")
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown", nil
	case ".txt", ".text", ".diff", ".patch":
		return "text/plain", nil
	case ".csv":
		return "text/csv", nil
	case ".json":
		return "application/json", nil
	}
	return "text/plain", nil
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

func reviewOptionMetadataJSON(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	encoded, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		return "", fmt.Errorf("option metadata: %w", err)
	}
	return string(encoded), nil
}

func runSkillOptExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id to export")
	output := fs.String("output", "", "path to write the training package; stdout when omitted")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt export does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt export requires --run")
		return 2
	}
	var pkg skillopt.TrainingPackage
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		pkg, err = skillopt.ExportTrainingPackage(context.Background(), store, *runID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if strings.TrimSpace(*output) == "" {
		_, err = stdout.Write(encoded)
	} else {
		err = writeSkillOptFile(*output, encoded)
		if err == nil {
			writeLine(stdout, "exported %s to %s", pkg.EvalRun.ID, *output)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	return 0
}

func runSkillOptImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	file := fs.String("file", "", "candidate package JSON file to import")
	artifactDir := fs.String("artifact-dir", "", "directory containing candidate package artifacts")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt import does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "skillopt import requires --file")
		return 2
	}
	content, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt import: read candidate package: %v\n", err)
		return 1
	}
	var pkg skillopt.CandidatePackage
	if err := json.Unmarshal(content, &pkg); err != nil {
		fmt.Fprintf(stderr, "skillopt import: decode candidate package: %v\n", err)
		return 1
	}
	var versionID string
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		version, err := skillopt.ImportCandidatePackageWithOptions(context.Background(), store, pkg, skillopt.CandidateImportOptions{
			SourcePath:  *file,
			ArtifactDir: *artifactDir,
			BlobStore:   artifact.NewStore(paths.ArtifactBlobs),
		})
		if err != nil {
			return err
		}
		versionID = version.ID
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt import: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported pending candidate %s", versionID)
	return 0
}

func runSkillOptCandidate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runSkillOptCandidateList(args[1:], stdout, stderr)
	case "show":
		return runSkillOptCandidateShow(args[1:], stdout, stderr)
	case "promote":
		return runSkillOptCandidatePromote(args[1:], stdout, stderr)
	case "reject":
		return runSkillOptCandidateReject(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt candidate command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptCandidateList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id to filter")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt candidate list does not accept positional arguments")
		return 2
	}
	var versions []db.AgentTemplateVersion
	var reviews map[string]db.AgentTemplateCandidateReview
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		versions, err = store.ListPendingAgentTemplateVersions(context.Background(), *templateID)
		if err != nil {
			return err
		}
		reviews = make(map[string]db.AgentTemplateCandidateReview, len(versions))
		for _, version := range versions {
			review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
			if err == nil {
				reviews[version.ID] = review
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate list: %v\n", err)
		return 1
	}
	if len(versions) == 0 {
		writeLine(stdout, "no pending candidates")
		return 0
	}
	fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", "VERSION", "TEMPLATE", "STATE", "SCORE", "SUMMARY")
	for _, version := range versions {
		review := reviews[version.ID]
		fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", version.ID, version.TemplateID, version.State, scoreText(review.Score), firstLine(review.PreferenceSummary))
	}
	return 0
}

func runSkillOptCandidateShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate show requires exactly one version id")
		return 2
	}
	versionID := fs.Arg(0)
	var version db.AgentTemplateVersion
	var review db.AgentTemplateCandidateReview
	var hasReview bool
	var base db.AgentTemplate
	var hasBase bool
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		version, err = store.GetAgentTemplateVersionByID(context.Background(), versionID)
		if err != nil {
			return err
		}
		review, err = store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
		if err == nil {
			hasReview = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		baseRef := strings.TrimSpace(review.BaseVersionID)
		if baseRef == "" {
			current, err := store.GetAgentTemplate(context.Background(), version.TemplateID)
			if err != nil {
				return err
			}
			baseRef = current.VersionID
		}
		if baseRef != "" && baseRef != version.ID {
			base, err = store.GetAgentTemplateReference(context.Background(), baseRef)
			if err == nil {
				hasBase = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate show: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "version: %s\n", version.ID)
	fmt.Fprintf(stdout, "template: %s\n", version.TemplateID)
	fmt.Fprintf(stdout, "state: %s\n", version.State)
	fmt.Fprintf(stdout, "source: %s@%s:%s\n", version.SourceRepo, version.SourceRef, version.SourcePath)
	fmt.Fprintf(stdout, "content_hash: %s\n", version.ContentHash)
	if hasReview {
		fmt.Fprintf(stdout, "base_version: %s\n", emptyText(review.BaseVersionID))
		fmt.Fprintf(stdout, "score: %s\n", scoreText(review.Score))
		fmt.Fprintf(stdout, "preference_summary: %s\n", emptyText(review.PreferenceSummary))
		fmt.Fprintf(stdout, "diff_artifact: %s\n", emptyText(review.DiffArtifactID))
		if strings.TrimSpace(review.EvalReportJSON) != "" {
			fmt.Fprintf(stdout, "eval_report:\n%s\n", indentJSON(review.EvalReportJSON))
		}
		if strings.TrimSpace(review.DecisionReason) != "" {
			fmt.Fprintf(stdout, "decision_reason: %s\n", review.DecisionReason)
		}
	}
	if hasBase {
		diff := artifact.TextDriver{}.Diff(base.VersionID+".md", version.ID+".md", []byte(base.Content), []byte(version.Content))
		fmt.Fprintf(stdout, "content_diff:\n%s", diff)
	}
	return 0
}

func runSkillOptCandidatePromote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate promote requires exactly one version id")
		return 2
	}
	var promoted db.AgentTemplateVersion
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		promoted, err = store.PromoteAgentTemplateVersion(context.Background(), fs.Arg(0))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate promote: %v\n", err)
		return 1
	}
	writeLine(stdout, "promoted candidate %s", promoted.ID)
	return 0
}

func runSkillOptCandidateReject(args []string, stdout, stderr io.Writer) int {
	parsed, help, ok := parseSkillOptCandidateRejectArgs(args, stderr)
	if help {
		printSkillOptUsage(stdout)
		return 0
	}
	if !ok {
		return 2
	}
	if parsed.versionID == "" {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	if parsed.extraVersion {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	var rejected db.AgentTemplateVersion
	if err := withStore(parsed.home, func(store *db.Store) error {
		var err error
		rejected, err = store.RejectAgentTemplateVersion(context.Background(), parsed.versionID, parsed.reason)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate reject: %v\n", err)
		return 1
	}
	writeLine(stdout, "rejected candidate %s", rejected.ID)
	return 0
}

type skillOptCandidateRejectArgs struct {
	home         string
	reason       string
	versionID    string
	extraVersion bool
}

func parseSkillOptCandidateRejectArgs(args []string, stderr io.Writer) (skillOptCandidateRejectArgs, bool, bool) {
	var parsed skillOptCandidateRejectArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			return parsed, true, true
		case arg == "--home" || arg == "--reason":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "skillopt candidate reject: %s requires a value\n", arg)
				return parsed, false, false
			}
			i++
			if arg == "--home" {
				parsed.home = args[i]
			} else {
				parsed.reason = args[i]
			}
		case strings.HasPrefix(arg, "--home="):
			parsed.home = strings.TrimPrefix(arg, "--home=")
		case strings.HasPrefix(arg, "--reason="):
			parsed.reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "skillopt candidate reject: unknown flag %s\n", arg)
			return parsed, false, false
		case parsed.versionID == "":
			parsed.versionID = arg
		default:
			parsed.extraVersion = true
		}
	}
	return parsed, false, true
}

func writeSkillOptFile(path string, content []byte) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return os.WriteFile(path, content, 0o644)
}

func runSkillOptFeedback(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	if args[0] != "markdown" && args[0] != "github" {
		fmt.Fprintf(stderr, "unknown skillopt feedback collector %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
	if len(args) < 2 {
		fmt.Fprintf(stderr, "skillopt feedback %s requires a subcommand\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
	if args[0] == "markdown" {
		switch args[1] {
		case "export":
			return runSkillOptFeedbackMarkdownExport(args[2:], stdout, stderr)
		case "import":
			return runSkillOptFeedbackMarkdownImport(args[2:], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "unknown skillopt feedback markdown command %q\n\n", args[1])
			printSkillOptUsage(stderr)
			return 2
		}
	}
	switch args[1] {
	case "publish":
		return runSkillOptFeedbackGitHubPublish(args[2:], stdout, stderr)
	case "sync":
		return runSkillOptFeedbackGitHubSync(args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt feedback github command %q\n\n", args[1])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptFeedbackMarkdownExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback markdown export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	output := fs.String("output", "", "packet output directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback markdown export does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*output) == "" {
		fmt.Fprintln(stderr, "skillopt feedback markdown export requires --run and --output")
		return 2
	}
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		collector := feedback.MarkdownCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
		return collector.WritePacket(context.Background(), store, *runID, *output)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback markdown export: %v\n", err)
		return 1
	}
	writeLine(stdout, "wrote markdown feedback packet for %s to %s", *runID, *output)
	return 0
}

func runSkillOptFeedbackMarkdownImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback markdown import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	packet := fs.String("packet", "", "packet directory containing feedback.yml")
	reviewer := fs.String("reviewer", "", "reviewer name override")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback markdown import does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*packet) == "" {
		fmt.Fprintln(stderr, "skillopt feedback markdown import requires --packet")
		return 2
	}
	var count int
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		collector := feedback.MarkdownCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
		result, err := collector.ImportPacket(context.Background(), store, *packet, *reviewer)
		if err != nil {
			return err
		}
		count = result.Count()
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback markdown import: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported %d feedback events", count)
	return 0
}

func runSkillOptFeedbackGitHubPublish(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback github publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	repoFlag := fs.String("repo", "", "GitHub repository owner/repo")
	pullRequest := fs.Int64("pr", 0, "existing pull request number to comment on instead of creating an issue")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback github publish does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt feedback github publish requires --run")
		return 2
	}
	var result feedback.GitHubPublishResult
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		run, err := store.GetEvalRun(context.Background(), strings.TrimSpace(*runID))
		if err != nil {
			return err
		}
		repo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, *repoFlag)
		if err != nil {
			return err
		}
		collector := feedback.GitHubCollector{
			BlobStore: artifact.NewStore(paths.ArtifactBlobs),
			GitHub:    newSkillOptGitHubClient(),
		}
		result, err = collector.Publish(context.Background(), store, run.ID, feedback.GitHubPublishTarget{
			Repo:        repo,
			PullRequest: *pullRequest,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback github publish: %v\n", err)
		return 1
	}
	writeLine(stdout, "published github feedback %s for %s to %s#%d: %s", result.Mode, strings.TrimSpace(*runID), result.Repo.FullName(), result.IssueNumber, result.URL)
	return 0
}

func runSkillOptFeedbackGitHubSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback github sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	repoFlag := fs.String("repo", "", "GitHub repository owner/repo")
	issueNumber := fs.Int64("issue", 0, "GitHub issue number containing feedback comments")
	pullRequest := fs.Int64("pr", 0, "GitHub pull request number containing feedback comments")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt feedback github sync requires --run")
		return 2
	}
	if *issueNumber > 0 && *pullRequest > 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync accepts only one of --issue or --pr")
		return 2
	}
	targetNumber := *issueNumber
	if targetNumber == 0 {
		targetNumber = *pullRequest
	}
	if targetNumber <= 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync requires --issue or --pr")
		return 2
	}
	var count int
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		run, err := store.GetEvalRun(context.Background(), strings.TrimSpace(*runID))
		if err != nil {
			return err
		}
		repo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, *repoFlag)
		if err != nil {
			return err
		}
		collector := feedback.GitHubCollector{
			BlobStore: artifact.NewStore(paths.ArtifactBlobs),
			GitHub:    newSkillOptGitHubClient(),
		}
		result, err := collector.Sync(context.Background(), store, run.ID, repo, targetNumber)
		if err != nil {
			return err
		}
		count = result.Count()
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback github sync: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported %d github feedback events", count)
	return 0
}

func resolveSkillOptFeedbackRepo(ctx context.Context, paths config.Paths, store *db.Store, run db.EvalRun, repoFlag string) (github.Repository, error) {
	if strings.TrimSpace(repoFlag) != "" {
		requested, err := daemon.ParseRepository(repoFlag)
		if err != nil {
			return github.Repository{}, err
		}
		if expected, ok, err := resolveSkillOptTrainFeedbackRepo(ctx, store, run); err != nil {
			return github.Repository{}, err
		} else if ok && expected.FullName() != "" && !strings.EqualFold(requested.FullName(), expected.FullName()) {
			return github.Repository{}, fmt.Errorf("train run %s expects github feedback repo %s; got %s", run.ID, expected.FullName(), requested.FullName())
		}
		return requested, nil
	}
	if expected, ok, err := resolveSkillOptTrainFeedbackRepo(ctx, store, run); err != nil {
		return github.Repository{}, err
	} else if ok && expected.FullName() != "" {
		return expected, nil
	}
	if expectedRepo := skillOptMetadataString(run.MetadataJSON, "review", "expected_repo"); expectedRepo != "" {
		if repo, err := daemon.ParseRepository(expectedRepo); err == nil {
			return repo, nil
		}
	}
	if strings.TrimSpace(run.TargetRepo) != "" {
		if repo, err := daemon.ParseRepository(run.TargetRepo); err == nil {
			return repo, nil
		}
	}
	templateRef := strings.TrimSpace(run.TemplateVersionID)
	if templateRef == "" {
		templateRef = strings.TrimSpace(run.TemplateID)
	}
	if templateRef != "" {
		template, err := store.GetAgentTemplateReference(ctx, templateRef)
		if err == nil && strings.TrimSpace(template.SourceRepo) != "" {
			if repo, err := daemon.ParseRepository(template.SourceRepo); err == nil {
				return repo, nil
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return github.Repository{}, err
		}
	}
	defaultRepo, err := config.LoadDefaultFeedbackRepo(paths)
	if err != nil {
		return github.Repository{}, err
	}
	if strings.TrimSpace(defaultRepo) != "" {
		return daemon.ParseRepository(defaultRepo)
	}
	return github.Repository{}, errors.New("skillopt feedback github requires --repo because no target repo, template source repo, or [feedback].repo default is configured")
}

func resolveSkillOptTrainFeedbackRepo(ctx context.Context, store *db.Store, run db.EvalRun) (github.Repository, bool, error) {
	iteration, err := store.GetSkillOptTrainIterationByEvalRun(ctx, run.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return github.Repository{}, false, nil
	}
	if err != nil {
		return github.Repository{}, true, err
	}
	session, err := store.GetSkillOptTrainSession(ctx, iteration.SessionID)
	if err != nil {
		return github.Repository{}, true, err
	}
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	expectedRepo := strings.TrimSpace(policy.ExpectedReviewRepo)
	if expectedRepo == "" {
		return github.Repository{}, true, nil
	}
	repo, err := daemon.ParseRepository(expectedRepo)
	if err != nil {
		return github.Repository{}, true, fmt.Errorf("train expected review repo: %w", err)
	}
	return repo, true, nil
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
