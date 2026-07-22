package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"gopkg.in/yaml.v3"
)

func runSkillOptTrainStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	configPath := fs.String("config", "", "train init config.toml scaffold path")
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
	createRepos := fs.Bool("create-repos", false, "create the target and workspace repositories on GitHub if they do not exist")
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
	setFlags := flagNamesSet(fs)
	var configDefaults skillOptTrainStartConfigDefaults
	if strings.TrimSpace(*configPath) != "" {
		var err error
		configDefaults, err = applySkillOptTrainStartConfig(*configPath, setFlags, templateID, repoFlag, taskKind, mode, explorationLevel, optionsCount, previewRepoFlag, previewMode, previewRenderer, previewPublisher, requestText, requestFile, itemsFile)
		if err != nil {
			fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
			return 2
		}
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
	if workspaceRepo == "" {
		fmt.Fprintln(stderr, "skillopt train start requires --workspace-repo owner/repo; without it the session stays at request_confirmed and train continue cannot reach option generation")
		return 2
	}
	if *createRepos {
		for _, fullName := range []string{repo.FullName(), workspaceRepo} {
			if err := ensureSkillOptTrainRepo(*home, fullName, "train", strings.TrimSpace(*sessionID), stdout); err != nil {
				fmt.Fprintf(stderr, "skillopt train start: create repo %s: %v\n", fullName, err)
				return 1
			}
		}
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
		plan = buildSkillOptTrainStartPlan(template, repo.FullName(), workspaceRepo, policy, strings.TrimSpace(*sessionID), request, normalizedTaskKind, normalizedMode, normalizedExploration, effectiveOptionsCount, normalizedGate, items, itemWarnings, configDefaults)
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

// ensureSkillOptTrainRepo creates the given repo as a private GitHub repo when it
// does not already exist, recording it in created_repos so cleanup flows can
// offer deletion of exactly the repos gitmoot created. Deduplicated callers pass
// distinct names; an empty name is a no-op. An ambiguous existence check (e.g.
// auth error) is left alone so the later Preflight surfaces it, rather than
// wrongly attempting a create.
func ensureSkillOptTrainRepo(home, fullName, purpose, sessionID string, stdout io.Writer) error {
	if strings.TrimSpace(fullName) == "" {
		return nil
	}
	repo, err := daemon.ParseRepository(fullName)
	if err != nil {
		return err
	}
	client := newSkillOptGitHubClient()
	exists, err := client.RepositoryExists(context.Background(), repo)
	if err != nil {
		// Ambiguous (auth/network); do not attempt a create, let Preflight report.
		return nil
	}
	if exists {
		return nil
	}
	if err := client.CreateRepository(context.Background(), repo, true); err != nil {
		return err
	}
	if err := withStore(home, func(store *db.Store) error {
		return store.RecordCreatedRepo(context.Background(), db.CreatedRepo{Repo: fullName, Purpose: purpose, SessionID: strings.TrimSpace(sessionID)})
	}); err != nil {
		// The repo exists either way; a failed record only loses the cleanup offer.
		fmt.Fprintf(stdout, "warning: could not record created repo %s: %v\n", fullName, err)
	}
	writeLine(stdout, "created_repo: %s", fullName)
	return nil
}

func flagNamesSet(fs *flag.FlagSet) map[string]struct{} {
	set := map[string]struct{}{}
	fs.Visit(func(f *flag.Flag) {
		set[f.Name] = struct{}{}
	})
	return set
}

type skillOptTrainStartConfigDefaults struct {
	Optimizer skillOptTrainOptimizerRequest
}

func applySkillOptTrainStartConfig(configPath string, setFlags map[string]struct{}, templateID *string, repoFlag *string, taskKind *string, mode *string, explorationLevel *string, optionsCount *int, previewRepoFlag *string, previewMode *string, previewRenderer *string, previewPublisher *string, requestText *string, requestFile *string, itemsFile *string) (skillOptTrainStartConfigDefaults, error) {
	configPath = strings.TrimSpace(configPath)
	config, err := skillopt.LoadTrainInitConfig(configPath)
	if err != nil {
		return skillOptTrainStartConfigDefaults{}, fmt.Errorf("load config: %w", err)
	}
	scaffoldDir := filepath.Dir(configPath)
	if !flagWasSet(setFlags, "template") {
		if strings.TrimSpace(config.TemplateVersion) != "" {
			*templateID = config.TemplateVersion
		} else {
			*templateID = config.Template
		}
	}
	if !flagWasSet(setFlags, "repo") {
		*repoFlag = config.ReviewRepo
	}
	if !flagWasSet(setFlags, "task-kind") {
		*taskKind = config.TaskKind
	}
	if !flagWasSet(setFlags, "mode") {
		*mode = config.Mode
	}
	if !flagWasSet(setFlags, "exploration-level") {
		*explorationLevel = config.ExplorationLevel
	}
	if !flagWasSet(setFlags, "options") {
		*optionsCount = config.Options
	}
	if err := applySkillOptTrainStartPreviewConfig(config, setFlags, *repoFlag, previewRepoFlag, previewMode, previewRenderer, previewPublisher); err != nil {
		return skillOptTrainStartConfigDefaults{}, err
	}
	if !flagWasSet(setFlags, "request") && !flagWasSet(setFlags, "request-file") {
		taskPath := filepath.Join(scaffoldDir, skillopt.TrainInitTaskFileName)
		content, err := os.ReadFile(taskPath)
		if err != nil {
			return skillOptTrainStartConfigDefaults{}, fmt.Errorf("read %s: %w", taskPath, err)
		}
		*requestText = strings.TrimSpace(string(content))
		*requestFile = ""
	}
	if !flagWasSet(setFlags, "items-file") {
		defaultItemsPath := filepath.Join(scaffoldDir, skillopt.TrainInitReviewItemsName)
		if _, err := os.Stat(defaultItemsPath); err == nil {
			*itemsFile = defaultItemsPath
		} else if !errors.Is(err, os.ErrNotExist) {
			return skillOptTrainStartConfigDefaults{}, fmt.Errorf("inspect %s: %w", defaultItemsPath, err)
		}
	}
	return skillOptTrainStartConfigDefaults{Optimizer: skillOptTrainOptimizerDefaultsFromInitConfig(config)}, nil
}

func applySkillOptTrainStartPreviewConfig(config skillopt.TrainInitConfig, setFlags map[string]struct{}, effectiveReviewRepo string, previewRepoFlag *string, previewMode *string, previewRenderer *string, previewPublisher *string) error {
	preview, err := normalizeSkillOptTrainInitPreview(config.Preview)
	if err != nil {
		return err
	}
	defaultPreviewRepo := firstNonEmpty(strings.TrimSpace(effectiveReviewRepo), config.ReviewRepo)
	if flagWasSet(setFlags, "preview-mode") {
		switch strings.TrimSpace(strings.ToLower(*previewMode)) {
		case skillopt.TrainPreviewModeNone:
			if !flagWasSet(setFlags, "preview-renderer") {
				*previewRenderer = skillopt.TrainPreviewRendererNone
			}
			if !flagWasSet(setFlags, "preview-publisher") {
				*previewPublisher = skillopt.TrainPreviewPublisherNone
			}
		case skillopt.TrainPreviewModeRequired:
			if !flagWasSet(setFlags, "preview-renderer") {
				*previewRenderer = skillopt.TrainPreviewRendererVueVite
			}
			if !flagWasSet(setFlags, "preview-publisher") {
				*previewPublisher = skillopt.TrainPreviewPublisherGitHubPages
			}
			if !flagWasSet(setFlags, "preview-repo") {
				*previewRepoFlag = defaultPreviewRepo
			}
		case skillopt.TrainPreviewModeOptional:
			if preview == "vue" {
				if !flagWasSet(setFlags, "preview-renderer") {
					*previewRenderer = skillopt.TrainPreviewRendererVueVite
				}
				if !flagWasSet(setFlags, "preview-publisher") {
					*previewPublisher = skillopt.TrainPreviewPublisherGitHubPages
				}
				if !flagWasSet(setFlags, "preview-repo") {
					*previewRepoFlag = defaultPreviewRepo
				}
			}
		}
		return nil
	}
	switch preview {
	case "none", "text-table":
		if skillOptTrainPreviewOverrideFlagWasSet(setFlags) {
			return nil
		}
		*previewMode = skillopt.TrainPreviewModeNone
		if !flagWasSet(setFlags, "preview-renderer") {
			*previewRenderer = skillopt.TrainPreviewRendererNone
		}
		if !flagWasSet(setFlags, "preview-publisher") {
			*previewPublisher = skillopt.TrainPreviewPublisherNone
		}
	case "vue":
		*previewMode = skillopt.TrainPreviewModeRequired
		if !flagWasSet(setFlags, "preview-renderer") {
			*previewRenderer = skillopt.TrainPreviewRendererVueVite
		}
		if !flagWasSet(setFlags, "preview-publisher") {
			*previewPublisher = skillopt.TrainPreviewPublisherGitHubPages
		}
		if !flagWasSet(setFlags, "preview-repo") {
			*previewRepoFlag = defaultPreviewRepo
		}
	}
	return nil
}

func skillOptTrainPreviewOverrideFlagWasSet(setFlags map[string]struct{}) bool {
	for _, name := range []string{"preview-repo", "preview-renderer", "preview-publisher", "preview-route-template"} {
		if flagWasSet(setFlags, name) {
			return true
		}
	}
	return false
}

func flagWasSet(setFlags map[string]struct{}, name string) bool {
	_, ok := setFlags[name]
	return ok
}

func skillOptTrainOptimizerDefaultsFromInitConfig(config skillopt.TrainInitConfig) skillOptTrainOptimizerRequest {
	request := skillOptTrainOptimizerRequest{
		SkillUpdateMode:              config.Optimizer.SkillUpdateMode,
		OptimizerViews:               config.Optimizer.OptimizerViews,
		OptimizerViewsSet:            config.Optimizer.OptimizerViews > 0,
		RetryOptimizerViews:          config.Optimizer.RetryOptimizerViews,
		RetryOptimizerViewsSet:       strings.TrimSpace(config.Optimizer.RetryOptimizerViews) != "",
		NoopRetryBudget:              trainInitConfigInt(config.Optimizer.NoopRetryBudget),
		NoopRetryBudgetSet:           config.Optimizer.NoopRetryBudget != nil,
		GateRejectRetryBudget:        trainInitConfigInt(config.Optimizer.GateRejectRetryBudget),
		GateRejectRetryBudgetSet:     config.Optimizer.GateRejectRetryBudget != nil,
		WrongArtifactRetryBudget:     trainInitConfigInt(config.Optimizer.WrongArtifactRetryBudget),
		WrongArtifactRetryBudgetSet:  config.Optimizer.WrongArtifactRetryBudget != nil,
		TargetArtifactRetryBudget:    trainInitConfigInt(config.Optimizer.TargetArtifactRetryBudget),
		TargetArtifactRetryBudgetSet: config.Optimizer.TargetArtifactRetryBudget != nil,
		HardFailureRetryBudget:       trainInitConfigInt(config.Optimizer.HardFailureRetryBudget),
		HardFailureRetryBudgetSet:    config.Optimizer.HardFailureRetryBudget != nil,
		FinalEval:                    config.FinalEvaluatorEnabled,
	}
	optimizerBackend := strings.TrimSpace(config.Optimizer.OptimizerBackend)
	targetBackend := strings.TrimSpace(config.Optimizer.TargetBackend)
	evaluatorBackend := strings.TrimSpace(config.Optimizer.EvaluatorBackend)
	internalTargetAdapter := strings.TrimSpace(config.Optimizer.InternalTargetAdapter)
	if strings.EqualFold(optimizerBackend, "codex") && strings.EqualFold(targetBackend, "codex") && strings.EqualFold(evaluatorBackend, "codex") && strings.EqualFold(internalTargetAdapter, "codex_exec") {
		request.Backend = "codex"
	} else {
		request.OptimizerBackend = optimizerBackend
		request.TargetBackend = skillOptTrainTargetBackendFromInitConfig(targetBackend, internalTargetAdapter)
		request.EvaluatorBackend = evaluatorBackend
	}
	if value := strings.TrimSpace(config.Optimizer.OptimizerModel); value != "" {
		request.OptimizerModel = value
	}
	if value := strings.TrimSpace(config.Optimizer.TargetModel); value != "" {
		request.TargetModel = value
	}
	return request
}

func skillOptTrainTargetBackendFromInitConfig(targetBackend string, internalTargetAdapter string) string {
	targetBackend = strings.TrimSpace(targetBackend)
	internalTargetAdapter = strings.TrimSpace(internalTargetAdapter)
	if strings.EqualFold(targetBackend, "codex") && strings.EqualFold(internalTargetAdapter, "codex_exec") {
		return "codex_exec"
	}
	return firstNonEmpty(targetBackend, internalTargetAdapter)
}

func trainInitConfigInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

type skillOptTrainStartPlan struct {
	Session   db.SkillOptTrainSession
	Iteration db.SkillOptTrainIteration
	EvalRun   db.EvalRun
	Items     []db.EvalReviewItem
	Warnings  []string
	Summary   skillopt.TrainStatusSummary
}

func buildSkillOptTrainStartPlan(template db.AgentTemplate, repo string, workspaceRepo string, previewPolicy skillopt.TrainPreviewPolicy, sessionID string, request string, taskKind string, mode string, explorationLevel string, optionsCount int, preferredGate string, itemPlans []skillOptTrainItemPlan, warnings []string, configDefaults skillOptTrainStartConfigDefaults) skillOptTrainStartPlan {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = generatedSkillOptTrainSessionID(template.ID)
	}
	state := skillopt.TrainStateRequestConfirmed
	if workspaceRepo != "" {
		state = skillopt.TrainStateItemsReady
	}
	metadata := skillOptTrainStartMetadata(request, mode, explorationLevel, optionsCount, preferredGate, itemPlans, warnings, previewPolicy, configDefaults, skillOptTemplateJudgeEvaluation(template))
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

type skillOptTrainStatusSnapshot struct {
	SessionID          string                         `json:"session_id"`
	IterationID        string                         `json:"iteration_id,omitempty"`
	TemplateID         string                         `json:"template_id,omitempty"`
	TemplateVersion    string                         `json:"template_version,omitempty"`
	TargetRepo         string                         `json:"target_repo,omitempty"`
	WorkspaceRepo      string                         `json:"workspace_repo,omitempty"`
	TaskKind           string                         `json:"task_kind,omitempty"`
	StatusPhase        string                         `json:"status_phase"`
	CurrentPhase       string                         `json:"current_phase"`
	CurrentStep        string                         `json:"current_step"`
	CompletedSteps     []string                       `json:"completed_steps"`
	BlockedStep        string                         `json:"blocked_step,omitempty"`
	NextAction         string                         `json:"next_action"`
	IssueURL           string                         `json:"issue_url,omitempty"`
	PullRequestURL     string                         `json:"pull_request_url,omitempty"`
	ContinueFromGitHub string                         `json:"continue_from_github,omitempty"`
	CandidateVersion   string                         `json:"candidate_version,omitempty"`
	RecoveryAvailable  bool                           `json:"recovery_available"`
	NoCandidateReason  string                         `json:"no_candidate_reason,omitempty"`
	NoCandidateDetails map[string]any                 `json:"no_candidate_details,omitempty"`
	PreviewPolicy      skillOptTrainPreviewPolicyJSON `json:"preview_policy"`
	Counts             skillOptTrainStatusCountsJSON  `json:"counts"`
	Progress           skillOptTrainStatusProgress    `json:"progress"`
	Verbose            *skillOptTrainStatusVerbose    `json:"verbose,omitempty"`
}

type skillOptTrainPreviewPolicyJSON struct {
	Mode               string `json:"mode"`
	Renderer           string `json:"renderer"`
	Publisher          string `json:"publisher"`
	Repo               string `json:"repo,omitempty"`
	RouteTemplate      string `json:"route_template,omitempty"`
	ExpectedReviewRepo string `json:"expected_review_repo,omitempty"`
}

type skillOptTrainStatusCountsJSON struct {
	ReviewItems          int `json:"review_items"`
	FeedbackEvents       int `json:"feedback_events"`
	RankedFeedbackEvents int `json:"ranked_feedback_events"`
	PairwisePreferences  int `json:"pairwise_preferences"`
}

type skillOptTrainStatusProgress struct {
	ReviewItems          int    `json:"review_items"`
	FeedbackEvents       int    `json:"feedback_events"`
	RankedFeedbackEvents int    `json:"ranked_feedback_events"`
	PairwisePreferences  int    `json:"pairwise_preferences"`
	GeneratedOptions     int    `json:"generated_options"`
	ETA                  string `json:"eta"`
}

type skillOptTrainStatusVerbose struct {
	EvalRunID             string                         `json:"eval_run_id,omitempty"`
	BaseTemplateVersionID string                         `json:"base_template_version_id,omitempty"`
	Mode                  string                         `json:"mode,omitempty"`
	ExplorationLevel      string                         `json:"exploration_level,omitempty"`
	CreatedAt             string                         `json:"created_at,omitempty"`
	UpdatedAt             string                         `json:"updated_at,omitempty"`
	Elapsed               string                         `json:"elapsed"`
	ReviewIssue           skillOptTrainStatusReviewIssue `json:"review_issue,omitempty"`
	Candidate             skillOptTrainStatusCandidate   `json:"candidate,omitempty"`
	Optimizer             map[string]any                 `json:"optimizer,omitempty"`
	Generation            map[string]any                 `json:"generation,omitempty"`
	Jobs                  skillOptTrainStatusJobs        `json:"jobs"`
	ActiveLocks           []skillOptTrainStatusLock      `json:"active_locks,omitempty"`
	Items                 []skillOptTrainStatusItem      `json:"items,omitempty"`
	MetadataStatus        map[string]string              `json:"metadata_status,omitempty"`
}

type skillOptTrainStatusReviewIssue struct {
	Repo   string `json:"repo,omitempty"`
	Number int64  `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
}

type skillOptTrainStatusCandidate struct {
	VersionID          string         `json:"version_id,omitempty"`
	PullRequestURL     string         `json:"pull_request_url,omitempty"`
	NoCandidateReason  string         `json:"no_candidate_reason,omitempty"`
	NoCandidateDetails map[string]any `json:"no_candidate_details,omitempty"`
}

type skillOptTrainStatusJobs struct {
	Total     int                         `json:"total"`
	Queued    int                         `json:"queued"`
	Running   int                         `json:"running"`
	Succeeded int                         `json:"succeeded"`
	Failed    int                         `json:"failed"`
	Other     int                         `json:"other"`
	Items     []skillOptTrainStatusJobRef `json:"items,omitempty"`
}

type skillOptTrainStatusJobRef struct {
	ID    string `json:"id"`
	Agent string `json:"agent,omitempty"`
	Type  string `json:"type,omitempty"`
	State string `json:"state"`
}

type skillOptTrainStatusLock struct {
	Name          string `json:"name"`
	Key           string `json:"key"`
	Status        string `json:"status,omitempty"`
	OwnerJobID    string `json:"owner_job_id,omitempty"`
	OwnerPID      int64  `json:"owner_pid,omitempty"`
	OwnerHostname string `json:"owner_hostname,omitempty"`
	CommandHash   string `json:"command_hash,omitempty"`
	AcquiredAt    string `json:"acquired_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Elapsed       string `json:"elapsed,omitempty"`
}

type skillOptTrainStatusItem struct {
	ItemID       string   `json:"item_id"`
	Title        string   `json:"title,omitempty"`
	OptionLabels []string `json:"option_labels,omitempty"`
}

func runSkillOptTrainStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	jsonOutput := fs.Bool("json", false, "print status as JSON")
	verbose := fs.Bool("verbose", false, "include detailed progress and metadata")
	watch := fs.Bool("watch", false, "refresh status until the session reaches a waiting or terminal phase")
	poll := fs.Duration("poll", 2*time.Second, "watch poll interval")
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
	if *poll <= 0 {
		fmt.Fprintln(stderr, "skillopt train status poll interval must be positive")
		return 2
	}
	if *watch && *jsonOutput {
		fmt.Fprintln(stderr, "skillopt train status does not support --watch with --json; use --watch for text refreshes or --json without --watch")
		return 2
	}
	var snapshot skillOptTrainStatusSnapshot
	if err := withStore(*home, func(store *db.Store) error {
		for {
			loaded, err := loadSkillOptTrainStatusSnapshot(context.Background(), store, *sessionID, *verbose || *watch)
			if err != nil {
				return err
			}
			snapshot = loaded
			outputSnapshot := skillOptTrainStatusOutputSnapshot(snapshot, *verbose)
			if !*watch {
				return nil
			}
			if *jsonOutput {
				if err := writeJSON(stdout, outputSnapshot); err != nil {
					return err
				}
			} else {
				printSkillOptTrainStatusSnapshot(stdout, outputSnapshot, *verbose)
				writeLine(stdout, "watch_state: %s", skillOptTrainWatchState(snapshot))
			}
			if skillOptTrainWatchDone(snapshot) {
				return nil
			}
			time.Sleep(*poll)
		}
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train status: %v\n", err)
		return 1
	}
	if *watch {
		return 0
	}
	if *jsonOutput {
		if err := writeJSON(stdout, skillOptTrainStatusOutputSnapshot(snapshot, *verbose)); err != nil {
			fmt.Fprintf(stderr, "skillopt train status: %v\n", err)
			return 1
		}
		return 0
	}
	printSkillOptTrainStatusSnapshot(stdout, snapshot, *verbose)
	return 0
}

func skillOptTrainStatusOutputSnapshot(snapshot skillOptTrainStatusSnapshot, verbose bool) skillOptTrainStatusSnapshot {
	if verbose {
		return snapshot
	}
	snapshot.Verbose = nil
	return snapshot
}

func runSkillOptTrainContinue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train continue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	generatorAgent := fs.String("generator-agent", "", "existing agent to use for option generation")
	generatorType := fs.String("generator-type", "", "managed agent type to use for option generation; overrides the default current-skill generator")
	skillOptBin := fs.String("skillopt-bin", "", "gitmoot-skillopt executable path; defaults to gitmoot-skillopt on PATH")
	backend := fs.String("backend", "", "backend preset for optimizer, target, and evaluator; currently supports codex")
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
	optimizerViews := fs.Int("optimizer-views", 0, "independent optimizer perspectives over imported human review feedback; omit to use gitmoot-skillopt default")
	retryOptimizerViews := fs.String("retry-optimizer-views", "", "optimizer perspectives for gate-reject retries: auto, inherit, or a positive integer; omit to use gitmoot-skillopt default")
	noopRetryBudget := fs.Int("noop-retry-budget", -1, "noop optimizer retry budget; omit to use gitmoot-skillopt default")
	gateRejectRetryBudget := fs.Int("gate-reject-retry-budget", -1, "gate-rejection optimizer retry budget; omit to use gitmoot-skillopt default")
	wrongArtifactRetryBudget := fs.Int("wrong-artifact-retry-budget", -1, "wrong-artifact optimizer retry budget; omit to use gitmoot-skillopt default")
	targetArtifactRetryBudget := fs.Int("target-artifact-retry-budget", -1, "target artifact repair retry budget; omit to use gitmoot-skillopt default")
	hardFailureRetryBudget := fs.Int("hard-failure-retry-budget", -1, "hard-failure reflection retry budget; omit to use gitmoot-skillopt default")
	feedbackDirectMode := fs.String("feedback-direct-mode", "", "feedback-direct optimizer mode: auto, on, or off")
	finalEval := fs.Bool("final-eval", false, "run gitmoot-skillopt final test evaluation after selection; disabled by default")
	gate := fs.String("gate", "", "optimizer gate metric: hard, soft, or mixed")
	outRoot := fs.String("out-root", "", "optimizer output directory")
	timeout := fs.String("timeout", "", "optimizer timeout duration")
	dryRun := fs.Bool("dry-run", false, "ask gitmoot-skillopt to avoid model calls while still producing a candidate package")
	rerunOptimizer := fs.Bool("rerun-optimizer", false, "rerun gitmoot-skillopt after optimizer completion instead of retrying the existing candidate import")
	exportOnly := fs.Bool("export-only", false, "export the training package and stop before launching the optimizer")
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
	setFlags := map[string]bool{}
	fs.Visit(func(flag *flag.Flag) {
		setFlags[flag.Name] = true
	})
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train continue does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(stderr, "skillopt train continue requires --session")
		return 2
	}
	if *exportOnly {
		for _, conflicting := range []string{"rerun-optimizer", "dry-run", "promote", "reject", "start-next"} {
			if setFlags[conflicting] {
				fmt.Fprintf(stderr, "skillopt train continue: --export-only cannot be combined with --%s\n", conflicting)
				return 2
			}
		}
	}
	if *numEpochs < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --num-epochs must be zero or greater")
		return 2
	}
	if *batchSize < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --batch-size must be zero or greater")
		return 2
	}
	if setFlags["optimizer-views"] && *optimizerViews <= 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --optimizer-views must be greater than zero")
		return 2
	}
	normalizedRetryOptimizerViews := ""
	if setFlags["retry-optimizer-views"] {
		var err error
		normalizedRetryOptimizerViews, err = normalizeSkillOptRetryOptimizerViews(*retryOptimizerViews)
		if err != nil {
			fmt.Fprintf(stderr, "skillopt train continue: %v\n", err)
			return 2
		}
		if setFlags["optimizer-views"] {
			if retryViews, ok := parseSkillOptRetryOptimizerViewsNumber(normalizedRetryOptimizerViews); ok && retryViews > *optimizerViews {
				fmt.Fprintln(stderr, "skillopt train continue: --retry-optimizer-views cannot exceed --optimizer-views")
				return 2
			}
		}
	}
	if setFlags["noop-retry-budget"] && *noopRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --noop-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["gate-reject-retry-budget"] && *gateRejectRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --gate-reject-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["wrong-artifact-retry-budget"] && *wrongArtifactRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --wrong-artifact-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["target-artifact-retry-budget"] && *targetArtifactRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --target-artifact-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["hard-failure-retry-budget"] && *hardFailureRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --hard-failure-retry-budget must be zero or greater")
		return 2
	}
	if mode := strings.TrimSpace(strings.ToLower(*feedbackDirectMode)); mode != "" && mode != "auto" && mode != "on" && mode != "off" {
		fmt.Fprintln(stderr, "skillopt train continue: --feedback-direct-mode must be auto, on, or off")
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
				SkillOptBin:                  *skillOptBin,
				Backend:                      *backend,
				Model:                        *model,
				OptimizerModel:               *optimizerModel,
				TargetModel:                  *targetModel,
				OptimizerBackend:             *optimizerBackend,
				TargetBackend:                *targetBackend,
				EvaluatorID:                  *evaluatorID,
				EvaluatorModel:               *evaluatorModel,
				EvaluatorBackend:             *evaluatorBackend,
				SkillUpdateMode:              *skillUpdateMode,
				NumEpochs:                    *numEpochs,
				BatchSize:                    *batchSize,
				OptimizerViews:               *optimizerViews,
				OptimizerViewsSet:            setFlags["optimizer-views"],
				RetryOptimizerViews:          normalizedRetryOptimizerViews,
				RetryOptimizerViewsSet:       setFlags["retry-optimizer-views"],
				NoopRetryBudget:              *noopRetryBudget,
				NoopRetryBudgetSet:           setFlags["noop-retry-budget"],
				GateRejectRetryBudget:        *gateRejectRetryBudget,
				GateRejectRetryBudgetSet:     setFlags["gate-reject-retry-budget"],
				WrongArtifactRetryBudget:     *wrongArtifactRetryBudget,
				WrongArtifactRetryBudgetSet:  setFlags["wrong-artifact-retry-budget"],
				TargetArtifactRetryBudget:    *targetArtifactRetryBudget,
				TargetArtifactRetryBudgetSet: setFlags["target-artifact-retry-budget"],
				HardFailureRetryBudget:       *hardFailureRetryBudget,
				HardFailureRetryBudgetSet:    setFlags["hard-failure-retry-budget"],
				FeedbackDirectMode:           strings.TrimSpace(strings.ToLower(*feedbackDirectMode)),
				FinalEval:                    *finalEval,
				FinalEvalSet:                 setFlags["final-eval"],
				Gate:                         *gate,
				OutRoot:                      *outRoot,
				Timeout:                      *timeout,
				DryRun:                       *dryRun,
				RerunOptimizer:               *rerunOptimizer,
				ExportOnly:                   *exportOnly,
			},
			Progress:         stderr,
			PromoteCandidate: *promote,
			RejectCandidate:  *reject,
			DecisionReason:   *reason,
			StartNext:        *startNext,
		})
		return err
	}); err != nil {
		if output.Summary.CurrentPhase != "" || len(output.Lines) > 0 {
			printSkillOptTrainContinueOutput(stdout, output)
		}
		fmt.Fprintf(stderr, "skillopt train continue: %v\n", err)
		return 1
	}
	printSkillOptTrainContinueOutput(stdout, output)
	if output.Summary.CurrentPhase == skillopt.TrainStateRunAbandoned {
		fmt.Fprintln(stderr, "skillopt train continue: train session is abandoned")
		return 1
	}
	return 0
}

