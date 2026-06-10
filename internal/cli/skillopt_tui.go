package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// skillOptTrainInitTUICapable reports whether the interactive train-init form
// should run: both stdin and stdout must be terminals (bubbletea needs raw-mode
// keys and a screen), and the user must not have opted out. It is a var so
// dispatch tests can stub it.
var skillOptTrainInitTUICapable = func() bool {
	if os.Getenv("GITMOOT_NO_TUI") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	in, errIn := os.Stdin.Stat()
	out, errOut := os.Stdout.Stat()
	return errIn == nil && errOut == nil &&
		in.Mode()&os.ModeCharDevice != 0 && out.Mode()&os.ModeCharDevice != 0
}

// runSkillOptTrainInitTUI runs the bubbletea form to fill the missing fields. It
// is a var so dispatch tests can stub the whole run.
var runSkillOptTrainInitTUI = runSkillOptTrainInitTUIImpl

func runSkillOptTrainInitTUIImpl(home, scope string, stdout io.Writer, values *skillOptTrainInitInputs, missing []string) error {
	return withStore(home, func(store *db.Store) error {
		fields, err := buildSkillOptTrainInitTUIFields(store, scope, *values, missing)
		if err != nil {
			return err
		}
		// Belt-and-braces: a Quit batched with a delete command may exit before the
		// delete runs, so sweep any leftover PENDING records for the form's fields
		// on the way out. Resolved answers are left for a rerun to consume.
		defer cleanupSkillOptTrainInitTUIPrompts(store, scope, missing)

		current := *values
		interpret := func(field, text string) (string, string) {
			return skillOptTrainInitInterpretCore(field, text, db.InteractivePrompt{}, nil)
		}
		summary := func(answers map[string]string) [][]string {
			return skillOptTrainInitTUISummaryRows(current, answers)
		}
		model := tui.NewTrainInit(store, fields, summary, interpret, skillOptTrainInitWizardPoll)
		final, err := tea.NewProgram(model, tea.WithOutput(stdout)).Run()
		if err != nil {
			return err
		}
		result := final.(tui.TrainInitModel).Result()
		if result.Aborted {
			return errSkillOptTrainInitAborted
		}
		applySkillOptTrainInitTUIResult(values, result.Values)
		return nil
	})
}

// buildSkillOptTrainInitTUIFields builds the form fields for the missing inputs,
// in the same stable order as the line wizard.
func buildSkillOptTrainInitTUIFields(store *db.Store, scope string, values skillOptTrainInitInputs, missing []string) ([]tui.Field, error) {
	missingSet := make(map[string]struct{}, len(missing))
	for _, field := range missing {
		missingSet[field] = struct{}{}
	}
	fields := make([]tui.Field, 0, len(missing))
	for _, field := range []string{"name", "template", "review_repo", "artifact_kind", "preview", "request"} {
		if _, ok := missingSet[field]; !ok {
			continue
		}
		descriptor := buildSkillOptTrainInitPrompt(scope, field, values)
		entry := tui.Field{
			Name:    field,
			Label:   skillOptTrainInitWizardLabel(field),
			Prompt:  descriptor.Prompt,
			Default: descriptor.Prompt.Default,
		}
		switch {
		case field == "template":
			choices, err := skillopt.ListTrainInitTemplateChoices(context.Background(), store)
			if err != nil {
				return nil, err
			}
			entry.Kind = tui.FieldTemplate
			entry.Label = "Choose a template"
			for _, choice := range choices {
				entry.Choices = append(entry.Choices, tui.Choice{Value: choice.ID, Label: skillOptTrainInitTemplateChoiceLabel(choice)})
			}
			entry.Choices = append(entry.Choices, tui.Choice{Custom: true, Label: "Custom file"})
		case len(descriptor.Prompt.Choices) > 0:
			entry.Kind = tui.FieldChoice
			for _, choice := range descriptor.Prompt.Choices {
				entry.Choices = append(entry.Choices, tui.Choice{Value: choice, Label: choice})
			}
		case field == "review_repo":
			entry.Kind = tui.FieldText
			entry.CheckRepo = skillOptTrainRepoChecker()
			entry.CreateRepo = skillOptTrainRepoCreator()
		default:
			entry.Kind = tui.FieldText
		}
		fields = append(fields, entry)
	}
	return fields, nil
}

