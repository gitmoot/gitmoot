package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// routerContextEnabled reports whether the off-by-default #530 coordinator
// routing-context injection is on for this home. It fails safe to disabled: any
// path/config-load error returns false, so a broken or absent config never turns
// the feature on. Mirrors canaryRoutingEnabled's off-by-default gate.
func routerContextEnabled(home string) bool {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return false
	}
	settings, err := config.LoadRouterSettings(paths)
	if err != nil {
		return false
	}
	return settings.ContextEnabled
}

func runRouter(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printRouterUsage(stdout)
		return 0
	}
	switch args[0] {
	case "summary":
		return runRouterSummary(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown router command %q\n\n", args[0])
		printRouterUsage(stderr)
		return 2
	}
}

func printRouterUsage(w io.Writer) {
	fmt.Fprintln(w, "Inspect execution-grounded routing telemetry (local observed performance, advisory only).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot router summary [--repo owner/repo] [--action ask|review|implement] [--since 30d] [--json]")
}

type routerSummaryJSON struct {
	Note   string                   `json:"note"`
	Total  int                      `json:"total_observations"`
	Groups []db.RoutingSummaryGroup `json:"groups"`
}

// routerSummaryNote is the mandatory disclaimer: this is local observed
// performance, never a global benchmark, and v1 routing is advisory only.
const routerSummaryNote = "local observed performance, not a benchmark (advisory only; routing is not auto-overridden)"

func runRouterSummary(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("router summary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "filter by repo (owner/repo)")
	action := fs.String("action", "", "filter by action (ask/review/implement/continuation/…)")
	since := fs.String("since", "", "only include observations newer than this (Go duration or <N>d, e.g. 30d)")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	filter := db.RoutingTelemetryFilter{Repo: *repo, Action: *action}
	if trimmed := *since; trimmed != "" {
		d, err := parseOlderThanDuration(trimmed)
		if err != nil {
			fmt.Fprintf(stderr, "router summary: parse --since: %v\n", err)
			return 2
		}
		if d > 0 {
			filter.Since = time.Now().Add(-d)
		}
	}

	var rows []db.RoutingTelemetry
	err := withReadOnlyStore(*home, func(store *db.Store) error {
		var err error
		rows, err = store.ListRoutingTelemetry(context.Background(), filter)
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "router summary: %v\n", err)
		return 1
	}

	groups := db.AggregateRoutingTelemetry(rows)

	if *jsonOut {
		if err := writeJSON(stdout, routerSummaryJSON{Note: routerSummaryNote, Total: len(rows), Groups: groups}); err != nil {
			fmt.Fprintf(stderr, "router summary: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "Routing summary — %s\n", routerSummaryNote)
	fmt.Fprintf(stdout, "%d observation(s)\n\n", len(rows))
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "no routing telemetry recorded yet")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tRUNTIME\tMODEL\tTEMPLATE\tN\tSUCCESS\tAPPROVAL\tMEDIAN(ms)\tTOKENS(in/out)")
	for _, g := range groups {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%.0f%%\t%.0f%%\t%d\t%d/%d\n",
			dashIfEmpty(g.Action), dashIfEmpty(g.Runtime), dashIfEmpty(g.Model), dashIfEmpty(g.TemplateID),
			g.Count, g.SuccessRate*100, g.ApprovalRate*100, g.MedianDurationMS, g.InputTokens, g.OutputTokens)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "router summary: %v\n", err)
		return 1
	}
	return 0
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
