package cli

import (
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/config"
)

// buildDashboardConfigView parses the config file into the read-only sections
// the Config page renders. Every parser tolerates a missing file (the daemon
// flags come from daemon.json). Cheap file reads, so it rides the snapshot.
func buildDashboardConfigView(paths config.Paths, daemon dashboardDaemonDetail) tui.ConfigView {
	view := tui.ConfigView{Path: paths.ConfigFile}

	// Sections are appended in domain order so the page reads top-to-bottom as
	// System (paths) → Agents (agent types) → Sessions (parallel sessions) →
	// Feedback → Daemon. The first-row keys "paths"/"agent types"/"feedback"
	// stay verbatim because the TUI render tests pin them; the parallel-sessions
	// title is enriched to name its domain.

	// System: filesystem paths.
	view.Sections = append(view.Sections, tui.ConfigSection{
		Title: "paths",
		Rows: [][]string{
			{"database", paths.Database},
			{"logs", paths.Logs},
			{"workspaces", paths.Workspaces},
		},
	})

	// Agents: managed agent types.
	if types, err := config.LoadAgentTypes(paths); err == nil && len(types) > 0 {
		names := make([]string, 0, len(types))
		for name := range types {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := [][]string{{"NAME", "RUNTIME", "TEMPLATE", "ROLE", "MAX_BG", "IDLE", "JOB"}}
		var editable []tui.ConfigField
		for _, name := range names {
			t := types[name]
			rows = append(rows, []string{
				name, t.Runtime, t.Template, dashConfig(t.Role),
				strconv.Itoa(t.MaxBackground), dashConfig(t.IdleTimeout), dashConfig(t.JobTimeout),
			})
			// Scalar fields safe to edit inline; the rest (runtime, template,
			// capabilities, adding/removing types) stay an $EDITOR job.
			editable = append(editable,
				tui.ConfigField{Label: name + " · max_background", KeyPath: []string{"agents", name, "max_background"}, Kind: tui.ConfigInt, Value: strconv.Itoa(t.MaxBackground)},
			)
			if strings.TrimSpace(t.IdleTimeout) != "" {
				editable = append(editable, tui.ConfigField{Label: name + " · idle_timeout", KeyPath: []string{"agents", name, "idle_timeout"}, Kind: tui.ConfigDuration, Value: t.IdleTimeout})
			}
			if strings.TrimSpace(t.JobTimeout) != "" {
				editable = append(editable, tui.ConfigField{Label: name + " · job_timeout", KeyPath: []string{"agents", name, "job_timeout"}, Kind: tui.ConfigDuration, Value: t.JobTimeout})
			}
		}
		view.Sections = append(view.Sections, tui.ConfigSection{Title: "agent types", Rows: rows, Editable: editable})
	}

	// Sessions: parallel-session policy. The scalar fields are inline-editable,
	// mirroring feedback.repo / the agent timeouts; same_session and merge_back
	// are short enums and max_temp_sessions_per_agent is an int. eligible_actions
	// is a string list (ConfigStringList): its editable Value is the bracketed
	// TOML literal so configScalarForKind round-trips it back as a list.
	//
	// LoadParallelSessionPolicy returns populated defaults (with a nil error) even
	// when the file has no [parallel_sessions] section, so the read-only Rows
	// always show the effective policy. But the inline-edit writer cannot create a
	// missing key (section creation is reserved for the $EDITOR path), so the
	// Editable affordance is only attached when the section actually exists —
	// otherwise an edit would fail with a confusing "key not found".
	if policy, err := config.LoadParallelSessionPolicy(paths); err == nil {
		section := tui.ConfigSection{
			Title: "parallel sessions (session policy)",
			Rows: [][]string{
				{"same_session", policy.SameSession},
				{"merge_back", policy.MergeBack},
				{"max_temp_sessions_per_agent", strconv.Itoa(policy.MaxTempSessionsPerAgent)},
				{"eligible_actions", strings.Join(policy.EligibleActions, ", ")},
			},
		}
		if configFileHasSection(paths, "parallel_sessions") {
			section.Editable = []tui.ConfigField{
				{Label: "parallel_sessions · same_session", KeyPath: []string{"parallel_sessions", "same_session"}, Kind: tui.ConfigText, Value: policy.SameSession, Allowed: []string{config.ParallelSessionQueue, config.ParallelSessionForkTempSession}},
				{Label: "parallel_sessions · merge_back", KeyPath: []string{"parallel_sessions", "merge_back"}, Kind: tui.ConfigText, Value: policy.MergeBack, Allowed: []string{config.ParallelSessionMergeBackOff, config.ParallelSessionMergeBackSummary}},
				{Label: "parallel_sessions · max_temp_sessions_per_agent", KeyPath: []string{"parallel_sessions", "max_temp_sessions_per_agent"}, Kind: tui.ConfigInt, Value: strconv.Itoa(policy.MaxTempSessionsPerAgent)},
				{Label: "parallel_sessions · eligible_actions", KeyPath: []string{"parallel_sessions", "eligible_actions"}, Kind: tui.ConfigStringList, Value: configStringArrayLiteral(policy.EligibleActions), Allowed: []string{"ask", "review", "implement"}},
			}
		}
		view.Sections = append(view.Sections, section)
	}

	// Feedback: default feedback repo.
	if repo, err := config.LoadDefaultFeedbackRepo(paths); err == nil && strings.TrimSpace(repo) != "" {
		view.Sections = append(view.Sections, tui.ConfigSection{
			Title: "feedback",
			Rows:  [][]string{{"repo", dashConfig(repo)}},
			Editable: []tui.ConfigField{
				{Label: "feedback · repo", KeyPath: []string{"feedback", "repo"}, Kind: tui.ConfigText, Value: repo},
			},
		})
	}

	// Daemon: persisted daemon launch state.
	daemonRows := [][]string{}
	if len(daemon.Flags) > 0 {
		daemonRows = append(daemonRows, []string{"flags", strings.Join(daemon.Flags, " ")})
	}
	if daemon.WorkDir != "" {
		daemonRows = append(daemonRows, []string{"workdir", daemon.WorkDir})
	}
	if len(daemonRows) > 0 {
		view.Sections = append(view.Sections, tui.ConfigSection{Title: "daemon (persisted)", Rows: daemonRows})
	}

	return view
}

// configScalarForKind types the value for the writer from the field's own kind
// (the single source of truth), so the target's TOML shape never depends on the
// value's syntax. Only ConfigStringList writes a TOML array; ConfigText (repo,
// the session enums) always writes a plain string even if the user typed
// brackets, and ConfigInt writes an int. This avoids mistyping a string field as
// an array when its value happens to look bracketed.
func configScalarForKind(kind tui.ConfigKind, value string) config.ConfigScalar {
	switch kind {
	case tui.ConfigInt:
		if n, err := strconv.Atoi(value); err == nil {
			return config.IntScalar(n)
		}
	case tui.ConfigStringList:
		if items, ok := parseConfigStringArrayLiteral(value); ok {
			return config.StringListScalar(items)
		}
	}
	return config.StringScalar(value)
}

// configFileHasSection reports whether the config file declares the given
// [section] header. Inline edits cannot create a missing section (that is
// reserved for the $EDITOR path), so the Config page only offers inline edits
// for keys whose section already exists.
func configFileHasSection(paths config.Paths, section string) bool {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return false
	}
	header := "[" + section + "]"
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == header {
			return true
		}
	}
	return false
}

