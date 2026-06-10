package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

func pickerField(checkRepo func(string) (bool, error), createRepo func(string) error) Field {
	return Field{
		Name:  "review_repo",
		Label: "Review repository",
		Kind:  FieldChoice,
		Choices: []Choice{
			{Value: "o/existing", Label: "o/existing"},
			{Custom: true, Label: "another repo…", Placeholder: "owner/repo"},
		},
		Prompt:     db.InteractivePrompt{ID: "picker-review-repo"},
		CheckRepo:  checkRepo,
		CreateRepo: createRepo,
	}
}

func TestRepoPickerSelectsKnownRepoDirectly(t *testing.T) {
	form := NewTrainInit(newFakeStore(), []Field{pickerField(nil, nil)}, nil, nil, 0)
	var model tea.Model = form
	step := func(msg tea.Msg) { next, _ := model.Update(msg); model = next }
	step(initMsg{})
	step(tea.KeyMsg{Type: tea.KeyEnter}) // first choice
	m := model.(TrainInitModel)
	if m.Result().Values["review_repo"] != "o/existing" {
		t.Fatalf("answers = %v", m.Result().Values)
	}
}

func TestRepoPickerCustomEntryRunsValidationAndRepoCheck(t *testing.T) {
	checked := ""
	created := ""
	field := pickerField(
		func(value string) (bool, error) { checked = value; return true, nil }, // missing
		func(value string) error { created = value; return nil },
	)
	interpret := func(name, text string) (string, string) {
		if !strings.Contains(text, "/") {
			return "", "reask"
		}
		return strings.TrimSpace(text), "ok"
	}
	form := NewTrainInit(newFakeStore(), []Field{field}, nil, interpret, 0)
	var model tea.Model = form
	var lastCmd tea.Cmd
	step := func(msg tea.Msg) {
		next, cmd := model.Update(msg)
		model = next
		lastCmd = cmd
	}
	step(initMsg{})
	step(tea.KeyMsg{Type: tea.KeyDown})  // → custom entry
	step(tea.KeyMsg{Type: tea.KeyEnter}) // open text sub-state
	if m := model.(TrainInitModel); m.input.Placeholder != "owner/repo" {
		t.Fatalf("placeholder = %q", m.input.Placeholder)
	}
	// Invalid shape re-asks.
	for _, r := range "nope" {
		step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	step(tea.KeyMsg{Type: tea.KeyEnter})
	if m := model.(TrainInitModel); m.inlineErr == "" {
		t.Fatal("invalid custom value must show an inline error")
	}
	// Valid shape goes through the repo check → missing → create offer.
	for i := 0; i < 4; i++ {
		step(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	for _, r := range "o/new" {
		step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	step(tea.KeyMsg{Type: tea.KeyEnter})
	if lastCmd == nil {
		t.Fatal("valid custom value should run the repo check")
	}
	step(lastCmd()) // repoCheckMsg → missing → create offer
	step(key("c"))  // create
	if lastCmd == nil {
		t.Fatal("c should run the create")
	}
	step(lastCmd()) // repoCreatedMsg → commit
	m := model.(TrainInitModel)
	if checked != "o/new" || created != "o/new" {
		t.Fatalf("check/create = %q/%q", checked, created)
	}
	if m.Result().Values["review_repo"] != "o/new" {
		t.Fatalf("answers = %v", m.Result().Values)
	}
}
