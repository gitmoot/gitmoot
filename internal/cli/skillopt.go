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

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

func runSkillOpt(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "export":
		return runSkillOptExport(args[1:], stdout, stderr)
	case "import":
		return runSkillOptImport(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func printSkillOptUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt export --run <run-id> [--output package.json]")
	fmt.Fprintln(w, "  gitmoot skillopt import --file candidate.json")
}

func runSkillOptExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id to export")
	output := fs.String("output", "", "path to write the training package; stdout when omitted")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt export does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt export requires --run")
		return 2
	}
	var pkg skillopt.TrainingPackage
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		pkg, err = skillopt.ExportTrainingPackage(context.Background(), store, *runID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if strings.TrimSpace(*output) == "" {
		_, err = stdout.Write(encoded)
	} else {
		err = writeSkillOptFile(*output, encoded)
		if err == nil {
			writeLine(stdout, "exported %s to %s", pkg.EvalRun.ID, *output)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "skillopt export: %v\n", err)
		return 1
	}
	return 0
}

func runSkillOptImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	file := fs.String("file", "", "candidate package JSON file to import")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt import does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "skillopt import requires --file")
		return 2
	}
	content, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt import: read candidate package: %v\n", err)
		return 1
	}
	var pkg skillopt.CandidatePackage
	if err := json.Unmarshal(content, &pkg); err != nil {
		fmt.Fprintf(stderr, "skillopt import: decode candidate package: %v\n", err)
		return 1
	}
	var versionID string
	if err := withStore(*home, func(store *db.Store) error {
		version, err := skillopt.ImportCandidatePackage(context.Background(), store, pkg, *file)
		if err != nil {
			return err
		}
		versionID = version.ID
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt import: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported pending candidate %s", versionID)
	return 0
}

func writeSkillOptFile(path string, content []byte) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return os.WriteFile(path, content, 0o644)
}
