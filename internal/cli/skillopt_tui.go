package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
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
		fields, err := buildSkillOptTrainInitTUIFields(store, home, scope, *values, missing)
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
func buildSkillOptTrainInitTUIFields(store *db.Store, home, scope string, values skillOptTrainInitInputs, missing []string) ([]tui.Field, error) {
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
			entry.CreateRepo = skillOptTrainRepoCreator(home)
			if choices := skillOptRepoPickerChoices(skillOptKnownRepoNames(context.Background(), store)); len(choices) > 0 {
				entry.Kind = tui.FieldChoice
				entry.Choices = choices
			}
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

// skillOptTrainRepoCreator returns a form CreateRepo callback that provisions a
// missing repo into a *usable* generation repo (created with an initial commit,
// cloned to a gitmoot-managed checkout, and registered) so train generate can
// run in it. home is the gitmoot home the checkout and store live under.
func skillOptTrainRepoCreator(home string) func(string) error {
	return func(value string) error {
		repo, err := daemon.ParseRepository(value)
		if err != nil {
			return err
		}
		return provisionTrainGenerationRepo(context.Background(), home, repo)
	}
}

// trainGenerationCheckoutPath is the gitmoot-managed checkout location for a
// generation repo: <home>/checkouts/<owner>/<name>. It resolves the home the
// same way the store does (pathsFromFlag → DefaultPaths when home is empty), so
// the default-home case lands under the real ~/.gitmoot rather than a
// cwd-relative ".gitmoot".
func trainGenerationCheckoutPath(home string, repo github.Repository) (string, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.Home, "checkouts", repo.Owner, repo.Name), nil
}

// provisionTrainGenerationRepo creates a missing generation repo with an initial
// commit (so it has a default branch), clones it into a gitmoot-managed checkout
// under <home>/checkouts/<owner>/<name>, registers that checkout, and records the
// repo for later cleanup. It is idempotent: a pre-existing valid checkout of the
// same repo is reused rather than re-cloned.
func provisionTrainGenerationRepo(ctx context.Context, home string, repo github.Repository) error {
	client := newSkillOptGitHubClient()
	if err := client.CreateRepository(ctx, repo, true); err != nil {
		return err
	}
	checkout, err := trainGenerationCheckoutPath(home, repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
		return err
	}
	switch _, statErr := os.Stat(checkout); {
	case statErr == nil:
		// A checkout dir already exists (e.g. a prior attempt) — reuse it only if
		// it is genuinely this repo's checkout; otherwise refuse rather than clone
		// into a non-empty/foreign directory.
		if _, err := repoRecordFromPath(ctx, repo, checkout); err != nil {
			return fmt.Errorf("checkout path %s already exists but is not a checkout of %s: %w", checkout, repo.FullName(), err)
		}
	case errors.Is(statErr, os.ErrNotExist):
		if err := client.CloneRepository(ctx, repo, checkout); err != nil {
			// Remove any partial clone so a retry isn't blocked by a stale dir; the
			// repo now exists on GitHub, so recovery is `gitmoot repo add` (or a
			// later train continue once the checkout is registered).
			_ = os.RemoveAll(checkout)
			return fmt.Errorf("created %s but cloning it to %s failed (register a checkout with `gitmoot repo add %s --path <dir>`): %w", repo.FullName(), checkout, repo.FullName(), err)
		}
	default:
		return statErr
	}
	record, err := repoRecordFromPath(ctx, repo, checkout)
	if err != nil {
		return err
	}
	return withStore(home, func(store *db.Store) error {
		if err := store.UpsertRepo(ctx, record); err != nil {
			return err
		}
		return store.RecordCreatedRepo(ctx, db.CreatedRepo{Repo: repo.FullName(), Purpose: "train"})
	})
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
	fields := buildAgentOptimizeFields("", nil)
	ids := make([]string, 0, len(fields))
	for _, field := range fields {
		ids = append(ids, field.Prompt.ID)
	}
	return ids
}