// skillOptTrainRepoChecker reports whether a "owner/repo" value is missing on
// GitHub. An unparseable value or an ambiguous (auth/network) check returns the
// error so the form re-asks rather than offering a create.
func skillOptTrainRepoChecker() func(string) (bool, error) {
	return func(value string) (bool, error) {
		repo, err := daemon.ParseRepository(value)
		if err != nil {
			return false, err
		}
		exists, err := newSkillOptGitHubClient().RepositoryExists(context.Background(), repo)
		if err != nil {
			return false, err
		}
		return !exists, nil
	}
}

func skillOptTrainRepoCreator() func(string) error {
	return func(value string) error {
		repo, err := daemon.ParseRepository(value)
		if err != nil {
			return err
		}
		return newSkillOptGitHubClient().CreateRepository(context.Background(), repo, true)
	}
}

// skillOptTrainInitTUISummaryRows renders the confirm-screen rows: each field's
// collected answer if the form gathered it, otherwise the pre-supplied value
// (flags / defaults for task_kind and mode).
func skillOptTrainInitTUISummaryRows(values skillOptTrainInitInputs, answers map[string]string) [][]string {
	pick := func(field, current string) string {
		if value, ok := answers[field]; ok {
			return value
		}
		return current
	}
	return [][]string{
		{"name", pick("name", values.Name)},
		{"template", pick("template", values.Template)},
		{"review repo", pick("review_repo", values.ReviewRepo)},
		{"task kind", values.TaskKind},
		{"artifact kind", pick("artifact_kind", values.ArtifactKind)},
		{"preview", pick("preview", values.Preview)},
		{"mode", values.Mode},
		{"request", firstLine(pick("request", values.Request))},
	}
}

// applySkillOptTrainInitTUIResult copies the collected answers back into values.
func applySkillOptTrainInitTUIResult(values *skillOptTrainInitInputs, answers map[string]string) {
	for field, value := range answers {
		switch field {
		case "name":
			values.Name = value
		case "template":
			values.Template = value
		case "review_repo":
			values.ReviewRepo = value
		case "artifact_kind":
			values.ArtifactKind = value
		case "preview":
			values.Preview = value
		case "request":
			values.Request = value
		}
	}
}

// cleanupSkillOptTrainInitTUIPrompts deletes any still-pending prompt records for
// the form's fields. Resolved records are left intact so a rerun can consume an
// answer that arrived externally.
func cleanupSkillOptTrainInitTUIPrompts(store *db.Store, scope string, fields []string) {
	ctx := context.Background()
	for _, field := range fields {
		id := skillOptTrainInitPromptID(scope, field)
		prompt, err := store.GetInteractivePrompt(ctx, id)
		if err != nil {
			continue
		}
		if prompt.State == db.InteractivePromptStatePending {
			_ = store.DeleteInteractivePrompt(ctx, id)
		}
	}
}

// agentOptimizePromptScope namespaces the prompt records the dashboard's
// optimize form publishes (one form per dashboard process).
const agentOptimizePromptScope = "dashboard-optimize"

// agentOptimizePromptIDs returns the prompt record ids the optimize form
// publishes (derived from the field definitions, so a new field cannot be
// missed), letting the dashboard sweep them on exit.
func agentOptimizePromptIDs() []string {
	fields := buildAgentOptimizeFields()
	ids := make([]string, 0, len(fields))
	for _, field := range fields {
		ids = append(ids, field.Prompt.ID)
	}
	return ids
}