func normalizeSkillOptRetryOptimizerViews(value string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "auto" || trimmed == "inherit" {
		return trimmed, nil
	}
	if parsed, err := strconv.Atoi(trimmed); err == nil && parsed > 0 {
		return strconv.Itoa(parsed), nil
	}
	return "", fmt.Errorf("--retry-optimizer-views must be auto, inherit, or a positive integer")
}

func parseSkillOptRetryOptimizerViewsNumber(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func printSkillOptTrainContinueOutput(stdout io.Writer, output skillOptTrainContinueOutput) {
	if output.Summary.CurrentPhase != "" {
		printSkillOptTrainStatus(stdout, output.Summary, output.Counts)
	}
	writeLine(stdout, "continue_ready: %t", output.ContinueReady)
	for _, line := range output.Lines {
		writeLine(stdout, "%s", line)
	}
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
	applySkillOptTrainOptimizerDefaultsFromMetadata(session.MetadataJSON, &request.Optimizer)
	if err := validateSkillOptTrainOptimizerRequestAfterDefaults(&request.Optimizer); err != nil {
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
		releaseGenerationLock, extendGenerationLock, _, err := acquireSkillOptTrainGenerationLock(ctx, store, session.ID, iteration.ID, generationLockTTL)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseGenerationLock(context.Background())
		}()
		request.GenerationLockExtend = extendGenerationLock
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
		var lines []string
		if result.LocalSurface {
			lines = []string{
				"review_surface: local",
				"review: local markdown import (feedback already imported)",
				"next: run train continue to sync the imported feedback",
			}
		} else {
			lines = []string{
				fmt.Sprintf("review: %s", result.URL),
				fmt.Sprintf("review_repo: %s", result.Repo.FullName()),
				fmt.Sprintf("preview_urls: %d", result.PreviewURLs),
				"next: wait for feedback, then run train continue after sync",
			}
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateReviewPublished:
		releaseReviewSyncLock, _, err := acquireSkillOptTrainReviewLock(ctx, store, session.ID, iteration.ID)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseReviewSyncLock(context.Background())
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
		if summary.CurrentPhase != skillopt.TrainStateReviewPublished {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		status, err := loadSkillOptReviewStatus(ctx, store, artifact.NewStore(paths.ArtifactBlobs), iteration.EvalRunID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		feedbackCount := len(status.Feedback) + len(status.RankedFeedback)
		var syncLines []string
		if !status.TrainingReady {
			lines, synced := autoSyncSkillOptTrainReviewFeedback(ctx, paths, store, *iteration)
			syncLines = append(syncLines, lines...)
			if synced {
				status, err = loadSkillOptReviewStatus(ctx, store, artifact.NewStore(paths.ArtifactBlobs), iteration.EvalRunID)
				if err != nil {
					return skillOptTrainContinueOutput{}, err
				}
				feedbackCount = len(status.Feedback) + len(status.RankedFeedback)
			}
		}
		if !status.TrainingReady {
			lines := append([]string{}, syncLines...)
			lines = append(lines,
				fmt.Sprintf("feedback_events: %d", feedbackCount),
				fmt.Sprintf("pairwise_preferences: %d", len(status.PairwisePreferences)),
				fmt.Sprintf("packet_blockers: %d", len(status.PacketBlockers)),
				fmt.Sprintf("training_blockers: %d", len(status.TrainingBlockers)),
			)
			for _, blocker := range status.PacketBlockers {
				lines = append(lines, fmt.Sprintf("packet_blocker: %s", blocker))
			}
			for _, blocker := range status.TrainingBlockers {
				lines = append(lines, fmt.Sprintf("training_blocker: %s", blocker))
			}
			if url := strings.TrimSpace(iteration.IssueURL); url != "" {
				lines = append(lines, fmt.Sprintf("continue_from_github: %s", url))
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
		if len(syncLines) > 0 {
			lines = append(syncLines, lines...)
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
		releaseOptimizerLock, optimizerLockState, err := acquireSkillOptTrainOptimizerLock(ctx, store, session.ID, iteration.ID, optimizerLockTTL, request.Optimizer)
		if err != nil {
			return output, err
		}
		request.Optimizer.OptimizerLockState = optimizerLockState
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
		if skillOptTrainContinueNeedsOptimizerPreflight(summary.CurrentPhase, request.Optimizer) {
			result, err := preflightSkillOptTrainOptimizerForContinue(ctx, paths, store, session, *iteration, request.Optimizer)
			if err != nil {
				if skillOptTrainOptimizerResultHasReport(result) {
					output.Lines = skillOptTrainOptimizerReportLines(result)
				}
				return output, err
			}
		}
		result, err := continueSkillOptTrainOptimizer(ctx, paths, store, session, *iteration, request.Optimizer, request.Progress)
		if err != nil {
			if skillOptTrainOptimizerResultHasReport(result) {
				output.Lines = skillOptTrainOptimizerReportLines(result)
			}
			return output, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := skillOptTrainOptimizerReportLines(result)
		if result.ExportedOnly {
			lines = append(lines,
				fmt.Sprintf("training_package: %s", result.TrainingPackagePath),
				"next: run train continue without --export-only to launch the optimizer",
			)
			return skillOptTrainContinueOutput{
				Summary:       updatedSummary,
				Counts:        updatedCounts,
				ContinueReady: true,
				Lines:         lines,
			}, nil
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
			return continueSkillOptTrainCandidateDecision(ctx, paths, store, session, *iteration, counts, request)
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
			return continueSkillOptTrainCandidateDecision(ctx, paths, store, updatedSession, *updatedIteration, updatedCounts, request)
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
			"next: choose promote, reject with a reason, or wait; keep improving by rejecting with an actionable reason and then running --start-next",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateCandidateReviewPublished:
		return continueSkillOptTrainCandidateDecision(ctx, paths, store, session, *iteration, counts, request)
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

func loadSkillOptTrainStatusSnapshot(ctx context.Context, store *db.Store, sessionID string, verbose bool) (skillOptTrainStatusSnapshot, error) {
	session, iteration, counts, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainStatusSnapshot{}, err
	}
	summary := skillopt.BuildTrainStatusSummary(session, iteration, counts)
	snapshot := buildSkillOptTrainStatusSnapshot(session, iteration, summary, counts)
	details, err := buildSkillOptTrainStatusVerbose(ctx, store, session, iteration)
	if err != nil {
		return skillOptTrainStatusSnapshot{}, err
	}
	snapshot.Verbose = &details
	snapshot = applySkillOptTrainStableStatus(snapshot)
	if !verbose {
		snapshot.Verbose = nil
	}
	return snapshot, nil
}

func buildSkillOptTrainStatusSnapshot(session db.SkillOptTrainSession, iteration *db.SkillOptTrainIteration, summary skillopt.TrainStatusSummary, counts skillopt.TrainStatusCounts) skillOptTrainStatusSnapshot {
	policy := summary.PreviewPolicy
	generatedOptions := 0
	if iteration != nil {
		generatedOptions = metadataNumber(decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["generation"]), "generated_options")
	} else {
		generatedOptions = metadataNumber(decodedSkillOptMetadataValue(decodedSkillOptMetadata(session.MetadataJSON)["generation"]), "generated_options")
	}
	currentStep := strings.TrimSpace(summary.BlockedStep)
	if currentStep == "" {
		currentStep = summary.CurrentPhase
	}
	return skillOptTrainStatusSnapshot{
		SessionID:          summary.SessionID,
		IterationID:        summary.IterationID,
		TemplateID:         strings.TrimSpace(session.TemplateID),
		TemplateVersion:    strings.TrimSpace(session.TemplateVersionID),
		TargetRepo:         strings.TrimSpace(session.TargetRepo),
		WorkspaceRepo:      strings.TrimSpace(session.WorkspaceRepo),
		TaskKind:           strings.TrimSpace(session.TaskKind),
		StatusPhase:        summary.CurrentPhase,
		CurrentPhase:       summary.CurrentPhase,
		CurrentStep:        currentStep,
		CompletedSteps:     append([]string(nil), summary.CompletedSteps...),
		BlockedStep:        summary.BlockedStep,
		NextAction:         summary.NextAction,
		IssueURL:           summary.IssueURL,
		PullRequestURL:     summary.PullRequestURL,
		ContinueFromGitHub: skillOptTrainContinueFromGitHubURL(summary.CurrentPhase, summary.IssueURL),
		CandidateVersion:   summary.CandidateVersion,
		PreviewPolicy: skillOptTrainPreviewPolicyJSON{
			Mode:               policy.Mode,
			Renderer:           policy.Renderer,
			Publisher:          policy.Publisher,
			Repo:               policy.Repo,
			RouteTemplate:      policy.RouteTemplate,
			ExpectedReviewRepo: policy.ExpectedReviewRepo,
		},
		Counts: skillOptTrainStatusCountsJSON{
			ReviewItems:          counts.ReviewItems,
			FeedbackEvents:       counts.FeedbackEvents,
			RankedFeedbackEvents: counts.RankedFeedbackEvents,
			PairwisePreferences:  counts.PairwisePreferences,
		},
		Progress: skillOptTrainStatusProgress{
			ReviewItems:          counts.ReviewItems,
			FeedbackEvents:       counts.FeedbackEvents,
			RankedFeedbackEvents: counts.RankedFeedbackEvents,
			PairwisePreferences:  counts.PairwisePreferences,
			GeneratedOptions:     generatedOptions,
			ETA:                  "unknown",
		},
	}
}

func applySkillOptTrainStableStatus(snapshot skillOptTrainStatusSnapshot) skillOptTrainStatusSnapshot {
	if strings.TrimSpace(snapshot.StatusPhase) == "" {
		snapshot.StatusPhase = strings.TrimSpace(snapshot.CurrentPhase)
	}
	if snapshot.Verbose != nil {
		if reason := strings.TrimSpace(snapshot.Verbose.Candidate.NoCandidateReason); reason != "" {
			snapshot.NoCandidateReason = reason
		}
		if len(snapshot.Verbose.Candidate.NoCandidateDetails) > 0 {
			snapshot.NoCandidateDetails = snapshot.Verbose.Candidate.NoCandidateDetails
		}
		if optimizer := snapshot.Verbose.Optimizer; optimizer != nil {
			if available, ok := optimizer["recovery_available"].(bool); ok {
				snapshot.RecoveryAvailable = available
			}
		}
	}
	snapshot.StatusPhase = skillOptTrainStableStatusPhase(snapshot)
	return snapshot
}

// skillOptTrainLockPhase maps an active generation/optimizer resource lock to a
// stable status phase. It is shared by `train status` and the dashboard so both
// report the same live phase. ok is false when no lock determines the phase.
func skillOptTrainLockPhase(locks []skillOptTrainStatusLock) (string, bool) {
	for _, lock := range locks {
		switch lock.Name {
		case "optimizer", "optimizer_legacy":
			switch strings.TrimSpace(lock.Status) {
			case "active":
				return "optimizer_running", true
			case "active_expired_heartbeat":
				return "optimizer_heartbeat_stale", true
			case "stale":
				return "blocked_stale_lock", true
			}
		case "generation":
			switch strings.TrimSpace(lock.Status) {
			case "active":
				return "generating_options", true
			case "active_expired_heartbeat":
				return "generating_options_heartbeat_stale", true
			case "stale":
				return "blocked_stale_lock", true
			}
		}
	}
	return "", false
}

func skillOptTrainStableStatusPhase(snapshot skillOptTrainStatusSnapshot) string {
	if snapshot.Verbose != nil {
		if phase, ok := skillOptTrainLockPhase(snapshot.Verbose.ActiveLocks); ok {
			return phase
		}
		statuses := snapshot.Verbose.MetadataStatus
		optimizerStatus := strings.TrimSpace(statuses["optimizer"])
		candidateImportStatus := strings.TrimSpace(statuses["candidate_import"])
		if optimizerStatus == "preflight_running" || optimizerStatus == "preflight" {
			return "preflight_running"
		}
		if snapshot.RecoveryAvailable && optimizerStatus == "failed" {
			return "recovery_available"
		}
		if skillOptStatusFailureLooksConfigBlocked(snapshot.Verbose.Optimizer) {
			return "blocked_config"
		}
		if optimizerStatus == "failed" || candidateImportStatus == "failed" {
			return "failed_unrecoverable"
		}
	}
	switch strings.TrimSpace(snapshot.CurrentPhase) {
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		return "optimizer_completed_no_candidate"
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		return "optimizer_completed_candidate"
	}
	if snapshot.RecoveryAvailable {
		return "recovery_available"
	}
	if strings.TrimSpace(snapshot.StatusPhase) != "" {
		return strings.TrimSpace(snapshot.StatusPhase)
	}
	return strings.TrimSpace(snapshot.CurrentPhase)
}

func skillOptStatusFailureLooksConfigBlocked(metadata map[string]any) bool {
	if metadataString(metadata, "status") != "failed" {
		return false
	}
	errorText := strings.ToLower(metadataString(metadata, "error"))
	for _, marker := range []string{"config", "credential", "api key", "openai", "azure", "backend", "gitmoot-skillopt", "executable", "not found", "install", "path"} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

func buildSkillOptTrainStatusVerbose(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration *db.SkillOptTrainIteration) (skillOptTrainStatusVerbose, error) {
	details := skillOptTrainStatusVerbose{
		CreatedAt: session.CreatedAt,
		UpdatedAt: session.UpdatedAt,
		Elapsed:   skillOptElapsedText(session.CreatedAt),
		Jobs:      skillOptTrainStatusJobs{},
	}
	statuses := map[string]string{}
	addStatus := func(name string, metadata map[string]any) {
		if status := metadataString(metadata, "status"); status != "" {
			statuses[name] = status
		}
	}
	statusMetadata := decodedSkillOptMetadata(session.MetadataJSON)
	var iterationMetadata map[string]any
	if iteration != nil {
		iterationMetadata = decodedSkillOptMetadata(iteration.MetadataJSON)
		statusMetadata = iterationMetadata
	}
	addStatus("generation", decodedSkillOptMetadataValue(statusMetadata["generation"]))
	addStatus("review", decodedSkillOptMetadataValue(statusMetadata["review"]))
	addStatus("feedback_sync", decodedSkillOptMetadataValue(statusMetadata["feedback_sync"]))
	addStatus("optimizer", decodedSkillOptMetadataValue(statusMetadata["optimizer"]))
	addStatus("candidate_import", decodedSkillOptMetadataValue(statusMetadata["candidate_import"]))
	addStatus("candidate_review", decodedSkillOptMetadataValue(statusMetadata["candidate_review"]))
	addStatus("candidate_decision", decodedSkillOptMetadataValue(statusMetadata["candidate_decision"]))
	if len(statuses) > 0 {
		details.MetadataStatus = statuses
	}
	// Carry the full generation metadata (status + error) so a failed background
	// generate can be surfaced in the train-run view rather than silently stalling.
	if generation := decodedSkillOptMetadataValue(statusMetadata["generation"]); len(generation) > 0 {
		details.Generation = generation
	}
	if iteration == nil {
		return details, nil
	}
	details.EvalRunID = strings.TrimSpace(iteration.EvalRunID)
	details.BaseTemplateVersionID = strings.TrimSpace(iteration.BaseTemplateVersionID)
	details.Mode = strings.TrimSpace(iteration.Mode)
	details.ExplorationLevel = strings.TrimSpace(iteration.ExplorationLevel)
	details.CreatedAt = iteration.CreatedAt
	details.UpdatedAt = iteration.UpdatedAt
	details.Elapsed = skillOptElapsedText(iteration.CreatedAt)
	details.ReviewIssue = skillOptTrainStatusReviewIssue{
		Repo:   strings.TrimSpace(iteration.IssueRepo),
		Number: iteration.IssueNumber,
		URL:    strings.TrimSpace(iteration.IssueURL),
	}
	candidateImport := decodedSkillOptMetadataValue(iterationMetadata["candidate_import"])
	details.Candidate = skillOptTrainStatusCandidate{
		VersionID:          strings.TrimSpace(iteration.CandidateVersionID),
		PullRequestURL:     strings.TrimSpace(iteration.PullRequestURL),
		NoCandidateReason:  metadataString(candidateImport, "no_candidate_reason"),
		NoCandidateDetails: decodedSkillOptMetadataValue(candidateImport["no_candidate_details"]),
	}
	if details.Candidate.PullRequestURL == "" {
		// Issue-based candidate reviews never set iteration.PullRequestURL; the
		// decision link lives in the candidate_review metadata instead.
		review := decodedSkillOptMetadataValue(iterationMetadata["candidate_review"])
		details.Candidate.PullRequestURL = skillOptCandidateReviewURLFromMetadata(review)
	}
	if optimizer := decodedSkillOptMetadataValue(iterationMetadata["optimizer"]); len(optimizer) > 0 {
		candidateImport := decodedSkillOptMetadataValue(iterationMetadata["candidate_import"])
		if attemptState := skillOptTrainOptimizerAttemptState(skillopt.NormalizeTrainState(iteration.State), optimizer, candidateImport); attemptState != "" {
			optimizer["optimizer_attempt_state"] = attemptState
		}
		optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(config.Paths{}, session, *iteration, skillOptTrainOptimizerRequest{})
		if err == nil {
			optimizer["recovery_available"] = skillOptTrainOptimizerRecoveryAvailable(optimizerPaths)
		}
		details.Optimizer = optimizer
	}
	activeLocks, err := skillOptTrainActiveLocks(ctx, store, session.ID, iteration.ID)
	if err != nil {
		return skillOptTrainStatusVerbose{}, err
	}
	details.ActiveLocks = activeLocks
	jobs, err := skillOptTrainStatusJobSummary(ctx, store, iterationMetadata)
	if err != nil {
		return skillOptTrainStatusVerbose{}, err
	}
	details.Jobs = jobs
	items, err := skillOptTrainStatusItems(ctx, store, iteration.EvalRunID)
	if err != nil {
		return skillOptTrainStatusVerbose{}, err
	}
	details.Items = items
	return details, nil
}

func skillOptTrainActiveLocks(ctx context.Context, store *db.Store, sessionID string, iterationID string) ([]skillOptTrainStatusLock, error) {
	candidates := []struct {
		name string
		key  string
	}{
		{name: "generation", key: skillOptTrainGenerationLockKey(sessionID, iterationID)},
		{name: "review", key: skillOptTrainReviewLockKey(sessionID, iterationID)},
		{name: "optimizer", key: skillOptTrainOptimizerLockKey(sessionID, iterationID)},
		{name: "optimizer_legacy", key: skillOptTrainLegacyOptimizerLockKey(sessionID, iterationID)},
		{name: "candidate_review", key: skillOptTrainCandidateReviewLockKey(sessionID, iterationID)},
		{name: "start_next", key: skillOptTrainStartNextLockKey(sessionID)},
	}
	locks := []skillOptTrainStatusLock{}
	now := time.Now().UTC()
	for _, candidate := range candidates {
		lock, err := store.GetResourceLock(ctx, candidate.key)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		status := "active"
		switch candidate.name {
		case "optimizer", "optimizer_legacy", "generation":
			// Report a heartbeat-stale/stale status (via OwnerPID liveness)
			// instead of dropping an expired-but-running lock, so the phase
			// does not flap back to items_ready mid-generation.
			status = skillOptTrainOptimizerLockStatus(lock, now)
		default:
			if !skillOptResourceLockActive(lock, now) {
				continue
			}
		}
		locks = append(locks, skillOptTrainStatusLock{
			Name:          candidate.name,
			Key:           lock.ResourceKey,
			Status:        status,
			OwnerJobID:    strings.TrimSpace(lock.OwnerJobID),
			OwnerPID:      lock.OwnerPID,
			OwnerHostname: strings.TrimSpace(lock.OwnerHostname),
			CommandHash:   strings.TrimSpace(lock.CommandHash),
			AcquiredAt:    strings.TrimSpace(lock.AcquiredAt),
			UpdatedAt:     strings.TrimSpace(lock.UpdatedAt),
			ExpiresAt:     strings.TrimSpace(lock.ExpiresAt),
			Elapsed:       skillOptLockElapsedText(lock.AcquiredAt, now),
		})
	}
	return locks, nil
}

func skillOptResourceLockActive(lock db.ResourceLock, now time.Time) bool {
	expiresAt := strings.TrimSpace(lock.ExpiresAt)
	if expiresAt == "" {
		return true
	}
	parsed, ok := parseSkillOptStatusTime(expiresAt)
	if !ok {
		return true
	}
	return parsed.After(now)
}

func skillOptTrainStatusJobSummary(ctx context.Context, store *db.Store, metadata map[string]any) (skillOptTrainStatusJobs, error) {
	generation := decodedSkillOptMetadataValue(metadata["generation"])
	jobIDs := metadataStringSlice(generation, "jobs")
	summary := skillOptTrainStatusJobs{}
	for _, jobID := range jobIDs {
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return skillOptTrainStatusJobs{}, err
		}
		summary.Total++
		switch strings.TrimSpace(strings.ToLower(job.State)) {
		case "queued":
			summary.Queued++
		case "running":
			summary.Running++
		case "succeeded":
			summary.Succeeded++
		case "failed":
			summary.Failed++
		default:
			summary.Other++
		}
		summary.Items = append(summary.Items, skillOptTrainStatusJobRef{
			ID:    strings.TrimSpace(job.ID),
			Agent: strings.TrimSpace(job.Agent),
			Type:  strings.TrimSpace(job.Type),
			State: strings.TrimSpace(job.State),
		})
	}
	return summary, nil
}

func skillOptTrainStatusItems(ctx context.Context, store *db.Store, runID string) ([]skillOptTrainStatusItem, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, nil
	}
	run, err := store.GetEvalRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	rankedRun := skillOptRunUsesRankedOptions(run)
	reviewItems, err := store.ListEvalReviewItems(ctx, runID)
	if err != nil {
		return nil, err
	}
	items := make([]skillOptTrainStatusItem, 0, len(reviewItems))
	for _, item := range reviewItems {
		statusItem := skillOptTrainStatusItem{
			ItemID: strings.TrimSpace(item.ItemID),
			Title:  strings.TrimSpace(item.Title),
		}
		options, err := store.ListEvalReviewOptions(ctx, runID, item.ItemID)
		if err != nil {
			return nil, err
		}
		for _, option := range options {
			statusItem.OptionLabels = append(statusItem.OptionLabels, strings.ToUpper(strings.TrimSpace(option.Label)))
		}
		if len(statusItem.OptionLabels) == 0 && !rankedRun {
			if strings.TrimSpace(item.BaselineArtifactID) != "" {
				statusItem.OptionLabels = append(statusItem.OptionLabels, "BASELINE")
			}
			if strings.TrimSpace(item.CandidateArtifactID) != "" {
				statusItem.OptionLabels = append(statusItem.OptionLabels, "CANDIDATE")
			}
			if len(statusItem.OptionLabels) == 0 {
				statusItem.OptionLabels = append(statusItem.OptionLabels, "BASELINE", "CANDIDATE")
			}
		}
		items = append(items, statusItem)
	}
	return items, nil
}

func skillOptElapsedText(startedAt string) string {
	started, ok := parseSkillOptStatusTime(startedAt)
	if !ok {
		return "unknown"
	}
	elapsed := time.Since(started)
	if elapsed < 0 {
		return "unknown"
	}
	return elapsed.Round(time.Second).String()
}

func parseSkillOptStatusTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func skillOptTrainWatchState(snapshot skillOptTrainStatusSnapshot) string {
	if skillOptTrainWatchDone(snapshot) {
		return "waiting"
	}
	return "active"
}

func skillOptTrainWatchDone(snapshot skillOptTrainStatusSnapshot) bool {
	if skillopt.IsTerminalTrainState(snapshot.CurrentPhase) {
		return true
	}
	if snapshot.Verbose == nil {
		return true
	}
	for _, lock := range snapshot.Verbose.ActiveLocks {
		if strings.TrimSpace(lock.Status) != "stale" {
			return false
		}
	}
	return true
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
	if url := skillOptTrainContinueFromGitHubURL(summary.CurrentPhase, summary.IssueURL); url != "" {
		writeLine(stdout, "continue_from_github: %s", url)
	}
	writeLine(stdout, "review_items: %d", counts.ReviewItems)
	writeLine(stdout, "feedback: %d", summary.FeedbackCount)
	writeLine(stdout, "pairwise_preferences: %d", counts.PairwisePreferences)
}

// skillOptTrainContinueFromGitHubURL returns the GitHub issue/PR URL a human can
// act on to advance the train from a review-blocked phase (the review-watcher
// imports comments). It returns "" for phases that are not blocked on a human
// reviewing on GitHub. At candidate_review_published the iteration's IssueURL
// already points at the candidate review issue.
func skillOptTrainContinueFromGitHubURL(phase, issueURL string) string {
	switch phase {
	case "review_published", "candidate_review_published":
		return strings.TrimSpace(issueURL)
	default:
		return ""
	}
}

func printSkillOptTrainStatusSnapshot(stdout io.Writer, snapshot skillOptTrainStatusSnapshot, verbose bool) {
	writeLine(stdout, "session: %s", snapshot.SessionID)
	writeLine(stdout, "iteration: %s", emptyText(snapshot.IterationID))
	writeLine(stdout, "preview_mode: %s", snapshot.PreviewPolicy.Mode)
	writeLine(stdout, "preview_renderer: %s", snapshot.PreviewPolicy.Renderer)
	writeLine(stdout, "preview_publisher: %s", snapshot.PreviewPolicy.Publisher)
	writeLine(stdout, "preview_repo: %s", emptyText(snapshot.PreviewPolicy.Repo))
	writeLine(stdout, "preview_route_template: %s", emptyText(snapshot.PreviewPolicy.RouteTemplate))
	writeLine(stdout, "expected_review_repo: %s", emptyText(snapshot.PreviewPolicy.ExpectedReviewRepo))
	writeLine(stdout, "status_phase: %s", emptyText(snapshot.StatusPhase))
	writeLine(stdout, "current_phase: %s", snapshot.CurrentPhase)
	writeLine(stdout, "completed_steps: %s", strings.Join(snapshot.CompletedSteps, ","))
	writeLine(stdout, "blocked_step: %s", emptyText(snapshot.BlockedStep))
	writeLine(stdout, "current_step: %s", snapshot.CurrentStep)
	writeLine(stdout, "next_action: %s", snapshot.NextAction)
	writeLine(stdout, "issue: %s", emptyText(snapshot.IssueURL))
	writeLine(stdout, "pull_request: %s", emptyText(snapshot.PullRequestURL))
	writeLine(stdout, "candidate: %s", emptyText(snapshot.CandidateVersion))
	if url := skillOptTrainContinueFromGitHubURL(snapshot.CurrentPhase, snapshot.IssueURL); url != "" {
		writeLine(stdout, "continue_from_github: %s", url)
	}
	writeLine(stdout, "recovery_available: %t", snapshot.RecoveryAvailable)
	if snapshot.NoCandidateReason != "" {
		writeLine(stdout, "no_candidate_reason: %s", snapshot.NoCandidateReason)
	}
	printSkillOptNoCandidateDetails(stdout, snapshot.NoCandidateDetails)
	writeLine(stdout, "review_items: %d", snapshot.Counts.ReviewItems)
	writeLine(stdout, "feedback: %d", snapshot.Counts.FeedbackEvents+snapshot.Counts.RankedFeedbackEvents)
	writeLine(stdout, "pairwise_preferences: %d", snapshot.Counts.PairwisePreferences)
	if !verbose || snapshot.Verbose == nil {
		return
	}
	writeLine(stdout, "elapsed: %s", snapshot.Verbose.Elapsed)
	writeLine(stdout, "eta: %s", snapshot.Progress.ETA)
	writeLine(stdout, "generated_options: %d", snapshot.Progress.GeneratedOptions)
	writeLine(stdout, "jobs_total: %d", snapshot.Verbose.Jobs.Total)
	writeLine(stdout, "jobs_running: %d", snapshot.Verbose.Jobs.Running)
	writeLine(stdout, "jobs_succeeded: %d", snapshot.Verbose.Jobs.Succeeded)
	writeLine(stdout, "jobs_failed: %d", snapshot.Verbose.Jobs.Failed)
	if snapshot.Verbose.EvalRunID != "" {
		writeLine(stdout, "eval_run: %s", snapshot.Verbose.EvalRunID)
	}
	if snapshot.Verbose.Mode != "" {
		writeLine(stdout, "mode: %s", snapshot.Verbose.Mode)
	}
	if snapshot.Verbose.ExplorationLevel != "" {
		writeLine(stdout, "exploration_level: %s", snapshot.Verbose.ExplorationLevel)
	}
	if snapshot.Verbose.ReviewIssue.URL != "" {
		writeLine(stdout, "review_issue: %s", snapshot.Verbose.ReviewIssue.URL)
	}
	if snapshot.NoCandidateReason == "" && snapshot.Verbose.Candidate.NoCandidateReason != "" {
		writeLine(stdout, "no_candidate_reason: %s", snapshot.Verbose.Candidate.NoCandidateReason)
	}
	if len(snapshot.NoCandidateDetails) == 0 {
		printSkillOptNoCandidateDetails(stdout, snapshot.Verbose.Candidate.NoCandidateDetails)
	}
	if snapshot.Verbose.Optimizer != nil {
		if attempt := metadataString(snapshot.Verbose.Optimizer, "optimizer_attempt"); attempt != "" {
			writeLine(stdout, "optimizer_attempt: %s", attempt)
		}
		if status := metadataString(snapshot.Verbose.Optimizer, "optimizer_attempt_state"); status != "" {
			writeLine(stdout, "optimizer_attempt_state: %s", status)
		}
		if path := metadataString(snapshot.Verbose.Optimizer, "optimizer_attempt_path"); path != "" {
			writeLine(stdout, "optimizer_attempt_path: %s", path)
		}
		if mode := metadataString(snapshot.Verbose.Optimizer, "feedback_direct_mode"); mode != "" {
			writeLine(stdout, "feedback_direct_mode: %s", mode)
		}
		for _, key := range []string{
			"optimizer_views",
			"retry_optimizer_views",
			"target_artifact_retry_budget",
			"hard_failure_retry_budget",
			"noop_retry_budget",
			"gate_reject_retry_budget",
			"wrong_artifact_retry_budget",
		} {
			if value := metadataString(snapshot.Verbose.Optimizer, key); value != "" {
				writeLine(stdout, "%s: %s", key, value)
			}
		}
		if available, ok := snapshot.Verbose.Optimizer["recovery_available"]; ok {
			writeLine(stdout, "optimizer_recovery_available: %v", available)
		}
	}
	for _, lock := range snapshot.Verbose.ActiveLocks {
		writeLine(stdout, "active_lock: %s", skillOptTrainStatusLockText(lock))
	}
}

func skillOptTrainOptimizerAttemptState(currentPhase string, optimizer map[string]any, candidateImport map[string]any) string {
	optimizerStatus := metadataString(optimizer, "status")
	if optimizerStatus == "running" {
		return "running"
	}
	switch strings.TrimSpace(currentPhase) {
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		return "completed_no_candidate"
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		return "completed_candidate"
	}
	optimizerAttempt := metadataString(optimizer, "optimizer_attempt")
	importAttempt := metadataString(candidateImport, "optimizer_attempt")
	if optimizerAttempt != "" && importAttempt != "" && optimizerAttempt != importAttempt {
		candidateImport = nil
	}
	switch metadataString(candidateImport, "status") {
	case "no_candidate":
		return "completed_no_candidate"
	case "succeeded", "recovered":
		return "completed_candidate"
	case "failed":
		return "candidate_import_failed"
	}
	return optimizerStatus
}

func printSkillOptNoCandidateDetails(stdout io.Writer, details map[string]any) {
	if len(details) == 0 {
		return
	}
	for _, key := range []string{
		"feedback_source",
		"feedback_target",
		"review_issue",
		"review_run_id",
		"reviewed_skill_version",
		"score_basis",
	} {
		if value := metadataString(details, key); value != "" {
			writeLine(stdout, "%s: %s", key, value)
		}
	}
	if attemptedPatch := metadataString(details, "attempted_patch"); attemptedPatch != "" {
		writeLine(stdout, "attempted_patch: %s", attemptedPatch)
	}
	for _, key := range []string{
		"baseline_hard",
		"baseline_soft",
		"baseline_gate",
		"candidate_hard",
		"candidate_soft",
		"candidate_gate",
	} {
		if value := metadataString(details, key); value != "" {
			writeLine(stdout, "%s: %s", key, value)
		}
	}
	if retryAttempts := metadataString(details, "retry_attempts"); retryAttempts != "" {
		writeLine(stdout, "retry_attempts: %s", retryAttempts)
	}
	if retryBudget := metadataString(details, "retry_budget"); retryBudget != "" {
		writeLine(stdout, "retry_budget: %s", retryBudget)
	}
	if duplicateRetry := metadataBoolPtr(details, "duplicate_retry_detected"); duplicateRetry != nil {
		writeLine(stdout, "duplicate_retry_detected: %t", *duplicateRetry)
	}
	if diagnosticCategories := metadataStringSlice(details, "diagnostic_categories"); len(diagnosticCategories) > 0 {
		writeLine(stdout, "diagnostic_categories: %s", strings.Join(diagnosticCategories, ","))
	}
	if selectionGateRelation := metadataString(details, "selection_gate_relation"); selectionGateRelation != "" {
		writeLine(stdout, "selection_gate_relation: %s", selectionGateRelation)
	}
	if retryBudgetExhausted := metadataBoolPtr(details, "retry_budget_exhausted"); retryBudgetExhausted != nil {
		writeLine(stdout, "retry_budget_exhausted: %t", *retryBudgetExhausted)
	}
	if retryStopReasons := metadataStringSlice(details, "retry_stop_reasons"); len(retryStopReasons) > 0 {
		writeLine(stdout, "retry_stop_reasons: %s", strings.Join(retryStopReasons, ","))
	}
	if optimizerContextItems := metadataStringSlice(details, "optimizer_context_items"); len(optimizerContextItems) > 0 {
		writeLine(stdout, "optimizer_context_items: %s", strings.Join(optimizerContextItems, ","))
	}
	if scoreGap := metadataString(details, "score_gap"); scoreGap != "" {
		writeLine(stdout, "score_gap: %s", scoreGap)
	}
	if scoreGapHandling := metadataString(details, "score_gap_handling"); scoreGapHandling != "" {
		writeLine(stdout, "score_gap_handling: %s", scoreGapHandling)
	}
	if hardScoreHandling := metadataString(details, "hard_score_handling"); hardScoreHandling != "" {
		writeLine(stdout, "hard_score_handling: %s", hardScoreHandling)
	}
	if stopReason := metadataString(details, "stop_reason"); stopReason != "" {
		writeLine(stdout, "stop_reason: %s", stopReason)
	}
	if evaluatorReason := metadataString(details, "evaluator_reason"); evaluatorReason != "" {
		writeLine(stdout, "evaluator_reason: %s", evaluatorReason)
	}
	if optimizerHint := metadataString(details, "optimizer_hint"); optimizerHint != "" {
		writeLine(stdout, "optimizer_hint: %s", optimizerHint)
	}
	if failedDimensions := metadataStringSlice(details, "failed_dimensions"); len(failedDimensions) > 0 {
		writeLine(stdout, "failed_dimensions: %s", strings.Join(failedDimensions, ","))
	}
	if feedbackThemes := metadataStringSlice(details, "feedback_themes"); len(feedbackThemes) > 0 {
		writeLine(stdout, "feedback_themes: %s", strings.Join(feedbackThemes, "; "))
	}
	printSkillOptHumanFeedbackContext(stdout, decodedSkillOptMetadataValue(details["human_feedback_context"]))
	nextActions := metadataStringSlice(details, "next_actions")
	if len(nextActions) == 0 {
		nextActions = metadataStringSlice(details, "next_action")
	}
	for _, nextAction := range nextActions {
		writeLine(stdout, "next_action_option: %s", nextAction)
	}
	rejection := decodedSkillOptMetadataValue(details["rejection"])
	if len(rejection) == 0 {
		return
	}
	baseline := decodedSkillOptMetadataValue(rejection["baseline"])
	candidate := decodedSkillOptMetadataValue(rejection["candidate"])
	if len(baseline) > 0 || len(candidate) > 0 {
		writeLine(stdout, "rejection: baseline_gate=%s candidate_gate=%s", metadataString(baseline, "gate_score"), metadataString(candidate, "gate_score"))
	}
	if optimizerHint := metadataString(rejection, "optimizer_hint"); optimizerHint != "" {
		writeLine(stdout, "rejection_optimizer_hint: %s", optimizerHint)
	}
	if failedDimensions := metadataStringSlice(rejection, "failed_dimensions"); len(failedDimensions) > 0 {
		writeLine(stdout, "rejection_failed_dimensions: %s", strings.Join(failedDimensions, ","))
	}
	printSkillOptHumanFeedbackContext(stdout, decodedSkillOptMetadataValue(rejection["human_feedback_context"]))
}

func printSkillOptHumanFeedbackContext(stdout io.Writer, context map[string]any) {
	if len(context) == 0 {
		return
	}
	for _, key := range []string{
		"feedback_source",
		"feedback_target",
		"review_issue",
		"review_run_id",
		"reviewed_skill_version",
		"source_item_ids",
		"rankings",
		"themes",
		"preserve",
		"improve",
		"avoid",
	} {
		if values := metadataStringSlice(context, key); len(values) > 0 {
			writeLine(stdout, "human_feedback_%s: %s", key, strings.Join(values, "; "))
		} else if value := metadataString(context, key); value != "" {
			writeLine(stdout, "human_feedback_%s: %s", key, value)
		}
	}
}

func skillOptTrainStatusLockText(lock skillOptTrainStatusLock) string {
	parts := []string{
		strings.TrimSpace(lock.Name),
		strings.TrimSpace(lock.Key),
		"status=" + emptyText(lock.Status),
	}
	if strings.TrimSpace(lock.OwnerJobID) != "" {
		parts = append(parts, "owner="+strings.TrimSpace(lock.OwnerJobID))
	}
	if lock.OwnerPID > 0 {
		parts = append(parts, "pid="+strconv.FormatInt(lock.OwnerPID, 10))
	}
	if strings.TrimSpace(lock.OwnerHostname) != "" {
		parts = append(parts, "host="+strings.TrimSpace(lock.OwnerHostname))
	}
	if strings.TrimSpace(lock.UpdatedAt) != "" {
		parts = append(parts, "heartbeat="+strings.TrimSpace(lock.UpdatedAt))
	}
	if strings.TrimSpace(lock.ExpiresAt) != "" {
		parts = append(parts, "expires="+strings.TrimSpace(lock.ExpiresAt))
	}
	if strings.TrimSpace(lock.Elapsed) != "" {
		parts = append(parts, "elapsed="+strings.TrimSpace(lock.Elapsed))
	}
	if strings.TrimSpace(lock.CommandHash) != "" {
		parts = append(parts, "hash="+strings.TrimSpace(lock.CommandHash))
	}
	return strings.Join(parts, " ")
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

// skillOptTemplateJudgeEvaluation extracts a template's promoted judge prompt
// fields (written by `skillopt judge promote`, #354) into the shape the
// evaluator reader consumes: judge_prompt_templates as a real object and
// judge_prompt_version as a string. Returns nil when the template carries none,
// so train-start metadata stays byte-identical for templates without a promoted
// judge prompt. This is the consumption half of #354: the promoted prompt is
// folded into the eval-run's evaluation config so a subsequent run resolves it.
func skillOptTemplateJudgeEvaluation(template db.AgentTemplate) map[string]any {
	metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
	if err != nil {
		return nil
	}
	out := map[string]any{}
	if raw := strings.TrimSpace(metadata.Evaluation["judge_prompt_templates"]); raw != "" {
		templates := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &templates); err == nil && len(templates) > 0 {
			out["judge_prompt_templates"] = templates
		}
	}
	if version := strings.TrimSpace(metadata.Evaluation["judge_prompt_version"]); version != "" {
		out["judge_prompt_version"] = version
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func skillOptTrainStartMetadata(request string, mode string, explorationLevel string, optionsCount int, preferredGate string, items []skillOptTrainItemPlan, warnings []string, previewPolicy skillopt.TrainPreviewPolicy, configDefaults skillOptTrainStartConfigDefaults, judgeEvaluation map[string]any) string {
	lines := strings.Count(request, "\n") + 1
	words := len(strings.Fields(request))
	previewMetadata, reviewMetadata := previewPolicy.Metadata()
	evaluation := map[string]any{
		"preferred_gate": preferredGate,
	}
	for key, value := range judgeEvaluation {
		evaluation[key] = value
	}
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
		"evaluation":        evaluation,
		"preview":           previewMetadata,
		"review":            reviewMetadata,
		"source":            "gitmoot skillopt train start",
	}
	if optimizerDefaults := skillOptTrainOptimizerDefaultsMetadata(configDefaults.Optimizer); len(optimizerDefaults) > 0 {
		metadata["optimizer_defaults"] = optimizerDefaults
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
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
