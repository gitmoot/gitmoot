package tui

import (
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

type tiState int

const (
	tiField tiState = iota
	tiCustomPath
	tiRepoCheck
	tiRepoMissing
	tiConfirm
	tiDone
)

// defaultTrainInitPoll is how often the form checks whether the current field's
// prompt was answered externally (mirrors the line wizard's 200ms ticker).
const defaultTrainInitPoll = 200 * time.Millisecond

// TrainInitModel is the bubbletea form for `skillopt train init`. It walks the
// fields one at a time, publishing a prompt record per field so an agent can
// answer it with `gitmoot interactive answer` while a human uses the keyboard.
type TrainInitModel struct {
	store     PromptStore
	fields    []Field
	summary   func(answers map[string]string) [][]string
	interpret Interpret
	poll      time.Duration

	state     tiState
	idx       int
	gen       int // bumped on every field transition; stale poll msgs are dropped
	answers   map[string]string
	external  bool
	flash     string // "answered externally by <source>"
	inlineErr string

	input     textinput.Model
	choiceIdx int

	pendingPromptID string
	aborted         bool

	// pendingValue holds a validated free-text answer awaiting a repo
	// existence check / creation decision (tiRepoCheck / tiRepoMissing).
	pendingValue string
}

// NewTrainInit builds the form. summary renders the confirm-screen rows from the
// collected answers (the caller folds in defaulted fields like task_kind/mode).
func NewTrainInit(store PromptStore, fields []Field, summary func(map[string]string) [][]string, interpret Interpret, poll time.Duration) TrainInitModel {
	if poll <= 0 {
		poll = defaultTrainInitPoll
	}
	return TrainInitModel{
		store:     store,
		fields:    fields,
		summary:   summary,
		interpret: interpret,
		poll:      poll,
		answers:   map[string]string{},
	}
}

// Result is read by the caller after the program exits.
func (m TrainInitModel) Result() Result {
	return Result{Values: m.answers, Aborted: m.aborted, ExternallyAnswered: m.external}
}

// Init kicks off field setup. The actual enterField mutation happens in Update
// (on initMsg) because Init's value receiver cannot persist state changes.
func (m TrainInitModel) Init() tea.Cmd {
	return func() tea.Msg { return initMsg{} }
}

func (m TrainInitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case initMsg:
		if len(m.fields) == 0 {
			m.state = tiConfirm
			return m, nil
		}
		return m, m.enterField(0)
	case tea.KeyMsg:
		return m.updateKey(msg)
	case promptReadyMsg:
		if msg.gen == m.gen && msg.err != nil {
			m.inlineErr = "prompt publish failed: " + msg.err.Error()
		}
		return m, nil
	case pollTickMsg:
		if msg.gen != m.gen {
			return m, nil // stale tick from a field we already left
		}
		return m, checkPromptCmd(m.store, m.pendingPromptID, m.gen)
	case pollResultMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err == nil && msg.prompt.State == db.InteractivePromptStateResolved {
			m.external = true
			m.flash = "answered externally by " + emptyOr(msg.prompt.AnswerSource, "agent")
			return m.commit(msg.prompt.AnswerValue)
		}
		// Not answered yet (or the record is not visible yet) — keep polling.
		return m, pollTick(m.poll, m.gen)
	case repoCheckMsg:
		if msg.gen != m.gen || m.state != tiRepoCheck {
			return m, nil
		}
		if msg.err != nil {
			m.state = tiField
			m.inlineErr = "repo check failed: " + msg.err.Error()
			return m, nil
		}
		if !msg.missing {
			return m.commit(msg.value)
		}
		m.state = tiRepoMissing
		return m, nil
	case repoCreatedMsg:
		if msg.gen != m.gen || m.state != tiRepoMissing {
			return m, nil // the user re-entered the field before the create returned
		}
		if msg.err != nil {
			m.state = tiRepoMissing
			m.inlineErr = "create failed: " + msg.err.Error()
			return m, nil
		}
		return m.commit(msg.value)
	}
	return m, nil
}