// buildAgentOptimizeFields builds the optimize-form fields: the standard
// train-init questions (minus the pre-filled template), plus the workspace
// repo, a codex/claude backend pick, and an optional model override.
func buildAgentOptimizeFields() []tui.Field {
	standard := func(field string) tui.Field {
		descriptor := buildSkillOptTrainInitPrompt(agentOptimizePromptScope, field, skillOptTrainInitInputs{})
		entry := tui.Field{
			Name:    field,
			Label:   skillOptTrainInitWizardLabel(field),
			Prompt:  descriptor.Prompt,
			Default: descriptor.Prompt.Default,
		}
		if len(descriptor.Prompt.Choices) > 0 {
			entry.Kind = tui.FieldChoice
			for _, choice := range descriptor.Prompt.Choices {
				entry.Choices = append(entry.Choices, tui.Choice{Value: choice, Label: choice})
			}
		} else {
			entry.Kind = tui.FieldText
		}
		return entry
	}
	custom := func(field, label, question string, choices []string, def string, required bool) tui.Field {
		format := "text"
		if len(choices) > 0 {
			format = "choice"
		}
		entry := tui.Field{
			Name:    field,
			Label:   label,
			Kind:    tui.FieldText,
			Default: def,
			Prompt: db.InteractivePrompt{
				ID:            skillOptTrainInitPromptID(agentOptimizePromptScope, field),
				Question:      question,
				Choices:       choices,
				Default:       def,
				Required:      required,
				AnswerFormat:  format,
				SourceCommand: "gitmoot dashboard agent optimize",
				State:         db.InteractivePromptStatePending,
			},
		}
		if len(choices) > 0 {
			entry.Kind = tui.FieldChoice
			for _, choice := range choices {
				entry.Choices = append(entry.Choices, tui.Choice{Value: choice, Label: choice})
			}
		}
		return entry
	}
	return []tui.Field{
		standard("name"),
		standard("review_repo"),
		custom("workspace_repo", "Workspace repo", "Workspace repository in owner/repo form? (options are generated there)", nil, "", true),
		standard("artifact_kind"),
		standard("preview"),
		standard("request"),
		custom("backend", "Optimizer backend", "Backend for the optimizer and target runs?", []string{"codex", "claude"}, "codex", true),
		custom("model", "Model (optional)", "Model override for the optimizer and target runs? (empty = backend default)", nil, "", false),
	}
}

// agentOptimizeInterpret validates the optimize-form free-text answers with
// the same core the train-init wizard uses; the model is the one optional
// field, and the workspace repo borrows the review-repo format check.
func agentOptimizeInterpret(field, text string) (string, string) {
	switch field {
	case "model":
		return strings.TrimSpace(text), "ok"
	case "name":
		value := strings.TrimSpace(text)
		if err := skillopt.ValidateTrainInitName(value); err != nil {
			return "", "reask"
		}
		return value, "ok"
	case "review_repo", "workspace_repo":
		// Validate the owner/repo shape in the form, so a typo re-asks here
		// instead of failing the whole pipeline after the form closed.
		value := strings.TrimSpace(text)
		if _, err := daemon.ParseRepository(value); err != nil {
			return "", "reask"
		}
		return value, "ok"
	default:
		return skillOptTrainInitInterpretCore(field, text, db.InteractivePrompt{}, nil)
	}
}

// agentOptimizeSummaryRows renders the optimize confirm screen.
func agentOptimizeSummaryRows(template string) func(map[string]string) [][]string {
	return func(answers map[string]string) [][]string {
		model := strings.TrimSpace(answers["model"])
		if model == "" {
			model = "backend default"
		}
		return [][]string{
			{"template", template},
			{"name", answers["name"]},
			{"review repo", answers["review_repo"]},
			{"workspace repo", answers["workspace_repo"]},
			{"artifact kind", answers["artifact_kind"]},
			{"preview", answers["preview"]},
			{"backend", answers["backend"]},
			{"model", model},
			{"request", firstLine(answers["request"])},
		}
	}
}