// configStringArrayLiteral renders a string slice as the bracketed TOML literal
// the eligible_actions editor prefills (and round-trips back through
// configScalarForKind). Empty slice → "[]".
func configStringArrayLiteral(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, strconv.Quote(item))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// parseConfigStringArrayLiteral parses a bracketed, comma-separated list of
// (optionally quoted) items, e.g. `["ask", "review"]` or `[ask, review]`. It
// returns ok=false for any value that is not bracketed, so plain ConfigText
// scalars fall through to a string write unchanged.
func parseConfigStringArrayLiteral(value string) ([]string, bool) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	if inner == "" {
		return []string{}, true
	}
	items := make([]string, 0)
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if unquoted, err := strconv.Unquote(part); err == nil {
			part = unquoted
		}
		part = strings.TrimSpace(part)
		if part != "" {
			items = append(items, part)
		}
	}
	return items, true
}

func dashConfig(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// validateDashboardConfig re-runs the config parsers and returns
// human-readable problems (empty when the file parses cleanly). It is the
// safety net after an external edit.
func validateDashboardConfig(paths config.Paths) []string {
	var problems []string
	if _, err := config.LoadAgentTypes(paths); err != nil {
		problems = append(problems, "[agents.*] "+err.Error())
	}
	if _, err := config.LoadParallelSessionPolicy(paths); err != nil {
		problems = append(problems, "[parallel_sessions] "+err.Error())
	}
	if _, err := config.LoadDefaultFeedbackRepo(paths); err != nil {
		problems = append(problems, "[feedback] "+err.Error())
	}
	return problems
}

// editConfigCmd opens the config file in $EDITOR (fallback vi) via
// tea.ExecProcess, which suspends the program for the editor and resumes on
// exit, delivering a tui.ConfigEditedMsg.
// agentPromptScaffold seeds the custom-prompt editor when no base template is
// chosen. It includes the gitmoot_result contract reminder so the default does
// not trip the "missing contract" notice.
const agentPromptScaffold = `You are a Gitmoot-managed agent.

Describe this agent's role and how it should approach jobs here.

Always end every job with a concise, truthful gitmoot_result JSON object, e.g.:
  {"decision": "implemented", "summary": "what you did"}
Decisions: approved | changes_requested | blocked | implemented | failed | skipped.
Use "blocked" when you need human input or external state, and "failed" when an
attempted action errored.
`

// editAgentPromptCmd writes seed to a temp file, opens it in $EDITOR, and on
// close reads the saved content back into an AgentPromptEditedMsg. Mirrors
// editConfigCmd but round-trips a throwaway file rather than the config.
func editAgentPromptCmd(seed string) tea.Cmd {
	f, err := os.CreateTemp("", "gitmoot-agent-prompt-*.md")
	if err != nil {
		return func() tea.Msg { return tui.AgentPromptEditedMsg{Err: err} }
	}
	path := f.Name()
	_, writeErr := f.WriteString(seed)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(path)
		if writeErr == nil {
			writeErr = closeErr
		}
		return func() tea.Msg { return tui.AgentPromptEditedMsg{Err: writeErr} }
	}
	editor := strings.TrimSpace(os.Getenv("EDITOR"))
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	args := append(parts[1:], path)
	cmd := exec.Command(parts[0], args...)
	return tea.ExecProcess(cmd, func(runErr error) tea.Msg {
		defer os.Remove(path)
		if runErr != nil {
			return tui.AgentPromptEditedMsg{Err: runErr}
		}
		content, readErr := os.ReadFile(path)
		return tui.AgentPromptEditedMsg{Content: string(content), Err: readErr}
	})
}

func editConfigCmd(configFile string) tea.Cmd {
	editor := strings.TrimSpace(os.Getenv("EDITOR"))
	if editor == "" {
		editor = "vi"
	}
	// Honor a multi-word EDITOR ("code --wait"), splitting on spaces.
	parts := strings.Fields(editor)
	args := append(parts[1:], configFile)
	cmd := exec.Command(parts[0], args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return tui.ConfigEditedMsg{Err: err}
	})
}