// skillOptRepoPickerChoices turns known repos into picker entries with a
// trailing free-text entry, so the user selects instead of typing. An empty
// list yields nil and the field stays free text.
func skillOptRepoPickerChoices(repos []string) []tui.Choice {
	if len(repos) == 0 {
		return nil
	}
	choices := make([]tui.Choice, 0, len(repos)+1)
	for _, repo := range repos {
		choices = append(choices, tui.Choice{Value: repo, Label: repo})
	}
	choices = append(choices, tui.Choice{Custom: true, Label: "another repo… (created on GitHub if missing)", Placeholder: "owner/repo"})
	return choices
}

// skillOptKnownRepoNames lists repos to offer in the setup pickers: the
// subscribed store repos first, then the user's most recently updated GitHub
// repos (deduped, best effort — gh flakiness must not break the form).
func skillOptKnownRepoNames(ctx context.Context, store *db.Store) []string {
	// Best effort with a hard deadline: this can run during form construction,
	// and a hung gh must not block the setup from appearing.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	seen := map[string]bool{}
	names := []string{}
	if repos, err := store.ListRepos(ctx); err == nil {
		for _, repo := range repos {
			full := repo.Owner + "/" + repo.Name
			if !seen[full] {
				seen[full] = true
				names = append(names, full)
			}
		}
	}
	if recent, err := newSkillOptGitHubClient().ListUserRepositories(ctx, 30); err == nil {
		for _, repo := range recent {
			if !seen[repo.FullName] {
				seen[repo.FullName] = true
				names = append(names, repo.FullName)
			}
		}
	}
	const maxPickerRepos = 20
	if len(names) > maxPickerRepos {
		names = names[:maxPickerRepos]
	}
	return names
}

// buildAgentOptimizeFields builds the optimize-form fields: the standard
// train-init questions (minus the pre-filled template), plus the workspace
// repo, the review-item count, a codex/claude backend pick, and an optional
// model override. repoChoices, when non-empty, turns the repo questions into
// pickers.
func buildAgentOptimizeFields(home string, repoChoices []tui.Choice) []tui.Field {
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
	review := standard("review_repo")
	workspace := custom("workspace_repo", "Workspace repo", "Workspace repository in owner/repo form? (options are generated there)", nil, "", true)
	for _, field := range []*tui.Field{&review, &workspace} {
		field.CheckRepo = skillOptTrainRepoChecker()
		field.CreateRepo = skillOptTrainRepoCreator(home)
		if len(repoChoices) > 0 {
			field.Kind = tui.FieldChoice
			field.Choices = repoChoices
		}
	}
	return []tui.Field{
		standard("name"),
		review,
		workspace,
		custom("items", "Review items", "How many review items should the training scaffold start with? (minimum 2)", nil, "2", true),
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
	case "items":
		value := strings.TrimSpace(text)
		if n, err := strconv.Atoi(value); err != nil || n < 2 {
			return "", "reask"
		}
		return value, "ok"
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
			{"review items", firstNonEmpty(strings.TrimSpace(answers["items"]), "2")},
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
	itemCount := 2
	if value := strings.TrimSpace(answers["items"]); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n >= 2 {
			itemCount = n
		}
	}
	paths, err := skillopt.WriteTrainInitScaffold(cwd, skillopt.TrainInitScaffold{
		Config:          config,
		TaskMarkdown:    values.Request,
		ReviewItemsYAML: skillOptTrainInitStarterReviewItemsYAMLN(values, itemCount),
	})
	if err != nil {
		return "", err
	}
	sessionID := generatedSkillOptTrainSessionID(template.ID)
	// Repos the form created before the session existed are recorded with an
	// empty session id; adopt them so delete-time cleanup offers them.
	if err := withStore(home, func(store *db.Store) error {
		return store.AdoptCreatedRepoRecords(context.Background(), sessionID, []string{reviewRepo.FullName(), workspaceRepo.FullName()})
	}); err != nil {
		return "", err
	}
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