// startAgentOptimizeSession runs the full optimize pipeline headlessly: write
// the train-init scaffold (with the backend/model choices in its [optimizer]
// section, which train start persists into the session's optimizer_defaults
// metadata), then run `skillopt train start --config --yes` with a
// pre-generated session id so the caller can open its phase view.
func startAgentOptimizeSession(home, templateID string, answers map[string]string) (string, error) {
	name := strings.TrimSpace(answers["name"])
	if err := skillopt.ValidateTrainInitName(name); err != nil {
		return "", err
	}
	reviewRepo, err := daemon.ParseRepository(strings.TrimSpace(answers["review_repo"]))
	if err != nil {
		return "", fmt.Errorf("review repo: %w", err)
	}
	workspaceRepo, err := daemon.ParseRepository(strings.TrimSpace(answers["workspace_repo"]))
	if err != nil {
		return "", fmt.Errorf("workspace repo: %w", err)
	}
	preview, err := normalizeSkillOptTrainInitPreview(strings.TrimSpace(answers["preview"]))
	if err != nil {
		return "", err
	}
	var template db.AgentTemplate
	if err := withStore(home, func(store *db.Store) error {
		var err error
		template, err = skillopt.ResolveTrainInitTemplateChoice(context.Background(), store, newAgentTemplateFetcher(), templateID)
		return err
	}); err != nil {
		return "", err
	}
	values := skillOptTrainInitInputs{
		Name:         name,
		Template:     template.ID,
		ReviewRepo:   reviewRepo.FullName(),
		TaskKind:     "custom",
		ArtifactKind: strings.TrimSpace(answers["artifact_kind"]),
		Preview:      preview,
		Mode:         db.EvalRunModeExplore,
		Request:      strings.TrimSpace(answers["request"]),
	}
	// DefaultTrainInitConfig already carries the explore mode and its
	// exploration level; only the per-run fields are overlaid here.
	config := skillopt.DefaultTrainInitConfig()
	config.Name = values.Name
	config.Template = template.ID
	config.TemplateVersion = template.VersionID
	config.ReviewRepo = values.ReviewRepo
	config.TaskKind = values.TaskKind
	config.ArtifactKind = values.ArtifactKind
	config.Preview = values.Preview
	config.Options = skillOptTrainInitDefaultOptions(config.Mode)
	if backend := strings.TrimSpace(answers["backend"]); backend != "" {
		config.Optimizer.OptimizerBackend = backend
		config.Optimizer.TargetBackend = backend
		config.Optimizer.EvaluatorBackend = backend
		if !strings.EqualFold(backend, "codex") {
			// The codex_exec adapter default only applies to codex targets;
			// leaving it in the scaffold would contradict the chosen backend.
			config.Optimizer.InternalTargetAdapter = ""
		}
	}
	if model := strings.TrimSpace(answers["model"]); model != "" {
		config.Optimizer.OptimizerModel = model
		config.Optimizer.TargetModel = model
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	scaffoldRoot := filepath.Join(cwd, ".gitmoot", skillopt.TrainInitScaffoldDirName, values.Name)
	if _, err := os.Stat(scaffoldRoot); err == nil {
		return "", fmt.Errorf("a train scaffold named %q already exists at %s; pick a different name", values.Name, scaffoldRoot)
	}
	paths, err := skillopt.WriteTrainInitScaffold(cwd, skillopt.TrainInitScaffold{
		Config:          config,
		TaskMarkdown:    values.Request,
		ReviewItemsYAML: skillOptTrainInitStarterReviewItemsYAML(values),
	})
	if err != nil {
		return "", err
	}
	sessionID := generatedSkillOptTrainSessionID(template.ID)
	var stdout, stderr bytes.Buffer
	args := []string{
		"--home", home,
		"--config", paths.ConfigPath,
		"--session", sessionID,
		"--workspace-repo", workspaceRepo.FullName(),
		"--create-repos",
		"--yes",
	}
	if code := runSkillOptTrainStart(args, &stdout, &stderr); code != 0 {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("train start failed: %s", reason)
	}
	return sessionID, nil
}
