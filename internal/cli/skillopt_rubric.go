package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// `gitmoot skillopt rubric induce` (#347) — offline, deterministic rubric
// induction (AutoLibra-style). It reads the human feedback already captured for
// a template (RankedFeedbackEvent traits), induces a criterion-separated rubric,
// meta-evaluates it for coverage/redundancy on a held-out split, and writes the
// frozen rubric + report as files. It is read-only over the store and never
// injects anywhere — a human reviews the frozen JSON and maps it onto the
// judge's evaluator_config['rubric'] per the docs recipe.

func runSkillOptRubric(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptRubricUsage(stdout)
		return 0
	}
	switch args[0] {
	case "induce":
		return runSkillOptRubricInduce(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt rubric command %q\n\n", args[0])
		printSkillOptRubricUsage(stderr)
		return 2
	}
}

func printSkillOptRubricUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt rubric induce --template <id> [--out <dir>] [--holdout 0.2] [--min-events N] [--home path] [--json]")
}

// skillOptRubricInduceResult is the machine-readable result of one induce run
// (emitted under --json): the report numbers plus the paths of the written
// artifacts.
type skillOptRubricInduceResult struct {
	Report     skillopt.RubricReport `json:"report"`
	RubricPath string                `json:"rubric_path"`
	ReportPath string                `json:"report_path"`
	TextPath   string                `json:"report_text_path"`
}

func runSkillOptRubricInduce(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt rubric induce", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id whose feedback to induce a rubric from (required)")
	out := fs.String("out", "", "output directory for the frozen rubric + report (default <home>/skillopt/rubrics/<template>)")
	holdout := fs.Float64("holdout", -1, "fraction of aspects held out to measure coverage (default 0.2; 0 keeps in-sample)")
	minEvents := fs.Int("min-events", 0, "minimum usable feedback events required (default/floor 3)")
	jsonOutput := fs.Bool("json", false, "print the result (report + artifact paths) as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt rubric induce does not accept positional arguments")
		return 2
	}
	template := strings.TrimSpace(*templateID)
	if template == "" {
		fmt.Fprintln(stderr, "skillopt rubric induce: --template is required")
		return 2
	}

	var events []db.RankedFeedbackEventWithTemplate
	var outDir string
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		events, err = store.ListRankedFeedbackEventsAcrossRuns(context.Background(), template)
		if err != nil {
			return err
		}
		if trimmed := strings.TrimSpace(*out); trimmed != "" {
			outDir = trimmed
		} else {
			outDir = filepath.Join(paths.Home, "skillopt", "rubrics", template)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt rubric induce: %v\n", err)
		return 1
	}

	rubric, report, err := skillopt.InduceRubric(template, events, skillopt.RubricInduceOptions{
		HoldoutFraction: *holdout,
		MinEvents:       *minEvents,
	})
	if err != nil {
		fmt.Fprintf(stderr, "skillopt rubric induce: %v\n", err)
		return 1
	}

	rubricPath := filepath.Join(outDir, "rubric.json")
	reportPath := filepath.Join(outDir, "report.json")
	textPath := filepath.Join(outDir, "report.txt")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "skillopt rubric induce: create output dir: %v\n", err)
		return 1
	}
	if err := writeJSONFile(rubricPath, rubric); err != nil {
		fmt.Fprintf(stderr, "skillopt rubric induce: write rubric: %v\n", err)
		return 1
	}
	if err := writeJSONFile(reportPath, report); err != nil {
		fmt.Fprintf(stderr, "skillopt rubric induce: write report: %v\n", err)
		return 1
	}
	reportText := skillopt.RenderRubricReport(report)
	if err := os.WriteFile(textPath, []byte(reportText), 0o644); err != nil {
		fmt.Fprintf(stderr, "skillopt rubric induce: write report text: %v\n", err)
		return 1
	}

	if *jsonOutput {
		encoded, err := json.MarshalIndent(skillOptRubricInduceResult{
			Report:     report,
			RubricPath: rubricPath,
			ReportPath: reportPath,
			TextPath:   textPath,
		}, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "skillopt rubric induce: encode result: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}

	fmt.Fprint(stdout, reportText)
	fmt.Fprintf(stdout, "\nwrote frozen rubric -> %s\n", rubricPath)
	fmt.Fprintf(stdout, "wrote report (json)  -> %s\n", reportPath)
	fmt.Fprintf(stdout, "wrote report (text)  -> %s\n", textPath)
	fmt.Fprintln(stdout, "\nHUMAN-GATED: review rubric.json, then map its metrics onto the judge's")
	fmt.Fprintln(stdout, "evaluator_config['rubric'] (see docs). Nothing was injected automatically.")
	return 0
}

// writeJSONFile marshals v as indented JSON to path (with a trailing newline).
func writeJSONFile(path string, v any) error {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(path, encoded, 0o644)
}
