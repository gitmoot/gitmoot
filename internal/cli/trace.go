package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/issuetrace"
)

var newIssueTraceReader = func() issuetrace.Reader {
	return github.NewClient("")
}

func runTrace(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTraceUsage(stderr)
		return 2
	}
	switch args[0] {
	case "issue", "pr":
		return runTraceSelector(args[0], args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printTraceUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown trace command %q\n\n", args[0])
		printTraceUsage(stderr)
		return 2
	}
}

func runTraceSelector(kind string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace "+kind, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the schema-versioned trace as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "trace %s requires one owner/repo#N selector (flags must precede it)\n", kind)
		return 2
	}
	repo, number, err := parseTraceSelector(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "trace %s: %v\n", kind, err)
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "trace %s: resolve home: %v\n", kind, err)
		return 1
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "trace %s: open read-only store: %v\n", kind, err)
		return 1
	}
	defer store.Close()

	resolver := issuetrace.Resolver{Remote: newIssueTraceReader(), Local: store}
	var trace issuetrace.IssueTrace
	if kind == "issue" {
		trace, err = resolver.TraceIssue(context.Background(), repo, number)
	} else {
		trace, err = resolver.TracePullRequest(context.Background(), repo, number)
	}
	if err != nil {
		fmt.Fprintf(stderr, "trace %s: %v\n", kind, err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(trace); err != nil {
			fmt.Fprintf(stderr, "trace %s: encode output: %v\n", kind, err)
			return 1
		}
		return 0
	}
	renderIssueTrace(stdout, trace)
	return 0
}

func parseTraceSelector(value string) (github.Repository, int64, error) {
	repoText, numberText, ok := strings.Cut(strings.TrimSpace(value), "#")
	if !ok || strings.Contains(numberText, "#") {
		return github.Repository{}, 0, fmt.Errorf("selector must be owner/repo#N")
	}
	parts := strings.Split(repoText, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return github.Repository{}, 0, fmt.Errorf("selector must be owner/repo#N")
	}
	number, err := strconv.ParseInt(strings.TrimSpace(numberText), 10, 64)
	if err != nil || number <= 0 {
		return github.Repository{}, 0, fmt.Errorf("selector must end in a positive issue or PR number")
	}
	return github.Repository{Owner: strings.TrimSpace(parts[0]), Name: strings.TrimSpace(parts[1])}, number, nil
}

func renderIssueTrace(w io.Writer, trace issuetrace.IssueTrace) {
	fmt.Fprintf(w, "Issue trace: %s", trace.Repository)
	if trace.Issue.Number > 0 {
		fmt.Fprintf(w, "#%d", trace.Issue.Number)
	}
	fmt.Fprintln(w)
	if trace.Issue.Title != "" {
		fmt.Fprintf(w, "Issue: #%d %s (%s)\n", trace.Issue.Number, trace.Issue.Title, trace.Issue.State)
	}
	fmt.Fprintf(w, "Assessment: %s\n", strings.ToUpper(strings.ReplaceAll(trace.Assessment, "_", " ")))
	if trace.RemoteUnavailable {
		fmt.Fprintf(w, "Remote: unavailable (%s)\n", trace.RefreshError)
	}
	fmt.Fprintln(w, "PR attempts:")
	if len(trace.Attempts) == 0 {
		fmt.Fprintln(w, "  none found")
	}
	for _, attempt := range trace.Attempts {
		fmt.Fprintf(w, "  #%d %s [%s]", attempt.Number, attempt.Title, attempt.State)
		if attempt.MergeSHA != "" {
			fmt.Fprintf(w, " merge_commit=%s", attempt.MergeSHA)
		}
		if attempt.OnDefault != nil {
			fmt.Fprintf(w, " on_default_branch=%t", *attempt.OnDefault)
		}
		fmt.Fprintln(w)
		for _, evidence := range attempt.Evidence {
			fmt.Fprintf(w, "    - %s/%s: %s\n", evidence.Certainty, evidence.Basis, evidence.Detail)
		}
	}
	if len(trace.Successors) > 0 {
		fmt.Fprintln(w, "Successor graph:")
		for _, edge := range trace.Successors {
			fmt.Fprintf(w, "  #%d -> #%d (%s, %s)\n", edge.From, edge.To, edge.Basis, edge.Certainty)
		}
	}
	if len(trace.Tasks) > 0 {
		fmt.Fprintln(w, "Local tasks:")
		for _, task := range trace.Tasks {
			fmt.Fprintf(w, "  %s [%s] branch=%s (%s)\n", task.ID, task.State, task.Branch, task.Basis.Certainty)
		}
	}
	if len(trace.UnlinkedLocal) > 0 {
		fmt.Fprintln(w, "Unlinked local records:")
		for _, evidence := range trace.UnlinkedLocal {
			fmt.Fprintf(w, "  - %s/%s: %s\n", evidence.Certainty, evidence.Basis, evidence.Detail)
		}
	}
	fmt.Fprintf(w, "Deployment: %s (%s) - %s\n", trace.Deployment.Status, trace.Deployment.Certainty, trace.Deployment.Reason)
	for _, warning := range trace.Warnings {
		fmt.Fprintf(w, "Warning [%s]: %s\n", warning.Code, warning.Detail)
	}
}

func printTraceUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot trace issue [--home <path>] [--json] owner/repo#N")
	fmt.Fprintln(w, "  gitmoot trace pr [--home <path>] [--json] owner/repo#N")
}
