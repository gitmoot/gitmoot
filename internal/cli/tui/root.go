package tui

import tea "github.com/charmbracelet/bubbletea"

// PushModelMsg asks the Root to push a child model onto the navigation stack
// (e.g. the dashboard opening a train session's phase view).
type PushModelMsg struct {
	Model tea.Model
}

// PopModelMsg asks the Root to pop the top model and return to the one below.
type PopModelMsg struct{}

// Push returns a command that pushes model onto the Root's stack.
func Push(model tea.Model) tea.Cmd {
	return func() tea.Msg { return PushModelMsg{Model: model} }
}

// Pop returns a command that pops the Root's top model.
func Pop() tea.Cmd {
	return func() tea.Msg { return PopModelMsg{} }
}

// Root is the navigation shell: a message router and screen compositor over a
// stack of child models, with the dashboard at the bottom. It owns only
// ctrl+c (global quit) and the push/pop messages; every other message is
// delegated to the top of the stack, so child sub-modes (text inputs, confirm
// overlays) keep full key ownership. Window sizes are broadcast to the whole
// stack so a model resumed by a pop is already sized.
type Root struct {
	stack []tea.Model
	size  *tea.WindowSizeMsg
}

// NewRoot returns a Root with base at the bottom of the stack.
func NewRoot(base tea.Model) Root {
	return Root{stack: []tea.Model{base}}
}

func (r Root) top() tea.Model { return r.stack[len(r.stack)-1] }

// Init initializes the base model.
func (r Root) Init() tea.Cmd {
	return r.top().Init()
}

func (r Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return r, tea.Quit
		}
	case tea.WindowSizeMsg:
		r.size = &msg
		var cmds []tea.Cmd
		for i, child := range r.stack {
			next, cmd := child.Update(msg)
			r.stack[i] = next
			cmds = append(cmds, cmd)
		}
		return r, tea.Batch(cmds...)
	case PushModelMsg:
		r.stack = append(r.stack, msg.Model)
		cmds := []tea.Cmd{msg.Model.Init()}
		if r.size != nil {
			next, cmd := r.top().Update(*r.size)
			r.stack[len(r.stack)-1] = next
			cmds = append(cmds, cmd)
		}
		return r, tea.Batch(cmds...)
	case PopModelMsg:
		if len(r.stack) > 1 {
			r.stack = r.stack[:len(r.stack)-1]
			// Nudge the resumed model to refresh immediately rather than
			// waiting for its next interval tick (refreshNudgeMsg does NOT
			// re-arm the tick, so pops cannot accumulate tick loops).
			next, cmd := r.top().Update(refreshNudgeMsg{})
			r.stack[len(r.stack)-1] = next
			return r, cmd
		}
		return r, tea.Quit
	}
	next, cmd := r.top().Update(msg)
	r.stack[len(r.stack)-1] = next
	return r, cmd
}

func (r Root) View() string {
	return r.top().View()
}