func checkRepoCmd(field Field, value string, gen int) tea.Cmd {
	return func() tea.Msg {
		if field.CheckRepo == nil {
			return repoCheckMsg{gen: gen, value: value}
		}
		missing, err := field.CheckRepo(value)
		return repoCheckMsg{gen: gen, value: value, missing: missing, err: err}
	}
}

func createRepoCmd(field Field, value string, gen int) tea.Cmd {
	return func() tea.Msg {
		var err error
		if field.CreateRepo != nil {
			err = field.CreateRepo(value)
		}
		return repoCreatedMsg{gen: gen, value: value, err: err}
	}
}

func (m TrainInitModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m.abort()
	}
	// A keypress dismisses a lingering "answered externally" flash.
	m.flash = ""
	switch m.state {
	case tiField:
		return m.updateFieldKey(msg)
	case tiCustomPath:
		return m.updateCustomPathKey(msg)
	case tiRepoCheck:
		if msg.String() == "esc" {
			return m.abort()
		}
		return m, nil // checking; ignore keys until the result arrives
	case tiRepoMissing:
		switch msg.String() {
		case "c", "C":
			m.inlineErr = ""
			return m, createRepoCmd(m.fields[m.idx], m.pendingValue, m.gen)
		case "e", "E", "esc":
			// Re-enter the field with the value prefilled.
			m.state = tiField
			m.inlineErr = ""
			ti := textinput.New()
			ti.SetValue(m.pendingValue)
			ti.CursorEnd()
			m.input = ti
			return m, m.input.Focus()
		}
		return m, nil
	case tiConfirm:
		switch msg.String() {
		case "y", "Y", "enter":
			m.state = tiDone
			return m, tea.Quit
		case "n", "N", "esc":
			return m.abort()
		}
		return m, nil
	}
	return m, nil
}

func (m TrainInitModel) updateFieldKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	field := m.fields[m.idx]
	if msg.String() == "esc" {
		return m.abort()
	}
	if field.Kind == FieldText {
		if msg.String() == "enter" {
			value, status := m.validate(field.Name, m.input.Value())
			if status != "ok" {
				m.inlineErr = "value required"
				return m, nil
			}
			if field.CheckRepo != nil {
				m.pendingValue = value
				m.state = tiRepoCheck
				m.inlineErr = ""
				return m, checkRepoCmd(field, value, m.gen)
			}
			return m.commit(value)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	// FieldChoice / FieldTemplate: a cursor list.
	switch msg.String() {
	case "up", "k":
		if m.choiceIdx > 0 {
			m.choiceIdx--
		}
	case "down", "j":
		if m.choiceIdx < len(field.Choices)-1 {
			m.choiceIdx++
		}
	case "enter":
		choice := field.Choices[m.choiceIdx]
		if choice.Custom {
			m.state = tiCustomPath
			m.inlineErr = ""
			m.input = newPathInput()
			return m, m.input.Focus()
		}
		return m.commit(choice.Value)
	}
	return m, nil
}

func (m TrainInitModel) updateCustomPathKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Back to the template list; the prompt record stays live (same gen).
		m.state = tiField
		m.inlineErr = ""
		return m, nil
	case "enter":
		value := strings.TrimSpace(m.input.Value())
		if value == "" {
			m.inlineErr = "enter a template id, version, or file path"
			return m, nil
		}
		return m.commit(value)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m TrainInitModel) validate(field, text string) (string, string) {
	if m.interpret != nil {
		return m.interpret(field, text)
	}
	if strings.TrimSpace(text) == "" {
		return "", "reask"
	}
	return strings.TrimSpace(text), "ok"
}

