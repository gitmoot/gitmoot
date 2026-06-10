package tui

import (
	"context"

	"github.com/jerryfane/gitmoot/internal/db"
)

// FieldKind selects the input widget for a train-init field.
type FieldKind int

const (
	// FieldText collects free text in a single-line input.
	FieldText FieldKind = iota
	// FieldChoice picks one of a fixed list of values.
	FieldChoice
	// FieldTemplate is a choice list whose final "Custom file" entry switches to
	// a free-text path sub-state.
	FieldTemplate
)

// Choice is one selectable option in a FieldChoice/FieldTemplate list.
type Choice struct {
	Value  string // value stored as the answer
	Label  string // display text
	Custom bool   // template "Custom file" sentinel → path sub-state
}

// Field describes one train-init question the form walks through.
type Field struct {
	Name    string               // field key, e.g. "name", "template"
	Label   string               // human header, e.g. "Training name"
	Kind    FieldKind            //
	Prompt  db.InteractivePrompt // record upserted so an agent can answer externally
	Choices []Choice             // for FieldChoice/FieldTemplate
	Default string               // text prefill / preselected choice value

	// CheckRepo / CreateRepo, when set on a FieldText, gate advancing on the
	// answer being an existing GitHub repo; a missing repo offers creation.
	CheckRepo  func(value string) (missing bool, err error)
	CreateRepo func(value string) error
}

// PromptStore is the subset of *db.Store the form needs to publish a prompt
// record per field and observe external answers.
type PromptStore interface {
	UpsertInteractivePrompt(ctx context.Context, prompt db.InteractivePrompt) error
	GetInteractivePrompt(ctx context.Context, id string) (db.InteractivePrompt, error)
	DeleteInteractivePrompt(ctx context.Context, id string) error
}

// Interpret validates a free-text answer for a field, returning the cleaned
// value and a status of "ok" or "reask". The cli wraps skillopt's interpret
// core so the TUI and line wizard share identical validation.
type Interpret func(field, text string) (value, status string)

// Result is what the caller reads after the program exits.
type Result struct {
	Values             map[string]string
	Aborted            bool
	ExternallyAnswered bool
}
