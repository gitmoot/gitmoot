package cli

import (
	"context"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
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
		default:
			entry.Kind = tui.FieldText
		}
		fields = append(fields, entry)
	}
	return fields, nil
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