// commit records the answer for the current field, deletes its prompt record,
// and advances to the next field or the confirm screen.
func (m TrainInitModel) commit(value string) (tea.Model, tea.Cmd) {
	field := m.fields[m.idx]
	m.answers[field.Name] = value
	cmds := []tea.Cmd{deletePromptCmd(m.store, field.Prompt.ID)}
	if m.idx+1 < len(m.fields) {
		cmds = append(cmds, m.enterField(m.idx+1))
		return m, tea.Batch(cmds...)
	}
	// All fields answered → confirm (auto-accept when an agent drove the form).
	m.gen++
	m.pendingPromptID = ""
	if m.external {
		m.state = tiDone
		cmds = append(cmds, tea.Quit)
		return m, tea.Batch(cmds...)
	}
	m.state = tiConfirm
	return m, tea.Batch(cmds...)
}

// enterField sets up the widget for field i, publishes its prompt record, and
// starts the external-answer poll. It mutates m through the pointer receiver and
// returns the batched commands.
func (m *TrainInitModel) enterField(i int) tea.Cmd {
	m.state = tiField
	m.idx = i
	m.gen++
	// Note: m.flash is intentionally NOT reset here, so an "answered externally"
	// flash set just before advancing remains visible on the next field until the
	// user presses a key (cleared in updateKey).
	m.inlineErr = ""
	field := m.fields[i]
	m.pendingPromptID = field.Prompt.ID
	cmds := []tea.Cmd{
		upsertPromptCmd(m.store, field.Prompt, m.gen),
		pollTick(m.poll, m.gen),
	}
	if field.Kind == FieldText {
		ti := textinput.New()
		ti.SetValue(field.Default)
		ti.CursorEnd()
		m.input = ti
		cmds = append(cmds, m.input.Focus())
	} else {
		m.choiceIdx = choiceIndex(field.Choices, field.Default)
	}
	return tea.Batch(cmds...)
}

func (m TrainInitModel) abort() (tea.Model, tea.Cmd) {
	m.aborted = true
	m.state = tiDone
	cmds := []tea.Cmd{tea.Quit}
	if m.pendingPromptID != "" {
		cmds = append(cmds, deletePromptCmd(m.store, m.pendingPromptID))
	}
	m.gen++
	return m, tea.Batch(cmds...)
}

func newPathInput() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "template id, version, or file path"
	return ti
}

func choiceIndex(choices []Choice, value string) int {
	if value == "" {
		return 0 // no default → first item (avoids matching a Custom entry's empty Value)
	}
	for i, c := range choices {
		if c.Value == value {
			return i
		}
	}
	return 0
}

func emptyOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// --- messages and commands ---

// initMsg triggers first-field setup inside Update (where mutations persist).
type initMsg struct{}

type promptReadyMsg struct {
	gen int
	err error
}

type pollTickMsg struct{ gen int }

type pollResultMsg struct {
	gen    int
	prompt db.InteractivePrompt
	err    error
}

type promptGoneMsg struct{ err error }

type repoCheckMsg struct {
	gen     int
	value   string
	missing bool
	err     error
}

type repoCreatedMsg struct {
	gen   int
	value string
	err   error
}

func upsertPromptCmd(store PromptStore, prompt db.InteractivePrompt, gen int) tea.Cmd {
	return func() tea.Msg {
		return promptReadyMsg{gen: gen, err: store.UpsertInteractivePrompt(context.Background(), prompt)}
	}
}

func pollTick(d time.Duration, gen int) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return pollTickMsg{gen: gen} })
}

func checkPromptCmd(store PromptStore, id string, gen int) tea.Cmd {
	return func() tea.Msg {
		prompt, err := store.GetInteractivePrompt(context.Background(), id)
		return pollResultMsg{gen: gen, prompt: prompt, err: err}
	}
}

func deletePromptCmd(store PromptStore, id string) tea.Cmd {
	return func() tea.Msg {
		return promptGoneMsg{err: store.DeleteInteractivePrompt(context.Background(), id)}
	}
}
