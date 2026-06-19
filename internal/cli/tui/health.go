package tui

import (
	"strconv"
	"strings"
)

// healthContent renders the Health page: the daemon block (running state,
// persisted flags, log error tail) followed by the environment checks.
func (m Model) healthContent() string {
	var b strings.Builder

	b.WriteString(headerStyle.Render("daemon"))
	b.WriteByte('\n')
	d := m.snap.Daemon
	if d.Running {
		b.WriteString(greenStyle.Render("running") + "  " + mutedStyle.Render("pid "+strconv.Itoa(d.PID)) + "\n")
	} else {
		hint := "press s to start"
		if m.daemonBusy {
			hint = "starting…"
		}
		b.WriteString(redStyle.Render("stopped") + "  " + mutedStyle.Render(hint) + "\n")
		if m.daemonErr != "" {
			b.WriteString(errorStyle.Render(m.daemonErr) + "\n")
		}
	}
	rows := [][]string{}
	if len(d.Flags) > 0 {
		rows = append(rows, []string{"flags", strings.Join(d.Flags, " ")})
	}
	if d.WorkDir != "" {
		rows = append(rows, []string{"workdir", d.WorkDir})
	}
	if d.StartedAt != "" {
		rows = append(rows, []string{"started", d.StartedAt})
	}
	if d.LogFile != "" {
		rows = append(rows, []string{"log", d.LogFile})
	}
	if len(rows) > 0 {
		b.WriteString(renderRows(rows))
	}
	if len(d.LogErrors) > 0 {
		b.WriteByte('\n')
		b.WriteString(mutedStyle.Render("recent log errors:"))
		b.WriteByte('\n')
		for _, line := range d.LogErrors {
			b.WriteString(redStyle.Render("│ "+truncate(line, 100)) + "\n")
		}
	}

	b.WriteByte('\n')
	b.WriteString(headerStyle.Render("environment"))
	b.WriteByte('\n')
	switch {
	case m.healthErr != "":
		b.WriteString(errorStyle.Render(m.healthErr) + "\n")
	case m.healthLoading:
		b.WriteString(mutedStyle.Render("running checks…") + "\n")
	case !m.healthLoaded:
		b.WriteString(mutedStyle.Render("press r to run checks") + "\n")
	case len(m.healthChecks) == 0:
		b.WriteString(mutedStyle.Render("no checks") + "\n")
	default:
		b.WriteString(renderHealthChecks(m.healthChecks))
	}

	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("r re-run checks · s start daemon"))
	b.WriteByte('\n')
	return b.String()
}

// renderHealthChecks renders the global checks (Scope == "") as one table, then
// a labelled table per repo scope, in first-seen order. This keeps the global
// block visible once and groups the per-repo rows under each repo.
func renderHealthChecks(checks []HealthCheck) string {
	var b strings.Builder
	global := make([]HealthCheck, 0, len(checks))
	scopeOrder := []string{}
	byScope := map[string][]HealthCheck{}
	for _, c := range checks {
		if c.Scope == "" {
			global = append(global, c)
			continue
		}
		if _, seen := byScope[c.Scope]; !seen {
			scopeOrder = append(scopeOrder, c.Scope)
		}
		byScope[c.Scope] = append(byScope[c.Scope], c)
	}

	writeTable := func(group []HealthCheck) {
		rows := [][]string{{"CHECK", "STATUS", "DETAIL"}}
		for _, check := range group {
			rows = append(rows, []string{check.Name, healthStatusColor(check.Status), truncate(dash(check.Detail), 70)})
		}
		b.WriteString(renderRows(rows))
	}

	if len(global) > 0 {
		writeTable(global)
	}
	for _, scope := range scopeOrder {
		b.WriteByte('\n')
		b.WriteString(headerStyle.Render(scope))
		b.WriteByte('\n')
		writeTable(byScope[scope])
	}
	return b.String()
}

func healthStatusColor(status string) string {
	switch status {
	case "ok":
		return greenStyle.Render(status)
	case "fail":
		return redStyle.Render(status)
	case "warn":
		return redStyle.Render(status)
	default:
		return status
	}
}
