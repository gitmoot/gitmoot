package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/cli/style"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/feedback"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"gopkg.in/yaml.v3"
)

var newSkillOptGitHubClient = func() github.Client {
	return github.NewClient("")
}

var skillOptTrainOptimizerRunner subprocess.Runner = subprocess.ExecRunner{}

var skillOptTrainPreviewRunner subprocess.Runner = subprocess.ExecRunner{}

var skillOptTrainInitInteractive = func() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// skillOptTrainInitStdin is the reader the interactive train-init wizard reads
// answers from. It is a function so tests can substitute a scripted stdin.
var skillOptTrainInitStdin = func() io.Reader { return os.Stdin }

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
	case "review":
		return runSkillOptReview(args[1:], stdout, stderr)
	case "candidate":
		return runSkillOptCandidate(args[1:], stdout, stderr)
	case "feedback":
		return runSkillOptFeedback(args[1:], stdout, stderr)
	case "judge-report":
		return runSkillOptJudgeReport(args[1:], stdout, stderr)
	case "judge":
		return runSkillOptJudge(args[1:], stdout, stderr)
	case "train":
		return runSkillOptTrain(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func printSkillOptUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt export --run <run-id> [--output package.json]")
	fmt.Fprintln(w, "  gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]")
	fmt.Fprintln(w, "  gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id> [--mode validate|explore|refine|distill] [--options N]")
	fmt.Fprintln(w, "  gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md [--title text]")
	fmt.Fprintln(w, "  gitmoot skillopt review item add --run <run-id> --item <item-id> --option a=option-a.md --option b=option-b.md [...] [--title text]")
	fmt.Fprintln(w, "  gitmoot skillopt review status --run <run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate list [--template id]")
	fmt.Fprintln(w, "  gitmoot skillopt candidate show <version-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate promote <version-id>")
	fmt.Fprintln(w, "  gitmoot skillopt candidate reject <version-id> [--reason text]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>")
	fmt.Fprintln(w, "  gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]")
	fmt.Fprintln(w, "  gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)")
	fmt.Fprintln(w, "  gitmoot skillopt judge-report [--template id]")
	fmt.Fprintln(w, "  gitmoot skillopt judge promote --template <id> --task-kind <kind> --file <pkg.json> [--home <h>] [--yes] [--json]")
	fmt.Fprintln(w, "  gitmoot skillopt train init --name <name> --template <id> --review-repo owner/repo --artifact-kind kind --preview kind (--request text|--request-file path)")
	fmt.Fprintln(w, "  gitmoot skillopt train init templates --json")
	fmt.Fprintln(w, "  gitmoot skillopt train start --config .gitmoot/skillopt/<name>/config.toml [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file path [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train status --session <id>")
	fmt.Fprintln(w, "  gitmoot skillopt train run [--config path | --session <id>] [--plain]")
	fmt.Fprintln(w, "  gitmoot skillopt train continue --session <id> [--backend codex] [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--model name] [--optimizer-model name] [--target-model name] [--optimizer-backend name] [--target-backend name] [--evaluator-id id] [--evaluator-model name] [--evaluator-backend name] [--skill-update-mode mode] [--num-epochs N] [--batch-size N] [--optimizer-views N] [--retry-optimizer-views auto|inherit|N] [--gate hard|soft|mixed] [--out-root path] [--timeout duration] [--dry-run] [--rerun-optimizer] [--export-only] [--promote version|--reject version --reason text] [--start-next]")
	fmt.Fprintln(w, "  gitmoot skillopt train recover --session <id> [--out-root path]")
	fmt.Fprintln(w, "  gitmoot skillopt train stop --session <id> --reason <text>")
}

func runSkillOptTrain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptTrainUsage(stdout)
		return 0
	}
	switch args[0] {
	case "init":
		return runSkillOptTrainInit(args[1:], stdout, stderr)
	case "start":
		return runSkillOptTrainStart(args[1:], stdout, stderr)
	case "status":
		return runSkillOptTrainStatus(args[1:], stdout, stderr)
	case "run":
		return runSkillOptTrainRun(args[1:], stdout, stderr)
	case "continue":
		return runSkillOptTrainContinue(args[1:], stdout, stderr)
	case "recover":
		return runSkillOptTrainRecover(args[1:], stdout, stderr)
	case "stop":
		return runSkillOptTrainStop(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt train command %q\n\n", args[0])
		printSkillOptTrainUsage(stderr)
		return 2
	}
}

func printSkillOptTrainUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt train init --name <name> --template <id> --review-repo owner/repo --artifact-kind kind --preview kind (--request text|--request-file path)")
	fmt.Fprintln(w, "  gitmoot skillopt train init templates --json")
	fmt.Fprintln(w, "  gitmoot skillopt train start --config .gitmoot/skillopt/<name>/config.toml [--session <id>] [--items-file path] [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file path [--session <id>] [--workspace-repo owner/repo] [--preview-repo owner/repo] [--preview-mode none|optional|required] [--preview-renderer none|vue-vite] [--preview-publisher none|github-pages] [--preview-route-template template] [--request-file path] [--task-kind kind] [--mode explore|refine|distill|validate] [--exploration-level high|medium|low] [--options N] [--min-items N] [--preferred-gate hard|soft|hard_then_soft] [--dry-run] [--yes]")
	fmt.Fprintln(w, "  gitmoot skillopt train status --session <id>")
	fmt.Fprintln(w, "  gitmoot skillopt train run [--config path | --session <id>] [--plain]")
	fmt.Fprintln(w, "  gitmoot skillopt train continue --session <id> [--backend codex] [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--model name] [--optimizer-model name] [--target-model name] [--optimizer-backend name] [--target-backend name] [--evaluator-id id] [--evaluator-model name] [--evaluator-backend name] [--skill-update-mode mode] [--num-epochs N] [--batch-size N] [--optimizer-views N] [--retry-optimizer-views auto|inherit|N] [--gate hard|soft|mixed] [--out-root path] [--timeout duration] [--dry-run] [--rerun-optimizer] [--export-only] [--promote version|--reject version --reason text] [--start-next]")
	fmt.Fprintln(w, "  gitmoot skillopt train recover --session <id> [--out-root path]")
	fmt.Fprintln(w, "  gitmoot skillopt train stop --session <id> --reason <text>")
}

func runSkillOptTrainInit(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printSkillOptTrainInitUsage(stdout)
		return 0
	}
	if len(args) > 0 && args[0] == "templates" {
		return runSkillOptTrainInitTemplates(args[1:], stdout, stderr)
	}
	return runSkillOptTrainInitCreate(args, stdout, stderr)
}

func printSkillOptTrainInitUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt train init --name <name> --template <id> --review-repo owner/repo --artifact-kind kind --preview kind (--request text|--request-file path) [--task-kind kind] [--mode explore|refine|distill|validate]")
	fmt.Fprintln(w, "  gitmoot skillopt train init templates --json")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "With missing fields, an interactive terminal runs a line-oriented wizard;")
	fmt.Fprintln(w, "each question is also a prompt record an agent can answer with")
	fmt.Fprintln(w, "gitmoot interactive answer. Use --prompts to emit all prompt records at once")
	fmt.Fprintln(w, "and exit instead, or --yes to fail on missing fields.")
}

func runSkillOptTrainInitCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	name := fs.String("name", "", "train scaffold name")
	templateRef := fs.String("template", "", "agent template id or version to train")
	reviewRepoFlag := fs.String("review-repo", "", "review repository in owner/repo form")
	taskKind := fs.String("task-kind", "custom", "task kind")
	artifactKind := fs.String("artifact-kind", "", "artifact kind, such as text, vue, pdf, or custom")
	preview := fs.String("preview", "", "preview kind: none, text-table, or vue")
	mode := fs.String("mode", db.EvalRunModeExplore, "train mode: explore, refine, distill, or validate")
	requestText := fs.String("request", "", "human training request for task.md")
	requestFile := fs.String("request-file", "", "file containing the human training request")
	yes := fs.Bool("yes", false, "do not prompt for missing fields; fail if any are missing")
	prompts := fs.Bool("prompts", false, "emit interactive prompt records for missing fields instead of running the inline wizard")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train init does not accept positional arguments")
		return 2
	}
	request, err := readSkillOptTrainRequest(*requestText, *requestFile)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 2
	}
	values := skillOptTrainInitInputs{
		Name:         strings.TrimSpace(*name),
		Template:     strings.TrimSpace(*templateRef),
		ReviewRepo:   strings.TrimSpace(*reviewRepoFlag),
		TaskKind:     strings.TrimSpace(*taskKind),
		ArtifactKind: strings.TrimSpace(*artifactKind),
		Preview:      strings.TrimSpace(*preview),
		Mode:         strings.TrimSpace(*mode),
		Request:      strings.TrimSpace(request),
	}
	setFlags := flagNamesSet(fs)
	if values.TaskKind == "" {
		values.TaskKind = "custom"
	}
	if values.Mode == "" {
		values.Mode = db.EvalRunModeExplore
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train init: get working directory: %v\n", err)
		return 1
	}
	promptScope := skillOptTrainInitPromptScope(cwd, values.Name)
	rerunCommand := skillOptTrainInitRerunCommand(*home, values, strings.TrimSpace(*requestFile), setFlags)
	appliedPromptFields, err := applySkillOptTrainInitPromptAnswers(*home, promptScope, setFlags, &values)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 1
	}
	if missing := missingSkillOptTrainInitInputs(values); len(missing) > 0 {
		switch {
		case *yes:
			fmt.Fprintf(stderr, "skillopt train init missing required fields: %s\n", strings.Join(missing, ", "))
			fmt.Fprintf(stderr, "example: %s\n", skillOptTrainInitExampleCommand(values.Name))
			return 2
		case *prompts:
			created, err := createSkillOptTrainInitPrompts(*home, promptScope, values, missing)
			if err != nil {
				fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
				return 1
			}
			printSkillOptTrainInitPromptInstructions(stdout, *home, rerunCommand, created)
			return 0
		case skillOptTrainInitTUICapable():
			// A real terminal: run the interactive form. Ordered before the line
			// wizard so that under `go test` (pipes, not char devices) capable() is
			// false and the existing wizard path runs unchanged.
			if err := runSkillOptTrainInitTUI(*home, promptScope, stdout, &values, missing); err != nil {
				if errors.Is(err, errSkillOptTrainInitAborted) {
					writeLine(stdout, "aborted: no scaffold written")
					return 0
				}
				fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
				return 1
			}
			if stillMissing := missingSkillOptTrainInitInputs(values); len(stillMissing) > 0 {
				fmt.Fprintf(stderr, "skillopt train init missing required fields: %s\n", strings.Join(stillMissing, ", "))
				fmt.Fprintf(stderr, "example: %s\n", skillOptTrainInitExampleCommand(values.Name))
				return 2
			}
		case skillOptTrainInitInteractive():
			if err := runSkillOptTrainInitWizard(*home, promptScope, skillOptTrainInitStdin(), stdout, &values, missing); err != nil {
				if errors.Is(err, errSkillOptTrainInitAborted) {
					writeLine(stdout, "aborted: no scaffold written")
					return 0
				}
				fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
				return 1
			}
			if stillMissing := missingSkillOptTrainInitInputs(values); len(stillMissing) > 0 {
				fmt.Fprintf(stderr, "skillopt train init missing required fields: %s\n", strings.Join(stillMissing, ", "))
				fmt.Fprintf(stderr, "example: %s\n", skillOptTrainInitExampleCommand(values.Name))
				return 2
			}
		default:
			fmt.Fprintf(stderr, "skillopt train init missing required fields: %s\n", strings.Join(missing, ", "))
			fmt.Fprintf(stderr, "example: %s\n", skillOptTrainInitExampleCommand(values.Name))
			return 2
		}
	}
	if err := skillopt.ValidateTrainInitName(values.Name); err != nil {
		clearSkillOptTrainInitPromptAnswerOnError(*home, promptScope, appliedPromptFields, "name")
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 2
	}
	reviewRepo, err := daemon.ParseRepository(values.ReviewRepo)
	if err != nil {
		clearSkillOptTrainInitPromptAnswerOnError(*home, promptScope, appliedPromptFields, "review_repo")
		fmt.Fprintf(stderr, "skillopt train init: review-repo: %v\n", err)
		return 2
	}
	if _, err := normalizeSkillOptTrainTaskKind(values.TaskKind); err != nil {
		clearSkillOptTrainInitPromptAnswerOnError(*home, promptScope, appliedPromptFields, "task_kind")
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 2
	}
	normalizedPreview, err := normalizeSkillOptTrainInitPreview(values.Preview)
	if err != nil {
		clearSkillOptTrainInitPromptAnswerOnError(*home, promptScope, appliedPromptFields, "preview")
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 2
	}
	values.Preview = normalizedPreview
	normalizedMode, normalizedExploration, err := normalizeSkillOptTrainMode(values.Mode, "")
	if err != nil {
		clearSkillOptTrainInitPromptAnswerOnError(*home, promptScope, appliedPromptFields, "mode")
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 2
	}
	values.Mode = normalizedMode
	var template db.AgentTemplate
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		template, err = skillopt.ResolveTrainInitTemplateChoice(context.Background(), store, newAgentTemplateFetcher(), values.Template)
		return err
	}); err != nil {
		clearSkillOptTrainInitPromptAnswerOnError(*home, promptScope, appliedPromptFields, "template")
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 1
	}
	config := skillopt.DefaultTrainInitConfig()
	config.Name = values.Name
	config.Template = template.ID
	config.TemplateVersion = template.VersionID
	config.ReviewRepo = reviewRepo.FullName()
	config.TaskKind = values.TaskKind
	config.ArtifactKind = values.ArtifactKind
	config.Preview = values.Preview
	config.Mode = values.Mode
	config.ExplorationLevel = normalizedExploration
	config.Options = skillOptTrainInitDefaultOptions(normalizedMode)
	paths, err := skillopt.WriteTrainInitScaffold(cwd, skillopt.TrainInitScaffold{
		Config:          config,
		TaskMarkdown:    values.Request,
		ReviewItemsYAML: skillOptTrainInitStarterReviewItemsYAML(values),
	})
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 1
	}
	if err := consumeSkillOptTrainInitPromptAnswers(*home, promptScope); err != nil {
		fmt.Fprintf(stderr, "skillopt train init: %v\n", err)
		return 1
	}
	writeLine(stdout, "created train init scaffold %s", relativeOrAbsolutePath(cwd, paths.Root))
	writeLine(stdout, "config: %s", relativeOrAbsolutePath(cwd, paths.ConfigPath))
	writeLine(stdout, "task: %s", relativeOrAbsolutePath(cwd, paths.TaskPath))
	writeLine(stdout, "next: gitmoot skillopt train start --config %s --workspace-repo <owner/repo>", filepath.ToSlash(filepath.Join(".gitmoot", skillopt.TrainInitScaffoldDirName, values.Name, skillopt.TrainInitConfigFileName)))
	return 0
}

type skillOptTrainInitInputs struct {
	Name         string
	Template     string
	ReviewRepo   string
	TaskKind     string
	ArtifactKind string
	Preview      string
	Mode         string
	Request      string
}

type skillOptTrainInitPrompt struct {
	Field  string
	Flag   string
	Prompt db.InteractivePrompt
}

func missingSkillOptTrainInitInputs(values skillOptTrainInitInputs) []string {
	missing := []string{}
	for field, value := range map[string]string{
		"name":          values.Name,
		"template":      values.Template,
		"review_repo":   values.ReviewRepo,
		"task_kind":     values.TaskKind,
		"artifact_kind": values.ArtifactKind,
		"preview":       values.Preview,
		"mode":          values.Mode,
		"request":       values.Request,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, field)
		}
	}
	sort.Strings(missing)
	return missing
}

func skillOptTrainInitPromptScope(workspaceRoot string, name string) string {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if abs, err := filepath.Abs(workspaceRoot); err == nil {
		workspaceRoot = abs
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	sum := sha256.Sum256([]byte(workspaceRoot))
	workspaceScope := "ws-" + hex.EncodeToString(sum[:8])
	name = strings.TrimSpace(name)
	if name == "" {
		return workspaceScope + "-empty"
	}
	return workspaceScope + "-name-" + hex.EncodeToString([]byte(name))
}

func skillOptTrainInitPromptID(scope string, field string) string {
	return "skillopt-train-init." + strings.TrimSpace(scope) + "." + strings.ReplaceAll(field, "_", "-")
}

func applySkillOptTrainInitPromptAnswers(home string, scope string, setFlags map[string]struct{}, values *skillOptTrainInitInputs) (map[string]struct{}, error) {
	if values == nil || !skillOptTrainInitPromptDatabaseExists(home) {
		return nil, nil
	}
	paths, err := skillOptTrainInitConfigPaths(home)
	if err != nil {
		return nil, err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	prompts, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		return nil, err
	}
	answers := map[string]string{}
	for _, prompt := range prompts {
		if prompt.State != db.InteractivePromptStateResolved {
			continue
		}
		field, ok := skillOptTrainInitPromptField(scope, prompt.ID)
		if !ok {
			continue
		}
		answers[field] = prompt.AnswerValue
	}
	applied := map[string]struct{}{}
	for field, answer := range answers {
		if strings.TrimSpace(answer) == "" || skillOptTrainInitFieldWasSet(field, setFlags) {
			continue
		}
		switch field {
		case "name":
			values.Name = answer
		case "template":
			values.Template = answer
		case "review_repo":
			values.ReviewRepo = answer
		case "task_kind":
			values.TaskKind = answer
		case "artifact_kind":
			values.ArtifactKind = answer
		case "preview":
			values.Preview = answer
		case "mode":
			values.Mode = answer
		case "request":
			values.Request = answer
		}
		applied[field] = struct{}{}
	}
	return applied, nil
}

func clearSkillOptTrainInitPromptAnswerOnError(home string, scope string, appliedPromptFields map[string]struct{}, field string) {
	if _, ok := appliedPromptFields[field]; !ok {
		return
	}
	_ = deleteSkillOptTrainInitPrompt(home, scope, field)
}

func deleteSkillOptTrainInitPrompt(home string, scope string, field string) error {
	if !skillOptTrainInitPromptDatabaseExists(home) {
		return nil
	}
	paths, err := skillOptTrainInitConfigPaths(home)
	if err != nil {
		return err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.DeleteInteractivePrompt(context.Background(), skillOptTrainInitPromptID(scope, field))
}

func consumeSkillOptTrainInitPromptAnswers(home string, scope string) error {
	if !skillOptTrainInitPromptDatabaseExists(home) {
		return nil
	}
	paths, err := skillOptTrainInitConfigPaths(home)
	if err != nil {
		return err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	prompts, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		return err
	}
	for _, prompt := range prompts {
		if _, ok := skillOptTrainInitPromptField(scope, prompt.ID); !ok {
			continue
		}
		if err := store.DeleteInteractivePrompt(context.Background(), prompt.ID); err != nil && !errors.Is(err, db.ErrInteractivePromptNotFound) {
			return err
		}
	}
	return nil
}

func skillOptTrainInitPromptDatabaseExists(home string) bool {
	paths, err := skillOptTrainInitConfigPaths(home)
	if err != nil {
		return false
	}
	info, err := os.Stat(paths.Database)
	return err == nil && !info.IsDir()
}

func skillOptTrainInitConfigPaths(home string) (config.Paths, error) {
	if strings.TrimSpace(home) != "" {
		return config.PathsForHome(home), nil
	}
	return config.DefaultPaths()
}

func skillOptTrainInitPromptField(scope string, promptID string) (string, bool) {
	prefix := "skillopt-train-init." + strings.TrimSpace(scope) + "."
	if !strings.HasPrefix(promptID, prefix) {
		return "", false
	}
	field := strings.TrimPrefix(promptID, prefix)
	field = strings.ReplaceAll(field, "-", "_")
	switch field {
	case "name", "template", "review_repo", "task_kind", "artifact_kind", "preview", "mode", "request":
		return field, true
	default:
		return "", false
	}
}

func skillOptTrainInitFieldWasSet(field string, setFlags map[string]struct{}) bool {
	switch field {
	case "request":
		return flagWasSet(setFlags, "request") || flagWasSet(setFlags, "request-file")
	case "review_repo":
		return flagWasSet(setFlags, "review-repo")
	case "task_kind":
		return flagWasSet(setFlags, "task-kind")
	case "artifact_kind":
		return flagWasSet(setFlags, "artifact-kind")
	default:
		return flagWasSet(setFlags, strings.ReplaceAll(field, "_", "-"))
	}
}

func createSkillOptTrainInitPrompts(home string, scope string, values skillOptTrainInitInputs, missing []string) ([]skillOptTrainInitPrompt, error) {
	var created []skillOptTrainInitPrompt
	err := withStore(home, func(store *db.Store) error {
		for _, field := range missing {
			prompt := buildSkillOptTrainInitPrompt(scope, field, values)
			if err := store.UpsertInteractivePrompt(context.Background(), prompt.Prompt); err != nil {
				return err
			}
			created = append(created, prompt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// skillOptTrainInitWizardPoll is how often the wizard checks whether the current
// question's prompt record was answered externally (via `interactive answer`).
// It is a var so tests can shorten it.
var skillOptTrainInitWizardPoll = 200 * time.Millisecond

type skillOptTrainInitWizardLine struct {
	text string
	eof  bool
}

// runSkillOptTrainInitWizard fills the missing train-init fields by asking the
// human one question at a time. Each question is also published as an
// interactive prompt record (from #195), so an agent driving the wizard in a
// PTY can answer it with `gitmoot interactive answer` instead of stdin; the
// wizard blocks on each question until it is answered on stdin or resolved
// externally. Numbered choices are shown for the template (with a "Custom file"
// option) and the preview style. Semantic validation happens in the caller
// after the wizard returns.
func runSkillOptTrainInitWizard(home, scope string, stdin io.Reader, stdout io.Writer, values *skillOptTrainInitInputs, missing []string) error {
	missingSet := make(map[string]struct{}, len(missing))
	for _, field := range missing {
		missingSet[field] = struct{}{}
	}
	lines := make(chan skillOptTrainInitWizardLine)
	done := make(chan struct{})
	defer close(done)
	go func() {
		// This goroutine can park in ReadString when the wizard finishes via an
		// external answer while stdin is still open; close(done) unblocks a
		// pending send but not the read. `train init` exits right after the
		// wizard, so the parked read is reclaimed at process exit (and tests
		// close their stdin), keeping the leak bounded.
		reader := bufio.NewReader(stdin)
		for {
			text, err := reader.ReadString('\n')
			select {
			case lines <- skillOptTrainInitWizardLine{text: text, eof: err != nil}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// task_kind and mode are defaulted before missing-field detection, so they
	// never reach the wizard; ask the remaining missing fields in a stable order.
	ask := make([]string, 0, len(missing))
	for _, field := range []string{"name", "template", "review_repo", "artifact_kind", "preview", "request"} {
		if _, ok := missingSet[field]; ok {
			ask = append(ask, field)
		}
	}
	st := style.For(stdout)
	if len(ask) > 0 {
		fmt.Fprintln(stdout, st.Dim("tip: answer any question from another terminal with: gitmoot interactive answer <prompt-id> <value> (ids: gitmoot interactive list)"))
	}

	stdinClosed := false
	externallyAnswered := false
	return withStore(home, func(store *db.Store) error {
		for index, field := range ask {
			value, err := skillOptTrainInitAwaitField(store, scope, field, *values, stdout, st, index+1, len(ask), lines, &stdinClosed, &externallyAnswered)
			if err != nil {
				return err
			}
			switch field {
			case "name":
				values.Name = value
			case "template":
				values.Template = value
			case "review_repo":
				values.ReviewRepo = value
			case "artifact_kind":
				values.ArtifactKind = value
			case "preview":
				values.Preview = value
			case "request":
				values.Request = value
			}
		}
		// Only confirm when every field was actually answered (a cut-short stdin
		// leaves missing fields for the caller to report) and when a human drove
		// the wizard on stdin (an external driver answered each field
		// deliberately, so auto-proceed).
		if len(missingSkillOptTrainInitInputs(*values)) == 0 {
			if !skillOptTrainInitWizardConfirm(stdout, st, *values, lines, stdinClosed, externallyAnswered) {
				return errSkillOptTrainInitAborted
			}
		}
		return nil
	})
}

// errSkillOptTrainInitAborted signals that the human declined the wizard's
// confirm step; the caller exits cleanly without writing a scaffold.
var errSkillOptTrainInitAborted = errors.New("train init aborted by user")

// skillOptTrainInitWizardConfirm prints a summary of the answers and asks for
// confirmation. It auto-accepts when the wizard was driven externally (via
// `gitmoot interactive answer`) or stdin has closed, so the agent-assisted and
// scripted flows proceed without blocking. A blank line or EOF accepts; "n"/"no"
// declines.
func skillOptTrainInitWizardConfirm(stdout io.Writer, st style.Style, values skillOptTrainInitInputs, lines <-chan skillOptTrainInitWizardLine, stdinClosed, externallyAnswered bool) bool {
	fmt.Fprintln(stdout, st.Bold("Review:"))
	rows := [][]string{
		{"name", values.Name},
		{"template", values.Template},
		{"review repo", values.ReviewRepo},
		{"task kind", values.TaskKind},
		{"artifact kind", values.ArtifactKind},
		{"preview", values.Preview},
		{"mode", values.Mode},
		{"request", firstLine(values.Request)},
	}
	for _, line := range style.Columns(rows) {
		fmt.Fprintf(stdout, "  %s\n", st.Dim(line))
	}
	if externallyAnswered || stdinClosed {
		return true
	}
	for {
		fmt.Fprint(stdout, st.Bold("Create scaffold? [Y/n] "))
		msg, ok := <-lines
		if !ok {
			return true
		}
		switch strings.ToLower(strings.TrimSpace(msg.text)) {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		default:
			if msg.eof {
				return true
			}
			// Unrecognized non-EOF input: re-ask.
		}
	}
}

// skillOptTrainInitAwaitField publishes the prompt record for one field, renders
// the question, and blocks until an answer arrives on stdin or the prompt is
// resolved externally. It returns "" (and marks stdin closed) when stdin ends
// before the field is answered, leaving the caller to report the missing field.
func skillOptTrainInitAwaitField(store *db.Store, scope, field string, values skillOptTrainInitInputs, stdout io.Writer, st style.Style, index, total int, lines <-chan skillOptTrainInitWizardLine, stdinClosed, externallyAnswered *bool) (string, error) {
	ctx := context.Background()
	prompt := buildSkillOptTrainInitPrompt(scope, field, values).Prompt
	if err := store.UpsertInteractivePrompt(ctx, prompt); err != nil {
		return "", err
	}
	defer func() { _ = store.DeleteInteractivePrompt(ctx, prompt.ID) }()

	var templateChoices []skillopt.TrainInitTemplateChoice
	if field == "template" {
		var err error
		templateChoices, err = skillopt.ListTrainInitTemplateChoices(ctx, store)
		if err != nil {
			return "", err
		}
	}
	skillOptTrainInitWizardRenderQuestion(stdout, st, field, prompt, templateChoices, index, total)
	if *stdinClosed {
		// No stdin remains and waiting only on an external answer could hang, so
		// give up and let the caller report the missing field.
		return "", nil
	}

	ticker := time.NewTicker(skillOptTrainInitWizardPoll)
	defer ticker.Stop()
	for {
		select {
		case msg := <-lines:
			// A message can carry EOF together with a final unterminated line;
			// once seen, the stdin goroutine has exited, so later fields must not
			// wait on stdin again.
			if msg.eof {
				*stdinClosed = true
			}
			value, status := skillOptTrainInitWizardInterpret(field, msg.text, prompt, templateChoices)
			switch status {
			case "ok":
				return value, nil
			case "custom":
				if *stdinClosed {
					// No stdin remains to read the custom path.
					return "", nil
				}
				ref, eof := skillOptTrainInitWizardReadLine(lines, stdout, "Enter a template id, version, or file path: ")
				if eof {
					*stdinClosed = true
				}
				return ref, nil
			default:
				if *stdinClosed {
					return "", nil
				}
				// Short re-ask without reprinting the whole question/list.
				skillOptTrainInitWizardReask(stdout, st, field, prompt, templateChoices)
			}
		case <-ticker.C:
			current, err := store.GetInteractivePrompt(ctx, prompt.ID)
			if err != nil {
				return "", err
			}
			if current.State == db.InteractivePromptStateResolved {
				// Resolved by an external `gitmoot interactive answer`, not stdin.
				*externallyAnswered = true
				return current.AnswerValue, nil
			}
		}
	}
}

// skillOptTrainInitWizardReadLine reads one non-empty stdin line for a follow-up
// question (the custom template path). The bool return reports whether stdin
// reached EOF (so the caller can stop waiting on stdin), which may be true even
// when a final unterminated line still yielded a value.
func skillOptTrainInitWizardReadLine(lines <-chan skillOptTrainInitWizardLine, stdout io.Writer, prompt string) (string, bool) {
	fmt.Fprint(stdout, prompt)
	for msg := range lines {
		if value := strings.TrimSpace(msg.text); value != "" {
			return value, msg.eof
		}
		if msg.eof {
			return "", true
		}
	}
	return "", true
}

// skillOptTrainInitWizardInterpret maps a raw stdin line to a field value. It
// returns status "ok" (use value), "custom" (template custom-file selected, the
// caller must read the path), or "reask".
func skillOptTrainInitWizardInterpret(field, text string, prompt db.InteractivePrompt, templateChoices []skillopt.TrainInitTemplateChoice) (string, string) {
	return skillOptTrainInitInterpretCore(field, text, prompt, templateChoices)
}

// skillOptTrainInitInterpretCore is the shared validation core for a train-init
// field answer, used by both the line wizard and the bubbletea TUI form. It
// returns status "ok" (use value), "custom" (template custom-file selected), or
// "reask".
func skillOptTrainInitInterpretCore(field, text string, prompt db.InteractivePrompt, templateChoices []skillopt.TrainInitTemplateChoice) (string, string) {
	value := strings.TrimSpace(text)
	if field == "template" {
		if value == "" {
			return "", "reask"
		}
		if n, err := strconv.Atoi(value); err == nil {
			switch {
			case n >= 1 && n <= len(templateChoices):
				return templateChoices[n-1].ID, "ok"
			case n == len(templateChoices)+1:
				return "", "custom"
			default:
				return "", "reask"
			}
		}
		for _, choice := range templateChoices {
			if strings.EqualFold(value, choice.ID) {
				return choice.ID, "ok"
			}
		}
		return value, "ok"
	}
	if len(prompt.Choices) > 0 {
		if value == "" {
			if strings.TrimSpace(prompt.Default) != "" {
				return prompt.Default, "ok"
			}
			return "", "reask"
		}
		if n, err := strconv.Atoi(value); err == nil {
			if n >= 1 && n <= len(prompt.Choices) {
				return prompt.Choices[n-1], "ok"
			}
			return "", "reask"
		}
		for _, choice := range prompt.Choices {
			if strings.EqualFold(value, choice) {
				return choice, "ok"
			}
		}
		return "", "reask"
	}
	if value == "" {
		return "", "reask"
	}
	return value, "ok"
}

// skillOptTrainInitWizardLabel is the short human-facing prompt for a field.
// The interactive prompt RECORD keeps its own flag-hint Question text; only the
// terminal rendering uses these labels.
func skillOptTrainInitWizardLabel(field string) string {
	switch field {
	case "name":
		return "Training name"
	case "review_repo":
		return "Review repository (owner/repo)"
	case "artifact_kind":
		return "Artifact kind (text, vue, pdf, custom)"
	case "preview":
		return "Preview kind"
	case "request":
		return "Training request"
	default:
		return field
	}
}

func skillOptTrainInitWizardRenderQuestion(stdout io.Writer, st style.Style, field string, prompt db.InteractivePrompt, templateChoices []skillopt.TrainInitTemplateChoice, index, total int) {
	progress := st.Dim(fmt.Sprintf("[%d/%d] ", index, total))
	if field == "template" {
		fmt.Fprintf(stdout, "%s%s\n", progress, st.Bold("Choose a template:"))
		for i, choice := range templateChoices {
			fmt.Fprintf(stdout, "  %s %s\n", st.Dim(fmt.Sprintf("%d.", i+1)), skillOptTrainInitTemplateChoiceLabel(choice))
		}
		fmt.Fprintf(stdout, "  %s Custom file\n", st.Dim(fmt.Sprintf("%d.", len(templateChoices)+1)))
		fmt.Fprint(stdout, "Enter number: ")
		return
	}
	if len(prompt.Choices) > 0 {
		fmt.Fprintf(stdout, "%s%s\n", progress, st.Bold(skillOptTrainInitWizardLabel(field)+":"))
		for i, choice := range prompt.Choices {
			suffix := ""
			if choice == prompt.Default {
				suffix = st.Dim(" (default)")
			}
			fmt.Fprintf(stdout, "  %s %s%s\n", st.Dim(fmt.Sprintf("%d.", i+1)), choice, suffix)
		}
		fmt.Fprint(stdout, "Enter number or value: ")
		return
	}
	fmt.Fprintf(stdout, "%s%s ", progress, st.Bold(skillOptTrainInitWizardLabel(field)+":"))
}

// skillOptTrainInitWizardReask prints a short field-aware re-prompt without
// reprinting the question or template list.
func skillOptTrainInitWizardReask(stdout io.Writer, st style.Style, field string, prompt db.InteractivePrompt, templateChoices []skillopt.TrainInitTemplateChoice) {
	switch {
	case field == "template":
		fmt.Fprintf(stdout, "%s ", st.Yellow(fmt.Sprintf("invalid — enter 1-%d:", len(templateChoices)+1)))
	case len(prompt.Choices) > 0:
		fmt.Fprintf(stdout, "%s ", st.Yellow(fmt.Sprintf("invalid — enter 1-%d or a listed value:", len(prompt.Choices))))
	default:
		fmt.Fprintf(stdout, "%s ", st.Yellow(skillOptTrainInitWizardLabel(field)+":"))
	}
}

func skillOptTrainInitTemplateChoiceLabel(choice skillopt.TrainInitTemplateChoice) string {
	label := choice.ID
	if choice.CurrentVersion != "" {
		// CurrentVersion is the full version id (e.g. "planner@v3"); strip the
		// redundant "<id>@" prefix so the label reads "planner @v3".
		label += " @" + strings.TrimPrefix(choice.CurrentVersion, choice.ID+"@")
	}
	status := choice.Source
	if !choice.Installed {
		if status != "" {
			status += ", "
		}
		status += "not installed"
	}
	if status != "" {
		label += " (" + status + ")"
	}
	return label
}

func buildSkillOptTrainInitPrompt(scope string, field string, values skillOptTrainInitInputs) skillOptTrainInitPrompt {
	descriptor := skillOptTrainInitPrompt{
		Field: field,
		Flag:  skillOptTrainInitFlagForField(field),
		Prompt: db.InteractivePrompt{
			ID:            skillOptTrainInitPromptID(scope, field),
			Required:      true,
			AnswerFormat:  "text",
			SourceCommand: "gitmoot skillopt train init",
		},
	}
	switch field {
	case "name":
		descriptor.Prompt.Question = "Training name? (flag: --name)"
	case "template":
		descriptor.Prompt.Question = "Agent template to train? (flag: --template)"
	case "review_repo":
		descriptor.Prompt.Question = "Review repository in owner/repo form? (flag: --review-repo)"
	case "task_kind":
		descriptor.Prompt.Question = "Task kind? (flag: --task-kind)"
		descriptor.Prompt.Choices = []string{"correctness", "ux", "design", "writing", "data", "custom"}
		descriptor.Prompt.Default = firstNonEmpty(values.TaskKind, "custom")
		descriptor.Prompt.AnswerFormat = "choice"
	case "artifact_kind":
		descriptor.Prompt.Question = "Artifact kind to optimize? (flag: --artifact-kind)"
	case "preview":
		descriptor.Prompt.Question = "Preview kind? (flag: --preview)"
		descriptor.Prompt.Choices = []string{"none", "text-table", "vue"}
		descriptor.Prompt.AnswerFormat = "choice"
	case "mode":
		descriptor.Prompt.Question = "Training mode? (flag: --mode)"
		descriptor.Prompt.Choices = []string{db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate}
		descriptor.Prompt.Default = firstNonEmpty(values.Mode, db.EvalRunModeExplore)
		descriptor.Prompt.AnswerFormat = "choice"
	case "request":
		descriptor.Prompt.Question = "Training request text? (flag: --request)"
	default:
		descriptor.Prompt.Question = field + "? (flag: " + descriptor.Flag + ")"
	}
	return descriptor
}

func skillOptTrainInitFlagForField(field string) string {
	switch field {
	case "review_repo":
		return "--review-repo"
	case "task_kind":
		return "--task-kind"
	case "artifact_kind":
		return "--artifact-kind"
	default:
		return "--" + strings.ReplaceAll(field, "_", "-")
	}
}

func printSkillOptTrainInitPromptInstructions(stdout io.Writer, home string, rerunCommand []string, prompts []skillOptTrainInitPrompt) {
	writeLine(stdout, "interactive_prompts_created: %d", len(prompts))
	for _, prompt := range prompts {
		writeLine(stdout, "prompt: %s field=%s flag=%s", prompt.Prompt.ID, prompt.Field, prompt.Flag)
		writeLine(stdout, "question: %s", prompt.Prompt.Question)
		writeLine(stdout, "show_command: %s", shellArgs(append(skillOptTrainInitInteractiveCommandPrefix("show", home), prompt.Prompt.ID, "--json")))
		writeLine(stdout, "answer_command: %s <value>", shellArgs(append(skillOptTrainInitInteractiveCommandPrefix("answer", home), prompt.Prompt.ID)))
	}
	writeLine(stdout, "next: answer the prompts, then rerun %s", shellArgs(rerunCommand))
}

func skillOptTrainInitInteractiveCommandPrefix(command string, home string) []string {
	args := []string{"gitmoot", "interactive", command}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", strings.TrimSpace(home))
	}
	return args
}

func skillOptTrainInitRerunCommand(home string, values skillOptTrainInitInputs, requestFile string, setFlags map[string]struct{}) []string {
	args := []string{"gitmoot", "skillopt", "train", "init"}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", strings.TrimSpace(home))
	}
	for _, field := range []struct {
		flag  string
		value string
	}{
		{"name", values.Name},
		{"template", values.Template},
		{"review-repo", values.ReviewRepo},
		{"task-kind", values.TaskKind},
		{"artifact-kind", values.ArtifactKind},
		{"preview", values.Preview},
		{"mode", values.Mode},
		{"request", values.Request},
		{"request-file", requestFile},
	} {
		if !flagWasSet(setFlags, field.flag) || strings.TrimSpace(field.value) == "" {
			continue
		}
		if field.flag == "request-file" && strings.TrimSpace(values.Request) == "" {
			continue
		}
		args = append(args, "--"+field.flag, strings.TrimSpace(field.value))
	}
	return args
}

func skillOptTrainInitExampleCommand(name string) string {
	if strings.TrimSpace(name) == "" {
		name = "my-skill-training"
	}
	return "gitmoot skillopt train init --name " + name + " --template <template-id> --review-repo owner/repo --task-kind custom --artifact-kind text --preview text-table --mode explore --request \"Describe what to improve\""
}

func skillOptTrainInitStarterReviewItemsYAML(values skillOptTrainInitInputs) []byte {
	return skillOptTrainInitStarterReviewItemsYAMLN(values, 2)
}

// skillOptTrainInitStarterReviewItemsYAMLN emits count starter review items
// (floor 2, the train-start minimum). The first two keep their established
// titles/briefs; further items are numbered variation scenarios.
func skillOptTrainInitStarterReviewItemsYAMLN(values skillOptTrainInitInputs, count int) []byte {
	if count < 2 {
		count = 2
	}
	artifactKind := firstNonEmpty(strings.TrimSpace(values.ArtifactKind), "artifact")
	lines := []string{"items:"}
	for i := 1; i <= count; i++ {
		title, brief := "Variation scenario "+strconv.Itoa(i), "Generate another "+artifactKind+" output with meaningfully different constraints or context."
		switch i {
		case 1:
			title, brief = "Primary scenario", "Generate a representative "+artifactKind+" output for the training request."
		case 2:
			title, brief = "Variation scenario", "Generate a second "+artifactKind+" output with meaningfully different constraints or context."
		}
		lines = append(lines,
			fmt.Sprintf("  - item_id: item-%03d", i),
			"    title: "+strconv.Quote(title),
			"    brief: "+strconv.Quote(brief),
			"    target_audience: \"Primary reviewer\"",
			"    output_type: "+strconv.Quote(artifactKind),
		)
	}
	lines = append(lines, "")
	return []byte(strings.Join(lines, "\n"))
}

func normalizeSkillOptTrainInitPreview(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none":
		return "none", nil
	case "text", "text-table":
		return "text-table", nil
	case "vue", "vue-vite":
		return "vue", nil
	default:
		return "", fmt.Errorf("preview %q is not supported; use none, text-table, or vue", value)
	}
}

func skillOptTrainInitDefaultOptions(mode string) int {
	if mode == db.EvalRunModeExplore {
		return skillopt.DefaultTrainInitConfig().Options
	}
	return effectiveSkillOptOptionsCount(mode, 0)
}

func relativeOrAbsolutePath(base string, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel) {
		return filepath.ToSlash(rel)
	}
	return path
}

func runSkillOptTrainInitTemplates(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train init templates", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "write template choices as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train init templates does not accept positional arguments")
		return 2
	}
	if !*jsonOutput {
		fmt.Fprintln(stderr, "skillopt train init templates requires --json")
		return 2
	}
	return withStoreExit(*home, stderr, "list skillopt train init templates", func(store *db.Store) error {
		choices, err := skillopt.ListTrainInitTemplateChoices(context.Background(), store)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(choices)
	})
}

func runSkillOptTrainStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	configPath := fs.String("config", "", "train init config.toml scaffold path")
	templateID := fs.String("template", "", "agent template id or version to train")
	repoFlag := fs.String("repo", "", "target repository in owner/repo form")
	sessionID := fs.String("session", "", "train session id")
	workspaceRepoFlag := fs.String("workspace-repo", "", "workspace repository in owner/repo form")
	previewRepoFlag := fs.String("preview-repo", "", "preview repository in owner/repo form")
	previewMode := fs.String("preview-mode", "", "preview mode: none, optional, or required")
	previewRenderer := fs.String("preview-renderer", "", "preview renderer: none or vue-vite")
	previewPublisher := fs.String("preview-publisher", "", "preview publisher: none or github-pages")
	previewRouteTemplate := fs.String("preview-route-template", "", "preview route template for published options")
	requestText := fs.String("request", "", "human training request")
	requestFile := fs.String("request-file", "", "file containing the human training request")
	taskKind := fs.String("task-kind", "custom", "task kind: correctness, ux, design, writing, data, or custom")
	mode := fs.String("mode", db.EvalRunModeExplore, "train mode: explore, refine, distill, or validate")
	explorationLevel := fs.String("exploration-level", "", "exploration level: high, medium, or low")
	optionsCount := fs.Int("options", 0, "expected number of review options")
	itemsFile := fs.String("items-file", "", "YAML or JSON file describing training review items")
	minItems := fs.Int("min-items", 2, "minimum number of training review items")
	preferredGate := fs.String("preferred-gate", "", "evaluation gate: hard, soft, or hard_then_soft")
	dryRun := fs.Bool("dry-run", false, "print inferred session state without writing")
	createRepos := fs.Bool("create-repos", false, "create the target and workspace repositories on GitHub if they do not exist")
	yes := fs.Bool("yes", false, "confirm creation without an interactive prompt")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train start does not accept positional arguments")
		return 2
	}
	setFlags := flagNamesSet(fs)
	var configDefaults skillOptTrainStartConfigDefaults
	if strings.TrimSpace(*configPath) != "" {
		var err error
		configDefaults, err = applySkillOptTrainStartConfig(*configPath, setFlags, templateID, repoFlag, taskKind, mode, explorationLevel, optionsCount, previewRepoFlag, previewMode, previewRenderer, previewPublisher, requestText, requestFile, itemsFile)
		if err != nil {
			fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
			return 2
		}
	}
	request, err := readSkillOptTrainRequest(*requestText, *requestFile)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*templateID) == "" || strings.TrimSpace(*repoFlag) == "" || strings.TrimSpace(request) == "" {
		fmt.Fprintln(stderr, "skillopt train start requires --template, --repo, and --request or --request-file")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	workspaceRepo, err := parseOptionalSkillOptTrainRepo("workspace-repo", *workspaceRepoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	if workspaceRepo == "" {
		fmt.Fprintln(stderr, "skillopt train start requires --workspace-repo owner/repo; without it the session stays at request_confirmed and train continue cannot reach option generation")
		return 2
	}
	if *createRepos {
		for _, fullName := range []string{repo.FullName(), workspaceRepo} {
			if err := ensureSkillOptTrainRepo(*home, fullName, "train", strings.TrimSpace(*sessionID), stdout); err != nil {
				fmt.Fprintf(stderr, "skillopt train start: create repo %s: %v\n", fullName, err)
				return 1
			}
		}
	}
	previewRepo, err := parseOptionalSkillOptTrainRepo("preview-repo", *previewRepoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	policy, err := skillopt.BuildTrainPreviewPolicy(repo.FullName(), previewRepo, *previewMode, *previewRenderer, *previewPublisher, *previewRouteTemplate)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	normalizedTaskKind, err := normalizeSkillOptTrainTaskKind(*taskKind)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	normalizedMode, normalizedExploration, err := normalizeSkillOptTrainMode(*mode, *explorationLevel)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	if *optionsCount < 0 || *optionsCount == 1 {
		fmt.Fprintln(stderr, "skillopt train start: --options must be zero or at least 2")
		return 2
	}
	effectiveOptionsCount := effectiveSkillOptOptionsCount(normalizedMode, *optionsCount)
	items, itemWarnings, err := readSkillOptTrainItems(*itemsFile)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	if *minItems < 2 {
		fmt.Fprintln(stderr, "skillopt train start: --min-items must be at least 2")
		return 2
	}
	if len(items) < *minItems {
		fmt.Fprintf(stderr, "skillopt train start: --items-file must contain at least %d items, got %d\n", *minItems, len(items))
		return 2
	}
	normalizedGate, err := normalizeSkillOptPreferredGate(*preferredGate, normalizedTaskKind)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 2
	}
	itemWarnings = append(itemWarnings, detectSkillOptTrainItemWarnings(items)...)
	itemWarnings = append(itemWarnings, detectSkillOptTrainPreviewWarnings(policy)...)
	var plan skillOptTrainStartPlan
	openStore := withStore
	if *dryRun || !*yes {
		openStore = withReadOnlyStore
	}
	if err := openStore(*home, func(store *db.Store) error {
		template, err := loadInstalledTemplate(context.Background(), store, *templateID)
		if err != nil {
			return err
		}
		plan = buildSkillOptTrainStartPlan(template, repo.FullName(), workspaceRepo, policy, strings.TrimSpace(*sessionID), request, normalizedTaskKind, normalizedMode, normalizedExploration, effectiveOptionsCount, normalizedGate, items, itemWarnings, configDefaults)
		if *dryRun {
			return nil
		}
		if !*yes {
			return nil
		}
		if _, err := store.GetSkillOptTrainSession(context.Background(), plan.Session.ID); err == nil {
			return fmt.Errorf("train session %s already exists; use a different --session or inspect it with gitmoot skillopt train status --session %s", plan.Session.ID, plan.Session.ID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := store.GetEvalRun(context.Background(), plan.EvalRun.ID); err == nil {
			return fmt.Errorf("eval run %s already exists; use a different --session or inspect it with gitmoot skillopt review status --run %s", plan.EvalRun.ID, plan.EvalRun.ID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := store.UpsertSkillOptTrainSession(context.Background(), plan.Session); err != nil {
			return err
		}
		if err := store.UpsertSkillOptTrainIteration(context.Background(), plan.Iteration); err != nil {
			return err
		}
		if err := store.UpsertEvalRun(context.Background(), plan.EvalRun); err != nil {
			return err
		}
		for _, item := range plan.Items {
			if err := store.UpsertEvalReviewItem(context.Background(), item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train start: %v\n", err)
		return 1
	}
	printSkillOptTrainStartPlan(stdout, plan)
	if *dryRun {
		writeLine(stdout, "dry_run: true")
		return 0
	}
	if !*yes {
		writeLine(stdout, "confirmation_required: true")
		writeLine(stdout, "confirm_command: %s", skillOptTrainConfirmCommand(args, plan.Session.ID))
		return 2
	}
	writeLine(stdout, "created train session %s", plan.Session.ID)
	return 0
}

// ensureSkillOptTrainRepo creates the given repo as a private GitHub repo when it
// does not already exist, recording it in created_repos so cleanup flows can
// offer deletion of exactly the repos gitmoot created. Deduplicated callers pass
// distinct names; an empty name is a no-op. An ambiguous existence check (e.g.
// auth error) is left alone so the later Preflight surfaces it, rather than
// wrongly attempting a create.
func ensureSkillOptTrainRepo(home, fullName, purpose, sessionID string, stdout io.Writer) error {
	if strings.TrimSpace(fullName) == "" {
		return nil
	}
	repo, err := daemon.ParseRepository(fullName)
	if err != nil {
		return err
	}
	client := newSkillOptGitHubClient()
	exists, err := client.RepositoryExists(context.Background(), repo)
	if err != nil {
		// Ambiguous (auth/network); do not attempt a create, let Preflight report.
		return nil
	}
	if exists {
		return nil
	}
	if err := client.CreateRepository(context.Background(), repo, true); err != nil {
		return err
	}
	if err := withStore(home, func(store *db.Store) error {
		return store.RecordCreatedRepo(context.Background(), db.CreatedRepo{Repo: fullName, Purpose: purpose, SessionID: strings.TrimSpace(sessionID)})
	}); err != nil {
		// The repo exists either way; a failed record only loses the cleanup offer.
		fmt.Fprintf(stdout, "warning: could not record created repo %s: %v\n", fullName, err)
	}
	writeLine(stdout, "created_repo: %s", fullName)
	return nil
}

func flagNamesSet(fs *flag.FlagSet) map[string]struct{} {
	set := map[string]struct{}{}
	fs.Visit(func(f *flag.Flag) {
		set[f.Name] = struct{}{}
	})
	return set
}

type skillOptTrainStartConfigDefaults struct {
	Optimizer skillOptTrainOptimizerRequest
}

func applySkillOptTrainStartConfig(configPath string, setFlags map[string]struct{}, templateID *string, repoFlag *string, taskKind *string, mode *string, explorationLevel *string, optionsCount *int, previewRepoFlag *string, previewMode *string, previewRenderer *string, previewPublisher *string, requestText *string, requestFile *string, itemsFile *string) (skillOptTrainStartConfigDefaults, error) {
	configPath = strings.TrimSpace(configPath)
	config, err := skillopt.LoadTrainInitConfig(configPath)
	if err != nil {
		return skillOptTrainStartConfigDefaults{}, fmt.Errorf("load config: %w", err)
	}
	scaffoldDir := filepath.Dir(configPath)
	if !flagWasSet(setFlags, "template") {
		if strings.TrimSpace(config.TemplateVersion) != "" {
			*templateID = config.TemplateVersion
		} else {
			*templateID = config.Template
		}
	}
	if !flagWasSet(setFlags, "repo") {
		*repoFlag = config.ReviewRepo
	}
	if !flagWasSet(setFlags, "task-kind") {
		*taskKind = config.TaskKind
	}
	if !flagWasSet(setFlags, "mode") {
		*mode = config.Mode
	}
	if !flagWasSet(setFlags, "exploration-level") {
		*explorationLevel = config.ExplorationLevel
	}
	if !flagWasSet(setFlags, "options") {
		*optionsCount = config.Options
	}
	if err := applySkillOptTrainStartPreviewConfig(config, setFlags, *repoFlag, previewRepoFlag, previewMode, previewRenderer, previewPublisher); err != nil {
		return skillOptTrainStartConfigDefaults{}, err
	}
	if !flagWasSet(setFlags, "request") && !flagWasSet(setFlags, "request-file") {
		taskPath := filepath.Join(scaffoldDir, skillopt.TrainInitTaskFileName)
		content, err := os.ReadFile(taskPath)
		if err != nil {
			return skillOptTrainStartConfigDefaults{}, fmt.Errorf("read %s: %w", taskPath, err)
		}
		*requestText = strings.TrimSpace(string(content))
		*requestFile = ""
	}
	if !flagWasSet(setFlags, "items-file") {
		defaultItemsPath := filepath.Join(scaffoldDir, skillopt.TrainInitReviewItemsName)
		if _, err := os.Stat(defaultItemsPath); err == nil {
			*itemsFile = defaultItemsPath
		} else if !errors.Is(err, os.ErrNotExist) {
			return skillOptTrainStartConfigDefaults{}, fmt.Errorf("inspect %s: %w", defaultItemsPath, err)
		}
	}
	return skillOptTrainStartConfigDefaults{Optimizer: skillOptTrainOptimizerDefaultsFromInitConfig(config)}, nil
}

func applySkillOptTrainStartPreviewConfig(config skillopt.TrainInitConfig, setFlags map[string]struct{}, effectiveReviewRepo string, previewRepoFlag *string, previewMode *string, previewRenderer *string, previewPublisher *string) error {
	preview, err := normalizeSkillOptTrainInitPreview(config.Preview)
	if err != nil {
		return err
	}
	defaultPreviewRepo := firstNonEmpty(strings.TrimSpace(effectiveReviewRepo), config.ReviewRepo)
	if flagWasSet(setFlags, "preview-mode") {
		switch strings.TrimSpace(strings.ToLower(*previewMode)) {
		case skillopt.TrainPreviewModeNone:
			if !flagWasSet(setFlags, "preview-renderer") {
				*previewRenderer = skillopt.TrainPreviewRendererNone
			}
			if !flagWasSet(setFlags, "preview-publisher") {
				*previewPublisher = skillopt.TrainPreviewPublisherNone
			}
		case skillopt.TrainPreviewModeRequired:
			if !flagWasSet(setFlags, "preview-renderer") {
				*previewRenderer = skillopt.TrainPreviewRendererVueVite
			}
			if !flagWasSet(setFlags, "preview-publisher") {
				*previewPublisher = skillopt.TrainPreviewPublisherGitHubPages
			}
			if !flagWasSet(setFlags, "preview-repo") {
				*previewRepoFlag = defaultPreviewRepo
			}
		case skillopt.TrainPreviewModeOptional:
			if preview == "vue" {
				if !flagWasSet(setFlags, "preview-renderer") {
					*previewRenderer = skillopt.TrainPreviewRendererVueVite
				}
				if !flagWasSet(setFlags, "preview-publisher") {
					*previewPublisher = skillopt.TrainPreviewPublisherGitHubPages
				}
				if !flagWasSet(setFlags, "preview-repo") {
					*previewRepoFlag = defaultPreviewRepo
				}
			}
		}
		return nil
	}
	switch preview {
	case "none", "text-table":
		if skillOptTrainPreviewOverrideFlagWasSet(setFlags) {
			return nil
		}
		*previewMode = skillopt.TrainPreviewModeNone
		if !flagWasSet(setFlags, "preview-renderer") {
			*previewRenderer = skillopt.TrainPreviewRendererNone
		}
		if !flagWasSet(setFlags, "preview-publisher") {
			*previewPublisher = skillopt.TrainPreviewPublisherNone
		}
	case "vue":
		*previewMode = skillopt.TrainPreviewModeRequired
		if !flagWasSet(setFlags, "preview-renderer") {
			*previewRenderer = skillopt.TrainPreviewRendererVueVite
		}
		if !flagWasSet(setFlags, "preview-publisher") {
			*previewPublisher = skillopt.TrainPreviewPublisherGitHubPages
		}
		if !flagWasSet(setFlags, "preview-repo") {
			*previewRepoFlag = defaultPreviewRepo
		}
	}
	return nil
}

func skillOptTrainPreviewOverrideFlagWasSet(setFlags map[string]struct{}) bool {
	for _, name := range []string{"preview-repo", "preview-renderer", "preview-publisher", "preview-route-template"} {
		if flagWasSet(setFlags, name) {
			return true
		}
	}
	return false
}

func flagWasSet(setFlags map[string]struct{}, name string) bool {
	_, ok := setFlags[name]
	return ok
}

func skillOptTrainOptimizerDefaultsFromInitConfig(config skillopt.TrainInitConfig) skillOptTrainOptimizerRequest {
	request := skillOptTrainOptimizerRequest{
		SkillUpdateMode:              config.Optimizer.SkillUpdateMode,
		OptimizerViews:               config.Optimizer.OptimizerViews,
		OptimizerViewsSet:            config.Optimizer.OptimizerViews > 0,
		RetryOptimizerViews:          config.Optimizer.RetryOptimizerViews,
		RetryOptimizerViewsSet:       strings.TrimSpace(config.Optimizer.RetryOptimizerViews) != "",
		NoopRetryBudget:              trainInitConfigInt(config.Optimizer.NoopRetryBudget),
		NoopRetryBudgetSet:           config.Optimizer.NoopRetryBudget != nil,
		GateRejectRetryBudget:        trainInitConfigInt(config.Optimizer.GateRejectRetryBudget),
		GateRejectRetryBudgetSet:     config.Optimizer.GateRejectRetryBudget != nil,
		WrongArtifactRetryBudget:     trainInitConfigInt(config.Optimizer.WrongArtifactRetryBudget),
		WrongArtifactRetryBudgetSet:  config.Optimizer.WrongArtifactRetryBudget != nil,
		TargetArtifactRetryBudget:    trainInitConfigInt(config.Optimizer.TargetArtifactRetryBudget),
		TargetArtifactRetryBudgetSet: config.Optimizer.TargetArtifactRetryBudget != nil,
		HardFailureRetryBudget:       trainInitConfigInt(config.Optimizer.HardFailureRetryBudget),
		HardFailureRetryBudgetSet:    config.Optimizer.HardFailureRetryBudget != nil,
		FinalEval:                    config.FinalEvaluatorEnabled,
	}
	optimizerBackend := strings.TrimSpace(config.Optimizer.OptimizerBackend)
	targetBackend := strings.TrimSpace(config.Optimizer.TargetBackend)
	evaluatorBackend := strings.TrimSpace(config.Optimizer.EvaluatorBackend)
	internalTargetAdapter := strings.TrimSpace(config.Optimizer.InternalTargetAdapter)
	if strings.EqualFold(optimizerBackend, "codex") && strings.EqualFold(targetBackend, "codex") && strings.EqualFold(evaluatorBackend, "codex") && strings.EqualFold(internalTargetAdapter, "codex_exec") {
		request.Backend = "codex"
	} else {
		request.OptimizerBackend = optimizerBackend
		request.TargetBackend = skillOptTrainTargetBackendFromInitConfig(targetBackend, internalTargetAdapter)
		request.EvaluatorBackend = evaluatorBackend
	}
	if value := strings.TrimSpace(config.Optimizer.OptimizerModel); value != "" {
		request.OptimizerModel = value
	}
	if value := strings.TrimSpace(config.Optimizer.TargetModel); value != "" {
		request.TargetModel = value
	}
	return request
}

func skillOptTrainTargetBackendFromInitConfig(targetBackend string, internalTargetAdapter string) string {
	targetBackend = strings.TrimSpace(targetBackend)
	internalTargetAdapter = strings.TrimSpace(internalTargetAdapter)
	if strings.EqualFold(targetBackend, "codex") && strings.EqualFold(internalTargetAdapter, "codex_exec") {
		return "codex_exec"
	}
	return firstNonEmpty(targetBackend, internalTargetAdapter)
}

func trainInitConfigInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

type skillOptTrainStartPlan struct {
	Session   db.SkillOptTrainSession
	Iteration db.SkillOptTrainIteration
	EvalRun   db.EvalRun
	Items     []db.EvalReviewItem
	Warnings  []string
	Summary   skillopt.TrainStatusSummary
}

func buildSkillOptTrainStartPlan(template db.AgentTemplate, repo string, workspaceRepo string, previewPolicy skillopt.TrainPreviewPolicy, sessionID string, request string, taskKind string, mode string, explorationLevel string, optionsCount int, preferredGate string, itemPlans []skillOptTrainItemPlan, warnings []string, configDefaults skillOptTrainStartConfigDefaults) skillOptTrainStartPlan {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = generatedSkillOptTrainSessionID(template.ID)
	}
	state := skillopt.TrainStateRequestConfirmed
	if workspaceRepo != "" {
		state = skillopt.TrainStateItemsReady
	}
	metadata := skillOptTrainStartMetadata(request, mode, explorationLevel, optionsCount, preferredGate, itemPlans, warnings, previewPolicy, configDefaults, skillOptTemplateJudgeEvaluation(template))
	session := db.SkillOptTrainSession{
		ID:                sessionID,
		TemplateID:        template.ID,
		TemplateVersionID: template.VersionID,
		TargetRepo:        repo,
		WorkspaceRepo:     workspaceRepo,
		PreviewRepo:       previewPolicy.Repo,
		RequestSummary:    firstLine(request),
		TaskKind:          taskKind,
		State:             state,
		MetadataJSON:      metadata,
	}
	iteration := db.SkillOptTrainIteration{
		ID:                    sessionID + "-001",
		SessionID:             sessionID,
		EvalRunID:             sessionID + "-review-001",
		BaseTemplateVersionID: template.VersionID,
		Mode:                  mode,
		ExplorationLevel:      explorationLevel,
		State:                 state,
		MetadataJSON:          metadata,
	}
	run := db.EvalRun{
		ID:                iteration.EvalRunID,
		TemplateID:        template.ID,
		TemplateVersionID: template.VersionID,
		TargetRepo:        repo,
		State:             "review",
		Mode:              mode,
		ExplorationLevel:  explorationLevel,
		OptionsCount:      optionsCount,
		MetadataJSON:      metadata,
	}
	items := make([]db.EvalReviewItem, 0, len(itemPlans))
	for _, item := range itemPlans {
		items = append(items, db.EvalReviewItem{
			RunID:        run.ID,
			ItemID:       item.ItemID,
			Title:        item.Title,
			MetadataJSON: skillOptTrainItemMetadata(item),
		})
	}
	summary := skillopt.BuildTrainStatusSummary(session, &iteration, skillopt.TrainStatusCounts{})
	return skillOptTrainStartPlan{Session: session, Iteration: iteration, EvalRun: run, Items: items, Warnings: warnings, Summary: summary}
}

func printSkillOptTrainStartPlan(stdout io.Writer, plan skillOptTrainStartPlan) {
	writeLine(stdout, "session: %s", plan.Session.ID)
	writeLine(stdout, "template: %s", plan.Session.TemplateID)
	writeLine(stdout, "template_version: %s", plan.Session.TemplateVersionID)
	writeLine(stdout, "repo: %s", plan.Session.TargetRepo)
	writeLine(stdout, "workspace_repo: %s", emptyText(plan.Session.WorkspaceRepo))
	writeLine(stdout, "preview_repo: %s", emptyText(plan.Session.PreviewRepo))
	writeLine(stdout, "preview_mode: %s", plan.Summary.PreviewPolicy.Mode)
	writeLine(stdout, "preview_renderer: %s", plan.Summary.PreviewPolicy.Renderer)
	writeLine(stdout, "preview_publisher: %s", plan.Summary.PreviewPolicy.Publisher)
	writeLine(stdout, "preview_route_template: %s", emptyText(plan.Summary.PreviewPolicy.RouteTemplate))
	writeLine(stdout, "expected_review_repo: %s", emptyText(plan.Summary.PreviewPolicy.ExpectedReviewRepo))
	writeLine(stdout, "task_kind: %s", plan.Session.TaskKind)
	writeLine(stdout, "request_summary: %s", plan.Session.RequestSummary)
	writeLine(stdout, "iteration: %s", plan.Iteration.ID)
	writeLine(stdout, "eval_run: %s", plan.Iteration.EvalRunID)
	writeLine(stdout, "mode: %s", plan.Iteration.Mode)
	writeLine(stdout, "exploration_level: %s", plan.Iteration.ExplorationLevel)
	writeLine(stdout, "preferred_gate: %s", skillOptMetadataString(plan.EvalRun.MetadataJSON, "evaluation", "preferred_gate"))
	writeLine(stdout, "items: %d", len(plan.Items))
	for _, warning := range plan.Warnings {
		writeLine(stdout, "warning: %s", warning)
	}
	writeLine(stdout, "current_phase: %s", plan.Summary.CurrentPhase)
	writeLine(stdout, "blocked_step: %s", plan.Summary.BlockedStep)
	writeLine(stdout, "next_action: %s", plan.Summary.NextAction)
}

type skillOptTrainStatusSnapshot struct {
	SessionID          string                         `json:"session_id"`
	IterationID        string                         `json:"iteration_id,omitempty"`
	TemplateID         string                         `json:"template_id,omitempty"`
	TemplateVersion    string                         `json:"template_version,omitempty"`
	TargetRepo         string                         `json:"target_repo,omitempty"`
	WorkspaceRepo      string                         `json:"workspace_repo,omitempty"`
	TaskKind           string                         `json:"task_kind,omitempty"`
	StatusPhase        string                         `json:"status_phase"`
	CurrentPhase       string                         `json:"current_phase"`
	CurrentStep        string                         `json:"current_step"`
	CompletedSteps     []string                       `json:"completed_steps"`
	BlockedStep        string                         `json:"blocked_step,omitempty"`
	NextAction         string                         `json:"next_action"`
	IssueURL           string                         `json:"issue_url,omitempty"`
	PullRequestURL     string                         `json:"pull_request_url,omitempty"`
	ContinueFromGitHub string                         `json:"continue_from_github,omitempty"`
	CandidateVersion   string                         `json:"candidate_version,omitempty"`
	RecoveryAvailable  bool                           `json:"recovery_available"`
	NoCandidateReason  string                         `json:"no_candidate_reason,omitempty"`
	NoCandidateDetails map[string]any                 `json:"no_candidate_details,omitempty"`
	PreviewPolicy      skillOptTrainPreviewPolicyJSON `json:"preview_policy"`
	Counts             skillOptTrainStatusCountsJSON  `json:"counts"`
	Progress           skillOptTrainStatusProgress    `json:"progress"`
	Verbose            *skillOptTrainStatusVerbose    `json:"verbose,omitempty"`
}

type skillOptTrainPreviewPolicyJSON struct {
	Mode               string `json:"mode"`
	Renderer           string `json:"renderer"`
	Publisher          string `json:"publisher"`
	Repo               string `json:"repo,omitempty"`
	RouteTemplate      string `json:"route_template,omitempty"`
	ExpectedReviewRepo string `json:"expected_review_repo,omitempty"`
}

type skillOptTrainStatusCountsJSON struct {
	ReviewItems          int `json:"review_items"`
	FeedbackEvents       int `json:"feedback_events"`
	RankedFeedbackEvents int `json:"ranked_feedback_events"`
	PairwisePreferences  int `json:"pairwise_preferences"`
}

type skillOptTrainStatusProgress struct {
	ReviewItems          int    `json:"review_items"`
	FeedbackEvents       int    `json:"feedback_events"`
	RankedFeedbackEvents int    `json:"ranked_feedback_events"`
	PairwisePreferences  int    `json:"pairwise_preferences"`
	GeneratedOptions     int    `json:"generated_options"`
	ETA                  string `json:"eta"`
}

type skillOptTrainStatusVerbose struct {
	EvalRunID             string                         `json:"eval_run_id,omitempty"`
	BaseTemplateVersionID string                         `json:"base_template_version_id,omitempty"`
	Mode                  string                         `json:"mode,omitempty"`
	ExplorationLevel      string                         `json:"exploration_level,omitempty"`
	CreatedAt             string                         `json:"created_at,omitempty"`
	UpdatedAt             string                         `json:"updated_at,omitempty"`
	Elapsed               string                         `json:"elapsed"`
	ReviewIssue           skillOptTrainStatusReviewIssue `json:"review_issue,omitempty"`
	Candidate             skillOptTrainStatusCandidate   `json:"candidate,omitempty"`
	Optimizer             map[string]any                 `json:"optimizer,omitempty"`
	Generation            map[string]any                 `json:"generation,omitempty"`
	Jobs                  skillOptTrainStatusJobs        `json:"jobs"`
	ActiveLocks           []skillOptTrainStatusLock      `json:"active_locks,omitempty"`
	Items                 []skillOptTrainStatusItem      `json:"items,omitempty"`
	MetadataStatus        map[string]string              `json:"metadata_status,omitempty"`
}

type skillOptTrainStatusReviewIssue struct {
	Repo   string `json:"repo,omitempty"`
	Number int64  `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
}

type skillOptTrainStatusCandidate struct {
	VersionID          string         `json:"version_id,omitempty"`
	PullRequestURL     string         `json:"pull_request_url,omitempty"`
	NoCandidateReason  string         `json:"no_candidate_reason,omitempty"`
	NoCandidateDetails map[string]any `json:"no_candidate_details,omitempty"`
}

type skillOptTrainStatusJobs struct {
	Total     int                         `json:"total"`
	Queued    int                         `json:"queued"`
	Running   int                         `json:"running"`
	Succeeded int                         `json:"succeeded"`
	Failed    int                         `json:"failed"`
	Other     int                         `json:"other"`
	Items     []skillOptTrainStatusJobRef `json:"items,omitempty"`
}

type skillOptTrainStatusJobRef struct {
	ID    string `json:"id"`
	Agent string `json:"agent,omitempty"`
	Type  string `json:"type,omitempty"`
	State string `json:"state"`
}

type skillOptTrainStatusLock struct {
	Name          string `json:"name"`
	Key           string `json:"key"`
	Status        string `json:"status,omitempty"`
	OwnerJobID    string `json:"owner_job_id,omitempty"`
	OwnerPID      int64  `json:"owner_pid,omitempty"`
	OwnerHostname string `json:"owner_hostname,omitempty"`
	CommandHash   string `json:"command_hash,omitempty"`
	AcquiredAt    string `json:"acquired_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Elapsed       string `json:"elapsed,omitempty"`
}

type skillOptTrainStatusItem struct {
	ItemID       string   `json:"item_id"`
	Title        string   `json:"title,omitempty"`
	OptionLabels []string `json:"option_labels,omitempty"`
}

func runSkillOptTrainStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	jsonOutput := fs.Bool("json", false, "print status as JSON")
	verbose := fs.Bool("verbose", false, "include detailed progress and metadata")
	watch := fs.Bool("watch", false, "refresh status until the session reaches a waiting or terminal phase")
	poll := fs.Duration("poll", 2*time.Second, "watch poll interval")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train status does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(stderr, "skillopt train status requires --session")
		return 2
	}
	if *poll <= 0 {
		fmt.Fprintln(stderr, "skillopt train status poll interval must be positive")
		return 2
	}
	if *watch && *jsonOutput {
		fmt.Fprintln(stderr, "skillopt train status does not support --watch with --json; use --watch for text refreshes or --json without --watch")
		return 2
	}
	var snapshot skillOptTrainStatusSnapshot
	if err := withStore(*home, func(store *db.Store) error {
		for {
			loaded, err := loadSkillOptTrainStatusSnapshot(context.Background(), store, *sessionID, *verbose || *watch)
			if err != nil {
				return err
			}
			snapshot = loaded
			outputSnapshot := skillOptTrainStatusOutputSnapshot(snapshot, *verbose)
			if !*watch {
				return nil
			}
			if *jsonOutput {
				if err := writeJSON(stdout, outputSnapshot); err != nil {
					return err
				}
			} else {
				printSkillOptTrainStatusSnapshot(stdout, outputSnapshot, *verbose)
				writeLine(stdout, "watch_state: %s", skillOptTrainWatchState(snapshot))
			}
			if skillOptTrainWatchDone(snapshot) {
				return nil
			}
			time.Sleep(*poll)
		}
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train status: %v\n", err)
		return 1
	}
	if *watch {
		return 0
	}
	if *jsonOutput {
		if err := writeJSON(stdout, skillOptTrainStatusOutputSnapshot(snapshot, *verbose)); err != nil {
			fmt.Fprintf(stderr, "skillopt train status: %v\n", err)
			return 1
		}
		return 0
	}
	printSkillOptTrainStatusSnapshot(stdout, snapshot, *verbose)
	return 0
}

func skillOptTrainStatusOutputSnapshot(snapshot skillOptTrainStatusSnapshot, verbose bool) skillOptTrainStatusSnapshot {
	if verbose {
		return snapshot
	}
	snapshot.Verbose = nil
	return snapshot
}

func runSkillOptTrainRecover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train recover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	outRoot := fs.String("out-root", "", "optimizer output directory; defaults to the persisted train optimizer path")
	jsonOutput := fs.Bool("json", false, "print recovery result as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train recover does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(stderr, "skillopt train recover requires --session")
		return 2
	}
	var result skillOptTrainRecoverResult
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var recoverErr error
		result, recoverErr = recoverSkillOptTrainOptimizerArtifacts(context.Background(), paths, store, *sessionID, *outRoot)
		return recoverErr
	}); err != nil {
		if result.SessionID != "" {
			if *jsonOutput {
				_ = writeJSON(stdout, result)
			} else {
				printSkillOptTrainRecoverResult(stdout, result)
			}
		}
		fmt.Fprintf(stderr, "skillopt train recover: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "skillopt train recover: %v\n", err)
			return 1
		}
		return 0
	}
	printSkillOptTrainRecoverResult(stdout, result)
	return 0
}

func runSkillOptTrainContinue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train continue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	generatorAgent := fs.String("generator-agent", "", "existing agent to use for option generation")
	generatorType := fs.String("generator-type", "", "managed agent type to use for option generation; overrides the default current-skill generator")
	skillOptBin := fs.String("skillopt-bin", "", "gitmoot-skillopt executable path; defaults to gitmoot-skillopt on PATH")
	backend := fs.String("backend", "", "backend preset for optimizer, target, and evaluator; currently supports codex")
	model := fs.String("model", "", "model name to pass to both optimizer and target when specific model flags are omitted")
	optimizerModel := fs.String("optimizer-model", "", "optimizer model name")
	targetModel := fs.String("target-model", "", "target model name")
	optimizerBackend := fs.String("optimizer-backend", "", "optimizer backend")
	targetBackend := fs.String("target-backend", "", "target backend")
	evaluatorID := fs.String("evaluator-id", "", "evaluator id, such as landing_page_v1")
	evaluatorModel := fs.String("evaluator-model", "", "evaluator model name")
	evaluatorBackend := fs.String("evaluator-backend", "", "evaluator backend")
	skillUpdateMode := fs.String("skill-update-mode", "", "SkillOpt update mode")
	numEpochs := fs.Int("num-epochs", 0, "optimizer epoch count")
	batchSize := fs.Int("batch-size", 0, "optimizer batch size")
	optimizerViews := fs.Int("optimizer-views", 0, "independent optimizer perspectives over imported human review feedback; omit to use gitmoot-skillopt default")
	retryOptimizerViews := fs.String("retry-optimizer-views", "", "optimizer perspectives for gate-reject retries: auto, inherit, or a positive integer; omit to use gitmoot-skillopt default")
	noopRetryBudget := fs.Int("noop-retry-budget", -1, "noop optimizer retry budget; omit to use gitmoot-skillopt default")
	gateRejectRetryBudget := fs.Int("gate-reject-retry-budget", -1, "gate-rejection optimizer retry budget; omit to use gitmoot-skillopt default")
	wrongArtifactRetryBudget := fs.Int("wrong-artifact-retry-budget", -1, "wrong-artifact optimizer retry budget; omit to use gitmoot-skillopt default")
	targetArtifactRetryBudget := fs.Int("target-artifact-retry-budget", -1, "target artifact repair retry budget; omit to use gitmoot-skillopt default")
	hardFailureRetryBudget := fs.Int("hard-failure-retry-budget", -1, "hard-failure reflection retry budget; omit to use gitmoot-skillopt default")
	feedbackDirectMode := fs.String("feedback-direct-mode", "", "feedback-direct optimizer mode: auto, on, or off")
	finalEval := fs.Bool("final-eval", false, "run gitmoot-skillopt final test evaluation after selection; disabled by default")
	gate := fs.String("gate", "", "optimizer gate metric: hard, soft, or mixed")
	outRoot := fs.String("out-root", "", "optimizer output directory")
	timeout := fs.String("timeout", "", "optimizer timeout duration")
	dryRun := fs.Bool("dry-run", false, "ask gitmoot-skillopt to avoid model calls while still producing a candidate package")
	rerunOptimizer := fs.Bool("rerun-optimizer", false, "rerun gitmoot-skillopt after optimizer completion instead of retrying the existing candidate import")
	exportOnly := fs.Bool("export-only", false, "export the training package and stop before launching the optimizer")
	promote := fs.String("promote", "", "candidate version to promote after candidate review")
	reject := fs.String("reject", "", "candidate version to reject after candidate review")
	reason := fs.String("reason", "", "decision reason required with --reject")
	startNext := fs.Bool("start-next", false, "start the next train iteration after a promote or reject decision")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	setFlags := map[string]bool{}
	fs.Visit(func(flag *flag.Flag) {
		setFlags[flag.Name] = true
	})
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train continue does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(stderr, "skillopt train continue requires --session")
		return 2
	}
	if *exportOnly {
		for _, conflicting := range []string{"rerun-optimizer", "dry-run", "promote", "reject", "start-next"} {
			if setFlags[conflicting] {
				fmt.Fprintf(stderr, "skillopt train continue: --export-only cannot be combined with --%s\n", conflicting)
				return 2
			}
		}
	}
	if *numEpochs < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --num-epochs must be zero or greater")
		return 2
	}
	if *batchSize < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --batch-size must be zero or greater")
		return 2
	}
	if setFlags["optimizer-views"] && *optimizerViews <= 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --optimizer-views must be greater than zero")
		return 2
	}
	normalizedRetryOptimizerViews := ""
	if setFlags["retry-optimizer-views"] {
		var err error
		normalizedRetryOptimizerViews, err = normalizeSkillOptRetryOptimizerViews(*retryOptimizerViews)
		if err != nil {
			fmt.Fprintf(stderr, "skillopt train continue: %v\n", err)
			return 2
		}
		if setFlags["optimizer-views"] {
			if retryViews, ok := parseSkillOptRetryOptimizerViewsNumber(normalizedRetryOptimizerViews); ok && retryViews > *optimizerViews {
				fmt.Fprintln(stderr, "skillopt train continue: --retry-optimizer-views cannot exceed --optimizer-views")
				return 2
			}
		}
	}
	if setFlags["noop-retry-budget"] && *noopRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --noop-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["gate-reject-retry-budget"] && *gateRejectRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --gate-reject-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["wrong-artifact-retry-budget"] && *wrongArtifactRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --wrong-artifact-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["target-artifact-retry-budget"] && *targetArtifactRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --target-artifact-retry-budget must be zero or greater")
		return 2
	}
	if setFlags["hard-failure-retry-budget"] && *hardFailureRetryBudget < 0 {
		fmt.Fprintln(stderr, "skillopt train continue: --hard-failure-retry-budget must be zero or greater")
		return 2
	}
	if mode := strings.TrimSpace(strings.ToLower(*feedbackDirectMode)); mode != "" && mode != "auto" && mode != "on" && mode != "off" {
		fmt.Fprintln(stderr, "skillopt train continue: --feedback-direct-mode must be auto, on, or off")
		return 2
	}
	var output skillOptTrainContinueOutput
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		output, err = continueSkillOptTrain(context.Background(), paths, store, skillOptTrainContinueRequest{
			Home:           *home,
			SessionID:      *sessionID,
			GeneratorAgent: *generatorAgent,
			GeneratorType:  *generatorType,
			Optimizer: skillOptTrainOptimizerRequest{
				SkillOptBin:                  *skillOptBin,
				Backend:                      *backend,
				Model:                        *model,
				OptimizerModel:               *optimizerModel,
				TargetModel:                  *targetModel,
				OptimizerBackend:             *optimizerBackend,
				TargetBackend:                *targetBackend,
				EvaluatorID:                  *evaluatorID,
				EvaluatorModel:               *evaluatorModel,
				EvaluatorBackend:             *evaluatorBackend,
				SkillUpdateMode:              *skillUpdateMode,
				NumEpochs:                    *numEpochs,
				BatchSize:                    *batchSize,
				OptimizerViews:               *optimizerViews,
				OptimizerViewsSet:            setFlags["optimizer-views"],
				RetryOptimizerViews:          normalizedRetryOptimizerViews,
				RetryOptimizerViewsSet:       setFlags["retry-optimizer-views"],
				NoopRetryBudget:              *noopRetryBudget,
				NoopRetryBudgetSet:           setFlags["noop-retry-budget"],
				GateRejectRetryBudget:        *gateRejectRetryBudget,
				GateRejectRetryBudgetSet:     setFlags["gate-reject-retry-budget"],
				WrongArtifactRetryBudget:     *wrongArtifactRetryBudget,
				WrongArtifactRetryBudgetSet:  setFlags["wrong-artifact-retry-budget"],
				TargetArtifactRetryBudget:    *targetArtifactRetryBudget,
				TargetArtifactRetryBudgetSet: setFlags["target-artifact-retry-budget"],
				HardFailureRetryBudget:       *hardFailureRetryBudget,
				HardFailureRetryBudgetSet:    setFlags["hard-failure-retry-budget"],
				FeedbackDirectMode:           strings.TrimSpace(strings.ToLower(*feedbackDirectMode)),
				FinalEval:                    *finalEval,
				FinalEvalSet:                 setFlags["final-eval"],
				Gate:                         *gate,
				OutRoot:                      *outRoot,
				Timeout:                      *timeout,
				DryRun:                       *dryRun,
				RerunOptimizer:               *rerunOptimizer,
				ExportOnly:                   *exportOnly,
			},
			Progress:         stderr,
			PromoteCandidate: *promote,
			RejectCandidate:  *reject,
			DecisionReason:   *reason,
			StartNext:        *startNext,
		})
		return err
	}); err != nil {
		if output.Summary.CurrentPhase != "" || len(output.Lines) > 0 {
			printSkillOptTrainContinueOutput(stdout, output)
		}
		fmt.Fprintf(stderr, "skillopt train continue: %v\n", err)
		return 1
	}
	printSkillOptTrainContinueOutput(stdout, output)
	if output.Summary.CurrentPhase == skillopt.TrainStateRunAbandoned {
		fmt.Fprintln(stderr, "skillopt train continue: train session is abandoned")
		return 1
	}
	return 0
}

func normalizeSkillOptRetryOptimizerViews(value string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "auto" || trimmed == "inherit" {
		return trimmed, nil
	}
	if parsed, err := strconv.Atoi(trimmed); err == nil && parsed > 0 {
		return strconv.Itoa(parsed), nil
	}
	return "", fmt.Errorf("--retry-optimizer-views must be auto, inherit, or a positive integer")
}

func parseSkillOptRetryOptimizerViewsNumber(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func printSkillOptTrainContinueOutput(stdout io.Writer, output skillOptTrainContinueOutput) {
	if output.Summary.CurrentPhase != "" {
		printSkillOptTrainStatus(stdout, output.Summary, output.Counts)
	}
	writeLine(stdout, "continue_ready: %t", output.ContinueReady)
	for _, line := range output.Lines {
		writeLine(stdout, "%s", line)
	}
}

type skillOptTrainContinueRequest struct {
	Home              string
	SessionID         string
	GeneratorAgent    string
	GeneratorType     string
	GenerationLockTTL time.Duration
	Optimizer         skillOptTrainOptimizerRequest
	PromoteCandidate  string
	RejectCandidate   string
	DecisionReason    string
	StartNext         bool
	// Progress receives human-facing notices emitted while a continue step runs,
	// such as announcing a long-lived optimizer launch. It is nil for automated
	// callers (the review watcher) that have no attached terminal.
	Progress io.Writer
	// GenerationLockExtend, when set, is called after each generated option to
	// push the generation lock TTL forward so long runs do not outlive it.
	GenerationLockExtend func() error
}

type skillOptTrainOptimizerRequest struct {
	SkillOptBin                  string
	Backend                      string
	Model                        string
	OptimizerModel               string
	TargetModel                  string
	OptimizerBackend             string
	TargetBackend                string
	EvaluatorID                  string
	EvaluatorModel               string
	EvaluatorBackend             string
	SkillUpdateMode              string
	NumEpochs                    int
	BatchSize                    int
	OptimizerViews               int
	OptimizerViewsSet            bool
	RetryOptimizerViews          string
	RetryOptimizerViewsSet       bool
	NoopRetryBudget              int
	NoopRetryBudgetSet           bool
	GateRejectRetryBudget        int
	GateRejectRetryBudgetSet     bool
	WrongArtifactRetryBudget     int
	WrongArtifactRetryBudgetSet  bool
	TargetArtifactRetryBudget    int
	TargetArtifactRetryBudgetSet bool
	HardFailureRetryBudget       int
	HardFailureRetryBudgetSet    bool
	FeedbackDirectMode           string
	FinalEval                    bool
	FinalEvalSet                 bool
	Gate                         string
	OutRoot                      string
	Timeout                      string
	DryRun                       bool
	RerunOptimizer               bool
	ExportOnly                   bool
	OptimizerLockState           string
}

const skillOptTrainSkillOptWheelURL = "https://github.com/jerryfane/gitmoot-skillopt/releases/download/v0.3.1/gitmoot_skillopt-0.3.1-py3-none-any.whl"

type skillOptTrainOptimizerPreflightError struct {
	Executable string
	Step       string
	Result     subprocess.Result
	Err        error
}

func (e skillOptTrainOptimizerPreflightError) Error() string {
	executable := strings.TrimSpace(e.Executable)
	if executable == "" {
		executable = "gitmoot-skillopt"
	}
	step := strings.TrimSpace(e.Step)
	if step == "" {
		step = "preflight"
	}
	details := ""
	if e.Err != nil {
		details = ": " + e.Err.Error()
	}
	if diag := subprocessDiagnostics(e.Result); diag != "" {
		details += diag
	}
	return fmt.Sprintf(
		"gitmoot-skillopt is required for optimizer-backed train continue; %s failed for %q%s\n\n%s",
		step,
		executable,
		details,
		skillOptTrainSkillOptInstallHint(),
	)
}

func (e skillOptTrainOptimizerPreflightError) Unwrap() error {
	return e.Err
}

func skillOptTrainSkillOptInstallNextAction() string {
	return "install gitmoot-skillopt and rerun train continue"
}

func skillOptTrainSkillOptInstallHint() string {
	return "Install with pipx:\n" +
		"  python3 -m pip install --user pipx\n" +
		"  python3 -m pipx ensurepath\n" +
		"  pipx install " + skillOptTrainSkillOptWheelURL + "\n" +
		"  gitmoot-skillopt --version\n" +
		"  gitmoot-skillopt optimize --help\n\n" +
		"If pipx is unavailable, use a venv and pass --skillopt-bin:\n" +
		"  python3 -m venv ~/.gitmoot/skillopt-venv\n" +
		"  ~/.gitmoot/skillopt-venv/bin/python -m pip install --upgrade pip\n" +
		"  ~/.gitmoot/skillopt-venv/bin/python -m pip install " + skillOptTrainSkillOptWheelURL + "\n" +
		"  gitmoot skillopt train continue --session <id> --skillopt-bin ~/.gitmoot/skillopt-venv/bin/gitmoot-skillopt"
}

type skillOptTrainContinueOutput struct {
	Summary       skillopt.TrainStatusSummary
	Counts        skillopt.TrainStatusCounts
	ContinueReady bool
	Lines         []string
}

func continueSkillOptTrain(ctx context.Context, paths config.Paths, store *db.Store, request skillOptTrainContinueRequest) (skillOptTrainContinueOutput, error) {
	session, iteration, counts, err := loadSkillOptTrainStatus(ctx, store, request.SessionID)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	applySkillOptTrainOptimizerDefaultsFromMetadata(session.MetadataJSON, &request.Optimizer)
	if err := validateSkillOptTrainOptimizerRequestAfterDefaults(&request.Optimizer); err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	summary := skillopt.BuildTrainStatusSummary(session, iteration, counts)
	output := skillOptTrainContinueOutput{Summary: summary, Counts: counts}
	if summary.CurrentPhase == skillopt.TrainStateRunAbandoned {
		return output, nil
	}
	if iteration == nil {
		output.Lines = []string{"next: train session has no iteration to continue"}
		return output, nil
	}
	if strings.TrimSpace(request.PromoteCandidate) != "" && strings.TrimSpace(request.RejectCandidate) != "" {
		return skillOptTrainContinueOutput{}, errors.New("train continue accepts only one of --promote or --reject")
	}
	if skillOptTrainDecisionRequested(request) &&
		summary.CurrentPhase != skillopt.TrainStateCandidateReviewPublished &&
		summary.CurrentPhase != skillopt.TrainStateCandidateCreated &&
		summary.CurrentPhase != skillopt.TrainStateCandidatePromoted &&
		summary.CurrentPhase != skillopt.TrainStateCandidateRejected {
		return skillOptTrainContinueOutput{}, fmt.Errorf("candidate decisions require train iteration at %s; current phase is %s", skillopt.TrainStateCandidateReviewPublished, summary.CurrentPhase)
	}
	if request.StartNext &&
		summary.CurrentPhase != skillopt.TrainStateCandidateReviewPublished &&
		summary.CurrentPhase != skillopt.TrainStateCandidateCreated &&
		summary.CurrentPhase != skillopt.TrainStateOptimizerCompletedNoCandidate &&
		summary.CurrentPhase != skillopt.TrainStateCandidatePromoted &&
		summary.CurrentPhase != skillopt.TrainStateCandidateRejected {
		return skillOptTrainContinueOutput{}, fmt.Errorf("--start-next requires a promoted candidate, rejected candidate, or no-candidate optimizer result; current phase is %s", summary.CurrentPhase)
	}
	switch summary.CurrentPhase {
	case skillopt.TrainStateItemsReady:
		generationLockTTL, err := estimateSkillOptTrainGenerationLockTTL(ctx, store, request, *iteration)
		if err != nil {
			return output, err
		}
		request.GenerationLockTTL = generationLockTTL
		releaseGenerationLock, extendGenerationLock, _, err := acquireSkillOptTrainGenerationLock(ctx, store, session.ID, iteration.ID, generationLockTTL)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseGenerationLock(context.Background())
		}()
		request.GenerationLockExtend = extendGenerationLock
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase != skillopt.TrainStateItemsReady {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		result, err := generateSkillOptTrainOptions(ctx, paths, store, session, *iteration, request)
		if err != nil {
			if metaErr := recordSkillOptTrainGenerationFailure(ctx, store, session, *iteration, request, err); metaErr != nil {
				return skillOptTrainContinueOutput{}, fmt.Errorf("%w; failed to record generation failure: %v", err, metaErr)
			}
			return skillOptTrainContinueOutput{}, err
		}
		session.State = skillopt.TrainStateOptionsGenerated
		iteration.State = skillopt.TrainStateOptionsGenerated
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "generation", result.Metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "generation", result.Metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, *iteration); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		return skillOptTrainContinueOutput{
			Summary:       updatedSummary,
			Counts:        updatedCounts,
			ContinueReady: true,
			Lines: []string{
				fmt.Sprintf("generated_options: %d", result.GeneratedOptions),
				fmt.Sprintf("jobs: %d", len(result.JobIDs)),
				fmt.Sprintf("generator_agent: %s", result.AgentName),
				fmt.Sprintf("generator_runtime: %s", result.Runtime),
				"next: publish the human review packet",
			},
		}, nil
	case skillopt.TrainStateOptionsGenerated:
		releaseReviewLock, _, err := acquireSkillOptTrainReviewLock(ctx, store, session.ID, iteration.ID)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseReviewLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase != skillopt.TrainStateOptionsGenerated {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		result, err := publishSkillOptTrainReview(ctx, paths, store, session, *iteration)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("review: %s", result.URL),
			fmt.Sprintf("review_repo: %s", result.Repo.FullName()),
			fmt.Sprintf("preview_urls: %d", result.PreviewURLs),
			"next: wait for feedback, then run train continue after sync",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateReviewPublished:
		releaseReviewSyncLock, _, err := acquireSkillOptTrainReviewLock(ctx, store, session.ID, iteration.ID)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseReviewSyncLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase != skillopt.TrainStateReviewPublished {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		status, err := loadSkillOptReviewStatus(ctx, store, artifact.NewStore(paths.ArtifactBlobs), iteration.EvalRunID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		feedbackCount := len(status.Feedback) + len(status.RankedFeedback)
		var syncLines []string
		if !status.TrainingReady {
			lines, synced := autoSyncSkillOptTrainReviewFeedback(ctx, paths, store, *iteration)
			syncLines = append(syncLines, lines...)
			if synced {
				status, err = loadSkillOptReviewStatus(ctx, store, artifact.NewStore(paths.ArtifactBlobs), iteration.EvalRunID)
				if err != nil {
					return skillOptTrainContinueOutput{}, err
				}
				feedbackCount = len(status.Feedback) + len(status.RankedFeedback)
			}
		}
		if !status.TrainingReady {
			lines := append([]string{}, syncLines...)
			lines = append(lines,
				fmt.Sprintf("feedback_events: %d", feedbackCount),
				fmt.Sprintf("pairwise_preferences: %d", len(status.PairwisePreferences)),
				fmt.Sprintf("packet_blockers: %d", len(status.PacketBlockers)),
				fmt.Sprintf("training_blockers: %d", len(status.TrainingBlockers)),
			)
			for _, blocker := range status.PacketBlockers {
				lines = append(lines, fmt.Sprintf("packet_blocker: %s", blocker))
			}
			for _, blocker := range status.TrainingBlockers {
				lines = append(lines, fmt.Sprintf("training_blocker: %s", blocker))
			}
			if url := strings.TrimSpace(iteration.IssueURL); url != "" {
				lines = append(lines, fmt.Sprintf("continue_from_github: %s", url))
			}
			lines = append(lines, "next: sync human feedback from the review surface")
			output.Lines = lines
			return output, nil
		}
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateFeedbackSynced); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		metadata := map[string]any{
			"status":                 "succeeded",
			"source":                 "gitmoot skillopt train continue",
			"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
			"feedback_events":        feedbackCount,
			"ranked_feedback_events": len(status.RankedFeedback),
			"pairwise_preferences":   len(status.PairwisePreferences),
			"recommended_next_mode":  status.Recommendation.RecommendedMode,
			"ranking_stability":      status.Recommendation.RankingStability,
			"recommendation_summary": status.Recommendation.Summary(),
			"training_blocker_count": len(status.TrainingBlockers),
			"review_packet_ready":    status.PacketReady,
			"review_training_ready":  status.TrainingReady,
		}
		session.State = skillopt.TrainStateFeedbackSynced
		iteration.State = skillopt.TrainStateFeedbackSynced
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "feedback_sync", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "feedback_sync", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, *iteration); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("feedback_events: %d", feedbackCount),
			fmt.Sprintf("pairwise_preferences: %d", len(status.PairwisePreferences)),
			fmt.Sprintf("recommended_next_mode: %s", status.Recommendation.RecommendedMode),
			fmt.Sprintf("ranking_stability: %s", status.Recommendation.RankingStability),
			"next: export the training package before running the optimizer",
		}
		if len(syncLines) > 0 {
			lines = append(syncLines, lines...)
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateFeedbackSynced, skillopt.TrainStateTrainingPackageCreated, skillopt.TrainStateOptimizerCompleted, skillopt.TrainStateOptimizerCompletedNoCandidate:
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase == skillopt.TrainStateOptimizerCompletedNoCandidate {
			if request.StartNext {
				next, err := startNextSkillOptTrainIteration(ctx, store, session, *iteration)
				if err != nil {
					return skillOptTrainContinueOutput{}, err
				}
				updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
				if err != nil {
					return skillOptTrainContinueOutput{}, err
				}
				updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
				lines := []string{
					fmt.Sprintf("started_iteration: %s", next.ID),
					fmt.Sprintf("base_version: %s", next.BaseTemplateVersionID),
					"next: generate review options with train continue",
				}
				return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
			}
			if !request.Optimizer.RerunOptimizer {
				output.Lines = []string{"next: revise feedback and run --start-next, rerun the optimizer with --rerun-optimizer, or stop"}
				return output, nil
			}
		}
		optimizerLockTTL, err := skillOptTrainOptimizerLockTTLForRequest(request.Optimizer)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		releaseOptimizerLock, optimizerLockState, err := acquireSkillOptTrainOptimizerLock(ctx, store, session.ID, iteration.ID, optimizerLockTTL, request.Optimizer)
		if err != nil {
			return output, err
		}
		request.Optimizer.OptimizerLockState = optimizerLockState
		defer func() {
			_ = releaseOptimizerLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if summary.CurrentPhase != skillopt.TrainStateFeedbackSynced &&
			summary.CurrentPhase != skillopt.TrainStateTrainingPackageCreated &&
			summary.CurrentPhase != skillopt.TrainStateOptimizerCompleted &&
			summary.CurrentPhase != skillopt.TrainStateOptimizerCompletedNoCandidate {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		if skillOptTrainContinueNeedsOptimizerPreflight(summary.CurrentPhase, request.Optimizer) {
			result, err := preflightSkillOptTrainOptimizerForContinue(ctx, paths, store, session, *iteration, request.Optimizer)
			if err != nil {
				if skillOptTrainOptimizerResultHasReport(result) {
					output.Lines = skillOptTrainOptimizerReportLines(result)
				}
				return output, err
			}
		}
		result, err := continueSkillOptTrainOptimizer(ctx, paths, store, session, *iteration, request.Optimizer, request.Progress)
		if err != nil {
			if skillOptTrainOptimizerResultHasReport(result) {
				output.Lines = skillOptTrainOptimizerReportLines(result)
			}
			return output, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := skillOptTrainOptimizerReportLines(result)
		if result.ExportedOnly {
			lines = append(lines,
				fmt.Sprintf("training_package: %s", result.TrainingPackagePath),
				"next: run train continue without --export-only to launch the optimizer",
			)
			return skillOptTrainContinueOutput{
				Summary:       updatedSummary,
				Counts:        updatedCounts,
				ContinueReady: true,
				Lines:         lines,
			}, nil
		}
		if result.NoCandidateReason != "" {
			lines = append(lines,
				fmt.Sprintf("no_candidate_reason: %s", result.NoCandidateReason),
				fmt.Sprintf("next: %s", result.NoCandidateNextAction),
			)
		} else {
			lines = append(lines,
				fmt.Sprintf("imported_candidate: %s", result.CandidateVersionID),
				"next: publish candidate diff and preview review",
			)
		}
		return skillOptTrainContinueOutput{
			Summary:       updatedSummary,
			Counts:        updatedCounts,
			ContinueReady: true,
			Lines:         lines,
		}, nil
	case skillopt.TrainStateCandidateCreated:
		releaseCandidateReviewLock, _, err := acquireSkillOptTrainCandidateReviewLock(ctx, store, session.ID, iteration.ID)
		if err != nil {
			return output, err
		}
		defer func() {
			_ = releaseCandidateReviewLock(context.Background())
		}()
		session, iteration, counts, err = loadSkillOptTrainStatus(ctx, store, request.SessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		summary = skillopt.BuildTrainStatusSummary(session, iteration, counts)
		output = skillOptTrainContinueOutput{Summary: summary, Counts: counts}
		if iteration == nil {
			output.Lines = []string{"next: train session has no iteration to continue"}
			return output, nil
		}
		if skillOptTrainDecisionRequested(request) && summary.CurrentPhase == skillopt.TrainStateCandidateReviewPublished {
			return continueSkillOptTrainCandidateDecision(ctx, store, session, *iteration, counts, request)
		}
		if summary.CurrentPhase != skillopt.TrainStateCandidateCreated {
			output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
			return output, nil
		}
		candidateID := strings.TrimSpace(iteration.CandidateVersionID)
		if requestedCandidateID := requestedSkillOptTrainCandidateID(request); requestedCandidateID != "" && requestedCandidateID != candidateID {
			return skillOptTrainContinueOutput{}, fmt.Errorf("candidate %s does not match train iteration candidate %s", requestedCandidateID, candidateID)
		}
		if result, err := syncSkillOptTrainCandidateDecision(ctx, store, session, *iteration, candidateID, requestedSkillOptTrainCandidateDecision(request), strings.TrimSpace(request.DecisionReason)); err != nil || result.Decided {
			if err != nil {
				return skillOptTrainContinueOutput{}, err
			}
			return continueSkillOptTrainAfterCandidateDecision(ctx, store, session.ID, request, result)
		}
		if request.StartNext {
			return skillOptTrainContinueOutput{}, fmt.Errorf("--start-next requires a promoted or rejected candidate; current phase is %s", summary.CurrentPhase)
		}
		if skillOptTrainDecisionRequested(request) {
			if candidateID == "" {
				return skillOptTrainContinueOutput{}, errors.New("train iteration has no candidate version to review")
			}
			if _, recovered, err := recoverSkillOptCandidateReviewPublication(ctx, paths, store, session, *iteration, candidateID); err != nil {
				return skillOptTrainContinueOutput{}, err
			} else if !recovered {
				return skillOptTrainContinueOutput{}, fmt.Errorf("candidate decisions require train iteration at %s; current phase is %s", skillopt.TrainStateCandidateReviewPublished, summary.CurrentPhase)
			}
			updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
			if err != nil {
				return skillOptTrainContinueOutput{}, err
			}
			if updatedIteration == nil {
				return skillOptTrainContinueOutput{}, errors.New("train session has no recovered iteration to decide")
			}
			return continueSkillOptTrainCandidateDecision(ctx, store, updatedSession, *updatedIteration, updatedCounts, request)
		}
		result, err := publishSkillOptTrainCandidateReview(ctx, paths, store, session, *iteration, request.Home)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("candidate_review: %s", result.URL),
			fmt.Sprintf("candidate: %s", result.CandidateVersionID),
			"next: choose promote, reject with a reason, or wait; keep improving by rejecting with an actionable reason and then running --start-next",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	case skillopt.TrainStateCandidateReviewPublished:
		return continueSkillOptTrainCandidateDecision(ctx, store, session, *iteration, counts, request)
	case skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		if err := validateTerminalSkillOptTrainDecisionRequest(*iteration, request); err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		if !request.StartNext {
			output.Lines = []string{"next: stop or run --start-next"}
			return output, nil
		}
		next, err := startNextSkillOptTrainIteration(ctx, store, session, *iteration)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, session.ID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{}
		if decision := requestedSkillOptTrainCandidateDecision(request); decision != "" {
			lines = append(lines, fmt.Sprintf("%s_candidate: %s", decision, requestedSkillOptTrainCandidateID(request)))
		}
		lines = append(lines,
			fmt.Sprintf("started_iteration: %s", next.ID),
			fmt.Sprintf("base_version: %s", next.BaseTemplateVersionID),
			"next: generate review options with train continue",
		)
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	default:
		output.Lines = []string{fmt.Sprintf("next: %s", summary.NextAction)}
		return output, nil
	}
}

func continueSkillOptTrainCandidateDecision(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, counts skillopt.TrainStatusCounts, request skillOptTrainContinueRequest) (skillOptTrainContinueOutput, error) {
	summary := skillopt.BuildTrainStatusSummary(session, &iteration, counts)
	output := skillOptTrainContinueOutput{Summary: summary, Counts: counts}
	result, err := decideSkillOptTrainCandidate(ctx, store, session, iteration, request)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	if !result.Decided {
		if request.StartNext {
			return skillOptTrainContinueOutput{}, fmt.Errorf("--start-next requires a promoted or rejected candidate; current phase is %s", summary.CurrentPhase)
		}
		lines := []string{}
		if url := strings.TrimSpace(iteration.IssueURL); url != "" {
			lines = append(lines, fmt.Sprintf("continue_from_github: %s", url))
		}
		lines = append(lines, "next: promote with --promote <candidate-version> or reject with --reject <candidate-version> --reason <text>")
		output.Lines = lines
		return output, nil
	}
	// Reflect the decision on the candidate-review issue so it doesn't sit open
	// with no record of the choice (works for a TUI p/x or the --promote/--reject
	// flags). Best-effort: a GitHub failure never undoes the recorded decision.
	decisionNotice := postSkillOptTrainCandidateDecisionComment(ctx, iteration, result, request)
	out, err := continueSkillOptTrainAfterCandidateDecision(ctx, store, session.ID, request, result)
	// Surface the GitHub notice whether or not the follow-up (e.g. --start-next)
	// succeeded — the decision was made and posted regardless.
	if decisionNotice != "" {
		out.Lines = append(out.Lines, decisionNotice)
	}
	return out, err
}

// postSkillOptTrainCandidateDecisionComment posts a promote/reject decision to the
// candidate-review issue (or PR) and closes the issue. It is best-effort and
// returns a one-line notice (or a warning) for the continue output; the recorded
// decision stands regardless. The comment carries a gitmoot marker so the review
// watcher skips it.
func postSkillOptTrainCandidateDecisionComment(ctx context.Context, iteration db.SkillOptTrainIteration, result skillOptTrainCandidateDecisionResult, request skillOptTrainContinueRequest) string {
	repoName := strings.TrimSpace(iteration.IssueRepo)
	number := iteration.IssueNumber
	onIssue := repoName != "" && number > 0
	if !onIssue {
		repoName = strings.TrimSpace(iteration.PullRequestRepo)
		number = iteration.PullRequestNumber
	}
	if repoName == "" || number <= 0 {
		return ""
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return fmt.Sprintf("warning: candidate-review repo %q is unparseable; decision not posted: %v", repoName, err)
	}
	version := strings.TrimSpace(result.CandidateVersionID)
	if version == "" {
		version = strings.TrimSpace(firstNonEmpty(request.PromoteCandidate, request.RejectCandidate))
	}
	var body strings.Builder
	body.WriteString(skillOptTrainDecisionMarker + "\n")
	switch {
	case strings.Contains(result.Decision, "promot"):
		fmt.Fprintf(&body, "✅ Promoted `%s` from gitmoot.", version)
	case strings.Contains(result.Decision, "reject"):
		fmt.Fprintf(&body, "❌ Rejected `%s` from gitmoot.", version)
		if reason := strings.TrimSpace(request.DecisionReason); reason != "" {
			fmt.Fprintf(&body, "\n\nReason: %s", reason)
		}
	default:
		fmt.Fprintf(&body, "Decision recorded for `%s`: %s", version, result.Decision)
	}
	client := newSkillOptGitHubClient()
	if _, err := client.PostIssueComment(ctx, repo, number, body.String()); err != nil {
		return fmt.Sprintf("warning: could not post the decision to %s#%d: %v", repo.FullName(), number, err)
	}
	notice := fmt.Sprintf("candidate_review: posted the decision to %s#%d", repo.FullName(), number)
	// Only close a dedicated review issue; never close a user's pull request.
	if onIssue {
		if _, err := client.CloseIssue(ctx, repo, number); err != nil {
			return notice + fmt.Sprintf(" (could not close it: %v)", err)
		}
		notice += " and closed it"
	}
	return notice
}

func continueSkillOptTrainAfterCandidateDecision(ctx context.Context, store *db.Store, sessionID string, request skillOptTrainContinueRequest, result skillOptTrainCandidateDecisionResult) (skillOptTrainContinueOutput, error) {
	updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
	if request.StartNext {
		if updatedIteration == nil {
			return skillOptTrainContinueOutput{}, errors.New("train session has no decided iteration to continue")
		}
		next, err := startNextSkillOptTrainIteration(ctx, store, updatedSession, *updatedIteration)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err = loadSkillOptTrainStatus(ctx, store, sessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary = skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("%s_candidate: %s", result.Decision, result.CandidateVersionID),
			fmt.Sprintf("started_iteration: %s", next.ID),
			fmt.Sprintf("base_version: %s", next.BaseTemplateVersionID),
			"next: generate review options with train continue",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	}
	lines := []string{
		fmt.Sprintf("%s_candidate: %s", result.Decision, result.CandidateVersionID),
		"next: stop or run --start-next",
	}
	return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
}

func validateTerminalSkillOptTrainDecisionRequest(iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) error {
	decision := requestedSkillOptTrainCandidateDecision(request)
	if decision == "" {
		return nil
	}
	candidateID := requestedSkillOptTrainCandidateID(request)
	expected := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID != expected {
		return fmt.Errorf("candidate %s does not match train iteration candidate %s", candidateID, expected)
	}
	currentDecision := ""
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidatePromoted:
		currentDecision = "promoted"
	case skillopt.TrainStateCandidateRejected:
		currentDecision = "rejected"
	}
	if currentDecision != "" && decision != currentDecision {
		return fmt.Errorf("candidate %s is already %s, not %s", candidateID, currentDecision, decision)
	}
	return nil
}

type skillOptTrainOptimizerResult struct {
	TrainingPackagePath   string
	OutRoot               string
	CandidatePackagePath  string
	ArtifactDir           string
	OptimizerRoot         string
	OptimizerAttempt      string
	OptimizerAttemptPath  string
	Command               string
	Args                  []string
	DryRun                bool
	Request               skillOptTrainOptimizerRequest
	BackendResolution     skillOptTrainBackendResolution
	RecoveryAvailable     bool
	OptimizerLockState    string
	CandidateVersionID    string
	NoCandidateReason     string
	NoCandidateNextAction string
	NextAction            string
	ExportedOnly          bool
}

type skillOptTrainRecoverResult struct {
	SessionID            string   `json:"session_id"`
	IterationID          string   `json:"iteration_id"`
	Classification       string   `json:"classification"`
	CurrentPhase         string   `json:"current_phase"`
	CandidateVersionID   string   `json:"candidate_version_id,omitempty"`
	NoCandidateReason    string   `json:"no_candidate_reason,omitempty"`
	NextAction           string   `json:"next_action,omitempty"`
	RecoveryAvailable    bool     `json:"recovery_available"`
	OutRoot              string   `json:"out_root,omitempty"`
	OptimizerRoot        string   `json:"optimizer_root,omitempty"`
	OptimizerAttempt     string   `json:"optimizer_attempt,omitempty"`
	OptimizerAttemptPath string   `json:"optimizer_attempt_path,omitempty"`
	CandidatePackagePath string   `json:"candidate_package,omitempty"`
	ArtifactDir          string   `json:"artifact_dir,omitempty"`
	Artifacts            []string `json:"artifacts,omitempty"`
}

type skillOptTrainBackendResolution struct {
	Backend               string
	OptimizerBackend      string
	TargetBackend         string
	InternalTargetAdapter string
	EvaluatorBackend      string
	ConfigStatus          string
}

type skillOptTrainOptimizerPaths struct {
	OutRoot              string
	OptimizerRoot        string
	OptimizerAttempt     string
	OptimizerAttemptPath string
	ArtifactRoot         string
	TrainingPackagePath  string
	CandidatePackagePath string
	ArtifactDir          string
}

type skillOptTrainCandidateReviewResult struct {
	URL                string
	CandidateVersionID string
	PublishedFiles     []skillOptTrainCandidateReviewFile
	PublishedPreviews  []skillOptTrainCandidateReviewPreview
}

type skillOptTrainCandidateReviewFile struct {
	Label string
	Path  string
	URL   string
}

type skillOptTrainCandidateReviewPreview struct {
	Label        string
	ArtifactID   string
	Route        string
	URL          string
	Renderer     string
	Content      string
	Status       string
	StatusReason string
	Error        string
}

type skillOptTrainCandidateDecisionResult struct {
	Decided            bool
	Decision           string
	CandidateVersionID string
}

func publishSkillOptTrainCandidateReview(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, commandHome string) (skillOptTrainCandidateReviewResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateCandidateReviewPublished); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID == "" {
		return skillOptTrainCandidateReviewResult{}, errors.New("train iteration has no candidate version to review")
	}
	if result, recovered, err := recoverSkillOptCandidateReviewPublication(ctx, paths, store, session, iteration, candidateID); recovered || err != nil {
		return result, err
	}
	refreshedSession, err := store.GetSkillOptTrainSession(ctx, session.ID)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	refreshedIteration, err := store.GetSkillOptTrainIteration(ctx, iteration.ID)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	session = refreshedSession
	iteration = refreshedIteration
	if err := preventDuplicateSkillOptCandidateReviewPublish(session, iteration, candidateID, time.Now().UTC()); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	repo, err := resolveSkillOptTrainCandidateReviewRepo(session, iteration)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	if iteration.PullRequestNumber > 0 && iteration.IssueNumber == 0 {
		iteration.PullRequestRepo = repo.FullName()
	} else {
		iteration.IssueRepo = repo.FullName()
	}
	client := newSkillOptGitHubClient()
	if err := client.Preflight(ctx, repo); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	publishedFiles := existingSkillOptCandidateReviewPublishedFiles(session, iteration, repo, candidateID)
	var filePublishErr error
	if len(publishedFiles) == 0 {
		publishedFiles, filePublishErr = publishSkillOptTrainCandidateReviewFiles(ctx, paths, store, client, repo, session, iteration)
	}
	filePublishWarnings := []string{}
	if filePublishErr != nil {
		filePublishWarnings = append(filePublishWarnings, filePublishErr.Error())
	}
	publishedPreviews := existingSkillOptCandidateReviewPublishedPreviews(session, iteration, candidateID)
	if len(publishedPreviews) == 0 {
		publishedPreviews = publishSkillOptTrainCandidateSamplePreviews(ctx, paths, store, session, iteration)
	}
	body, err := skillOptTrainCandidateReviewBody(ctx, store, session, iteration, commandHome, publishedFiles, publishedPreviews, filePublishWarnings)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	title := fmt.Sprintf("SkillOpt candidate review: %s", session.ID)
	publishingMetadata := map[string]any{
		"status":              "publishing",
		"candidate_version":   candidateID,
		"issue_repo":          iteration.IssueRepo,
		"issue_number":        iteration.IssueNumber,
		"issue_url":           iteration.IssueURL,
		"pull_request_repo":   iteration.PullRequestRepo,
		"pull_request_number": iteration.PullRequestNumber,
		"pull_request_url":    iteration.PullRequestURL,
		"issue_title":         title,
		"published_files":     skillOptCandidateReviewFilesMetadata(publishedFiles),
		"published_previews":  skillOptCandidateReviewPreviewsMetadata(publishedPreviews),
		"file_publish_errors": filePublishWarnings,
		"started_at":          time.Now().UTC().Format(time.RFC3339Nano),
		"source":              "gitmoot skillopt train continue",
	}
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, publishingMetadata); err != nil {
		return skillOptTrainCandidateReviewResult{}, fmt.Errorf("write candidate review pre-publish recovery marker: %w", err)
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", publishingMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", publishingMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	postingMetadata := make(map[string]any, len(publishingMetadata)+2)
	for key, value := range publishingMetadata {
		postingMetadata[key] = value
	}
	postingMetadata["status"] = "posting_external"
	postingMetadata["external_post_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, postingMetadata); err != nil {
		if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
			return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
		}
		return skillOptTrainCandidateReviewResult{}, fmt.Errorf("write candidate review external-post recovery marker: %w", err)
	}
	var url string
	if iteration.IssueNumber > 0 {
		comment, err := client.PostIssueComment(ctx, repo, iteration.IssueNumber, body)
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		url = comment.URL
		if strings.TrimSpace(iteration.IssueURL) == "" {
			iteration.IssueURL = skillOptReviewTargetURLFromCommentOrHost(comment.URL, repo, "issues", iteration.IssueNumber)
		}
	} else if iteration.PullRequestNumber > 0 {
		comment, err := client.PostIssueComment(ctx, repo, iteration.PullRequestNumber, body)
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		url = comment.URL
		if strings.TrimSpace(iteration.PullRequestURL) == "" {
			iteration.PullRequestURL = skillOptReviewTargetURLFromCommentOrHost(comment.URL, repo, "pull", iteration.PullRequestNumber)
		}
	} else {
		issue, err := client.CreateIssue(ctx, github.CreateIssueInput{
			Repo:  repo,
			Title: title,
			Body:  body,
		})
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		iteration.IssueNumber = issue.Number
		iteration.IssueURL = issue.URL
		url = issue.URL
	}
	externalMetadata := skillOptCandidateReviewPublicationMetadata(publishingMetadata, iteration, url, "published_external")
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", externalMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", externalMetadata)
	recoveryErr := writeSkillOptCandidateReviewRecovery(paths, session, iteration, externalMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		if recoveryErr != nil {
			return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; candidate review was published at %s but recovery marker write failed: %v", err, url, recoveryErr)
		}
		return skillOptTrainCandidateReviewResult{}, err
	}
	iteration.State = skillopt.TrainStateCandidateReviewPublished
	session.State = skillopt.TrainStateCandidateReviewPublished
	metadata := skillOptCandidateReviewPublicationMetadata(publishingMetadata, iteration, url, "published")
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
	return skillOptTrainCandidateReviewResult{URL: url, CandidateVersionID: candidateID, PublishedFiles: publishedFiles, PublishedPreviews: publishedPreviews}, nil
}

func recoverSkillOptCandidateReviewPublication(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string) (skillOptTrainCandidateReviewResult, bool, error) {
	sources := []map[string]any{
		decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["candidate_review"]),
		decodedSkillOptMetadataValue(decodedSkillOptMetadata(session.MetadataJSON)["candidate_review"]),
	}
	if review, ok, err := readSkillOptCandidateReviewRecovery(paths, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, true, err
	} else if ok {
		if metadataString(review, "status") == "publishing" && metadataString(review, "external_post_started_at") == "" {
			metadata := make(map[string]any, len(review)+3)
			for key, value := range review {
				metadata[key] = value
			}
			metadata["status"] = "failed"
			metadata["error"] = "candidate review publication interrupted before external post started"
			metadata["failed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
			iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
			if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
				return skillOptTrainCandidateReviewResult{}, true, err
			}
			_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
			return skillOptTrainCandidateReviewResult{}, false, nil
		}
		sources = append(sources, review)
	}
	for _, review := range sources {
		status := metadataString(review, "status")
		if status == "posting_external" {
			target := skillOptCandidateReviewRecoveryTarget(review)
			if target == "" {
				target = "inspect the configured GitHub review surface before retrying"
			}
			return skillOptTrainCandidateReviewResult{}, true, fmt.Errorf("candidate review publication for %s was interrupted after external post started; %s", candidateID, target)
		}
		if status != "published_external" && status != "published" {
			continue
		}
		reviewCandidate := metadataString(review, "candidate_version")
		if reviewCandidate != "" && reviewCandidate != candidateID {
			continue
		}
		url := skillOptCandidateReviewURLFromMetadata(review)
		if url == "" {
			return skillOptTrainCandidateReviewResult{}, true, fmt.Errorf("candidate review publication for %s is marked %s but has no recoverable review URL", candidateID, status)
		}
		applySkillOptCandidateReviewMetadataToIteration(review, &iteration)
		iteration.State = skillopt.TrainStateCandidateReviewPublished
		session.State = skillopt.TrainStateCandidateReviewPublished
		metadata := make(map[string]any, len(review)+3)
		for key, value := range review {
			metadata[key] = value
		}
		metadata["status"] = "published"
		metadata["review_url"] = url
		metadata["recovered_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
		if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
			return skillOptTrainCandidateReviewResult{}, true, err
		}
		_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
		return skillOptTrainCandidateReviewResult{URL: url, CandidateVersionID: candidateID}, true, nil
	}
	return skillOptTrainCandidateReviewResult{}, false, nil
}

func preventDuplicateSkillOptCandidateReviewPublish(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string, now time.Time) error {
	for _, source := range []struct {
		name     string
		metadata string
	}{
		{name: "iteration", metadata: iteration.MetadataJSON},
		{name: "session", metadata: session.MetadataJSON},
	} {
		review := decodedSkillOptMetadataValue(decodedSkillOptMetadata(source.metadata)["candidate_review"])
		status := metadataString(review, "status")
		if status == "publishing" && !skillOptCandidateReviewPublishingFresh(review, now) {
			continue
		}
		if status != "publishing" && status != "published" {
			continue
		}
		reviewCandidate := metadataString(review, "candidate_version")
		if reviewCandidate != "" && reviewCandidate != candidateID {
			continue
		}
		target := skillOptCandidateReviewRecoveryTarget(review)
		if target == "" {
			target = "inspect candidate_review metadata before retrying"
		}
		return fmt.Errorf("candidate review publication for %s is marked %s in %s metadata; %s", candidateID, status, source.name, target)
	}
	return nil
}

func skillOptCandidateReviewPublishingFresh(review map[string]any, now time.Time) bool {
	startedAt := metadataString(review, "started_at")
	if startedAt == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Before(started.Add(skillOptTrainCandidateReviewLockTTL))
}

func writeSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, metadata map[string]any) error {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return errors.New("candidate review recovery path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmpPath, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func readSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (map[string]any, bool, error) {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil, false, nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(content, &metadata); err != nil {
		return nil, true, fmt.Errorf("read candidate review recovery marker %s: %w", path, err)
	}
	return metadata, true, nil
}

func removeSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) error {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func skillOptCandidateReviewRecoveryPath(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) string {
	if strings.TrimSpace(paths.Home) == "" {
		return ""
	}
	name := skillOptCandidateReviewRecoveryName(session.ID, iteration.ID)
	if name == "" {
		return ""
	}
	return filepath.Join(paths.Home, "skillopt", "candidate-reviews", name+".json")
}

func skillOptCandidateReviewRecoveryName(sessionID string, iterationID string) string {
	sessionID = encodeSkillOptCandidateReviewRecoveryToken(sessionID)
	iterationID = encodeSkillOptCandidateReviewRecoveryToken(iterationID)
	if sessionID == "" || iterationID == "" {
		return ""
	}
	return sessionID + "-" + iterationID
}

func encodeSkillOptCandidateReviewRecoveryToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func skillOptCandidateReviewPublicationMetadata(base map[string]any, iteration db.SkillOptTrainIteration, reviewURL string, status string) map[string]any {
	metadata := make(map[string]any, len(base)+9)
	for key, value := range base {
		metadata[key] = value
	}
	metadata["status"] = status
	metadata["issue_repo"] = iteration.IssueRepo
	metadata["issue_number"] = iteration.IssueNumber
	metadata["issue_url"] = iteration.IssueURL
	metadata["pull_request_repo"] = iteration.PullRequestRepo
	metadata["pull_request_number"] = iteration.PullRequestNumber
	metadata["pull_request_url"] = iteration.PullRequestURL
	metadata["review_url"] = reviewURL
	metadata["published_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return metadata
}

func recordFailedSkillOptCandidateReviewPublish(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, publishingMetadata map[string]any, publishErr error) error {
	metadata := make(map[string]any, len(publishingMetadata)+3)
	for key, value := range publishingMetadata {
		metadata[key] = value
	}
	metadata["status"] = "failed"
	metadata["error"] = truncateForMetadata(publishErr.Error())
	metadata["failed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
	return store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration)
}

func applySkillOptCandidateReviewMetadataToIteration(review map[string]any, iteration *db.SkillOptTrainIteration) {
	if value := metadataString(review, "issue_repo"); value != "" {
		iteration.IssueRepo = value
	}
	if value := metadataString(review, "issue_number"); value != "" {
		if number, err := strconv.ParseInt(value, 10, 64); err == nil {
			iteration.IssueNumber = number
		}
	}
	if value := metadataString(review, "issue_url"); value != "" {
		iteration.IssueURL = value
	}
	if value := metadataString(review, "pull_request_repo"); value != "" {
		iteration.PullRequestRepo = value
	}
	if value := metadataString(review, "pull_request_number"); value != "" {
		if number, err := strconv.ParseInt(value, 10, 64); err == nil {
			iteration.PullRequestNumber = number
		}
	}
	if value := metadataString(review, "pull_request_url"); value != "" {
		iteration.PullRequestURL = value
	}
}

func skillOptCandidateReviewURLFromMetadata(review map[string]any) string {
	for _, key := range []string{"review_url", "issue_url", "pull_request_url"} {
		if value := metadataString(review, key); value != "" {
			return value
		}
	}
	repo := metadataString(review, "issue_repo")
	number := metadataString(review, "issue_number")
	if repo != "" && number != "" && number != "0" {
		return "https://github.com/" + repo + "/issues/" + number
	}
	repo = metadataString(review, "pull_request_repo")
	number = metadataString(review, "pull_request_number")
	if repo != "" && number != "" && number != "0" {
		return "https://github.com/" + repo + "/pull/" + number
	}
	return ""
}

func skillOptCandidateReviewRecoveryTarget(review map[string]any) string {
	for _, key := range []string{"review_url", "issue_url", "pull_request_url"} {
		if value := metadataString(review, key); value != "" {
			return "review target: " + value
		}
	}
	repo := metadataString(review, "issue_repo")
	number := metadataString(review, "issue_number")
	if repo != "" && number != "" && number != "0" {
		return "review issue: " + repo + "#" + number
	}
	repo = metadataString(review, "pull_request_repo")
	number = metadataString(review, "pull_request_number")
	if repo != "" && number != "" && number != "0" {
		return "review pull request: " + repo + "#" + number
	}
	if title := metadataString(review, "issue_title"); title != "" {
		return "search for review issue title: " + title
	}
	return ""
}

func resolveSkillOptTrainCandidateReviewRepo(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (github.Repository, error) {
	repoName := strings.TrimSpace(iteration.IssueRepo)
	if iteration.IssueNumber > 0 {
		if repoName == "" {
			repoName = skillOptGitHubIssueURLRepo(iteration.IssueURL)
		}
		if repoName == "" {
			return github.Repository{}, errors.New("candidate review issue repo is required when reusing an existing review issue")
		}
	} else if iteration.PullRequestNumber > 0 {
		repoName = strings.TrimSpace(iteration.PullRequestRepo)
		if repoName == "" {
			repoName = skillOptGitHubPullRequestURLRepo(iteration.PullRequestURL)
		}
		if repoName == "" {
			return github.Repository{}, errors.New("candidate review pull request repo is required when reusing an existing review pull request")
		}
	} else if repoName == "" {
		repoName = strings.TrimSpace(session.WorkspaceRepo)
		if repoName == "" {
			repoName = strings.TrimSpace(session.TargetRepo)
		}
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return github.Repository{}, fmt.Errorf("candidate review repo: %w", err)
	}
	return repo, nil
}

func skillOptGitHubIssueURLRepo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return ""
	}
	repo, err := daemon.ParseRepository(parts[0] + "/" + parts[1])
	if err != nil {
		return ""
	}
	return repo.FullName()
}

func skillOptGitHubPullRequestURLRepo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return ""
	}
	repo, err := daemon.ParseRepository(parts[0] + "/" + parts[1])
	if err != nil {
		return ""
	}
	return repo.FullName()
}

func skillOptReviewTargetURLFromCommentURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return ""
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func skillOptReviewTargetURLFromCommentOrHost(commentURL string, repo github.Repository, kind string, number int64) string {
	if target := skillOptReviewTargetURLFromCommentURL(commentURL); target != "" {
		return target
	}
	parsed, err := neturl.Parse(strings.TrimSpace(commentURL))
	if err != nil || strings.TrimSpace(parsed.Host) == "" {
		return ""
	}
	scheme := strings.TrimSpace(parsed.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	target := neturl.URL{
		Scheme: scheme,
		Host:   parsed.Host,
		Path:   "/" + repo.FullName() + "/" + strings.Trim(kind, "/") + "/" + fmt.Sprint(number),
	}
	return target.String()
}

func skillOptTrainDecisionRequested(request skillOptTrainContinueRequest) bool {
	return strings.TrimSpace(request.PromoteCandidate) != "" || strings.TrimSpace(request.RejectCandidate) != ""
}

func requestedSkillOptTrainCandidateDecision(request skillOptTrainContinueRequest) string {
	if strings.TrimSpace(request.PromoteCandidate) != "" {
		return "promoted"
	}
	if strings.TrimSpace(request.RejectCandidate) != "" {
		return "rejected"
	}
	return ""
}

func requestedSkillOptTrainCandidateID(request skillOptTrainContinueRequest) string {
	if value := strings.TrimSpace(request.PromoteCandidate); value != "" {
		return value
	}
	return strings.TrimSpace(request.RejectCandidate)
}

func publishSkillOptTrainCandidateReviewFiles(ctx context.Context, paths config.Paths, store *db.Store, client github.Client, repo github.Repository, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) ([]skillOptTrainCandidateReviewFile, error) {
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID == "" {
		return nil, nil
	}
	version, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return nil, fmt.Errorf("load candidate version %s for review files: %w", candidateID, err)
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		return nil, fmt.Errorf("load candidate review %s for review files: %w", candidateID, err)
	}
	basePath := skillOptCandidateReviewFileBasePath(session.ID, iteration.ID, candidateID)
	if basePath == "" {
		return nil, errors.New("candidate review file path is required")
	}
	files := []struct {
		label   string
		name    string
		content []byte
	}{
		{
			label:   "Best skill",
			name:    "best_skill.md",
			content: []byte(strings.TrimRight(version.Content, "\n") + "\n"),
		},
	}
	if baseID := firstNonEmpty(review.BaseVersionID, iteration.BaseTemplateVersionID); baseID != "" {
		if baseVersion, err := store.GetAgentTemplateVersionByID(ctx, baseID); err == nil {
			files = append(files, struct {
				label   string
				name    string
				content []byte
			}{
				label:   "Base skill",
				name:    "base_skill.md",
				content: []byte(strings.TrimRight(baseVersion.Content, "\n") + "\n"),
			})
		}
	}
	if diffContent, err := skillOptCandidateReviewDiffContent(ctx, paths, store, review); err == nil && len(diffContent) > 0 {
		files = append(files, struct {
			label   string
			name    string
			content []byte
		}{
			label:   "Candidate diff",
			name:    "candidate.diff.md",
			content: diffContent,
		})
	} else if err != nil {
		return nil, err
	}
	published := make([]skillOptTrainCandidateReviewFile, 0, len(files))
	for _, file := range files {
		repoPath := basePath + "/" + file.name
		result, err := client.UpsertFile(ctx, github.UpsertFileInput{
			Repo:    repo,
			Path:    repoPath,
			Content: file.content,
			Message: fmt.Sprintf("Publish SkillOpt candidate review file %s", candidateID),
		})
		if err != nil {
			return published, fmt.Errorf("publish candidate review file %s: %w", repoPath, err)
		}
		published = append(published, skillOptTrainCandidateReviewFile{
			Label: file.label,
			Path:  firstNonEmpty(result.Path, repoPath),
			URL:   result.URL,
		})
	}
	return published, nil
}

func skillOptCandidateReviewDiffContent(ctx context.Context, paths config.Paths, store *db.Store, review db.AgentTemplateCandidateReview) ([]byte, error) {
	diffID := strings.TrimSpace(review.DiffArtifactID)
	if diffID == "" {
		return nil, nil
	}
	record, err := store.GetEvalArtifact(ctx, diffID)
	if err != nil {
		return nil, fmt.Errorf("load candidate diff artifact %s: %w", diffID, err)
	}
	if strings.TrimSpace(record.Hash) == "" {
		return nil, nil
	}
	content, err := artifact.NewStore(paths.ArtifactBlobs).Read(record.Hash)
	if err != nil {
		return nil, fmt.Errorf("read candidate diff artifact %s: %w", diffID, err)
	}
	return content, nil
}

func skillOptCandidateReviewFileBasePath(sessionID string, iterationID string, candidateID string) string {
	parts := []string{
		"skillopt",
		"runs",
		skillOptCandidateReviewFileToken(sessionID),
		skillOptCandidateReviewFileToken(iterationID),
		skillOptCandidateReviewFileToken(candidateID),
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return ""
		}
	}
	return strings.Join(parts, "/")
}

func skillOptCandidateReviewFileToken(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '-', r == '_', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

func skillOptCandidateReviewFilesMetadata(files []skillOptTrainCandidateReviewFile) []map[string]string {
	if len(files) == 0 {
		return nil
	}
	metadata := make([]map[string]string, 0, len(files))
	for _, file := range files {
		metadata = append(metadata, map[string]string{
			"label": file.Label,
			"path":  file.Path,
			"url":   file.URL,
		})
	}
	return metadata
}

func existingSkillOptCandidateReviewPublishedFiles(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, repo github.Repository, candidateID string) []skillOptTrainCandidateReviewFile {
	for _, metadataJSON := range []string{iteration.MetadataJSON, session.MetadataJSON} {
		review := decodedSkillOptMetadataValue(decodedSkillOptMetadata(metadataJSON)["candidate_review"])
		if metadataString(review, "candidate_version") != candidateID {
			continue
		}
		if firstNonEmpty(metadataString(review, "issue_repo"), metadataString(review, "pull_request_repo")) != repo.FullName() {
			continue
		}
		if len(metadataSlice(review["file_publish_errors"])) > 0 {
			continue
		}
		files := skillOptCandidateReviewFilesFromMetadata(review["published_files"])
		if len(files) > 0 {
			return files
		}
	}
	return nil
}

func skillOptCandidateReviewFilesFromMetadata(value any) []skillOptTrainCandidateReviewFile {
	values := metadataSlice(value)
	if len(values) == 0 {
		return nil
	}
	files := make([]skillOptTrainCandidateReviewFile, 0, len(values))
	for _, raw := range values {
		metadata := decodedSkillOptMetadataValue(raw)
		label := metadataString(metadata, "label")
		path := metadataString(metadata, "path")
		url := metadataString(metadata, "url")
		if label == "" || path == "" {
			continue
		}
		files = append(files, skillOptTrainCandidateReviewFile{
			Label: label,
			Path:  path,
			URL:   url,
		})
	}
	return files
}

func publishSkillOptTrainCandidateSamplePreviews(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) []skillOptTrainCandidateReviewPreview {
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	artifactID := skillOptCandidateSelectionSampleArtifactID(ctx, store, candidateID)
	if candidateID == "" || artifactID == "" {
		return nil
	}
	preview := skillOptTrainCandidateReviewPreview{
		Label:      "Selection sample",
		ArtifactID: artifactID,
		Renderer:   skillopt.TrainPreviewRendererVueVite,
	}
	record, err := store.GetEvalArtifact(ctx, artifactID)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	content, err := artifact.NewStore(paths.ArtifactBlobs).Read(record.Hash)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	bundle, err := skillopt.ParsePreviewBundle(content)
	if err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired && policy.Renderer == skillopt.TrainPreviewRendererVueVite {
			preview.Error = err.Error()
			return []skillOptTrainCandidateReviewPreview{preview}
		}
		textPreview, ok := skillOptTextArtifactPreview(record, content)
		if !ok {
			preview.Error = err.Error()
			return []skillOptTrainCandidateReviewPreview{preview}
		}
		preview.Renderer = firstNonEmpty(strings.TrimSpace(record.Driver), strings.TrimSpace(record.MediaType), "text")
		preview.Content = textPreview
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	if policy.Mode == skillopt.TrainPreviewModeNone || policy.Renderer != skillopt.TrainPreviewRendererVueVite || policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages {
		preview.Error = "candidate sample preview publishing is not configured for vue-vite GitHub Pages"
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	preview.Renderer = bundle.Renderer
	previewRepo, err := previewRepoRecord(ctx, store, policy)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	distDir, cleanup, err := renderVueVitePreviewBundle(ctx, bundle)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	defer func() { _ = cleanup() }()
	route, err := skillOptPreviewRoute(policy.RouteTemplate, session.ID, iteration.ID, "candidate-selection-sample-"+candidateID)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	publication, err := publishGitHubPagesPreview(ctx, previewRepo, route, distDir)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	preview.Route = route
	preview.URL = publication.URL
	preview.Status = publication.PagesStatus
	preview.StatusReason = publication.StatusReason
	return []skillOptTrainCandidateReviewPreview{preview}
}

func skillOptTextArtifactPreview(record db.EvalArtifact, content []byte) (string, bool) {
	if !utf8.Valid(content) {
		return "", false
	}
	mediaType := strings.ToLower(strings.TrimSpace(record.MediaType))
	driver := strings.ToLower(strings.TrimSpace(record.Driver))
	if driver != "" && driver != "text" {
		return "", false
	}
	if !strings.HasPrefix(mediaType, "text/") &&
		mediaType != "application/json" &&
		mediaType != "application/x-ndjson" {
		return "", false
	}
	text := truncateForMetadata(feedback.TextArtifactPreview(string(content)))
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

func skillOptCandidateSelectionSampleArtifactID(ctx context.Context, store *db.Store, candidateID string) string {
	if strings.TrimSpace(candidateID) == "" {
		return ""
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		return ""
	}
	for _, artifactID := range skillOptMetadataStringSlice(decodedSkillOptMetadata(review.SummaryMetadataJSON), "artifact_ids") {
		if strings.HasSuffix(strings.TrimSpace(artifactID), "/candidate-selection-sample") {
			return strings.TrimSpace(artifactID)
		}
	}
	artifactID := strings.TrimSuffix(strings.TrimSpace(review.DiffArtifactID), "/candidate-diff") + "/candidate-selection-sample"
	if strings.TrimSpace(artifactID) == "/candidate-selection-sample" {
		return ""
	}
	if _, err := store.GetEvalArtifact(ctx, artifactID); err == nil {
		return artifactID
	}
	return ""
}

func skillOptMetadataStringSlice(metadata map[string]any, key string) []string {
	values := metadataSlice(metadata[key])
	output := make([]string, 0, len(values))
	for _, value := range values {
		text := strings.TrimSpace(fmt.Sprint(value))
		if text != "" {
			output = append(output, text)
		}
	}
	return output
}

func skillOptCandidateReviewPreviewsMetadata(previews []skillOptTrainCandidateReviewPreview) []map[string]string {
	if len(previews) == 0 {
		return nil
	}
	metadata := make([]map[string]string, 0, len(previews))
	for _, preview := range previews {
		metadata = append(metadata, map[string]string{
			"label":         preview.Label,
			"artifact_id":   preview.ArtifactID,
			"route":         preview.Route,
			"url":           preview.URL,
			"renderer":      preview.Renderer,
			"content":       preview.Content,
			"status":        preview.Status,
			"status_reason": preview.StatusReason,
			"error":         preview.Error,
		})
	}
	return metadata
}

func existingSkillOptCandidateReviewPublishedPreviews(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string) []skillOptTrainCandidateReviewPreview {
	for _, metadataJSON := range []string{iteration.MetadataJSON, session.MetadataJSON} {
		review := decodedSkillOptMetadataValue(decodedSkillOptMetadata(metadataJSON)["candidate_review"])
		if metadataString(review, "candidate_version") != candidateID {
			continue
		}
		previews := skillOptCandidateReviewPreviewsFromMetadata(review["published_previews"])
		if len(previews) > 0 {
			return previews
		}
	}
	return nil
}

func skillOptCandidateReviewPreviewsFromMetadata(value any) []skillOptTrainCandidateReviewPreview {
	values := metadataSlice(value)
	if len(values) == 0 {
		return nil
	}
	previews := make([]skillOptTrainCandidateReviewPreview, 0, len(values))
	for _, raw := range values {
		metadata := decodedSkillOptMetadataValue(raw)
		previews = append(previews, skillOptTrainCandidateReviewPreview{
			Label:        metadataString(metadata, "label"),
			ArtifactID:   metadataString(metadata, "artifact_id"),
			Route:        metadataString(metadata, "route"),
			URL:          metadataString(metadata, "url"),
			Renderer:     metadataString(metadata, "renderer"),
			Content:      metadataString(metadata, "content"),
			Status:       metadataString(metadata, "status"),
			StatusReason: metadataString(metadata, "status_reason"),
			Error:        metadataString(metadata, "error"),
		})
	}
	return previews
}

func skillOptTrainCandidateReviewBody(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, commandHome string, publishedFiles []skillOptTrainCandidateReviewFile, publishedPreviews []skillOptTrainCandidateReviewPreview, filePublishWarnings []string) (string, error) {
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	version, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return "", fmt.Errorf("load candidate version %s: %w", candidateID, err)
	}
	candidateID = strings.TrimSpace(version.ID)
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		return "", fmt.Errorf("load candidate review %s: %w", candidateID, err)
	}
	baseRef := strings.TrimSpace(review.BaseVersionID)
	if baseRef == "" {
		baseRef = strings.TrimSpace(iteration.BaseTemplateVersionID)
	}
	var builder strings.Builder
	builder.WriteString("## SkillOpt Candidate Review\n\n")
	fmt.Fprintf(&builder, "Session: `%s`\n", session.ID)
	fmt.Fprintf(&builder, "Iteration: `%s`\n", iteration.ID)
	fmt.Fprintf(&builder, "Template: `%s`\n", session.TemplateID)
	fmt.Fprintf(&builder, "Base: `%s`\n", emptyText(baseRef))
	fmt.Fprintf(&builder, "Candidate: `%s`\n", candidateID)
	if summary := strings.TrimSpace(review.PreferenceSummary); summary != "" {
		fmt.Fprintf(&builder, "\n### Candidate Summary\n%s\n", summary)
	}
	skillOptWriteCandidateReviewScores(&builder, review)
	if strings.TrimSpace(session.PreviewRepo) != "" {
		fmt.Fprintf(&builder, "\nPreview repo: `%s`\n", session.PreviewRepo)
	}
	if strings.TrimSpace(iteration.PullRequestURL) != "" {
		fmt.Fprintf(&builder, "\nCandidate PR: %s\n", iteration.PullRequestURL)
	}
	if artifactIDs := skillOptCandidateReviewArtifactIDs(review); len(artifactIDs) > 0 {
		builder.WriteString("\n### Artifacts\n")
		for _, artifactID := range artifactIDs {
			fmt.Fprintf(&builder, "- `%s`\n", artifactID)
		}
	}
	if len(publishedFiles) > 0 {
		builder.WriteString("\n### GitHub Files\n")
		for _, file := range publishedFiles {
			label := strings.TrimSpace(file.Label)
			if label == "" {
				label = "File"
			}
			if strings.TrimSpace(file.URL) != "" {
				fmt.Fprintf(&builder, "- %s: [%s](%s)\n", label, file.Path, file.URL)
			} else {
				fmt.Fprintf(&builder, "- %s: `%s`\n", label, file.Path)
			}
		}
	}
	builder.WriteString("\n### Candidate Sample Preview\n")
	if len(publishedPreviews) == 0 {
		builder.WriteString("- Preview: no selected candidate sample artifact was available to publish.\n")
	} else {
		writeSkillOptCandidateSamplePreviewTable(&builder, publishedPreviews)
	}
	skillOptWriteCandidateReviewFinalEval(&builder, review)
	if len(filePublishWarnings) > 0 {
		if len(publishedFiles) == 0 {
			builder.WriteString("\n### GitHub Files\n")
		}
		for _, warning := range filePublishWarnings {
			fmt.Fprintf(&builder, "- File publish warning: `%s`\n", truncateForMetadata(warning))
		}
	}
	if strings.TrimSpace(review.EvalReportJSON) != "" {
		builder.WriteString("\nEval report: stored with the pending candidate review record.\n")
	}
	builder.WriteString("\n### Decision\n")
	usesCustomHome := strings.TrimSpace(commandHome) != ""
	promotable, reason := skillOptCandidateReviewPromotability(review)
	if promotable {
		fmt.Fprintf(&builder, "- Promote: `%s`\n", skillOptTrainCandidateDecisionCommand(usesCustomHome, session.ID, "--promote", candidateID, false))
	} else {
		fmt.Fprintf(&builder, "- Promote: unavailable because %s.\n", reason)
	}
	fmt.Fprintf(&builder, "- Reject: `%s`\n", skillOptTrainCandidateDecisionCommand(usesCustomHome, session.ID, "--reject", candidateID, true))
	fmt.Fprintf(&builder, "- Wait: take no action; `%s` will keep reporting that a candidate decision is required.\n", skillOptTrainStatusCommand(usesCustomHome, session.ID))
	fmt.Fprintf(&builder, "- Keep improving: reject with an actionable reason, then run `%s` after the rejection completes.\n", skillOptTrainStartNextCommand(usesCustomHome, session.ID))
	return builder.String(), nil
}

func writeSkillOptCandidateSamplePreviewTable(builder *strings.Builder, previews []skillOptTrainCandidateReviewPreview) {
	builder.WriteString("| Sample | Preview | Artifact | Renderer | Status |\n")
	builder.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, preview := range previews {
		label := firstNonEmpty(strings.TrimSpace(preview.Label), "Selection sample")
		fmt.Fprintf(
			builder,
			"| %s | %s | %s | %s | %s |\n",
			skillOptMarkdownTableCell(label),
			skillOptCandidateSamplePreviewCell(preview),
			skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.ArtifactID)),
			skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.Renderer)),
			skillOptCandidateSamplePreviewStatusCell(preview),
		)
	}
}

func skillOptCandidateSamplePreviewCell(preview skillOptTrainCandidateReviewPreview) string {
	if strings.TrimSpace(preview.Error) != "" {
		return skillOptMarkdownTableCell(skillOptMarkdownInlineCode("publish failed"))
	}
	if strings.TrimSpace(preview.Content) != "" {
		return skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.Content))
	}
	if strings.TrimSpace(preview.URL) != "" {
		return skillOptMarkdownTableCell(fmt.Sprintf("[open](%s)", strings.TrimSpace(preview.URL)))
	}
	return skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.ArtifactID))
}

func skillOptCandidateSamplePreviewStatusCell(preview skillOptTrainCandidateReviewPreview) string {
	if strings.TrimSpace(preview.Error) != "" {
		return skillOptMarkdownTableCell(skillOptMarkdownInlineCode("publish failed: " + truncateForMetadata(preview.Error)))
	}
	statusText := strings.TrimSpace(preview.Status)
	if statusText == "" {
		return "-"
	}
	if strings.TrimSpace(preview.StatusReason) != "" {
		statusText += ": " + strings.TrimSpace(preview.StatusReason)
	}
	return skillOptMarkdownTableCell(skillOptMarkdownInlineCode(truncateForMetadata(statusText)))
}

func skillOptWriteCandidateReviewScores(builder *strings.Builder, review db.AgentTemplateCandidateReview) {
	evalReport := decodedSkillOptMetadata(review.EvalReportJSON)
	summaryMetadata := decodedSkillOptMetadata(review.SummaryMetadataJSON)
	builder.WriteString("\n### Scores And Gate\n")
	fmt.Fprintf(builder, "- Selection score: `%s`\n", scoreText(review.Score))
	skillOptWriteCandidateReviewScoreLine(builder, "Best selection hard", metadataFloatPtr(evalReport, "best_selection_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Best selection soft", metadataFloatPtr(evalReport, "best_selection_soft"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline selection hard", metadataFloatPtr(evalReport, "baseline_selection_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline selection soft", metadataFloatPtr(evalReport, "baseline_selection_soft"))
	if score := metadataFloatPtr(evalReport, "score"); score != nil {
		fmt.Fprintf(builder, "- Test score: `%s`\n", scoreText(score))
	} else {
		builder.WriteString("- Test score: `-`\n")
	}
	if hard := metadataFloatPtr(evalReport, "hard"); hard != nil {
		fmt.Fprintf(builder, "- Hard score: `%s`\n", scoreText(hard))
	}
	if soft := metadataFloatPtr(evalReport, "soft"); soft != nil {
		fmt.Fprintf(builder, "- Soft score: `%s`\n", scoreText(soft))
	}
	skillOptWriteCandidateReviewScoreLine(builder, "Test hard", metadataFloatPtr(evalReport, "test_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Test soft", metadataFloatPtr(evalReport, "test_soft"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline test hard", metadataFloatPtr(evalReport, "baseline_test_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline test soft", metadataFloatPtr(evalReport, "baseline_test_soft"))
	if dimensions := metadataScoreMap(evalReport, "dimension_scores"); len(dimensions) > 0 {
		labels := make([]string, 0, len(dimensions))
		for label := range dimensions {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		parts := make([]string, 0, len(labels))
		for _, label := range labels {
			score := dimensions[label]
			parts = append(parts, fmt.Sprintf("%s=%s", label, scoreText(&score)))
		}
		fmt.Fprintf(builder, "- Dimension scores: `%s`\n", strings.Join(parts, ", "))
	}
	fmt.Fprintf(builder, "- Gate status: `%s`\n", firstNonEmpty(
		metadataString(evalReport, "gate_status"),
		metadataString(evalReport, "gate"),
		metadataString(summaryMetadata, "gate_status"),
		metadataString(summaryMetadata, "gate"),
		"unknown",
	))
	fmt.Fprintf(builder, "- No-op status: `%s`\n", skillOptCandidateReviewNoOpStatus(evalReport, summaryMetadata))
	promotable, reason := skillOptCandidateReviewPromotability(review)
	if promotable {
		builder.WriteString("- Promotability: `promotable`\n")
	} else {
		fmt.Fprintf(builder, "- Promotability: `not promotable: %s`\n", reason)
	}
}

func skillOptWriteCandidateReviewFinalEval(builder *strings.Builder, review db.AgentTemplateCandidateReview) {
	evalReport := decodedSkillOptMetadata(review.EvalReportJSON)
	enabled := metadataBool(evalReport, "final_eval_enabled")
	ran := metadataBool(evalReport, "final_eval_ran")
	skippedReason := metadataString(evalReport, "final_test_skipped_reason")
	if !enabled {
		builder.WriteString("- Final eval: `disabled`\n")
		builder.WriteString("  - Reason: candidate review uses selection eval by default.\n")
		return
	}
	if !ran {
		fmt.Fprintf(builder, "- Final eval: `enabled, skipped%s`\n", finalEvalReasonSuffix(skippedReason))
		return
	}
	builder.WriteString("- Final eval: `enabled, ran`\n")
	skillOptWriteCandidateReviewScoreLine(builder, "  - Final test hard", metadataFloatPtr(evalReport, "test_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "  - Final test soft", metadataFloatPtr(evalReport, "test_soft"))
}

func finalEvalReasonSuffix(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	return ": " + reason
}

func skillOptWriteCandidateReviewScoreLine(builder *strings.Builder, label string, score *float64) {
	if score != nil {
		fmt.Fprintf(builder, "- %s: `%s`\n", label, scoreText(score))
	}
}

func skillOptCandidateReviewPromotability(review db.AgentTemplateCandidateReview) (bool, string) {
	for _, metadata := range []map[string]any{
		decodedSkillOptMetadata(review.EvalReportJSON),
		decodedSkillOptMetadata(review.SummaryMetadataJSON),
	} {
		if promotable := metadataBoolPtr(metadata, "promotable"); promotable != nil && !*promotable {
			reason := metadataString(metadata, "no_candidate_reason")
			if reason == "" {
				reason = metadataString(metadata, "promotability_reason")
			}
			if reason == "" {
				reason = metadataString(metadata, "reason")
			}
			if reason == "" {
				reason = skillOptCandidateReviewNoOpMetadataReason(metadata)
			}
			if reason == "" {
				reason = "candidate metadata marks it as not promotable"
			}
			return false, reason
		}
		if skillOptCandidateReviewExplicitPromotable(metadata) {
			continue
		}
		if reason := metadataString(metadata, "no_candidate_reason"); reason != "" {
			return false, reason
		}
		if reason := skillOptCandidateReviewNoOpMetadataReason(metadata); reason != "" {
			return false, reason
		}
	}
	return true, ""
}

func skillOptCandidateReviewNoOpStatus(evalReport map[string]any, summaryMetadata map[string]any) string {
	for _, metadata := range []map[string]any{evalReport, summaryMetadata} {
		if skillOptCandidateReviewExplicitPromotable(metadata) {
			continue
		}
		if reason := metadataString(metadata, "no_candidate_reason"); reason != "" {
			return "blocked: " + reason
		}
	}
	for _, metadata := range []map[string]any{summaryMetadata, evalReport} {
		if skillOptCandidateReviewExplicitPromotable(metadata) {
			continue
		}
		if reason := skillOptCandidateReviewNoOpMetadataReason(metadata); reason != "" {
			return "blocked: " + reason
		}
	}
	bestOrigin := firstNonEmpty(metadataString(summaryMetadata, "best_origin"), metadataString(evalReport, "best_origin"))
	totalAccepts := firstNonEmpty(metadataString(summaryMetadata, "total_accepts"), metadataString(evalReport, "total_accepts"))
	if bestOrigin != "" || totalAccepts != "" {
		parts := []string{"not detected"}
		if bestOrigin != "" {
			parts = append(parts, "best_origin="+bestOrigin)
		}
		if totalAccepts != "" {
			parts = append(parts, "total_accepts="+totalAccepts)
		}
		return strings.Join(parts, "; ")
	}
	return "not reported"
}

func skillOptCandidateReviewExplicitPromotable(metadata map[string]any) bool {
	promotable := metadataBoolPtr(metadata, "promotable")
	return promotable != nil && *promotable
}

func skillOptCandidateReviewNoOpMetadataReason(metadata map[string]any) string {
	if strings.EqualFold(metadataString(metadata, "best_origin"), "initial_skill") {
		return "best_origin_initial_skill"
	}
	if metadataString(metadata, "total_accepts") == "0" {
		return "total_accepts_zero"
	}
	return ""
}

func metadataFloatPtr(metadata map[string]any, key string) *float64 {
	switch value := metadata[key].(type) {
	case float64:
		return &value
	case int:
		score := float64(value)
		return &score
	case int64:
		score := float64(value)
		return &score
	case json.Number:
		if score, err := value.Float64(); err == nil {
			return &score
		}
	case string:
		if score, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			return &score
		}
	}
	return nil
}

func metadataBoolPtr(metadata map[string]any, key string) *bool {
	switch value := metadata[key].(type) {
	case bool:
		return &value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "yes":
			parsed := true
			return &parsed
		case "false", "no":
			parsed := false
			return &parsed
		}
	}
	return nil
}

func metadataBool(metadata map[string]any, key string) bool {
	value := metadataBoolPtr(metadata, key)
	return value != nil && *value
}

func metadataScoreMap(metadata map[string]any, key string) map[string]float64 {
	raw, ok := metadata[key].(map[string]any)
	if !ok {
		return nil
	}
	scores := map[string]float64{}
	for label, value := range raw {
		nested := map[string]any{"score": value}
		if score := metadataFloatPtr(nested, "score"); score != nil {
			scores[strings.TrimSpace(label)] = *score
		}
	}
	return scores
}

func skillOptCandidateReviewArtifactIDs(review db.AgentTemplateCandidateReview) []string {
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(review.DiffArtifactID)
	metadata := decodedSkillOptMetadata(review.SummaryMetadataJSON)
	rawIDs, ok := metadata["artifact_ids"].([]any)
	if !ok {
		return ids
	}
	for _, rawID := range rawIDs {
		add(fmt.Sprint(rawID))
	}
	return ids
}

func skillOptTrainCandidateDecisionCommand(usesCustomHome bool, sessionID, decisionFlag, candidateID string, includeReason bool) string {
	args := []string{"gitmoot", "skillopt", "train", "continue"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID), decisionFlag, strings.TrimSpace(candidateID))
	if includeReason {
		args = append(args, "--reason", "...")
	}
	return shellArgs(args)
}

func skillOptTrainStartNextCommand(usesCustomHome bool, sessionID string) string {
	args := []string{"gitmoot", "skillopt", "train", "continue"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID), "--start-next")
	return shellArgs(args)
}

func skillOptTrainStatusCommand(usesCustomHome bool, sessionID string) string {
	args := []string{"gitmoot", "skillopt", "train", "status"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID))
	return shellArgs(args)
}

func decideSkillOptTrainCandidate(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) (skillOptTrainCandidateDecisionResult, error) {
	promote := strings.TrimSpace(request.PromoteCandidate)
	reject := strings.TrimSpace(request.RejectCandidate)
	if promote != "" && reject != "" {
		return skillOptTrainCandidateDecisionResult{}, errors.New("train continue accepts only one of --promote or --reject")
	}
	expected := strings.TrimSpace(iteration.CandidateVersionID)
	if expected == "" {
		return skillOptTrainCandidateDecisionResult{}, errors.New("train iteration has no candidate version")
	}
	candidateID := promote
	decision := ""
	if promote != "" {
		decision = "promoted"
	} else if reject != "" {
		candidateID = reject
		decision = "rejected"
		if strings.TrimSpace(request.DecisionReason) == "" {
			return skillOptTrainCandidateDecisionResult{}, errors.New("train candidate rejection requires --reason")
		}
	}
	if candidateID != "" && candidateID != expected {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("candidate %s does not match train iteration candidate %s", candidateID, expected)
	}
	if decision == "promoted" {
		if err := validateSkillOptTrainCandidatePromotableForDecision(ctx, store, candidateID); err != nil {
			return skillOptTrainCandidateDecisionResult{}, err
		}
	}
	if decision == "" {
		return syncSkillOptTrainCandidateDecision(ctx, store, session, iteration, expected, "", "")
	}
	if result, err := syncSkillOptTrainCandidateDecision(ctx, store, session, iteration, expected, decision, strings.TrimSpace(request.DecisionReason)); err != nil || result.Decided {
		return result, err
	}
	if err := skillopt.CanTransitionTrainIteration(iteration.State, map[string]string{
		"promoted": skillopt.TrainStateCandidatePromoted,
		"rejected": skillopt.TrainStateCandidateRejected,
	}[decision]); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	if decision == "promoted" {
		session.TemplateVersionID = candidateID
		session.State = skillopt.TrainStateCandidatePromoted
		iteration.State = skillopt.TrainStateCandidatePromoted
	} else {
		session.State = skillopt.TrainStateCandidateRejected
		iteration.State = skillopt.TrainStateCandidateRejected
		iteration.DecisionReason = strings.TrimSpace(request.DecisionReason)
	}
	metadata := map[string]any{
		"decision":          decision,
		"candidate_version": candidateID,
		"reason":            strings.TrimSpace(request.DecisionReason),
		"decided_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"source":            "gitmoot skillopt train continue",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_decision", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_decision", metadata)
	if _, err := store.DecideSkillOptTrainCandidate(ctx, session, iteration, candidateID, decision); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	return skillOptTrainCandidateDecisionResult{Decided: true, Decision: decision, CandidateVersionID: candidateID}, nil
}

func validateSkillOptTrainCandidatePromotableForDecision(ctx context.Context, store *db.Store, candidateID string) error {
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("load candidate review %s: %w", candidateID, err)
	}
	promotable, reason := skillOptCandidateReviewPromotability(review)
	if promotable {
		return nil
	}
	return fmt.Errorf("candidate %s is not promotable: %s", candidateID, reason)
}

func syncSkillOptTrainCandidateDecision(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string, expectedDecision string, fallbackReason string) (skillOptTrainCandidateDecisionResult, error) {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return skillOptTrainCandidateDecisionResult{}, nil
	}
	candidate, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("load candidate version %s: %w", candidateID, err)
	}
	review, reviewErr := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if reviewErr != nil && !errors.Is(reviewErr, sql.ErrNoRows) {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("load candidate review %s: %w", candidateID, reviewErr)
	}
	var decision string
	switch candidate.State {
	case "current":
		decision = "promoted"
	case "rejected":
		decision = "rejected"
	default:
		if reviewErr == nil {
			switch strings.TrimSpace(review.State) {
			case "promoted":
				decision = "promoted"
			case "rejected":
				decision = "rejected"
			}
		}
		if decision == "" {
			return skillOptTrainCandidateDecisionResult{}, nil
		}
	}
	if expectedDecision != "" && expectedDecision != decision {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("candidate %s is already %s, not %s", candidateID, decision, expectedDecision)
	}
	targetState := map[string]string{
		"promoted": skillopt.TrainStateCandidatePromoted,
		"rejected": skillopt.TrainStateCandidateRejected,
	}[decision]
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished:
	default:
		if err := skillopt.CanTransitionTrainIteration(iteration.State, targetState); err != nil {
			return skillOptTrainCandidateDecisionResult{}, err
		}
	}
	reason := strings.TrimSpace(fallbackReason)
	if decision == "rejected" {
		if reviewErr == nil && strings.TrimSpace(review.DecisionReason) != "" {
			reason = strings.TrimSpace(review.DecisionReason)
		}
		if reason == "" {
			return skillOptTrainCandidateDecisionResult{}, errors.New("train candidate rejection requires --reason")
		}
	}
	if decision == "promoted" {
		session.TemplateVersionID = candidateID
		session.State = skillopt.TrainStateCandidatePromoted
		iteration.State = skillopt.TrainStateCandidatePromoted
	} else {
		session.State = skillopt.TrainStateCandidateRejected
		iteration.State = skillopt.TrainStateCandidateRejected
		iteration.DecisionReason = reason
	}
	metadata := map[string]any{
		"decision":          decision,
		"candidate_version": candidateID,
		"reason":            reason,
		"decided_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"source":            "gitmoot skillopt train continue synced candidate state",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_decision", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_decision", metadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	return skillOptTrainCandidateDecisionResult{Decided: true, Decision: decision, CandidateVersionID: candidateID}, nil
}

func startNextSkillOptTrainIteration(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, previous db.SkillOptTrainIteration) (db.SkillOptTrainIteration, error) {
	if err := skillopt.CanStartNextTrainIteration(previous); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	releaseStartNextLock, _, err := acquireSkillOptTrainStartNextLock(ctx, store, session.ID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	defer func() {
		_ = releaseStartNextLock(context.Background())
	}()
	baseVersion := strings.TrimSpace(previous.BaseTemplateVersionID)
	if skillopt.NormalizeTrainState(previous.State) == skillopt.TrainStateCandidatePromoted {
		baseVersion = strings.TrimSpace(previous.CandidateVersionID)
	}
	if baseVersion == "" {
		return db.SkillOptTrainIteration{}, errors.New("next train iteration base version is required")
	}
	previousRun, err := store.GetEvalRun(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("load previous eval run %s: %w", previous.EvalRunID, err)
	}
	iterations, err := store.ListSkillOptTrainIterations(ctx, session.ID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	nextNumber := len(iterations) + 1
	nextID := fmt.Sprintf("%s-%03d", session.ID, nextNumber)
	nextRunID := fmt.Sprintf("%s-review-%03d", session.ID, nextNumber)
	if _, err := store.GetSkillOptTrainIteration(ctx, nextID); err == nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("train iteration %s already exists; inspect it with gitmoot skillopt train status --session %s", nextID, session.ID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.SkillOptTrainIteration{}, err
	}
	if _, err := store.GetEvalRun(ctx, nextRunID); err == nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("eval run %s already exists; inspect it with gitmoot skillopt review status --run %s", nextRunID, nextRunID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.SkillOptTrainIteration{}, err
	}
	metadata := skillOptTrainNextIterationMetadata(session.MetadataJSON, previous.MetadataJSON, map[string]any{
		"id":         previous.ID,
		"state":      previous.State,
		"candidate":  previous.CandidateVersionID,
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	items, err := store.ListEvalReviewItems(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	feedbackEvents, err := store.ListFeedbackEvents(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	rankedFeedbackEvents, err := store.ListRankedFeedbackEvents(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	recommendation := skillopt.RecommendPhaseForItems(previousRun, items, feedbackEvents, rankedFeedbackEvents, pairwisePreferences)
	nextMode := skillOptTrainNextIterationMode(previous.Mode, recommendation.RecommendedMode)
	nextExplorationLevel := strings.TrimSpace(recommendation.ExplorationLevel)
	if nextExplorationLevel == "" {
		nextExplorationLevel = previous.ExplorationLevel
	}
	metadata = mergeSkillOptTrainMetadata(metadata, "phase_recommendation", map[string]any{
		"current_mode":     recommendation.CurrentMode,
		"recommended_mode": recommendation.RecommendedMode,
		"selected_mode":    nextMode,
		"reason":           recommendation.Reason,
	})
	next := db.SkillOptTrainIteration{
		ID:                    nextID,
		SessionID:             session.ID,
		EvalRunID:             nextRunID,
		BaseTemplateVersionID: baseVersion,
		Mode:                  nextMode,
		ExplorationLevel:      nextExplorationLevel,
		State:                 skillopt.TrainStateItemsReady,
		MetadataJSON:          metadata,
	}
	run := db.EvalRun{
		ID:                nextRunID,
		TemplateID:        session.TemplateID,
		TemplateVersionID: baseVersion,
		TargetRepo:        session.TargetRepo,
		State:             "review",
		Mode:              nextMode,
		ExplorationLevel:  nextExplorationLevel,
		OptionsCount:      previousRun.OptionsCount,
		MetadataJSON:      metadata,
	}
	session.TemplateVersionID = baseVersion
	session.State = skillopt.TrainStateItemsReady
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "next_iteration", map[string]any{
		"id":           next.ID,
		"base_version": baseVersion,
		"source":       "gitmoot skillopt train continue",
	})
	nextItems := make([]db.EvalReviewItem, 0, len(items))
	for _, item := range items {
		item.RunID = nextRunID
		item.ID = ""
		item.BaselineArtifactID = ""
		item.CandidateArtifactID = ""
		item.PreviewArtifactID = ""
		item.DiffArtifactID = ""
		nextItems = append(nextItems, item)
	}
	if err := store.UpsertSkillOptTrainNextIteration(ctx, session, next, run, nextItems); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	return next, nil
}

func skillOptTrainNextIterationMetadata(sessionMetadata string, previousMetadata string, previousIteration map[string]any) string {
	metadata := map[string]any{
		"previous_iteration": previousIteration,
	}
	for _, source := range []string{previousMetadata, sessionMetadata} {
		evaluation := decodedSkillOptMetadataValue(decodedSkillOptMetadata(source)["evaluation"])
		if len(evaluation) > 0 {
			metadata["evaluation"] = evaluation
			break
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func skillOptTrainNextIterationMode(previousMode string, recommendedMode string) string {
	switch strings.TrimSpace(recommendedMode) {
	case db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate:
		return strings.TrimSpace(recommendedMode)
	default:
		return strings.TrimSpace(previousMode)
	}
}

func skillOptTrainContinueNeedsOptimizerPreflight(phase string, request skillOptTrainOptimizerRequest) bool {
	if request.ExportOnly {
		return false
	}
	switch strings.TrimSpace(phase) {
	case skillopt.TrainStateFeedbackSynced, skillopt.TrainStateTrainingPackageCreated:
		return true
	case skillopt.TrainStateOptimizerCompleted, skillopt.TrainStateOptimizerCompletedNoCandidate:
		return request.RerunOptimizer
	default:
		return false
	}
}

func preflightSkillOptTrainOptimizerForContinue(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerResult, error) {
	resolvedRequest, backendResolution, err := resolveSkillOptTrainBackendRequest(request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	request = resolvedRequest
	optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	result := skillOptTrainOptimizerResult{
		TrainingPackagePath:  optimizerPaths.TrainingPackagePath,
		OutRoot:              optimizerPaths.OutRoot,
		CandidatePackagePath: optimizerPaths.CandidatePackagePath,
		ArtifactDir:          optimizerPaths.ArtifactDir,
		OptimizerRoot:        optimizerPaths.OptimizerRoot,
		OptimizerAttempt:     optimizerPaths.OptimizerAttempt,
		OptimizerAttemptPath: optimizerPaths.OptimizerAttemptPath,
		Command:              skillOptTrainOptimizerExecutable(request),
		DryRun:               request.DryRun,
		Request:              request,
		BackendResolution:    backendResolution,
		RecoveryAvailable:    skillOptTrainOptimizerRecoveryAvailable(optimizerPaths),
		OptimizerLockState:   skillOptTrainOptimizerLockState(request),
	}
	command, preflightResult, err := preflightSkillOptTrainOptimizerExecutable(ctx, request)
	if strings.TrimSpace(command) != "" {
		result.Command = command
	}
	if err != nil {
		result.NextAction = skillOptTrainSkillOptInstallNextAction()
		if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, result.Command, nil, preflightResult, err); metaErr != nil {
			return result, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
		}
		return result, err
	}
	return result, nil
}

func continueSkillOptTrainOptimizer(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, progress io.Writer) (skillOptTrainOptimizerResult, error) {
	if strings.TrimSpace(iteration.EvalRunID) == "" {
		return skillOptTrainOptimizerResult{}, fmt.Errorf("train iteration %s has no eval run id", iteration.ID)
	}
	resolvedRequest, backendResolution, err := resolveSkillOptTrainBackendRequest(request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	request = resolvedRequest
	optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, iteration, request)
	if err != nil {
		return skillOptTrainOptimizerResult{}, err
	}
	result := skillOptTrainOptimizerResult{
		TrainingPackagePath:  optimizerPaths.TrainingPackagePath,
		OutRoot:              optimizerPaths.OutRoot,
		CandidatePackagePath: optimizerPaths.CandidatePackagePath,
		ArtifactDir:          optimizerPaths.ArtifactDir,
		OptimizerRoot:        optimizerPaths.OptimizerRoot,
		OptimizerAttempt:     optimizerPaths.OptimizerAttempt,
		OptimizerAttemptPath: optimizerPaths.OptimizerAttemptPath,
		DryRun:               request.DryRun,
		Request:              request,
		BackendResolution:    backendResolution,
		RecoveryAvailable:    skillOptTrainOptimizerRecoveryAvailable(optimizerPaths),
		OptimizerLockState:   skillOptTrainOptimizerLockState(request),
	}
	state := skillopt.NormalizeTrainState(iteration.State)
	rerunFromCompletedOptimizer := request.RerunOptimizer && (state == skillopt.TrainStateOptimizerCompleted || state == skillopt.TrainStateOptimizerCompletedNoCandidate)
	if rerunFromCompletedOptimizer {
		state = skillopt.TrainStateTrainingPackageCreated
	}
	if state == skillopt.TrainStateFeedbackSynced {
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateTrainingPackageCreated); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		exportMetadata, err := exportSkillOptTrainPackage(ctx, store, iteration, optimizerPaths, request)
		if err != nil {
			return result, err
		}
		session.State = skillopt.TrainStateTrainingPackageCreated
		iteration.State = skillopt.TrainStateTrainingPackageCreated
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", exportMetadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", exportMetadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return result, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return result, err
		}
		state = skillopt.TrainStateTrainingPackageCreated
	}
	if request.ExportOnly && state == skillopt.TrainStateTrainingPackageCreated {
		// The training package now exists (exported above or already created)
		// and the optimizer has not run yet. Stop here so the caller can inspect
		// it before paying for a real optimizer run. For later states (the
		// optimizer already ran) export-only is a no-op and the normal
		// candidate-import path below proceeds.
		result.ExportedOnly = true
		return result, nil
	}
	if state == skillopt.TrainStateTrainingPackageCreated {
		if !rerunFromCompletedOptimizer {
			if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptimizerCompleted); err != nil {
				return skillOptTrainOptimizerResult{}, err
			}
		}
		command, args, err := buildSkillOptTrainOptimizerCommand(iteration, request, optimizerPaths)
		if err != nil {
			if rerunFromCompletedOptimizer {
				return result, err
			}
			if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, command, args, subprocess.Result{}, err); metaErr != nil {
				return result, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
			}
			return result, err
		}
		if request.RerunOptimizer {
			exportMetadata, err := exportSkillOptTrainPackage(ctx, store, iteration, optimizerPaths, request)
			if err != nil {
				return result, err
			}
			session.State = skillopt.TrainStateTrainingPackageCreated
			iteration.State = skillopt.TrainStateTrainingPackageCreated
			session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", exportMetadata)
			iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", exportMetadata)
			if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
				return result, err
			}
			if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
				return result, err
			}
		}
		if err := recordSkillOptTrainOptimizerStarted(ctx, store, &session, &iteration, request, optimizerPaths, command, args); err != nil {
			return result, err
		}
		result.Command = command
		result.Args = args
		// One serialized writer shared by the launch banner, the heartbeat
		// ticker, and the optimizer's streamed output: they write concurrently
		// and the destination is not necessarily concurrency-safe.
		sharedProgress := subprocess.SyncWriter(progress)
		announceSkillOptTrainOptimizerLaunch(sharedProgress, request)
		stopHeartbeat := startSkillOptTrainOptimizerHeartbeat(sharedProgress)
		runResult, err := runSkillOptTrainOptimizer(ctx, sharedProgress, optimizerPaths, request, command, args)
		stopHeartbeat()
		result.RecoveryAvailable = skillOptTrainOptimizerRecoveryAvailable(optimizerPaths)
		if err != nil {
			if metaErr := recordSkillOptTrainOptimizerFailure(ctx, store, session, iteration, request, optimizerPaths, command, args, runResult, err); metaErr != nil {
				return result, fmt.Errorf("%w; failed to record optimizer failure: %v", err, metaErr)
			}
			return result, err
		}
		metadata := skillOptTrainOptimizerMetadata(request, optimizerPaths, command, args, runResult, "succeeded", nil)
		session.State = skillopt.TrainStateOptimizerCompleted
		iteration.State = skillopt.TrainStateOptimizerCompleted
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return result, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return result, err
		}
		state = skillopt.TrainStateOptimizerCompleted
	}
	if state == skillopt.TrainStateOptimizerCompleted {
		if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateCandidateCreated); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		version, err := importSkillOptTrainCandidate(ctx, paths, store, session, iteration, optimizerPaths)
		if err != nil {
			if errors.Is(err, skillopt.ErrNoCandidate) {
				reason, nextAction := skillOptNoCandidateReasonAndNextAction(err, optimizerPaths.CandidatePackagePath)
				if metaErr := recordSkillOptTrainNoCandidate(ctx, store, session, iteration, optimizerPaths, reason); metaErr != nil {
					return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record no-candidate result: %v", err, metaErr)
				}
				result.NoCandidateReason = reason
				result.NoCandidateNextAction = nextAction
				return result, nil
			}
			if metaErr := recordSkillOptTrainCandidateImportFailure(ctx, store, session, iteration, optimizerPaths, err); metaErr != nil {
				return skillOptTrainOptimizerResult{}, fmt.Errorf("%w; failed to record candidate import failure: %v", err, metaErr)
			}
			return skillOptTrainOptimizerResult{}, err
		}
		result.CandidateVersionID = version.ID
		metadata := map[string]any{
			"status":                 "succeeded",
			"candidate_version":      version.ID,
			"candidate_package":      optimizerPaths.CandidatePackagePath,
			"artifact_dir":           optimizerPaths.ArtifactDir,
			"optimizer_root":         optimizerPaths.OptimizerRoot,
			"optimizer_attempt":      optimizerPaths.OptimizerAttempt,
			"optimizer_attempt_path": optimizerPaths.OptimizerAttemptPath,
			"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
			"source":                 "gitmoot skillopt train continue",
		}
		session.State = skillopt.TrainStateCandidateCreated
		iteration.State = skillopt.TrainStateCandidateCreated
		iteration.CandidateVersionID = version.ID
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
		if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
			return skillOptTrainOptimizerResult{}, err
		}
		return result, nil
	}
	return skillOptTrainOptimizerResult{}, fmt.Errorf("train iteration %s is at %s; expected %s, %s, or %s", iteration.ID, iteration.State, skillopt.TrainStateFeedbackSynced, skillopt.TrainStateTrainingPackageCreated, skillopt.TrainStateOptimizerCompleted)
}

func skillOptTrainOptimizerLockState(request skillOptTrainOptimizerRequest) string {
	state := strings.TrimSpace(request.OptimizerLockState)
	if state == "" {
		return "acquired"
	}
	return state
}

type skillOptTrainGenerationResult struct {
	GeneratedOptions int
	JobIDs           []string
	AgentName        string
	Runtime          string
	Metadata         map[string]any
}

var errSkillOptTrainGenerationBusy = errors.New("skillopt train generation is already running")

var errSkillOptTrainOptimizerBusy = errors.New("skillopt train optimizer is already running")

var errSkillOptTrainCandidateReviewBusy = errors.New("skillopt train candidate review is already publishing")

var errSkillOptTrainReviewBusy = errors.New("skillopt train review is already publishing")

var errSkillOptTrainStartNextBusy = errors.New("skillopt train next iteration is already starting")

const skillOptTrainGenerationLockTTL = 2 * time.Hour

const skillOptTrainGenerationLockBuffer = 10 * time.Minute

const skillOptTrainReviewOptionRetryBudget = 1

const skillOptTrainOptimizerLockTTL = 4 * time.Hour

const skillOptTrainOptimizerLockBuffer = 10 * time.Minute

const skillOptTrainOptimizerHeartbeatLeaseTTL = 2 * time.Minute

const skillOptTrainOptimizerExpiredHeartbeatGrace = 10 * time.Minute

const skillOptTrainCandidateReviewLockTTL = 30 * time.Minute

const skillOptTrainReviewLockTTL = 30 * time.Minute

const skillOptTrainStartNextLockTTL = 30 * time.Minute

func acquireSkillOptTrainCandidateReviewLock(ctx context.Context, store *db.Store, sessionID string, iterationID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainCandidateReviewLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-candidate-review", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainCandidateReviewLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainCandidateReviewBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainCandidateReviewLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-candidate-review:" + sessionID
	}
	return "skillopt-train-candidate-review:" + sessionID + ":" + iterationID
}

func acquireSkillOptTrainReviewLock(ctx context.Context, store *db.Store, sessionID string, iterationID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainReviewLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-review", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainReviewLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainReviewBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainReviewLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-review:" + sessionID
	}
	return "skillopt-train-review:" + sessionID + ":" + iterationID
}

func acquireSkillOptTrainStartNextLock(ctx context.Context, store *db.Store, sessionID string) (func(context.Context) error, bool, error) {
	key := skillOptTrainStartNextLockKey(sessionID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-start-next", strings.TrimSpace(sessionID))
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		ExpiresAt:   now.Add(skillOptTrainStartNextLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, false, err
	}
	if !acquired {
		return noopAgentReservationRelease, false, fmt.Errorf("%w: %s", errSkillOptTrainStartNextBusy, key)
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}, true, nil
}

func skillOptTrainStartNextLockKey(sessionID string) string {
	return "skillopt-train-start-next:" + strings.TrimSpace(sessionID)
}

func acquireSkillOptTrainOptimizerLock(ctx context.Context, store *db.Store, sessionID string, iterationID string, ttl time.Duration, request skillOptTrainOptimizerRequest) (func(context.Context) error, string, error) {
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, "", err
	}
	legacyToken, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, "", err
	}
	if ttl <= 0 {
		ttl = skillOptTrainOptimizerLockTTL
	}
	leaseTTL := skillOptTrainOptimizerLeaseTTL(ttl)
	now := time.Now().UTC()
	lockKeys := skillOptTrainOptimizerLockKeys(sessionID, iterationID)
	lockState := "acquired"
	for _, existingKey := range lockKeys {
		if existing, err := store.GetResourceLock(ctx, existingKey); err == nil {
			if skillOptTrainOptimizerLockStatus(existing, now) == "stale" {
				released, releaseErr := store.ReleaseResourceLock(ctx, existingKey, existing.OwnerJobID, existing.OwnerToken)
				if releaseErr != nil {
					return noopAgentReservationRelease, "", releaseErr
				}
				if !released {
					return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(existingKey, existing, now)
				}
				lockState = "recovered_stale"
				continue
			}
			return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(existingKey, existing, now)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return noopAgentReservationRelease, "", err
		}
	}
	ownerJobID := localAgentJobID("skillopt-train-optimizer", strings.TrimSpace(sessionID))
	hostname, _ := os.Hostname()
	newKey := skillOptTrainOptimizerLockKey(sessionID, iterationID)
	legacyKey := skillOptTrainLegacyOptimizerLockKey(sessionID, iterationID)
	lockMetadata := db.ResourceLock{
		OwnerJobID:    ownerJobID,
		OwnerToken:    token,
		OwnerPID:      int64(os.Getpid()),
		OwnerHostname: hostname,
		CommandHash:   skillOptTrainOptimizerRequestHash(request),
		ExpiresAt:     now.Add(leaseTTL).Format(time.RFC3339Nano),
	}
	lockMetadata.ResourceKey = newKey
	acquired, err := store.AcquireResourceLock(ctx, lockMetadata, now)
	if err != nil {
		return noopAgentReservationRelease, "", err
	}
	if !acquired {
		if existing, lockErr := store.GetResourceLock(ctx, newKey); lockErr == nil {
			return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(newKey, existing, time.Now().UTC())
		}
		return noopAgentReservationRelease, "", fmt.Errorf("%w: %s", errSkillOptTrainOptimizerBusy, newKey)
	}
	lockMetadata.ResourceKey = legacyKey
	lockMetadata.OwnerToken = legacyToken
	legacyAcquired, err := store.AcquireResourceLock(ctx, lockMetadata, now)
	if err != nil {
		_, _ = store.ReleaseResourceLock(context.Background(), newKey, ownerJobID, token)
		return noopAgentReservationRelease, "", err
	}
	if !legacyAcquired {
		_, _ = store.ReleaseResourceLock(context.Background(), newKey, ownerJobID, token)
		if existing, lockErr := store.GetResourceLock(ctx, legacyKey); lockErr == nil {
			return noopAgentReservationRelease, "", skillOptTrainOptimizerLockBusyError(legacyKey, existing, time.Now().UTC())
		}
		return noopAgentReservationRelease, "", fmt.Errorf("%w: %s", errSkillOptTrainOptimizerBusy, legacyKey)
	}
	stopHeartbeat := startSkillOptTrainOptimizerLockHeartbeat(context.Background(), store, newKey, ownerJobID, token, leaseTTL)
	stopLegacyHeartbeat := startSkillOptTrainOptimizerLockHeartbeat(context.Background(), store, legacyKey, ownerJobID, legacyToken, leaseTTL)
	return func(releaseCtx context.Context) error {
		stopHeartbeat()
		stopLegacyHeartbeat()
		_, err := store.ReleaseResourceLock(releaseCtx, newKey, ownerJobID, token)
		_, legacyErr := store.ReleaseResourceLock(releaseCtx, legacyKey, ownerJobID, legacyToken)
		if err != nil {
			return err
		}
		return legacyErr
	}, lockState, nil
}

func skillOptTrainOptimizerLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train:" + sessionID
	}
	return "skillopt-train:" + sessionID + ":" + iterationID
}

func skillOptTrainLegacyOptimizerLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-optimizer:" + sessionID
	}
	return "skillopt-train-optimizer:" + sessionID + ":" + iterationID
}

func skillOptTrainOptimizerLockKeys(sessionID string, iterationID string) []string {
	return []string{
		skillOptTrainOptimizerLockKey(sessionID, iterationID),
		skillOptTrainLegacyOptimizerLockKey(sessionID, iterationID),
	}
}

func skillOptTrainOptimizerLeaseTTL(maxRuntimeTTL time.Duration) time.Duration {
	if maxRuntimeTTL > 0 && maxRuntimeTTL < skillOptTrainOptimizerHeartbeatLeaseTTL {
		return maxRuntimeTTL
	}
	return skillOptTrainOptimizerHeartbeatLeaseTTL
}

func startSkillOptTrainOptimizerLockHeartbeat(ctx context.Context, store *db.Store, key string, ownerJobID string, ownerToken string, leaseTTL time.Duration) func() {
	heartbeatEvery := skillOptTrainOptimizerHeartbeatInterval(leaseTTL)
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case now := <-ticker.C:
				_, _ = store.HeartbeatResourceLock(context.Background(), key, ownerJobID, ownerToken, now.UTC(), now.UTC().Add(leaseTTL))
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func skillOptTrainOptimizerHeartbeatInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 30 * time.Second
	}
	interval := ttl / 4
	if interval <= 0 {
		return time.Second
	}
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func skillOptTrainOptimizerRequestHash(request skillOptTrainOptimizerRequest) string {
	content, err := json.Marshal(map[string]any{
		"backend":           strings.TrimSpace(request.Backend),
		"model":             strings.TrimSpace(request.Model),
		"optimizer_model":   strings.TrimSpace(request.OptimizerModel),
		"target_model":      strings.TrimSpace(request.TargetModel),
		"optimizer_backend": strings.TrimSpace(request.OptimizerBackend),
		"target_backend":    strings.TrimSpace(request.TargetBackend),
		"evaluator_id":      strings.TrimSpace(request.EvaluatorID),
		"evaluator_model":   strings.TrimSpace(request.EvaluatorModel),
		"evaluator_backend": strings.TrimSpace(request.EvaluatorBackend),
		"skill_update_mode": strings.TrimSpace(request.SkillUpdateMode),
		"num_epochs":        request.NumEpochs,
		"batch_size":        request.BatchSize,
		"optimizer_views": map[string]any{
			"set":   request.OptimizerViewsSet,
			"value": request.OptimizerViews,
		},
		"retry_optimizer_views": map[string]any{
			"set":   request.RetryOptimizerViewsSet,
			"value": strings.TrimSpace(request.RetryOptimizerViews),
		},
		"noop_retry_budget": map[string]any{
			"set":   request.NoopRetryBudgetSet,
			"value": request.NoopRetryBudget,
		},
		"gate_reject_retry_budget": map[string]any{
			"set":   request.GateRejectRetryBudgetSet,
			"value": request.GateRejectRetryBudget,
		},
		"wrong_artifact_retry_budget": map[string]any{
			"set":   request.WrongArtifactRetryBudgetSet,
			"value": request.WrongArtifactRetryBudget,
		},
		"target_artifact_retry_budget": map[string]any{
			"set":   request.TargetArtifactRetryBudgetSet,
			"value": request.TargetArtifactRetryBudget,
		},
		"hard_failure_retry_budget": map[string]any{
			"set":   request.HardFailureRetryBudgetSet,
			"value": request.HardFailureRetryBudget,
		},
		"feedback_direct_mode": strings.TrimSpace(request.FeedbackDirectMode),
		"final_eval":           request.FinalEval,
		"final_eval_set":       request.FinalEvalSet,
		"gate":                 strings.TrimSpace(request.Gate),
		"timeout":              strings.TrimSpace(request.Timeout),
		"dry_run":              request.DryRun,
		"rerun_optimizer":      request.RerunOptimizer,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func skillOptTrainOptimizerLockBusyError(key string, lock db.ResourceLock, now time.Time) error {
	status := skillOptTrainOptimizerLockStatus(lock, now)
	message := fmt.Sprintf("%s (%s owner=%s pid=%s host=%s heartbeat=%s expires=%s elapsed=%s hash=%s)",
		key,
		status,
		emptyText(lock.OwnerJobID),
		skillOptLockPIDText(lock.OwnerPID),
		emptyText(lock.OwnerHostname),
		emptyText(lock.UpdatedAt),
		emptyText(lock.ExpiresAt),
		skillOptLockElapsedText(lock.AcquiredAt, now),
		emptyText(lock.CommandHash),
	)
	return fmt.Errorf("%w: %s", errSkillOptTrainOptimizerBusy, message)
}

func skillOptTrainOptimizerLockStatus(lock db.ResourceLock, now time.Time) string {
	expired := false
	var expiresAt time.Time
	if parsed, ok := parseSkillOptStatusTime(lock.ExpiresAt); ok {
		expiresAt = parsed
		expired = !expiresAt.After(now)
	}
	if expired {
		if !skillOptOwnerPIDLive(lock.OwnerPID) || now.Sub(expiresAt) >= skillOptTrainOptimizerExpiredHeartbeatGrace {
			return "stale"
		}
		return "active_expired_heartbeat"
	}
	return "active"
}

func skillOptOwnerPIDLive(pid int64) bool {
	if pid <= 0 {
		return false
	}
	running, err := processRunning(int(pid))
	return err == nil && running
}

func skillOptLockPIDText(pid int64) string {
	if pid <= 0 {
		return "-"
	}
	return strconv.FormatInt(pid, 10)
}

func skillOptLockElapsedText(acquiredAt string, now time.Time) string {
	acquired, ok := parseSkillOptStatusTime(acquiredAt)
	if !ok {
		return "unknown"
	}
	elapsed := now.Sub(acquired)
	if elapsed < 0 {
		return "unknown"
	}
	return elapsed.Round(time.Second).String()
}

func skillOptTrainOptimizerLockTTLForRequest(request skillOptTrainOptimizerRequest) (time.Duration, error) {
	timeout := strings.TrimSpace(request.Timeout)
	if timeout == "" {
		return skillOptTrainOptimizerLockTTL, nil
	}
	duration, err := time.ParseDuration(timeout)
	if err != nil {
		return 0, fmt.Errorf("parse optimizer timeout: %w", err)
	}
	if duration <= 0 {
		return 0, errors.New("optimizer timeout must be greater than zero")
	}
	ttl := duration + skillOptTrainOptimizerLockBuffer
	if ttl < skillOptTrainOptimizerLockTTL {
		return skillOptTrainOptimizerLockTTL, nil
	}
	return ttl, nil
}

// skillOptTrainNoopExtend is a no-op lock-extend function for failure/automated paths.
func skillOptTrainNoopExtend() error { return nil }

func acquireSkillOptTrainGenerationLock(ctx context.Context, store *db.Store, sessionID string, iterationID string, ttl time.Duration) (release func(context.Context) error, extend func() error, acquired bool, err error) {
	key := skillOptTrainGenerationLockKey(sessionID, iterationID)
	token, err := newRuntimeLockOwnerToken()
	if err != nil {
		return noopAgentReservationRelease, skillOptTrainNoopExtend, false, err
	}
	if ttl <= 0 {
		ttl = skillOptTrainGenerationLockTTL
	}
	now := time.Now().UTC()
	ownerJobID := localAgentJobID("skillopt-train-generation", strings.TrimSpace(sessionID))
	ok, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  ownerJobID,
		OwnerToken:  token,
		OwnerPID:    int64(os.Getpid()),
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		return noopAgentReservationRelease, skillOptTrainNoopExtend, false, err
	}
	if !ok {
		return noopAgentReservationRelease, skillOptTrainNoopExtend, false, fmt.Errorf("%w: %s", errSkillOptTrainGenerationBusy, key)
	}
	release = func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, ownerJobID, token)
		return err
	}
	// extend pushes the lock TTL forward so a long generation run does not
	// outlive the upfront estimate (called from the per-option progress hook).
	extend = func() error {
		extendNow := time.Now().UTC()
		_, err := store.HeartbeatResourceLock(context.Background(), key, ownerJobID, token, extendNow, extendNow.Add(ttl))
		return err
	}
	return release, extend, true, nil
}

func estimateSkillOptTrainGenerationLockTTL(ctx context.Context, store *db.Store, request skillOptTrainContinueRequest, iteration db.SkillOptTrainIteration) (time.Duration, error) {
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("eval run %s not found", iteration.EvalRunID)
		}
		return 0, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return 0, err
	}
	itemCount := len(items)
	if itemCount <= 0 {
		itemCount = 1
	}
	roles := len(skillOptTrainGenerationRoles(run))
	if roles <= 0 {
		roles = 2
	}
	attemptsPerRole := 1
	if strings.TrimSpace(iteration.SessionID) != "" {
		session, err := store.GetSkillOptTrainSession(ctx, iteration.SessionID)
		if err != nil {
			return 0, err
		}
		if skillOptTrainRequiresVuePreviewBundle(session) {
			attemptsPerRole += skillOptTrainReviewOptionRetryBudget
		}
	}
	var dispatch skillOptTrainGenerationDispatch
	if strings.TrimSpace(iteration.SessionID) != "" {
		session, err := store.GetSkillOptTrainSession(ctx, iteration.SessionID)
		if err != nil {
			return 0, err
		}
		dispatch, err = skillOptTrainGeneratorSelection(ctx, store, session, iteration, run, request)
		if err != nil {
			return 0, err
		}
	} else {
		dispatch = skillOptTrainFallbackGeneratorDispatch(request)
	}
	if strings.TrimSpace(dispatch.Mode) == "" {
		dispatch = skillOptTrainFallbackGeneratorDispatch(request)
	}
	if strings.TrimSpace(dispatch.Agent) == "" && strings.TrimSpace(dispatch.Type) == "" && dispatch.Mode != skillOptTrainGenerationModeTargetSkill {
		return 0, errors.New("skillopt train generation dispatch is empty")
	}
	jobTimeout := skillOptTrainGenerationJobTimeoutHint(request, dispatch.Type)
	concurrency := skillOptTrainGenerationConcurrencyHint(request, dispatch.Type)
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > itemCount {
		concurrency = itemCount
	}
	batches := (itemCount + concurrency - 1) / concurrency
	estimated := time.Duration(batches*roles*attemptsPerRole)*jobTimeout + skillOptTrainGenerationLockBuffer
	if estimated < skillOptTrainGenerationLockTTL {
		return skillOptTrainGenerationLockTTL, nil
	}
	return estimated, nil
}

func skillOptTrainGenerationJobTimeoutHint(request skillOptTrainContinueRequest, dispatchType string) time.Duration {
	if strings.TrimSpace(dispatchType) == "" {
		return daemonRunningJobStaleAfter
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return daemonRunningJobStaleAfter
	}
	agentType, ok := types[dispatchType]
	if !ok {
		return daemonRunningJobStaleAfter
	}
	jobTimeout, err := time.ParseDuration(agentType.JobTimeout)
	if err != nil || jobTimeout <= 0 {
		return daemonRunningJobStaleAfter
	}
	return jobTimeout
}

func skillOptTrainGenerationConcurrencyHint(request skillOptTrainContinueRequest, dispatchType string) int {
	if strings.TrimSpace(dispatchType) == "" {
		return 1
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return 1
	}
	agentType, ok := types[dispatchType]
	if !ok || agentType.MaxBackground <= 0 {
		return 1
	}
	return agentType.MaxBackground
}

func skillOptTrainGenerationLockKey(sessionID string, iterationID string) string {
	sessionID = strings.TrimSpace(sessionID)
	iterationID = strings.TrimSpace(iterationID)
	if iterationID == "" {
		return "skillopt-train-generation:" + sessionID
	}
	return "skillopt-train-generation:" + sessionID + ":" + iterationID
}

// skillOptTrainGenerationCorrelationPrefix is the stable TaskID prefix shared by
// every generation child job for a run, so the jobs can be found by TaskID
// prefix. It is NOT used as ParentJobID/RootJobID: those are delegation-engine
// fields and AdvanceJob dereferences ParentJobID as a real job row, so stamping
// a synthetic value there would make a requeued generation job fail advancement
// permanently. Keyed on the eval run so it stays stable across resumes.
func skillOptTrainGenerationCorrelationPrefix(runID string) string {
	return "skillopt-train-generation:" + strings.TrimSpace(runID)
}

// skillOptTrainGenerationTaskID is the per-option TaskID stamped on each
// generation child job, uniquely identifying the (run, item, label, attempt)
// option it produced.
func skillOptTrainGenerationTaskID(runID string, itemID string, label string, attempt int) string {
	return fmt.Sprintf("skillopt-train-generation:%s:%s:%s:%d", strings.TrimSpace(runID), strings.TrimSpace(itemID), strings.TrimSpace(label), attempt)
}

// skillOptTrainGenerationProgress emits human-facing, one-line-per-option
// progress to a writer (typically stderr) while option jobs run concurrently.
// A nil receiver or nil writer is a no-op, so automated callers (the review
// watcher passes a nil Progress) stay silent. Writes are mutex-guarded because
// options across items complete on concurrent goroutines.
type skillOptTrainGenerationProgress struct {
	mu     sync.Mutex
	w      io.Writer
	done   int
	total  int
	extend func() error
}

func newSkillOptTrainGenerationProgress(w io.Writer, total int, extend func() error) *skillOptTrainGenerationProgress {
	return &skillOptTrainGenerationProgress{w: w, total: total, extend: extend}
}

func (p *skillOptTrainGenerationProgress) start(items, perItem int, runtime string) {
	if p == nil || p.w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	suffix := ""
	if strings.TrimSpace(runtime) != "" {
		suffix = " with " + runtime
	}
	fmt.Fprintf(p.w, "generating %d options (%d items x %d)%s...\n", p.total, items, perItem, suffix)
}

func (p *skillOptTrainGenerationProgress) optionDone(itemID, role string, elapsed time.Duration, failed bool) {
	if p == nil {
		return
	}
	// Keep the generation lock fresh while a long run is still producing options.
	if !failed && p.extend != nil {
		_ = p.extend()
	}
	if p.w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	status := "done"
	if failed {
		status = "failed"
	}
	fmt.Fprintf(p.w, "option %s/%s %s (%d/%d) - %s\n", itemID, role, status, p.done, p.total, formatShortDuration(elapsed))
}

func formatShortDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// skillOptTrainGenerationRuntimeLabel is a best-effort runtime name for the
// generation start line, taken from the resolved target/optimizer backend.
func skillOptTrainGenerationRuntimeLabel(request skillOptTrainContinueRequest) string {
	if backend := strings.TrimSpace(request.Optimizer.TargetBackend); backend != "" {
		return backend
	}
	return strings.TrimSpace(request.Optimizer.Backend)
}

func generateSkillOptTrainOptions(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) (skillOptTrainGenerationResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptionsGenerated); err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s not found", iteration.EvalRunID)
		}
		return skillOptTrainGenerationResult{}, err
	}
	rankedRun := skillOptRunUsesRankedOptions(run)
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	if len(items) == 0 {
		return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s has no review items to generate", run.ID)
	}
	roles := skillOptTrainGenerationRoles(run)
	if len(roles) < 2 {
		return skillOptTrainGenerationResult{}, fmt.Errorf("eval run %s expects at least 2 options", run.ID)
	}
	existingGenerated := 0
	// Count already-persisted options per item in ONE run-scoped query instead of
	// one query per item (the single-conn store would otherwise serialize N
	// round-trips on resume of a large run).
	existingOptionCount := map[string]int{}
	if rankedRun {
		allOptions, err := store.ListEvalReviewOptions(ctx, run.ID, "")
		if err != nil {
			return skillOptTrainGenerationResult{}, err
		}
		for _, opt := range allOptions {
			existingOptionCount[opt.ItemID]++
		}
	}
	// toGenerate holds only the items that still need generation; complete items
	// are skipped so resume regenerates nothing already persisted. A mix of
	// complete and incomplete items is the normal resume state (per-item commits),
	// so it is not an error — only a partially generated single item is.
	toGenerate := make([]db.EvalReviewItem, 0, len(items))
	for _, item := range items {
		if rankedRun {
			existing := existingOptionCount[item.ItemID]
			if existing > 0 {
				if existing == len(roles) {
					existingGenerated += existing
					continue
				}
				return skillOptTrainGenerationResult{}, fmt.Errorf("item %s has partial generated options; inspect or clear review options before continuing", item.ItemID)
			}
			toGenerate = append(toGenerate, item)
			continue
		}
		hasBaseline := strings.TrimSpace(item.BaselineArtifactID) != ""
		hasCandidate := strings.TrimSpace(item.CandidateArtifactID) != ""
		if hasBaseline || hasCandidate {
			if hasBaseline && hasCandidate {
				existingGenerated += 2
				continue
			}
			return skillOptTrainGenerationResult{}, fmt.Errorf("item %s has partial generated A/B artifacts; inspect or clear review item artifacts before continuing", item.ItemID)
		}
		toGenerate = append(toGenerate, item)
	}
	if len(toGenerate) == 0 {
		metadata := map[string]any{
			"status":            "recovered",
			"generated_options": existingGenerated,
			"strategy":          skillOptTrainGenerationStrategy(run),
			"completed_at":      time.Now().UTC().Format(time.RFC3339Nano),
		}
		return skillOptTrainGenerationResult{
			GeneratedOptions: existingGenerated,
			Metadata:         metadata,
		}, nil
	}
	dispatch, err := skillOptTrainGeneratorSelection(ctx, store, session, iteration, run, request)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	concurrency, err := skillOptTrainGenerationConcurrency(request, dispatch.Type)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	if err := ensureSkillOptTrainGenerationRepoReady(ctx, store, skillOptTrainGenerationRepo(session)); err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	progress := newSkillOptTrainGenerationProgress(request.Progress, len(toGenerate)*len(roles), request.GenerationLockExtend)
	progress.start(len(toGenerate), len(roles), skillOptTrainGenerationRuntimeLabel(request))
	generatedItems, err := generateSkillOptTrainItemOptions(ctx, store, blobStore, session, iteration, run, toGenerate, roles, rankedRun, request, dispatch, concurrency, progress)
	if err != nil {
		return skillOptTrainGenerationResult{}, err
	}
	// Each generated item was persisted atomically the moment it completed
	// (see generateSkillOptTrainItemOptions), so there is no end-of-phase batch
	// write here. This loop only aggregates metadata across the items.
	generated := 0
	jobIDs := []string{}
	var generatorAgent string
	var generatorRuntime string
	promptRecords := []map[string]any{}
	for _, item := range generatedItems {
		generated += len(item.Artifacts)
		jobIDs = append(jobIDs, item.JobIDs...)
		if generatorAgent == "" {
			generatorAgent = item.AgentName
		}
		if generatorRuntime == "" {
			generatorRuntime = item.Runtime
		}
		promptRecords = append(promptRecords, item.Prompts...)
	}
	metadata := map[string]any{
		"status":              "succeeded",
		"generated_options":   existingGenerated + generated,
		"jobs":                jobIDs,
		"agent":               generatorAgent,
		"runtime":             generatorRuntime,
		"generation_mode":     dispatch.Mode,
		"template_id":         dispatch.TemplateID,
		"template_version_id": dispatch.TemplateVersionID,
		"concurrency":         concurrency,
		"lock_ttl":            request.GenerationLockTTL.String(),
		"strategy":            skillOptTrainGenerationStrategy(run),
		"prompts":             promptRecords,
		"completed_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	return skillOptTrainGenerationResult{
		GeneratedOptions: existingGenerated + generated,
		JobIDs:           jobIDs,
		AgentName:        generatorAgent,
		Runtime:          generatorRuntime,
		Metadata:         metadata,
	}, nil
}

type skillOptTrainGeneratedItemOptions struct {
	ItemID     string
	ReviewItem *db.EvalReviewItem
	Artifacts  []db.EvalArtifact
	Options    []db.EvalReviewOption
	JobIDs     []string
	AgentName  string
	Runtime    string
	Prompts    []map[string]any
}

// skillOptTrainGenerationWriteForItem projects a generated item onto the store's
// per-item write shape (artifacts + review item + options).
func skillOptTrainGenerationWriteForItem(item skillOptTrainGeneratedItemOptions) db.EvalReviewGenerationWrite {
	return db.EvalReviewGenerationWrite{
		ItemID:     item.ItemID,
		ReviewItem: item.ReviewItem,
		Artifacts:  item.Artifacts,
		Options:    item.Options,
	}
}

type skillOptTrainGeneratedOption struct {
	Output                localAgentJobOutput
	Content               []byte
	MediaType             string
	Driver                string
	GenerationMode        string
	TemplateID            string
	TemplateVersionID     string
	SampleLabel           string
	PreviewBundleMetadata *skillopt.PreviewBundleMetadata
	Prompt                string
	Prompts               []map[string]any
	JobIDs                []string
	RetryAttempts         int
	ValidationErrors      []map[string]any
}

func generateSkillOptTrainItemOptions(ctx context.Context, store *db.Store, blobStore artifact.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, items []db.EvalReviewItem, roles []string, rankedRun bool, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, concurrency int, progress *skillOptTrainGenerationProgress) ([]skillOptTrainGeneratedItemOptions, error) {
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(items) {
		concurrency = len(items)
	}
	results := make([]skillOptTrainGeneratedItemOptions, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for index, item := range items {
		index := index
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[index] = ctx.Err()
				return
			}
			result, err := generateSkillOptTrainSingleItemOptions(ctx, store, blobStore, session, iteration, run, item, roles, rankedRun, request, dispatch, progress)
			if err != nil {
				errs[index] = err
				return
			}
			// Persist the completed item immediately so neither a later item's
			// failure nor a mid-run cancellation can lose it. Artifacts + item row
			// + options commit in one transaction (a partial item is never
			// written), using a non-cancellable context so a fully-generated item
			// is durable even if ctx is cancelled at this instant.
			if err := store.ReplaceGeneratedEvalReviewArtifactsForItem(context.WithoutCancel(ctx), run.ID, skillOptTrainGenerationWriteForItem(result)); err != nil {
				errs[index] = err
				return
			}
			results[index] = result
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func generateSkillOptTrainSingleItemOptions(ctx context.Context, store *db.Store, blobStore artifact.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, roles []string, rankedRun bool, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, progress *skillOptTrainGenerationProgress) (skillOptTrainGeneratedItemOptions, error) {
	generatedItem := skillOptTrainGeneratedItemOptions{ItemID: item.ItemID}
	replacementOptions := make([]db.EvalReviewOption, 0, len(roles))
	artifactRecords := make([]db.EvalArtifact, 0, len(roles))
	wantsVuePreviewBundle := skillOptTrainWantsVuePreviewBundle(session)
	requiresVuePreviewBundle := skillOptTrainRequiresVuePreviewBundle(session)
	for _, role := range roles {
		optionStart := time.Now()
		generatedOption, err := generateSkillOptTrainSingleOption(ctx, store, session, iteration, run, item, role, rankedRun, request, dispatch, wantsVuePreviewBundle, requiresVuePreviewBundle)
		if err != nil {
			progress.optionDone(item.ItemID, role, time.Since(optionStart), true)
			return skillOptTrainGeneratedItemOptions{}, err
		}
		progress.optionDone(item.ItemID, role, time.Since(optionStart), false)
		artifactRole := role
		if rankedRun {
			artifactRole = "option-" + role
		}
		artifactRecord, err := prepareReviewItemContentArtifact(blobStore, run.ID, item.ItemID, artifactRole, generatedOption.Content, generatedOption.MediaType, generatedOption.Driver)
		if err != nil {
			return skillOptTrainGeneratedItemOptions{}, err
		}
		artifactRecords = append(artifactRecords, artifactRecord)
		optionMetadata := skillOptTrainGeneratedOptionMetadata(generatedOption.Output, generatedOption.Prompt, generatedOption.GenerationMode, generatedOption.TemplateID, generatedOption.TemplateVersionID, generatedOption.SampleLabel, generatedOption.PreviewBundleMetadata, generatedOption.RetryAttempts, generatedOption.ValidationErrors)
		if rankedRun {
			replacementOptions = append(replacementOptions, db.EvalReviewOption{
				RunID:        run.ID,
				ItemID:       item.ItemID,
				Label:        role,
				ArtifactID:   artifactRecord.ID,
				Role:         "option",
				MetadataJSON: optionMetadata,
			})
		} else if role == "baseline" {
			item.BaselineArtifactID = artifactRecord.ID
		} else if role == "candidate" {
			item.CandidateArtifactID = artifactRecord.ID
		}
		generatedItem.JobIDs = append(generatedItem.JobIDs, generatedOption.JobIDs...)
		if generatedItem.AgentName == "" {
			generatedItem.AgentName = generatedOption.Output.Agent
		}
		if generatedItem.Runtime == "" {
			if agent, err := store.GetAgent(ctx, generatedOption.Output.Agent); err == nil {
				generatedItem.Runtime = agent.Runtime
			}
		}
		generatedItem.Prompts = append(generatedItem.Prompts, generatedOption.Prompts...)
	}
	generatedItem.Artifacts = artifactRecords
	generatedItem.Options = replacementOptions
	if !rankedRun {
		generatedItem.ReviewItem = &item
	}
	return generatedItem, nil
}

func generateSkillOptTrainSingleOption(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, rankedRun bool, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, wantsVuePreviewBundle bool, requiresVuePreviewBundle bool) (skillOptTrainGeneratedOption, error) {
	basePrompt := buildSkillOptTrainGenerationPrompt(session, iteration, run, item, role, rankedRun)
	prompt := basePrompt
	retryBudget := 0
	if wantsVuePreviewBundle && requiresVuePreviewBundle {
		retryBudget = skillOptTrainReviewOptionRetryBudget
	}
	validationErrors := []map[string]any{}
	promptRecords := []map[string]any{}
	jobIDs := []string{}
	for attempt := 0; ; attempt++ {
		output, err := dispatchSkillOptTrainGenerationJob(ctx, store, session, iteration, run, item, role, attempt, request, dispatch, prompt)
		if err != nil {
			return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: %w", role, item.ItemID, err)
		}
		promptRecord := map[string]any{
			"item_id": item.ItemID,
			"role":    role,
			"attempt": attempt,
			"job_id":  output.JobID,
			"prompt":  prompt,
		}
		promptRecords = append(promptRecords, promptRecord)
		jobIDs = append(jobIDs, output.JobID)
		if output.Result == nil {
			return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: job %s did not return a result", role, item.ItemID, output.JobID)
		}
		if output.Result.Decision != "implemented" {
			return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: job %s returned %s, want implemented: %s", role, item.ItemID, output.JobID, output.Result.Decision, output.Result.Summary)
		}
		content := []byte(output.Result.Summary)
		mediaType := "text/markdown"
		driver := "text"
		var previewBundleMetadata *skillopt.PreviewBundleMetadata
		if wantsVuePreviewBundle {
			bundle, err := skillopt.ParsePreviewBundle([]byte(output.Result.Summary))
			if err != nil {
				if requiresVuePreviewBundle {
					validationError := skillOptTrainOptionValidationError(item.ItemID, role, attempt, err)
					validationErrors = append(validationErrors, validationError)
					promptRecord["validation_error"] = validationError
					if attempt < retryBudget {
						prompt = buildSkillOptTrainGenerationRetryPrompt(basePrompt, validationError)
						continue
					}
					return skillOptTrainGeneratedOption{}, fmt.Errorf("generate option validation failed: item=%s option=%s validation_class=preview_bundle retry_count=%d error=%w", item.ItemID, role, attempt, err)
				}
			} else {
				content, err = json.Marshal(bundle)
				if err != nil {
					return skillOptTrainGeneratedOption{}, fmt.Errorf("generate %s for %s: encode preview bundle: %w", role, item.ItemID, err)
				}
				metadata := bundle.Metadata()
				previewBundleMetadata = &metadata
				mediaType = "application/json"
				driver = skillopt.TrainPreviewRendererVueVite
			}
		}
		return skillOptTrainGeneratedOption{
			Output:                output,
			Content:               content,
			MediaType:             mediaType,
			Driver:                driver,
			GenerationMode:        dispatch.Mode,
			TemplateID:            dispatch.TemplateID,
			TemplateVersionID:     dispatch.TemplateVersionID,
			SampleLabel:           skillOptTrainGenerationSampleLabel(item.ItemID, role),
			PreviewBundleMetadata: previewBundleMetadata,
			Prompt:                prompt,
			Prompts:               promptRecords,
			JobIDs:                jobIDs,
			RetryAttempts:         len(validationErrors),
			ValidationErrors:      validationErrors,
		}, nil
	}
}

func dispatchSkillOptTrainGenerationJob(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, attempt int, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch, prompt string) (localAgentJobOutput, error) {
	taskID := skillOptTrainGenerationTaskID(run.ID, item.ItemID, role, attempt)
	if dispatch.Mode != skillOptTrainGenerationModeTargetSkill {
		return dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
			RepoFlag:         skillOptTrainGenerationRepo(session),
			Agent:            dispatch.Agent,
			Action:           "ask",
			Instructions:     prompt,
			Type:             dispatch.Type,
			Home:             request.Home,
			AllowManagedSync: dispatch.Type != "",
			TaskID:           taskID,
		})
	}
	agentName, err := startSkillOptTrainTargetSkillAgent(ctx, store, session, iteration, run, item, role, attempt, request, dispatch)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	return dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag:     skillOptTrainGenerationRepo(session),
		Agent:        agentName,
		Action:       "ask",
		Instructions: prompt,
		Home:         request.Home,
		JobTimeout:   skillOptTrainGenerationJobTimeoutHint(request, dispatch.Type),
		TaskID:       taskID,
	})
}

func startSkillOptTrainTargetSkillAgent(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, attempt int, request skillOptTrainContinueRequest, dispatch skillOptTrainGenerationDispatch) (string, error) {
	repo, record, err := resolveLocalAgentRepo(ctx, store, skillOptTrainGenerationRepo(session))
	if err != nil {
		return "", err
	}
	if err := store.UpsertRepo(ctx, record); err != nil {
		return "", err
	}
	template, err := loadInstalledTemplate(ctx, store, dispatch.TemplateVersionID)
	if err != nil {
		return "", err
	}
	agent := runtime.Agent{
		Name:           skillOptTrainTargetSkillAgentName(run.ID, item.ItemID, role, attempt),
		Role:           "generator",
		Runtime:        firstNonEmpty(strings.TrimSpace(dispatch.Runtime), runtime.CodexRuntime),
		RepoScope:      repo.FullName(),
		TemplateID:     dispatch.TemplateVersionID,
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "idle",
	}
	adapter, err := runtimeStartAdapter(newRuntimeFactory(), agent.Runtime, record.CheckoutPath)
	if err != nil {
		return "", err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: agent, Prompt: agentStartupPrompt(agent, template)})
	if err != nil {
		return "", err
	}
	agent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(agent); err != nil {
		return "", err
	}
	if err := persistAgentSubscription(ctx, store, agent, []string{repo.FullName()}); err != nil {
		return "", err
	}
	return agent.Name, nil
}

func skillOptTrainTargetSkillAgentName(runID string, itemID string, role string, attempt int) string {
	parts := []string{"skillopt-target", runID, itemID, role}
	if attempt > 0 {
		parts = append(parts, fmt.Sprintf("retry-%d", attempt))
	}
	parts = append(parts, fmt.Sprintf("%x", time.Now().UTC().UnixNano()))
	return skillOptSafeAgentName(strings.Join(parts, "-"))
}

func skillOptSafeAgentName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			ok = r == '-' || r == '_' || r == '.' || r == '@'
		}
		if ok {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		return "skillopt-target"
	}
	return name
}

func skillOptTrainGenerationSampleLabel(itemID string, role string) string {
	return strings.TrimSpace(itemID) + "/" + strings.TrimSpace(role)
}

func skillOptTrainWantsVuePreviewBundle(session db.SkillOptTrainSession) bool {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	return policy.Mode != skillopt.TrainPreviewModeNone && policy.Renderer == skillopt.TrainPreviewRendererVueVite
}

func skillOptTrainRequiresVuePreviewBundle(session db.SkillOptTrainSession) bool {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	return policy.Mode == skillopt.TrainPreviewModeRequired && policy.Renderer == skillopt.TrainPreviewRendererVueVite
}

type skillOptTrainReviewPublishResult struct {
	Repo        github.Repository
	IssueNumber int64
	URL         string
	PreviewURLs int
}

type skillOptPreviewPublication struct {
	URL          string
	CommitSHA    string
	PagesStatus  string
	StatusReason string
}

type skillOptLatestPagesBuild struct {
	Status string `json:"status"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	CommitSHA string `json:"commit_sha"`
	Commit    string `json:"commit"`
}

const skillOptReviewWatchDefaultStaleThreshold = 24 * time.Hour

func autoSyncSkillOptTrainReviewFeedback(ctx context.Context, paths config.Paths, store *db.Store, iteration db.SkillOptTrainIteration) ([]string, bool) {
	repoText := strings.TrimSpace(iteration.IssueRepo)
	issueNumber := iteration.IssueNumber
	if repoText == "" || issueNumber <= 0 {
		return nil, false
	}
	repo, err := daemon.ParseRepository(repoText)
	if err != nil {
		return []string{
			"github_feedback_sync: failed",
			fmt.Sprintf("github_feedback_error: invalid review repo %q: %v", repoText, err),
		}, false
	}
	client := newSkillOptGitHubClient()
	if err := client.Preflight(ctx, repo); err != nil {
		return []string{
			"github_feedback_sync: failed",
			fmt.Sprintf("github_feedback_error: %v", err),
		}, false
	}
	collector := feedback.GitHubCollector{
		BlobStore: artifact.NewStore(paths.ArtifactBlobs),
		GitHub:    client,
	}
	result, err := collector.Sync(ctx, store, iteration.EvalRunID, repo, issueNumber)
	if err != nil {
		return []string{
			"github_feedback_sync: failed",
			fmt.Sprintf("github_feedback_error: %v", err),
		}, false
	}
	lines := []string{
		"github_feedback_sync: imported",
		fmt.Sprintf("github_feedback_events: %d", result.Count()),
	}
	for _, diagnostic := range result.Diagnostics {
		lines = append(lines, fmt.Sprintf("github_feedback_diagnostic: %s", diagnostic))
	}
	return lines, result.Count() > 0
}

func publishSkillOptTrainReview(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (skillOptTrainReviewPublishResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateReviewPublished); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	if strings.TrimSpace(iteration.EvalRunID) == "" {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("train iteration %s has no eval run id", iteration.ID)
	}
	if recovered, ok, err := recoverSkillOptTrainReviewPublication(ctx, paths, store, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	} else if ok {
		return recovered, nil
	}
	previewURLs, err := publishSkillOptTrainPreviewURLs(ctx, paths, store, session, iteration)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	repo, err := resolveSkillOptFeedbackRepo(ctx, paths, store, run, "")
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	client := newSkillOptGitHubClient()
	if err := client.Preflight(ctx, repo); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	publishingMetadata := map[string]any{
		"status":       "publishing",
		"repo":         repo.FullName(),
		"preview_urls": previewURLs,
		"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, publishingMetadata); err != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("write review pre-publish recovery marker: %w", err)
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", publishingMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", publishingMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	collector := feedback.GitHubCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
	body, err := collector.Body(ctx, store, run.ID)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	postingMetadata := make(map[string]any, len(publishingMetadata)+2)
	for key, value := range publishingMetadata {
		postingMetadata[key] = value
	}
	postingMetadata["status"] = "posting_external"
	postingMetadata["external_post_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, postingMetadata); err != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("write review external-post recovery marker: %w", err)
	}
	issue, err := client.CreateIssue(ctx, github.CreateIssueInput{
		Repo:  repo,
		Title: fmt.Sprintf("Gitmoot SkillOpt feedback: %s", strings.TrimSpace(run.ID)),
		Body:  body,
	})
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	published := feedback.GitHubPublishResult{Repo: repo, IssueNumber: issue.Number, URL: issue.URL, Mode: "issue"}
	externalMetadata := map[string]any{
		"status":       "published_external",
		"repo":         published.Repo.FullName(),
		"issue_number": published.IssueNumber,
		"url":          published.URL,
		"preview_urls": previewURLs,
		"published_at": time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	externalMarkerErr := writeSkillOptTrainReviewRecovery(paths, session, iteration, externalMetadata)
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	iteration.IssueRepo = published.Repo.FullName()
	iteration.IssueNumber = published.IssueNumber
	iteration.IssueURL = published.URL
	dbMetadata := make(map[string]any, len(externalMetadata))
	for key, value := range externalMetadata {
		dbMetadata[key] = value
	}
	dbMetadata["status"] = "published"
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", dbMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", dbMetadata)
	watch, watchErr := skillOptTrainReviewWatch(ctx, store, iteration, published.Repo, published.IssueNumber, published.URL, previewURLs, "gitmoot skillopt train continue")
	if watchErr != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("review was published at %s but watch registration preparation failed: %w", published.URL, watchErr)
	}
	if err := store.UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx, session, iteration, watch); err != nil {
		if externalMarkerErr != nil {
			return skillOptTrainReviewPublishResult{}, fmt.Errorf("%w; review was published at %s but recovery marker write failed: %v", err, published.URL, externalMarkerErr)
		}
		return skillOptTrainReviewPublishResult{}, err
	}
	if externalMarkerErr != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("review was published at %s and recorded in local state but recovery marker write failed: %w", published.URL, externalMarkerErr)
	}
	_ = removeSkillOptTrainReviewRecovery(paths, session, iteration)
	return skillOptTrainReviewPublishResult{Repo: published.Repo, IssueNumber: published.IssueNumber, URL: published.URL, PreviewURLs: previewURLs}, nil
}

func recoverSkillOptTrainReviewPublication(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (skillOptTrainReviewPublishResult, bool, error) {
	review, ok, err := readSkillOptTrainReviewRecovery(paths, session, iteration)
	if err != nil || !ok {
		return skillOptTrainReviewPublishResult{}, ok, err
	}
	status := metadataString(review, "status")
	if status == "posting_external" {
		return skillOptTrainReviewPublishResult{}, true, errors.New("train review publication was interrupted after the external GitHub post started; inspect the review repo before retrying to avoid duplicate issues")
	}
	if status != "published_external" {
		return skillOptTrainReviewPublishResult{}, false, nil
	}
	repo, err := daemon.ParseRepository(metadataString(review, "repo"))
	if err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	issueNumber := int64(metadataNumber(review, "issue_number"))
	url := metadataString(review, "url")
	previewURLs := metadataNumber(review, "preview_urls")
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	iteration.IssueRepo = repo.FullName()
	iteration.IssueNumber = issueNumber
	iteration.IssueURL = url
	metadata := map[string]any{
		"status":       "published",
		"repo":         repo.FullName(),
		"issue_number": issueNumber,
		"url":          url,
		"preview_urls": previewURLs,
		"published_at": time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", metadata)
	watch, err := skillOptTrainReviewWatch(ctx, store, iteration, repo, issueNumber, url, previewURLs, "gitmoot skillopt train recover")
	if err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	if err := store.UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx, session, iteration, watch); err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	_ = removeSkillOptTrainReviewRecovery(paths, session, iteration)
	return skillOptTrainReviewPublishResult{Repo: repo, IssueNumber: issueNumber, URL: url, PreviewURLs: previewURLs}, true, nil
}

func skillOptTrainReviewWatch(ctx context.Context, store *db.Store, iteration db.SkillOptTrainIteration, repo github.Repository, issueNumber int64, url string, previewURLs int, source string) (db.SkillOptReviewWatch, error) {
	if store == nil {
		return db.SkillOptReviewWatch{}, errors.New("store is required")
	}
	runID := strings.TrimSpace(iteration.EvalRunID)
	if runID == "" {
		return db.SkillOptReviewWatch{}, errors.New("train review watch run id is required")
	}
	items, err := store.ListEvalReviewItems(ctx, runID)
	if err != nil {
		return db.SkillOptReviewWatch{}, err
	}
	itemIDs := make([]string, 0, len(items))
	for _, item := range items {
		if itemID := strings.TrimSpace(item.ItemID); itemID != "" {
			itemIDs = append(itemIDs, itemID)
		}
	}
	itemIDsJSON, err := json.Marshal(itemIDs)
	if err != nil {
		return db.SkillOptReviewWatch{}, err
	}
	metadata := map[string]any{
		"session_id":   strings.TrimSpace(iteration.SessionID),
		"iteration_id": strings.TrimSpace(iteration.ID),
		"issue_url":    strings.TrimSpace(url),
		"preview_urls": previewURLs,
		"source":       strings.TrimSpace(source),
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return db.SkillOptReviewWatch{}, err
	}
	return db.SkillOptReviewWatch{
		Repo:                  repo.FullName(),
		IssueNumber:           issueNumber,
		RunID:                 runID,
		ExpectedItemIDsJSON:   string(itemIDsJSON),
		Status:                db.SkillOptReviewWatchStatusWatching,
		StaleAfter:            time.Now().UTC().Add(skillOptReviewWatchDefaultStaleThreshold).Format(time.RFC3339Nano),
		StaleThresholdSeconds: int64(skillOptReviewWatchDefaultStaleThreshold.Seconds()),
		MetadataJSON:          string(metadataJSON),
	}, nil
}

func writeSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, metadata map[string]any) error {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return errors.New("review recovery path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmpPath, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func readSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (map[string]any, bool, error) {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil, false, nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(content, &metadata); err != nil {
		return nil, true, fmt.Errorf("read review recovery marker %s: %w", path, err)
	}
	return metadata, true, nil
}

func removeSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) error {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func skillOptTrainReviewRecoveryPath(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) string {
	if strings.TrimSpace(paths.Home) == "" {
		return ""
	}
	name := skillOptCandidateReviewRecoveryName(session.ID, iteration.ID)
	if name == "" {
		return ""
	}
	return filepath.Join(paths.Home, "skillopt", "reviews", name+".json")
}

func publishSkillOptTrainPreviewURLs(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (int, error) {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	if policy.Mode == skillopt.TrainPreviewModeNone || policy.Renderer == skillopt.TrainPreviewRendererNone || policy.Publisher == skillopt.TrainPreviewPublisherNone {
		return 0, nil
	}
	if policy.Renderer != skillopt.TrainPreviewRendererVueVite || policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, fmt.Errorf("preview renderer %s with publisher %s is not implemented", policy.Renderer, policy.Publisher)
		}
		return 0, nil
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		return 0, err
	}
	options, err := store.ListEvalReviewOptions(ctx, run.ID, "")
	if err != nil {
		return 0, err
	}
	if len(options) == 0 {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, fmt.Errorf("train run %s has no generated options to publish previews", run.ID)
		}
		return 0, nil
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	previewRepo, err := previewRepoRecord(ctx, store, policy)
	if err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, err
		}
		return convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, options)
	}
	if err := requireCleanPreviewRepo(ctx, previewRepo.CheckoutPath); err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, err
		}
		return convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, options)
	}
	publishedCount := 0
	for _, option := range options {
		metadata := optionMetadataMap(option.MetadataJSON)
		if metadataStringValue(metadata, "preview_url") != "" || metadataStringValue(metadata, "url") != "" {
			publishedCount++
			continue
		}
		if metadata["preview_bundle"] == nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s is missing preview bundle metadata", option.ItemID, option.Label)
			}
			continue
		}
		record, err := store.GetEvalArtifact(ctx, option.ArtifactID)
		if err != nil {
			return publishedCount, fmt.Errorf("item %s option %s artifact: %w", option.ItemID, option.Label, err)
		}
		content, err := blobStore.Read(record.Hash)
		if err != nil {
			return publishedCount, fmt.Errorf("item %s option %s preview bundle blob: %w", option.ItemID, option.Label, err)
		}
		bundle, err := skillopt.ParsePreviewBundle(content)
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s preview bundle: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		distDir, cleanup, err := renderVueVitePreviewBundle(ctx, bundle)
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s render preview: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		route, err := skillOptPreviewRoute(policy.RouteTemplate, run.ID, option.ItemID, option.Label)
		if err != nil {
			_ = cleanup()
			return publishedCount, err
		}
		publication, err := publishGitHubPagesPreview(ctx, previewRepo, route, distDir)
		_ = cleanup()
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s publish preview: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		metadata["preview_url"] = publication.URL
		metadata["preview_route"] = route
		metadata["preview_repo"] = previewRepo.FullName()
		metadata["preview_commit"] = publication.CommitSHA
		metadata["preview_status"] = publication.PagesStatus
		if strings.TrimSpace(publication.StatusReason) != "" {
			metadata["preview_status_reason"] = publication.StatusReason
		}
		option.MetadataJSON = encodeOptionMetadata(metadata)
		if err := store.UpsertEvalReviewOption(ctx, option); err != nil {
			return publishedCount, err
		}
		publishedCount++
	}
	if policy.Mode == skillopt.TrainPreviewModeRequired && publishedCount != len(options) {
		return publishedCount, fmt.Errorf("required preview run has %d/%d preview URLs", publishedCount, len(options))
	}
	if policy.Mode == skillopt.TrainPreviewModeOptional {
		freshOptions, err := store.ListEvalReviewOptions(ctx, run.ID, "")
		if err != nil {
			return publishedCount, err
		}
		if _, err := convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, freshOptions); err != nil {
			return publishedCount, err
		}
	}
	return publishedCount, nil
}

func convertOptionalPreviewBundlesToFallback(ctx context.Context, store *db.Store, blobStore artifact.Store, options []db.EvalReviewOption) (int, error) {
	for _, option := range options {
		metadata := optionMetadataMap(option.MetadataJSON)
		if metadata["preview_bundle"] == nil || metadataStringValue(metadata, "preview_url") != "" || metadataStringValue(metadata, "url") != "" {
			continue
		}
		record, err := store.GetEvalArtifact(ctx, option.ArtifactID)
		if err != nil {
			return 0, err
		}
		content, err := blobStore.Read(record.Hash)
		if err != nil {
			return 0, err
		}
		bundle, err := skillopt.ParsePreviewBundle(content)
		if err != nil {
			return 0, err
		}
		fallbackContent := []byte(previewBundleInlineFallback(bundle))
		blob, err := blobStore.Put(fallbackContent)
		if err != nil {
			return 0, err
		}
		record.Hash = blob.Hash
		record.MediaType = "text/markdown"
		record.SizeBytes = blob.Size
		record.Driver = "text"
		if err := store.UpsertEvalArtifact(ctx, record); err != nil {
			return 0, err
		}
		delete(metadata, "preview_bundle")
		metadata["preview_fallback"] = "inline"
		metadata["preview_fallback_renderer"] = skillopt.TrainPreviewRendererVueVite
		option.MetadataJSON = encodeOptionMetadata(metadata)
		if err := store.UpsertEvalReviewOption(ctx, option); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func previewBundleInlineFallback(bundle skillopt.PreviewBundle) string {
	var builder strings.Builder
	builder.WriteString("# Vue/Vite preview source\n\n")
	builder.WriteString("A clickable preview was not available, so this option is included as inline Vue/Vite source for review.\n\n")
	for _, file := range bundle.Files {
		fmt.Fprintf(&builder, "## `%s`\n\n", file.Path)
		writeSkillOptMarkdownFence(&builder, file.Content)
		builder.WriteString("\n")
	}
	return builder.String()
}

func skillOptMarkdownInlineCode(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	value = strings.ReplaceAll(value, "`", "'")
	if value == "" {
		return "-"
	}
	return "`" + value + "`"
}

func skillOptMarkdownTableCell(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	if value == "" {
		return "-"
	}
	return value
}

func writeSkillOptMarkdownFence(builder *strings.Builder, content string) {
	fence := "```"
	for strings.Contains(content, fence) {
		fence += "`"
	}
	builder.WriteString(fence)
	builder.WriteString("text\n")
	builder.WriteString(strings.TrimRight(content, "\n"))
	builder.WriteString("\n")
	builder.WriteString(fence)
	builder.WriteString("\n")
}

func previewRepoRecord(ctx context.Context, store *db.Store, policy skillopt.TrainPreviewPolicy) (db.Repo, error) {
	repoName := strings.TrimSpace(policy.Repo)
	if repoName == "" {
		return db.Repo{}, errors.New("preview repo is required")
	}
	repo, err := store.GetRepo(ctx, repoName)
	if err != nil {
		return db.Repo{}, fmt.Errorf("preview repo %s is not registered with a checkout path; run `gitmoot repo add %s --path /path/to/checkout`: %w", repoName, repoName, err)
	}
	if strings.TrimSpace(repo.CheckoutPath) == "" {
		return db.Repo{}, fmt.Errorf("preview repo %s has no checkout path; run `gitmoot repo add %s --path /path/to/checkout`", repoName, repoName)
	}
	if _, err := os.Stat(repo.CheckoutPath); err != nil {
		return db.Repo{}, fmt.Errorf("preview repo %s checkout is not ready: %w", repo.FullName(), err)
	}
	return repo, nil
}

func requireCleanPreviewRepo(ctx context.Context, checkout string) error {
	result, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("check preview repo status: %w", err)
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("preview repo checkout %s is dirty; commit or clean it before publishing previews", checkout)
	}
	return nil
}

func renderVueVitePreviewBundle(ctx context.Context, bundle skillopt.PreviewBundle) (string, func() error, error) {
	workDir, err := os.MkdirTemp("", "gitmoot-vue-preview-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.RemoveAll(workDir) }
	for _, file := range bundle.Files {
		target, err := safeJoinPreviewPath(workDir, file.Path)
		if err != nil {
			_ = cleanup()
			return "", nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = cleanup()
			return "", nil, err
		}
		if err := os.WriteFile(target, []byte(file.Content), 0o644); err != nil {
			_ = cleanup()
			return "", nil, err
		}
	}
	if err := writeTrustedVueViteScaffold(workDir); err != nil {
		_ = cleanup()
		return "", nil, err
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, workDir, "npm", "install", "--ignore-scripts"); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("npm install: %w", err)
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, workDir, "npm", "run", "build"); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("%s: %w", bundle.BuildCommand, err)
	}
	distDir, err := safeJoinPreviewPath(workDir, bundle.DistDir)
	if err != nil {
		_ = cleanup()
		return "", nil, err
	}
	if _, err := os.Stat(filepath.Join(distDir, "index.html")); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("preview build output missing index.html in %s: %w", bundle.DistDir, err)
	}
	return distDir, cleanup, nil
}

func publishGitHubPagesPreview(ctx context.Context, repo db.Repo, route string, distDir string) (skillOptPreviewPublication, error) {
	checkout := strings.TrimSpace(repo.CheckoutPath)
	if err := requireCleanPreviewRepo(ctx, checkout); err != nil {
		return skillOptPreviewPublication{}, err
	}
	head, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return skillOptPreviewPublication{}, fmt.Errorf("read preview repo head: %w", err)
	}
	headSHA := strings.TrimSpace(head.Stdout)
	target, err := safeJoinPreviewPath(checkout, route)
	if err != nil {
		return skillOptPreviewPublication{}, err
	}
	if err := os.RemoveAll(target); err != nil {
		return skillOptPreviewPublication{}, err
	}
	if err := copyDir(distDir, target); err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return skillOptPreviewPublication{}, err
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "add", "--", route); err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return skillOptPreviewPublication{}, fmt.Errorf("git add preview route: %w", err)
	}
	status, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "status", "--porcelain", "--", route)
	if err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return skillOptPreviewPublication{}, fmt.Errorf("check preview route status: %w", err)
	}
	if strings.TrimSpace(status.Stdout) != "" {
		if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "commit", "-m", "Publish SkillOpt preview "+strings.TrimSuffix(route, "/")); err != nil {
			restorePreviewRoute(ctx, checkout, route, headSHA)
			return skillOptPreviewPublication{}, fmt.Errorf("git commit preview route: %w", err)
		}
		if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "push"); err != nil {
			restorePreviewRoute(ctx, checkout, route, headSHA)
			return skillOptPreviewPublication{}, fmt.Errorf("git push preview route: %w", err)
		}
	}
	commit, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return skillOptPreviewPublication{}, fmt.Errorf("read published preview commit: %w", err)
	}
	commitSHA := strings.TrimSpace(commit.Stdout)
	pagesStatus, pagesReason := observeGitHubPagesBuildStatus(ctx, repo, commitSHA)
	return skillOptPreviewPublication{
		URL:          githubPagesURL(repo, route),
		CommitSHA:    commitSHA,
		PagesStatus:  pagesStatus,
		StatusReason: pagesReason,
	}, nil
}

func observeGitHubPagesBuildStatus(ctx context.Context, repo db.Repo, commitSHA string) (string, string) {
	return observeGitHubPagesBuildStatusWithPoll(ctx, repo, commitSHA, 15*time.Second, 2*time.Second)
}

func observeGitHubPagesBuildStatusWithPoll(ctx context.Context, repo db.Repo, commitSHA string, timeout time.Duration, interval time.Duration) (string, string) {
	fullName := repo.FullName()
	if strings.TrimSpace(fullName) == "" {
		return "pending", "preview repo is unknown"
	}
	deadline := time.Now().Add(timeout)
	for {
		status, reason, done := readGitHubPagesBuildStatus(ctx, repo, commitSHA)
		if done {
			return status, reason
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return status, reason
		}
		select {
		case <-ctx.Done():
			return "pending", "latest GitHub Pages build wait was canceled: " + ctx.Err().Error()
		case <-time.After(interval):
		}
	}
}

func readGitHubPagesBuildStatus(ctx context.Context, repo db.Repo, commitSHA string) (string, string, bool) {
	fullName := repo.FullName()
	result, err := skillOptTrainPreviewRunner.Run(ctx, strings.TrimSpace(repo.CheckoutPath), "gh", "api", "repos/"+fullName+"/pages/builds/latest")
	if err != nil {
		return "pending", "latest GitHub Pages build could not be read: " + err.Error(), true
	}
	var build skillOptLatestPagesBuild
	if err := json.Unmarshal([]byte(result.Stdout), &build); err != nil {
		return "pending", "latest GitHub Pages build response could not be decoded: " + err.Error(), true
	}
	status := normalizeGitHubPagesBuildStatus(build.Status)
	buildCommit := firstNonEmpty(strings.TrimSpace(build.CommitSHA), strings.TrimSpace(build.Commit))
	if buildCommit != "" && commitSHA != "" && !strings.EqualFold(buildCommit, commitSHA) {
		return "stale", fmt.Sprintf("latest GitHub Pages build is for commit %s, expected %s", buildCommit, commitSHA), false
	}
	if status == "failed" && strings.TrimSpace(build.Error.Message) != "" {
		return status, strings.TrimSpace(build.Error.Message), true
	}
	if status == "pending" {
		return status, "", false
	}
	return status, "", true
}

func normalizeGitHubPagesBuildStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "built":
		return "ready"
	case "errored":
		return "failed"
	case "building", "queued":
		return "pending"
	case "":
		return "pending"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func restorePreviewRoute(ctx context.Context, checkout string, route string, headSHA string) {
	if strings.TrimSpace(headSHA) != "" {
		_, _ = skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "reset", "--hard", headSHA)
	}
	_, _ = skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "clean", "-fd", "--", route)
}

func skillOptPreviewRoute(template string, runID string, itemID string, label string) (string, error) {
	if strings.TrimSpace(template) == "" {
		template = skillopt.DefaultTrainPreviewRouteTemplate
	}
	route := strings.ReplaceAll(template, "{run_id}", previewRouteSlug(runID))
	route = strings.ReplaceAll(route, "{item_id}", previewRouteSlug(itemID))
	route = strings.ReplaceAll(route, "{option_label}", previewRouteSlug(label))
	route = strings.TrimSpace(route)
	if route == "" {
		return "", errors.New("preview route is required")
	}
	route = strings.TrimPrefix(route, "/")
	clean := filepath.ToSlash(filepath.Clean(route))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
		return "", fmt.Errorf("preview route %q is unsafe", route)
	}
	if !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	return clean, nil
}

func safeJoinPreviewPath(root string, relative string) (string, error) {
	root = filepath.Clean(root)
	relative = filepath.FromSlash(strings.TrimSpace(relative))
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("preview path %q must be relative", relative)
	}
	target := filepath.Clean(filepath.Join(root, relative))
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("preview path %q must stay inside %s", relative, root)
	}
	return target, nil
}

func copyDir(source string, target string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		if entry.IsDir() {
			if err := copyDir(sourcePath, targetPath); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(sourcePath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(targetPath, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func githubPagesURL(repo db.Repo, route string) string {
	if strings.EqualFold(repo.Name, repo.Owner+".github.io") {
		return fmt.Sprintf("https://%s.github.io/%s", repo.Owner, strings.TrimLeft(route, "/"))
	}
	return fmt.Sprintf("https://%s.github.io/%s/%s", repo.Owner, repo.Name, strings.TrimLeft(route, "/"))
}

func writeTrustedVueViteScaffold(workDir string) error {
	packageJSON := `{"type":"module","scripts":{"build":"vite build"},"dependencies":{"@vitejs/plugin-vue":"latest","vite":"latest","vue":"latest"}}`
	if err := os.WriteFile(filepath.Join(workDir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		return err
	}
	indexHTML := `<div id="app"></div><script type="module" src="/src/main.js"></script>`
	if err := os.WriteFile(filepath.Join(workDir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		return err
	}
	mainPath := filepath.Join(workDir, "src", "main.js")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o755); err != nil {
		return err
	}
	mainJS := `import { createApp } from 'vue'; import App from './App.vue'; createApp(App).mount('#app');`
	if err := os.WriteFile(mainPath, []byte(mainJS), 0o644); err != nil {
		return err
	}
	viteConfig := "import { defineConfig } from 'vite';\nimport vue from '@vitejs/plugin-vue';\n\nexport default defineConfig({ base: './', plugins: [vue()] });\n"
	return os.WriteFile(filepath.Join(workDir, "vite.config.js"), []byte(viteConfig), 0o644)
}

func previewRouteSlug(value string) string {
	trimmed := strings.TrimSpace(value)
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(trimmed) {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if allowed {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(builder.String(), ".-_")
	if slug == "" {
		slug = "value"
	}
	if slug != trimmed {
		slug = slug + "-" + shortHash(trimmed)
	}
	return slug
}

func optionMetadataMap(metadataJSON string) map[string]any {
	metadata := map[string]any{}
	if strings.TrimSpace(metadataJSON) != "" {
		_ = json.Unmarshal([]byte(metadataJSON), &metadata)
	}
	return metadata
}

func metadataStringValue(metadata map[string]any, key string) string {
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func metadataNumber(metadata map[string]any, key string) int {
	switch value := metadata[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	default:
		return 0
	}
}

func encodeOptionMetadata(metadata map[string]any) string {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func runSkillOptTrainStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt train stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	sessionID := fs.String("session", "", "train session id")
	reason := fs.String("reason", "", "reason for abandoning the train session")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt train stop does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*sessionID) == "" || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "skillopt train stop requires --session and --reason")
		return 2
	}
	var stopped db.SkillOptTrainIteration
	if err := withStore(*home, func(store *db.Store) error {
		iteration, err := stopSkillOptTrainSession(context.Background(), store, strings.TrimSpace(*sessionID), strings.TrimSpace(*reason))
		if err != nil {
			return err
		}
		stopped = iteration
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt train stop: %v\n", err)
		return 1
	}
	writeLine(stdout, "stopped train session %s", strings.TrimSpace(*sessionID))
	writeLine(stdout, "iteration: %s", stopped.ID)
	writeLine(stdout, "reason: %s", strings.TrimSpace(*reason))
	return 0
}

// stopSkillOptTrainSession abandons a train session with a reason — the shared
// body behind `train stop` and the dashboard's stop action.
func stopSkillOptTrainSession(ctx context.Context, store *db.Store, sessionID string, reason string) (db.SkillOptTrainIteration, error) {
	if sessionID == "" || strings.TrimSpace(reason) == "" {
		return db.SkillOptTrainIteration{}, errors.New("a session id and a reason are required")
	}
	session, err := store.GetSkillOptTrainSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.SkillOptTrainIteration{}, fmt.Errorf("train session %s not found", sessionID)
		}
		return db.SkillOptTrainIteration{}, err
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(ctx, session.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.SkillOptTrainIteration{}, fmt.Errorf("train session %s has no iteration to stop", session.ID)
		}
		return db.SkillOptTrainIteration{}, err
	}
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateRunAbandoned); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	session.State = skillopt.TrainStateRunAbandoned
	session.MetadataJSON = skillOptTrainDecisionMetadata(session.MetadataJSON, reason)
	iteration.State = skillopt.TrainStateRunAbandoned
	iteration.DecisionReason = strings.TrimSpace(reason)
	iteration.MetadataJSON = skillOptTrainDecisionMetadata(iteration.MetadataJSON, reason)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	return iteration, nil
}

// deleteSkillOptTrainSession removes a train session and its history, and
// returns the GitHub repos gitmoot recorded as created for it. The records
// deliberately survive the cascade so a caller can offer their cleanup; any
// future delete surface must list them BEFORE the delete or they orphan.
func deleteSkillOptTrainSession(ctx context.Context, store *db.Store, sessionID string) ([]string, error) {
	records, err := store.ListCreatedReposForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if err := store.DeleteSkillOptTrainSession(ctx, sessionID); err != nil {
		return nil, err
	}
	// Remove the per-option target agents this session scaffolded so they don't
	// accumulate on the Agents page / agent list (best-effort — internal plumbing).
	removeSkillOptTrainTargetAgents(ctx, store, sessionID)
	repos := make([]string, 0, len(records))
	for _, record := range records {
		repos = append(repos, record.Repo)
	}
	return repos, nil
}

// removeSkillOptTrainTargetAgents deletes the `skillopt-target-<run>-…` agents a
// training session scaffolded (one per generated option). The session id is
// embedded in each name, so it only touches this session's plumbing. Best-effort.
func removeSkillOptTrainTargetAgents(ctx context.Context, store *db.Store, sessionID string) {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return
	}
	marker := skillOptSafeAgentName(strings.TrimSpace(sessionID))
	for _, agent := range agents {
		if strings.HasPrefix(agent.Name, "skillopt-target-") && marker != "" && strings.Contains(agent.Name, marker) {
			_, _ = store.RemoveAgent(ctx, agent.Name)
		}
	}
}

// cleanupCreatedTrainRepo deletes a gitmoot-created GitHub repo and then its
// created_repos record — in that order, so a failed GitHub delete keeps the
// repo on offer for a retry.
func cleanupCreatedTrainRepo(ctx context.Context, store *db.Store, repo string) error {
	parsed, err := daemon.ParseRepository(repo)
	if err != nil {
		return err
	}
	if err := newSkillOptGitHubClient().DeleteRepository(ctx, parsed); err != nil {
		return err
	}
	return store.DeleteCreatedRepoRecord(ctx, repo)
}

func loadSkillOptTrainStatus(ctx context.Context, store *db.Store, sessionID string) (db.SkillOptTrainSession, *db.SkillOptTrainIteration, skillopt.TrainStatusCounts, error) {
	session, err := store.GetSkillOptTrainSession(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, fmt.Errorf("train session %s not found", strings.TrimSpace(sessionID))
		}
		return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, err
	}
	latest, err := store.GetLatestSkillOptTrainIteration(ctx, session.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return session, nil, skillopt.TrainStatusCounts{}, nil
		}
		return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, err
	}
	counts, err := loadSkillOptTrainStatusCounts(ctx, store, latest.EvalRunID)
	if err != nil {
		return db.SkillOptTrainSession{}, nil, skillopt.TrainStatusCounts{}, err
	}
	return session, &latest, counts, nil
}

func loadSkillOptTrainStatusCounts(ctx context.Context, store *db.Store, runID string) (skillopt.TrainStatusCounts, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return skillopt.TrainStatusCounts{}, nil
	}
	items, err := store.ListEvalReviewItems(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	feedbackEvents, err := store.ListFeedbackEvents(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	rankedFeedbackEvents, err := store.ListRankedFeedbackEvents(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, runID)
	if err != nil {
		return skillopt.TrainStatusCounts{}, err
	}
	return skillopt.TrainStatusCounts{
		ReviewItems:          len(items),
		FeedbackEvents:       len(feedbackEvents),
		RankedFeedbackEvents: len(rankedFeedbackEvents),
		PairwisePreferences:  len(pairwisePreferences),
	}, nil
}

func loadSkillOptTrainStatusSnapshot(ctx context.Context, store *db.Store, sessionID string, verbose bool) (skillOptTrainStatusSnapshot, error) {
	session, iteration, counts, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainStatusSnapshot{}, err
	}
	summary := skillopt.BuildTrainStatusSummary(session, iteration, counts)
	snapshot := buildSkillOptTrainStatusSnapshot(session, iteration, summary, counts)
	details, err := buildSkillOptTrainStatusVerbose(ctx, store, session, iteration)
	if err != nil {
		return skillOptTrainStatusSnapshot{}, err
	}
	snapshot.Verbose = &details
	snapshot = applySkillOptTrainStableStatus(snapshot)
	if !verbose {
		snapshot.Verbose = nil
	}
	return snapshot, nil
}

func buildSkillOptTrainStatusSnapshot(session db.SkillOptTrainSession, iteration *db.SkillOptTrainIteration, summary skillopt.TrainStatusSummary, counts skillopt.TrainStatusCounts) skillOptTrainStatusSnapshot {
	policy := summary.PreviewPolicy
	generatedOptions := 0
	if iteration != nil {
		generatedOptions = metadataNumber(decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["generation"]), "generated_options")
	} else {
		generatedOptions = metadataNumber(decodedSkillOptMetadataValue(decodedSkillOptMetadata(session.MetadataJSON)["generation"]), "generated_options")
	}
	currentStep := strings.TrimSpace(summary.BlockedStep)
	if currentStep == "" {
		currentStep = summary.CurrentPhase
	}
	return skillOptTrainStatusSnapshot{
		SessionID:          summary.SessionID,
		IterationID:        summary.IterationID,
		TemplateID:         strings.TrimSpace(session.TemplateID),
		TemplateVersion:    strings.TrimSpace(session.TemplateVersionID),
		TargetRepo:         strings.TrimSpace(session.TargetRepo),
		WorkspaceRepo:      strings.TrimSpace(session.WorkspaceRepo),
		TaskKind:           strings.TrimSpace(session.TaskKind),
		StatusPhase:        summary.CurrentPhase,
		CurrentPhase:       summary.CurrentPhase,
		CurrentStep:        currentStep,
		CompletedSteps:     append([]string(nil), summary.CompletedSteps...),
		BlockedStep:        summary.BlockedStep,
		NextAction:         summary.NextAction,
		IssueURL:           summary.IssueURL,
		PullRequestURL:     summary.PullRequestURL,
		ContinueFromGitHub: skillOptTrainContinueFromGitHubURL(summary.CurrentPhase, summary.IssueURL),
		CandidateVersion:   summary.CandidateVersion,
		PreviewPolicy: skillOptTrainPreviewPolicyJSON{
			Mode:               policy.Mode,
			Renderer:           policy.Renderer,
			Publisher:          policy.Publisher,
			Repo:               policy.Repo,
			RouteTemplate:      policy.RouteTemplate,
			ExpectedReviewRepo: policy.ExpectedReviewRepo,
		},
		Counts: skillOptTrainStatusCountsJSON{
			ReviewItems:          counts.ReviewItems,
			FeedbackEvents:       counts.FeedbackEvents,
			RankedFeedbackEvents: counts.RankedFeedbackEvents,
			PairwisePreferences:  counts.PairwisePreferences,
		},
		Progress: skillOptTrainStatusProgress{
			ReviewItems:          counts.ReviewItems,
			FeedbackEvents:       counts.FeedbackEvents,
			RankedFeedbackEvents: counts.RankedFeedbackEvents,
			PairwisePreferences:  counts.PairwisePreferences,
			GeneratedOptions:     generatedOptions,
			ETA:                  "unknown",
		},
	}
}

func applySkillOptTrainStableStatus(snapshot skillOptTrainStatusSnapshot) skillOptTrainStatusSnapshot {
	if strings.TrimSpace(snapshot.StatusPhase) == "" {
		snapshot.StatusPhase = strings.TrimSpace(snapshot.CurrentPhase)
	}
	if snapshot.Verbose != nil {
		if reason := strings.TrimSpace(snapshot.Verbose.Candidate.NoCandidateReason); reason != "" {
			snapshot.NoCandidateReason = reason
		}
		if len(snapshot.Verbose.Candidate.NoCandidateDetails) > 0 {
			snapshot.NoCandidateDetails = snapshot.Verbose.Candidate.NoCandidateDetails
		}
		if optimizer := snapshot.Verbose.Optimizer; optimizer != nil {
			if available, ok := optimizer["recovery_available"].(bool); ok {
				snapshot.RecoveryAvailable = available
			}
		}
	}
	snapshot.StatusPhase = skillOptTrainStableStatusPhase(snapshot)
	return snapshot
}

// skillOptTrainLockPhase maps an active generation/optimizer resource lock to a
// stable status phase. It is shared by `train status` and the dashboard so both
// report the same live phase. ok is false when no lock determines the phase.
func skillOptTrainLockPhase(locks []skillOptTrainStatusLock) (string, bool) {
	for _, lock := range locks {
		switch lock.Name {
		case "optimizer", "optimizer_legacy":
			switch strings.TrimSpace(lock.Status) {
			case "active":
				return "optimizer_running", true
			case "active_expired_heartbeat":
				return "optimizer_heartbeat_stale", true
			case "stale":
				return "blocked_stale_lock", true
			}
		case "generation":
			switch strings.TrimSpace(lock.Status) {
			case "active":
				return "generating_options", true
			case "active_expired_heartbeat":
				return "generating_options_heartbeat_stale", true
			case "stale":
				return "blocked_stale_lock", true
			}
		}
	}
	return "", false
}

func skillOptTrainStableStatusPhase(snapshot skillOptTrainStatusSnapshot) string {
	if snapshot.Verbose != nil {
		if phase, ok := skillOptTrainLockPhase(snapshot.Verbose.ActiveLocks); ok {
			return phase
		}
		statuses := snapshot.Verbose.MetadataStatus
		optimizerStatus := strings.TrimSpace(statuses["optimizer"])
		candidateImportStatus := strings.TrimSpace(statuses["candidate_import"])
		if optimizerStatus == "preflight_running" || optimizerStatus == "preflight" {
			return "preflight_running"
		}
		if snapshot.RecoveryAvailable && optimizerStatus == "failed" {
			return "recovery_available"
		}
		if skillOptStatusFailureLooksConfigBlocked(snapshot.Verbose.Optimizer) {
			return "blocked_config"
		}
		if optimizerStatus == "failed" || candidateImportStatus == "failed" {
			return "failed_unrecoverable"
		}
	}
	switch strings.TrimSpace(snapshot.CurrentPhase) {
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		return "optimizer_completed_no_candidate"
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		return "optimizer_completed_candidate"
	}
	if snapshot.RecoveryAvailable {
		return "recovery_available"
	}
	if strings.TrimSpace(snapshot.StatusPhase) != "" {
		return strings.TrimSpace(snapshot.StatusPhase)
	}
	return strings.TrimSpace(snapshot.CurrentPhase)
}

func skillOptStatusFailureLooksConfigBlocked(metadata map[string]any) bool {
	if metadataString(metadata, "status") != "failed" {
		return false
	}
	errorText := strings.ToLower(metadataString(metadata, "error"))
	for _, marker := range []string{"config", "credential", "api key", "openai", "azure", "backend", "gitmoot-skillopt", "executable", "not found", "install", "path"} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

func buildSkillOptTrainStatusVerbose(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration *db.SkillOptTrainIteration) (skillOptTrainStatusVerbose, error) {
	details := skillOptTrainStatusVerbose{
		CreatedAt: session.CreatedAt,
		UpdatedAt: session.UpdatedAt,
		Elapsed:   skillOptElapsedText(session.CreatedAt),
		Jobs:      skillOptTrainStatusJobs{},
	}
	statuses := map[string]string{}
	addStatus := func(name string, metadata map[string]any) {
		if status := metadataString(metadata, "status"); status != "" {
			statuses[name] = status
		}
	}
	statusMetadata := decodedSkillOptMetadata(session.MetadataJSON)
	var iterationMetadata map[string]any
	if iteration != nil {
		iterationMetadata = decodedSkillOptMetadata(iteration.MetadataJSON)
		statusMetadata = iterationMetadata
	}
	addStatus("generation", decodedSkillOptMetadataValue(statusMetadata["generation"]))
	addStatus("review", decodedSkillOptMetadataValue(statusMetadata["review"]))
	addStatus("feedback_sync", decodedSkillOptMetadataValue(statusMetadata["feedback_sync"]))
	addStatus("optimizer", decodedSkillOptMetadataValue(statusMetadata["optimizer"]))
	addStatus("candidate_import", decodedSkillOptMetadataValue(statusMetadata["candidate_import"]))
	addStatus("candidate_review", decodedSkillOptMetadataValue(statusMetadata["candidate_review"]))
	addStatus("candidate_decision", decodedSkillOptMetadataValue(statusMetadata["candidate_decision"]))
	if len(statuses) > 0 {
		details.MetadataStatus = statuses
	}
	// Carry the full generation metadata (status + error) so a failed background
	// generate can be surfaced in the train-run view rather than silently stalling.
	if generation := decodedSkillOptMetadataValue(statusMetadata["generation"]); len(generation) > 0 {
		details.Generation = generation
	}
	if iteration == nil {
		return details, nil
	}
	details.EvalRunID = strings.TrimSpace(iteration.EvalRunID)
	details.BaseTemplateVersionID = strings.TrimSpace(iteration.BaseTemplateVersionID)
	details.Mode = strings.TrimSpace(iteration.Mode)
	details.ExplorationLevel = strings.TrimSpace(iteration.ExplorationLevel)
	details.CreatedAt = iteration.CreatedAt
	details.UpdatedAt = iteration.UpdatedAt
	details.Elapsed = skillOptElapsedText(iteration.CreatedAt)
	details.ReviewIssue = skillOptTrainStatusReviewIssue{
		Repo:   strings.TrimSpace(iteration.IssueRepo),
		Number: iteration.IssueNumber,
		URL:    strings.TrimSpace(iteration.IssueURL),
	}
	candidateImport := decodedSkillOptMetadataValue(iterationMetadata["candidate_import"])
	details.Candidate = skillOptTrainStatusCandidate{
		VersionID:          strings.TrimSpace(iteration.CandidateVersionID),
		PullRequestURL:     strings.TrimSpace(iteration.PullRequestURL),
		NoCandidateReason:  metadataString(candidateImport, "no_candidate_reason"),
		NoCandidateDetails: decodedSkillOptMetadataValue(candidateImport["no_candidate_details"]),
	}
	if details.Candidate.PullRequestURL == "" {
		// Issue-based candidate reviews never set iteration.PullRequestURL; the
		// decision link lives in the candidate_review metadata instead.
		review := decodedSkillOptMetadataValue(iterationMetadata["candidate_review"])
		details.Candidate.PullRequestURL = skillOptCandidateReviewURLFromMetadata(review)
	}
	if optimizer := decodedSkillOptMetadataValue(iterationMetadata["optimizer"]); len(optimizer) > 0 {
		candidateImport := decodedSkillOptMetadataValue(iterationMetadata["candidate_import"])
		if attemptState := skillOptTrainOptimizerAttemptState(skillopt.NormalizeTrainState(iteration.State), optimizer, candidateImport); attemptState != "" {
			optimizer["optimizer_attempt_state"] = attemptState
		}
		optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(config.Paths{}, session, *iteration, skillOptTrainOptimizerRequest{})
		if err == nil {
			optimizer["recovery_available"] = skillOptTrainOptimizerRecoveryAvailable(optimizerPaths)
		}
		details.Optimizer = optimizer
	}
	activeLocks, err := skillOptTrainActiveLocks(ctx, store, session.ID, iteration.ID)
	if err != nil {
		return skillOptTrainStatusVerbose{}, err
	}
	details.ActiveLocks = activeLocks
	jobs, err := skillOptTrainStatusJobSummary(ctx, store, iterationMetadata)
	if err != nil {
		return skillOptTrainStatusVerbose{}, err
	}
	details.Jobs = jobs
	items, err := skillOptTrainStatusItems(ctx, store, iteration.EvalRunID)
	if err != nil {
		return skillOptTrainStatusVerbose{}, err
	}
	details.Items = items
	return details, nil
}

func skillOptTrainActiveLocks(ctx context.Context, store *db.Store, sessionID string, iterationID string) ([]skillOptTrainStatusLock, error) {
	candidates := []struct {
		name string
		key  string
	}{
		{name: "generation", key: skillOptTrainGenerationLockKey(sessionID, iterationID)},
		{name: "review", key: skillOptTrainReviewLockKey(sessionID, iterationID)},
		{name: "optimizer", key: skillOptTrainOptimizerLockKey(sessionID, iterationID)},
		{name: "optimizer_legacy", key: skillOptTrainLegacyOptimizerLockKey(sessionID, iterationID)},
		{name: "candidate_review", key: skillOptTrainCandidateReviewLockKey(sessionID, iterationID)},
		{name: "start_next", key: skillOptTrainStartNextLockKey(sessionID)},
	}
	locks := []skillOptTrainStatusLock{}
	now := time.Now().UTC()
	for _, candidate := range candidates {
		lock, err := store.GetResourceLock(ctx, candidate.key)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		status := "active"
		switch candidate.name {
		case "optimizer", "optimizer_legacy", "generation":
			// Report a heartbeat-stale/stale status (via OwnerPID liveness)
			// instead of dropping an expired-but-running lock, so the phase
			// does not flap back to items_ready mid-generation.
			status = skillOptTrainOptimizerLockStatus(lock, now)
		default:
			if !skillOptResourceLockActive(lock, now) {
				continue
			}
		}
		locks = append(locks, skillOptTrainStatusLock{
			Name:          candidate.name,
			Key:           lock.ResourceKey,
			Status:        status,
			OwnerJobID:    strings.TrimSpace(lock.OwnerJobID),
			OwnerPID:      lock.OwnerPID,
			OwnerHostname: strings.TrimSpace(lock.OwnerHostname),
			CommandHash:   strings.TrimSpace(lock.CommandHash),
			AcquiredAt:    strings.TrimSpace(lock.AcquiredAt),
			UpdatedAt:     strings.TrimSpace(lock.UpdatedAt),
			ExpiresAt:     strings.TrimSpace(lock.ExpiresAt),
			Elapsed:       skillOptLockElapsedText(lock.AcquiredAt, now),
		})
	}
	return locks, nil
}

func skillOptResourceLockActive(lock db.ResourceLock, now time.Time) bool {
	expiresAt := strings.TrimSpace(lock.ExpiresAt)
	if expiresAt == "" {
		return true
	}
	parsed, ok := parseSkillOptStatusTime(expiresAt)
	if !ok {
		return true
	}
	return parsed.After(now)
}

func skillOptTrainStatusJobSummary(ctx context.Context, store *db.Store, metadata map[string]any) (skillOptTrainStatusJobs, error) {
	generation := decodedSkillOptMetadataValue(metadata["generation"])
	jobIDs := metadataStringSlice(generation, "jobs")
	summary := skillOptTrainStatusJobs{}
	for _, jobID := range jobIDs {
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return skillOptTrainStatusJobs{}, err
		}
		summary.Total++
		switch strings.TrimSpace(strings.ToLower(job.State)) {
		case "queued":
			summary.Queued++
		case "running":
			summary.Running++
		case "succeeded":
			summary.Succeeded++
		case "failed":
			summary.Failed++
		default:
			summary.Other++
		}
		summary.Items = append(summary.Items, skillOptTrainStatusJobRef{
			ID:    strings.TrimSpace(job.ID),
			Agent: strings.TrimSpace(job.Agent),
			Type:  strings.TrimSpace(job.Type),
			State: strings.TrimSpace(job.State),
		})
	}
	return summary, nil
}

func skillOptTrainStatusItems(ctx context.Context, store *db.Store, runID string) ([]skillOptTrainStatusItem, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, nil
	}
	run, err := store.GetEvalRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	rankedRun := skillOptRunUsesRankedOptions(run)
	reviewItems, err := store.ListEvalReviewItems(ctx, runID)
	if err != nil {
		return nil, err
	}
	items := make([]skillOptTrainStatusItem, 0, len(reviewItems))
	for _, item := range reviewItems {
		statusItem := skillOptTrainStatusItem{
			ItemID: strings.TrimSpace(item.ItemID),
			Title:  strings.TrimSpace(item.Title),
		}
		options, err := store.ListEvalReviewOptions(ctx, runID, item.ItemID)
		if err != nil {
			return nil, err
		}
		for _, option := range options {
			statusItem.OptionLabels = append(statusItem.OptionLabels, strings.ToUpper(strings.TrimSpace(option.Label)))
		}
		if len(statusItem.OptionLabels) == 0 && !rankedRun {
			if strings.TrimSpace(item.BaselineArtifactID) != "" {
				statusItem.OptionLabels = append(statusItem.OptionLabels, "BASELINE")
			}
			if strings.TrimSpace(item.CandidateArtifactID) != "" {
				statusItem.OptionLabels = append(statusItem.OptionLabels, "CANDIDATE")
			}
			if len(statusItem.OptionLabels) == 0 {
				statusItem.OptionLabels = append(statusItem.OptionLabels, "BASELINE", "CANDIDATE")
			}
		}
		items = append(items, statusItem)
	}
	return items, nil
}

func metadataStringSlice(metadata map[string]any, key string) []string {
	value, ok := metadata[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				values = append(values, text)
			}
		}
		return values
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{strings.TrimSpace(typed)}
	default:
		return nil
	}
}

func skillOptElapsedText(startedAt string) string {
	started, ok := parseSkillOptStatusTime(startedAt)
	if !ok {
		return "unknown"
	}
	elapsed := time.Since(started)
	if elapsed < 0 {
		return "unknown"
	}
	return elapsed.Round(time.Second).String()
}

func parseSkillOptStatusTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func skillOptTrainWatchState(snapshot skillOptTrainStatusSnapshot) string {
	if skillOptTrainWatchDone(snapshot) {
		return "waiting"
	}
	return "active"
}

func skillOptTrainWatchDone(snapshot skillOptTrainStatusSnapshot) bool {
	if skillopt.IsTerminalTrainState(snapshot.CurrentPhase) {
		return true
	}
	if snapshot.Verbose == nil {
		return true
	}
	for _, lock := range snapshot.Verbose.ActiveLocks {
		if strings.TrimSpace(lock.Status) != "stale" {
			return false
		}
	}
	return true
}

func printSkillOptTrainStatus(stdout io.Writer, summary skillopt.TrainStatusSummary, counts skillopt.TrainStatusCounts) {
	writeLine(stdout, "session: %s", summary.SessionID)
	writeLine(stdout, "iteration: %s", emptyText(summary.IterationID))
	writeLine(stdout, "preview_mode: %s", summary.PreviewPolicy.Mode)
	writeLine(stdout, "preview_renderer: %s", summary.PreviewPolicy.Renderer)
	writeLine(stdout, "preview_publisher: %s", summary.PreviewPolicy.Publisher)
	writeLine(stdout, "preview_repo: %s", emptyText(summary.PreviewPolicy.Repo))
	writeLine(stdout, "preview_route_template: %s", emptyText(summary.PreviewPolicy.RouteTemplate))
	writeLine(stdout, "expected_review_repo: %s", emptyText(summary.PreviewPolicy.ExpectedReviewRepo))
	writeLine(stdout, "current_phase: %s", summary.CurrentPhase)
	writeLine(stdout, "completed_steps: %s", strings.Join(summary.CompletedSteps, ","))
	writeLine(stdout, "blocked_step: %s", emptyText(summary.BlockedStep))
	writeLine(stdout, "next_action: %s", summary.NextAction)
	writeLine(stdout, "issue: %s", emptyText(summary.IssueURL))
	writeLine(stdout, "pull_request: %s", emptyText(summary.PullRequestURL))
	writeLine(stdout, "candidate: %s", emptyText(summary.CandidateVersion))
	if url := skillOptTrainContinueFromGitHubURL(summary.CurrentPhase, summary.IssueURL); url != "" {
		writeLine(stdout, "continue_from_github: %s", url)
	}
	writeLine(stdout, "review_items: %d", counts.ReviewItems)
	writeLine(stdout, "feedback: %d", summary.FeedbackCount)
	writeLine(stdout, "pairwise_preferences: %d", counts.PairwisePreferences)
}

// skillOptTrainContinueFromGitHubURL returns the GitHub issue/PR URL a human can
// act on to advance the train from a review-blocked phase (the review-watcher
// imports comments). It returns "" for phases that are not blocked on a human
// reviewing on GitHub. At candidate_review_published the iteration's IssueURL
// already points at the candidate review issue.
func skillOptTrainContinueFromGitHubURL(phase, issueURL string) string {
	switch phase {
	case "review_published", "candidate_review_published":
		return strings.TrimSpace(issueURL)
	default:
		return ""
	}
}

func printSkillOptTrainStatusSnapshot(stdout io.Writer, snapshot skillOptTrainStatusSnapshot, verbose bool) {
	writeLine(stdout, "session: %s", snapshot.SessionID)
	writeLine(stdout, "iteration: %s", emptyText(snapshot.IterationID))
	writeLine(stdout, "preview_mode: %s", snapshot.PreviewPolicy.Mode)
	writeLine(stdout, "preview_renderer: %s", snapshot.PreviewPolicy.Renderer)
	writeLine(stdout, "preview_publisher: %s", snapshot.PreviewPolicy.Publisher)
	writeLine(stdout, "preview_repo: %s", emptyText(snapshot.PreviewPolicy.Repo))
	writeLine(stdout, "preview_route_template: %s", emptyText(snapshot.PreviewPolicy.RouteTemplate))
	writeLine(stdout, "expected_review_repo: %s", emptyText(snapshot.PreviewPolicy.ExpectedReviewRepo))
	writeLine(stdout, "status_phase: %s", emptyText(snapshot.StatusPhase))
	writeLine(stdout, "current_phase: %s", snapshot.CurrentPhase)
	writeLine(stdout, "completed_steps: %s", strings.Join(snapshot.CompletedSteps, ","))
	writeLine(stdout, "blocked_step: %s", emptyText(snapshot.BlockedStep))
	writeLine(stdout, "current_step: %s", snapshot.CurrentStep)
	writeLine(stdout, "next_action: %s", snapshot.NextAction)
	writeLine(stdout, "issue: %s", emptyText(snapshot.IssueURL))
	writeLine(stdout, "pull_request: %s", emptyText(snapshot.PullRequestURL))
	writeLine(stdout, "candidate: %s", emptyText(snapshot.CandidateVersion))
	if url := skillOptTrainContinueFromGitHubURL(snapshot.CurrentPhase, snapshot.IssueURL); url != "" {
		writeLine(stdout, "continue_from_github: %s", url)
	}
	writeLine(stdout, "recovery_available: %t", snapshot.RecoveryAvailable)
	if snapshot.NoCandidateReason != "" {
		writeLine(stdout, "no_candidate_reason: %s", snapshot.NoCandidateReason)
	}
	printSkillOptNoCandidateDetails(stdout, snapshot.NoCandidateDetails)
	writeLine(stdout, "review_items: %d", snapshot.Counts.ReviewItems)
	writeLine(stdout, "feedback: %d", snapshot.Counts.FeedbackEvents+snapshot.Counts.RankedFeedbackEvents)
	writeLine(stdout, "pairwise_preferences: %d", snapshot.Counts.PairwisePreferences)
	if !verbose || snapshot.Verbose == nil {
		return
	}
	writeLine(stdout, "elapsed: %s", snapshot.Verbose.Elapsed)
	writeLine(stdout, "eta: %s", snapshot.Progress.ETA)
	writeLine(stdout, "generated_options: %d", snapshot.Progress.GeneratedOptions)
	writeLine(stdout, "jobs_total: %d", snapshot.Verbose.Jobs.Total)
	writeLine(stdout, "jobs_running: %d", snapshot.Verbose.Jobs.Running)
	writeLine(stdout, "jobs_succeeded: %d", snapshot.Verbose.Jobs.Succeeded)
	writeLine(stdout, "jobs_failed: %d", snapshot.Verbose.Jobs.Failed)
	if snapshot.Verbose.EvalRunID != "" {
		writeLine(stdout, "eval_run: %s", snapshot.Verbose.EvalRunID)
	}
	if snapshot.Verbose.Mode != "" {
		writeLine(stdout, "mode: %s", snapshot.Verbose.Mode)
	}
	if snapshot.Verbose.ExplorationLevel != "" {
		writeLine(stdout, "exploration_level: %s", snapshot.Verbose.ExplorationLevel)
	}
	if snapshot.Verbose.ReviewIssue.URL != "" {
		writeLine(stdout, "review_issue: %s", snapshot.Verbose.ReviewIssue.URL)
	}
	if snapshot.NoCandidateReason == "" && snapshot.Verbose.Candidate.NoCandidateReason != "" {
		writeLine(stdout, "no_candidate_reason: %s", snapshot.Verbose.Candidate.NoCandidateReason)
	}
	if len(snapshot.NoCandidateDetails) == 0 {
		printSkillOptNoCandidateDetails(stdout, snapshot.Verbose.Candidate.NoCandidateDetails)
	}
	if snapshot.Verbose.Optimizer != nil {
		if attempt := metadataString(snapshot.Verbose.Optimizer, "optimizer_attempt"); attempt != "" {
			writeLine(stdout, "optimizer_attempt: %s", attempt)
		}
		if status := metadataString(snapshot.Verbose.Optimizer, "optimizer_attempt_state"); status != "" {
			writeLine(stdout, "optimizer_attempt_state: %s", status)
		}
		if path := metadataString(snapshot.Verbose.Optimizer, "optimizer_attempt_path"); path != "" {
			writeLine(stdout, "optimizer_attempt_path: %s", path)
		}
		if mode := metadataString(snapshot.Verbose.Optimizer, "feedback_direct_mode"); mode != "" {
			writeLine(stdout, "feedback_direct_mode: %s", mode)
		}
		for _, key := range []string{
			"optimizer_views",
			"retry_optimizer_views",
			"target_artifact_retry_budget",
			"hard_failure_retry_budget",
			"noop_retry_budget",
			"gate_reject_retry_budget",
			"wrong_artifact_retry_budget",
		} {
			if value := metadataString(snapshot.Verbose.Optimizer, key); value != "" {
				writeLine(stdout, "%s: %s", key, value)
			}
		}
		if available, ok := snapshot.Verbose.Optimizer["recovery_available"]; ok {
			writeLine(stdout, "optimizer_recovery_available: %v", available)
		}
	}
	for _, lock := range snapshot.Verbose.ActiveLocks {
		writeLine(stdout, "active_lock: %s", skillOptTrainStatusLockText(lock))
	}
}

func skillOptTrainOptimizerAttemptState(currentPhase string, optimizer map[string]any, candidateImport map[string]any) string {
	optimizerStatus := metadataString(optimizer, "status")
	if optimizerStatus == "running" {
		return "running"
	}
	switch strings.TrimSpace(currentPhase) {
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		return "completed_no_candidate"
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		return "completed_candidate"
	}
	optimizerAttempt := metadataString(optimizer, "optimizer_attempt")
	importAttempt := metadataString(candidateImport, "optimizer_attempt")
	if optimizerAttempt != "" && importAttempt != "" && optimizerAttempt != importAttempt {
		candidateImport = nil
	}
	switch metadataString(candidateImport, "status") {
	case "no_candidate":
		return "completed_no_candidate"
	case "succeeded", "recovered":
		return "completed_candidate"
	case "failed":
		return "candidate_import_failed"
	}
	return optimizerStatus
}

func printSkillOptNoCandidateDetails(stdout io.Writer, details map[string]any) {
	if len(details) == 0 {
		return
	}
	for _, key := range []string{
		"feedback_source",
		"feedback_target",
		"review_issue",
		"review_run_id",
		"reviewed_skill_version",
		"score_basis",
	} {
		if value := metadataString(details, key); value != "" {
			writeLine(stdout, "%s: %s", key, value)
		}
	}
	if attemptedPatch := metadataString(details, "attempted_patch"); attemptedPatch != "" {
		writeLine(stdout, "attempted_patch: %s", attemptedPatch)
	}
	for _, key := range []string{
		"baseline_hard",
		"baseline_soft",
		"baseline_gate",
		"candidate_hard",
		"candidate_soft",
		"candidate_gate",
	} {
		if value := metadataString(details, key); value != "" {
			writeLine(stdout, "%s: %s", key, value)
		}
	}
	if retryAttempts := metadataString(details, "retry_attempts"); retryAttempts != "" {
		writeLine(stdout, "retry_attempts: %s", retryAttempts)
	}
	if retryBudget := metadataString(details, "retry_budget"); retryBudget != "" {
		writeLine(stdout, "retry_budget: %s", retryBudget)
	}
	if duplicateRetry := metadataBoolPtr(details, "duplicate_retry_detected"); duplicateRetry != nil {
		writeLine(stdout, "duplicate_retry_detected: %t", *duplicateRetry)
	}
	if diagnosticCategories := metadataStringSlice(details, "diagnostic_categories"); len(diagnosticCategories) > 0 {
		writeLine(stdout, "diagnostic_categories: %s", strings.Join(diagnosticCategories, ","))
	}
	if selectionGateRelation := metadataString(details, "selection_gate_relation"); selectionGateRelation != "" {
		writeLine(stdout, "selection_gate_relation: %s", selectionGateRelation)
	}
	if retryBudgetExhausted := metadataBoolPtr(details, "retry_budget_exhausted"); retryBudgetExhausted != nil {
		writeLine(stdout, "retry_budget_exhausted: %t", *retryBudgetExhausted)
	}
	if retryStopReasons := metadataStringSlice(details, "retry_stop_reasons"); len(retryStopReasons) > 0 {
		writeLine(stdout, "retry_stop_reasons: %s", strings.Join(retryStopReasons, ","))
	}
	if optimizerContextItems := metadataStringSlice(details, "optimizer_context_items"); len(optimizerContextItems) > 0 {
		writeLine(stdout, "optimizer_context_items: %s", strings.Join(optimizerContextItems, ","))
	}
	if scoreGap := metadataString(details, "score_gap"); scoreGap != "" {
		writeLine(stdout, "score_gap: %s", scoreGap)
	}
	if scoreGapHandling := metadataString(details, "score_gap_handling"); scoreGapHandling != "" {
		writeLine(stdout, "score_gap_handling: %s", scoreGapHandling)
	}
	if hardScoreHandling := metadataString(details, "hard_score_handling"); hardScoreHandling != "" {
		writeLine(stdout, "hard_score_handling: %s", hardScoreHandling)
	}
	if stopReason := metadataString(details, "stop_reason"); stopReason != "" {
		writeLine(stdout, "stop_reason: %s", stopReason)
	}
	if evaluatorReason := metadataString(details, "evaluator_reason"); evaluatorReason != "" {
		writeLine(stdout, "evaluator_reason: %s", evaluatorReason)
	}
	if optimizerHint := metadataString(details, "optimizer_hint"); optimizerHint != "" {
		writeLine(stdout, "optimizer_hint: %s", optimizerHint)
	}
	if failedDimensions := metadataStringSlice(details, "failed_dimensions"); len(failedDimensions) > 0 {
		writeLine(stdout, "failed_dimensions: %s", strings.Join(failedDimensions, ","))
	}
	if feedbackThemes := metadataStringSlice(details, "feedback_themes"); len(feedbackThemes) > 0 {
		writeLine(stdout, "feedback_themes: %s", strings.Join(feedbackThemes, "; "))
	}
	printSkillOptHumanFeedbackContext(stdout, decodedSkillOptMetadataValue(details["human_feedback_context"]))
	nextActions := metadataStringSlice(details, "next_actions")
	if len(nextActions) == 0 {
		nextActions = metadataStringSlice(details, "next_action")
	}
	for _, nextAction := range nextActions {
		writeLine(stdout, "next_action_option: %s", nextAction)
	}
	rejection := decodedSkillOptMetadataValue(details["rejection"])
	if len(rejection) == 0 {
		return
	}
	baseline := decodedSkillOptMetadataValue(rejection["baseline"])
	candidate := decodedSkillOptMetadataValue(rejection["candidate"])
	if len(baseline) > 0 || len(candidate) > 0 {
		writeLine(stdout, "rejection: baseline_gate=%s candidate_gate=%s", metadataString(baseline, "gate_score"), metadataString(candidate, "gate_score"))
	}
	if optimizerHint := metadataString(rejection, "optimizer_hint"); optimizerHint != "" {
		writeLine(stdout, "rejection_optimizer_hint: %s", optimizerHint)
	}
	if failedDimensions := metadataStringSlice(rejection, "failed_dimensions"); len(failedDimensions) > 0 {
		writeLine(stdout, "rejection_failed_dimensions: %s", strings.Join(failedDimensions, ","))
	}
	printSkillOptHumanFeedbackContext(stdout, decodedSkillOptMetadataValue(rejection["human_feedback_context"]))
}

func printSkillOptHumanFeedbackContext(stdout io.Writer, context map[string]any) {
	if len(context) == 0 {
		return
	}
	for _, key := range []string{
		"feedback_source",
		"feedback_target",
		"review_issue",
		"review_run_id",
		"reviewed_skill_version",
		"source_item_ids",
		"rankings",
		"themes",
		"preserve",
		"improve",
		"avoid",
	} {
		if values := metadataStringSlice(context, key); len(values) > 0 {
			writeLine(stdout, "human_feedback_%s: %s", key, strings.Join(values, "; "))
		} else if value := metadataString(context, key); value != "" {
			writeLine(stdout, "human_feedback_%s: %s", key, value)
		}
	}
}

func skillOptTrainStatusLockText(lock skillOptTrainStatusLock) string {
	parts := []string{
		strings.TrimSpace(lock.Name),
		strings.TrimSpace(lock.Key),
		"status=" + emptyText(lock.Status),
	}
	if strings.TrimSpace(lock.OwnerJobID) != "" {
		parts = append(parts, "owner="+strings.TrimSpace(lock.OwnerJobID))
	}
	if lock.OwnerPID > 0 {
		parts = append(parts, "pid="+strconv.FormatInt(lock.OwnerPID, 10))
	}
	if strings.TrimSpace(lock.OwnerHostname) != "" {
		parts = append(parts, "host="+strings.TrimSpace(lock.OwnerHostname))
	}
	if strings.TrimSpace(lock.UpdatedAt) != "" {
		parts = append(parts, "heartbeat="+strings.TrimSpace(lock.UpdatedAt))
	}
	if strings.TrimSpace(lock.ExpiresAt) != "" {
		parts = append(parts, "expires="+strings.TrimSpace(lock.ExpiresAt))
	}
	if strings.TrimSpace(lock.Elapsed) != "" {
		parts = append(parts, "elapsed="+strings.TrimSpace(lock.Elapsed))
	}
	if strings.TrimSpace(lock.CommandHash) != "" {
		parts = append(parts, "hash="+strings.TrimSpace(lock.CommandHash))
	}
	return strings.Join(parts, " ")
}

func readSkillOptTrainRequest(requestText string, requestFile string) (string, error) {
	requestText = strings.TrimSpace(requestText)
	requestFile = strings.TrimSpace(requestFile)
	if requestText != "" && requestFile != "" {
		return "", errors.New("use only one of --request or --request-file")
	}
	if requestFile == "" {
		return requestText, nil
	}
	content, err := os.ReadFile(requestFile)
	if err != nil {
		return "", fmt.Errorf("read request-file: %w", err)
	}
	return strings.TrimSpace(string(content)), nil
}

type skillOptTrainItemsFile struct {
	Items []skillOptTrainItemPlan `json:"items" yaml:"items"`
}

type skillOptTrainItemPlan struct {
	ItemID         string   `json:"item_id" yaml:"item_id"`
	Title          string   `json:"title" yaml:"title"`
	Brief          string   `json:"brief" yaml:"brief"`
	TargetAudience string   `json:"target_audience" yaml:"target_audience"`
	OutputType     string   `json:"output_type" yaml:"output_type"`
	ArtifactHints  []string `json:"artifact_hints" yaml:"artifact_hints"`
}

func readSkillOptTrainItems(path string) ([]skillOptTrainItemPlan, []string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil, errors.New("skillopt train start requires --items-file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read items-file: %w", err)
	}
	var wrapped skillOptTrainItemsFile
	wrappedErr := yaml.Unmarshal(content, &wrapped)
	items := wrapped.Items
	if wrappedErr != nil && len(items) > 0 {
		return nil, nil, fmt.Errorf("decode items-file: %w", wrappedErr)
	}
	if len(items) == 0 {
		var direct []skillOptTrainItemPlan
		if err := yaml.Unmarshal(content, &direct); err != nil {
			if wrappedErr != nil {
				return nil, nil, fmt.Errorf("decode items-file: %w", wrappedErr)
			}
			return nil, nil, fmt.Errorf("decode items-file: %w", err)
		}
		items = direct
	}
	normalized := make([]skillOptTrainItemPlan, 0, len(items))
	seen := map[string]struct{}{}
	for index, item := range items {
		item.ItemID = strings.TrimSpace(item.ItemID)
		if item.ItemID == "" {
			item.ItemID = fmt.Sprintf("item-%03d", index+1)
		}
		item.Title = strings.TrimSpace(item.Title)
		item.Brief = strings.TrimSpace(item.Brief)
		item.TargetAudience = strings.TrimSpace(item.TargetAudience)
		item.OutputType = strings.TrimSpace(item.OutputType)
		item.ArtifactHints = trimStringSlice(item.ArtifactHints)
		if item.Title == "" {
			return nil, nil, fmt.Errorf("items-file item %s title is required", item.ItemID)
		}
		if item.Brief == "" {
			return nil, nil, fmt.Errorf("items-file item %s brief is required", item.ItemID)
		}
		if item.OutputType == "" {
			return nil, nil, fmt.Errorf("items-file item %s output_type is required", item.ItemID)
		}
		if _, exists := seen[item.ItemID]; exists {
			return nil, nil, fmt.Errorf("items-file item id %q is duplicated", item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		normalized = append(normalized, item)
	}
	return normalized, nil, nil
}

func detectSkillOptTrainItemWarnings(items []skillOptTrainItemPlan) []string {
	warnings := []string{}
	titleCounts := map[string]int{}
	briefCounts := map[string]int{}
	distinctTerms := map[string]struct{}{}
	for _, item := range items {
		titleCounts[strings.ToLower(item.Title)]++
		briefCounts[strings.ToLower(item.Brief)]++
		for _, term := range skillOptTrainItemTerms(item.Title + " " + item.Brief + " " + item.OutputType) {
			distinctTerms[term] = struct{}{}
		}
	}
	for title, count := range titleCounts {
		if title != "" && count > 1 {
			warnings = append(warnings, fmt.Sprintf("duplicate item title %q appears %d times", title, count))
		}
	}
	for brief, count := range briefCounts {
		if brief != "" && count > 1 {
			warnings = append(warnings, fmt.Sprintf("duplicate item brief %q appears %d times", brief, count))
		}
	}
	if len(items) >= 3 && len(distinctTerms) < len(items)*2 {
		warnings = append(warnings, "training items look homogeneous; add more distinct products, audiences, formats, or constraints for stronger feedback")
	}
	return warnings
}

func detectSkillOptTrainPreviewWarnings(policy skillopt.TrainPreviewPolicy) []string {
	if policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages || strings.TrimSpace(policy.Repo) == "" {
		return nil
	}
	return []string{"preview repo must be public or GitHub Pages-enabled before clickable demos can be published"}
}

func skillOptTrainItemTerms(value string) []string {
	parts := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	terms := make([]string, 0, len(parts))
	stop := map[string]struct{}{
		"and": {}, "for": {}, "the": {}, "with": {}, "that": {}, "this": {}, "from": {}, "into": {},
		"page": {}, "build": {}, "create": {}, "make": {}, "write": {}, "design": {}, "output": {},
	}
	for _, part := range parts {
		if len(part) < 4 {
			continue
		}
		if _, skip := stop[part]; skip {
			continue
		}
		terms = append(terms, part)
	}
	return terms
}

func skillOptTrainItemMetadata(item skillOptTrainItemPlan) string {
	metadata := map[string]any{
		"brief":           item.Brief,
		"target_audience": item.TargetAudience,
		"output_type":     item.OutputType,
		"artifact_hints":  item.ArtifactHints,
		"source":          "gitmoot skillopt train start",
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func trimStringSlice(values []string) []string {
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			output = append(output, value)
		}
	}
	return output
}

func parseOptionalSkillOptTrainRepo(name string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	repo, err := daemon.ParseRepository(value)
	if err != nil {
		return "", fmt.Errorf("--%s: %w", name, err)
	}
	return repo.FullName(), nil
}

func normalizeSkillOptTrainTaskKind(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "custom"
	}
	switch value {
	case "correctness", "ux", "design", "writing", "data", "custom":
		return value, nil
	default:
		return "", fmt.Errorf("task kind %q is not supported", value)
	}
}

func normalizeSkillOptTrainMode(mode string, explorationLevel string) (string, string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = db.EvalRunModeExplore
	}
	switch mode {
	case db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate:
	default:
		return "", "", fmt.Errorf("train mode %q is not supported", mode)
	}
	explorationLevel = strings.ToLower(strings.TrimSpace(explorationLevel))
	if explorationLevel == "" {
		switch mode {
		case db.EvalRunModeExplore:
			explorationLevel = db.ExplorationLevelHigh
		case db.EvalRunModeRefine:
			explorationLevel = db.ExplorationLevelMedium
		default:
			explorationLevel = db.ExplorationLevelLow
		}
	}
	switch explorationLevel {
	case db.ExplorationLevelHigh, db.ExplorationLevelMedium, db.ExplorationLevelLow:
		return mode, explorationLevel, nil
	default:
		return "", "", fmt.Errorf("exploration level %q is not supported", explorationLevel)
	}
}

func normalizeSkillOptPreferredGate(value string, taskKind string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		switch taskKind {
		case "correctness", "data":
			value = "hard"
		case "ux", "design", "writing":
			value = "soft"
		default:
			value = "hard_then_soft"
		}
	}
	switch value {
	case "hard", "soft", "hard_then_soft":
		return value, nil
	default:
		return "", fmt.Errorf("preferred gate %q is not supported", value)
	}
}

func effectiveSkillOptOptionsCount(mode string, optionsCount int) int {
	if optionsCount != 0 {
		return optionsCount
	}
	if mode == db.EvalRunModeExplore {
		return 5
	}
	return 2
}

func generatedSkillOptTrainSessionID(templateID string) string {
	base := strings.ToLower(strings.TrimSpace(templateID))
	if base == "" {
		base = "template"
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	now := time.Now().UTC()
	return "train-" + strings.Trim(b.String(), "-_") + "-" + now.Format("20060102-150405") + fmt.Sprintf("-%09d", now.Nanosecond())
}

// skillOptTemplateJudgeEvaluation extracts a template's promoted judge prompt
// fields (written by `skillopt judge promote`, #354) into the shape the
// evaluator reader consumes: judge_prompt_templates as a real object and
// judge_prompt_version as a string. Returns nil when the template carries none,
// so train-start metadata stays byte-identical for templates without a promoted
// judge prompt. This is the consumption half of #354: the promoted prompt is
// folded into the eval-run's evaluation config so a subsequent run resolves it.
func skillOptTemplateJudgeEvaluation(template db.AgentTemplate) map[string]any {
	metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
	if err != nil {
		return nil
	}
	out := map[string]any{}
	if raw := strings.TrimSpace(metadata.Evaluation["judge_prompt_templates"]); raw != "" {
		templates := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &templates); err == nil && len(templates) > 0 {
			out["judge_prompt_templates"] = templates
		}
	}
	if version := strings.TrimSpace(metadata.Evaluation["judge_prompt_version"]); version != "" {
		out["judge_prompt_version"] = version
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func skillOptTrainStartMetadata(request string, mode string, explorationLevel string, optionsCount int, preferredGate string, items []skillOptTrainItemPlan, warnings []string, previewPolicy skillopt.TrainPreviewPolicy, configDefaults skillOptTrainStartConfigDefaults, judgeEvaluation map[string]any) string {
	lines := strings.Count(request, "\n") + 1
	words := len(strings.Fields(request))
	previewMetadata, reviewMetadata := previewPolicy.Metadata()
	evaluation := map[string]any{
		"preferred_gate": preferredGate,
	}
	for key, value := range judgeEvaluation {
		evaluation[key] = value
	}
	metadata := map[string]any{
		"request":           request,
		"request_lines":     lines,
		"request_words":     words,
		"request_chars":     len(request),
		"mode":              mode,
		"exploration_level": explorationLevel,
		"options_count":     optionsCount,
		"items_count":       len(items),
		"item_warnings":     warnings,
		"evaluation":        evaluation,
		"preview":           previewMetadata,
		"review":            reviewMetadata,
		"source":            "gitmoot skillopt train start",
	}
	if optimizerDefaults := skillOptTrainOptimizerDefaultsMetadata(configDefaults.Optimizer); len(optimizerDefaults) > 0 {
		metadata["optimizer_defaults"] = optimizerDefaults
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func skillOptTrainOptimizerDefaultsMetadata(request skillOptTrainOptimizerRequest) map[string]any {
	metadata := map[string]any{}
	if value := strings.TrimSpace(request.Backend); value != "" {
		metadata["backend"] = value
	}
	if value := strings.TrimSpace(request.OptimizerBackend); value != "" {
		metadata["optimizer_backend"] = value
	}
	if value := strings.TrimSpace(request.TargetBackend); value != "" {
		metadata["target_backend"] = value
	}
	if value := strings.TrimSpace(request.OptimizerModel); value != "" {
		metadata["optimizer_model"] = value
	}
	if value := strings.TrimSpace(request.TargetModel); value != "" {
		metadata["target_model"] = value
	}
	if value := strings.TrimSpace(request.EvaluatorID); value != "" {
		metadata["evaluator_id"] = value
	}
	if value := strings.TrimSpace(request.EvaluatorBackend); value != "" {
		metadata["evaluator_backend"] = value
	}
	if value := strings.TrimSpace(request.SkillUpdateMode); value != "" {
		metadata["skill_update_mode"] = value
	}
	if request.OptimizerViewsSet {
		metadata["optimizer_views"] = request.OptimizerViews
	}
	if request.RetryOptimizerViewsSet {
		metadata["retry_optimizer_views"] = strings.TrimSpace(request.RetryOptimizerViews)
	}
	if request.NoopRetryBudgetSet {
		metadata["noop_retry_budget"] = request.NoopRetryBudget
	}
	if request.GateRejectRetryBudgetSet {
		metadata["gate_reject_retry_budget"] = request.GateRejectRetryBudget
	}
	if request.WrongArtifactRetryBudgetSet {
		metadata["wrong_artifact_retry_budget"] = request.WrongArtifactRetryBudget
	}
	if request.TargetArtifactRetryBudgetSet {
		metadata["target_artifact_retry_budget"] = request.TargetArtifactRetryBudget
	}
	if request.HardFailureRetryBudgetSet {
		metadata["hard_failure_retry_budget"] = request.HardFailureRetryBudget
	}
	if request.FinalEval {
		metadata["final_eval"] = true
	}
	return metadata
}

func applySkillOptTrainOptimizerDefaultsFromMetadata(metadataJSON string, request *skillOptTrainOptimizerRequest) {
	if request == nil {
		return
	}
	metadata := decodedSkillOptMetadataValue(decodedSkillOptMetadata(metadataJSON)["optimizer_defaults"])
	if len(metadata) == 0 {
		return
	}
	// Captured before backend inheritance below: stored model names are
	// backend-specific, so they are only inherited when the backends are too.
	backendOverridden := request.Backend != "" || request.OptimizerBackend != "" ||
		request.TargetBackend != "" || request.EvaluatorBackend != ""
	if request.Backend == "" && request.OptimizerBackend == "" && request.TargetBackend == "" && request.EvaluatorBackend == "" {
		request.Backend = metadataString(metadata, "backend")
	}
	if request.Backend == "" {
		if request.OptimizerBackend == "" {
			request.OptimizerBackend = metadataString(metadata, "optimizer_backend")
		}
		if request.TargetBackend == "" {
			request.TargetBackend = metadataString(metadata, "target_backend")
		}
		if request.EvaluatorBackend == "" {
			request.EvaluatorBackend = metadataString(metadata, "evaluator_backend")
		}
	}
	if !backendOverridden && request.Model == "" {
		if request.OptimizerModel == "" {
			request.OptimizerModel = metadataString(metadata, "optimizer_model")
		}
		if request.TargetModel == "" {
			request.TargetModel = metadataString(metadata, "target_model")
		}
	}
	if request.EvaluatorID == "" {
		request.EvaluatorID = metadataString(metadata, "evaluator_id")
	}
	if request.SkillUpdateMode == "" {
		request.SkillUpdateMode = metadataString(metadata, "skill_update_mode")
	}
	if !request.OptimizerViewsSet {
		if value, ok := metadataInt(metadata, "optimizer_views"); ok {
			request.OptimizerViews = value
			request.OptimizerViewsSet = true
		}
	}
	if !request.RetryOptimizerViewsSet {
		if value := metadataString(metadata, "retry_optimizer_views"); value != "" {
			request.RetryOptimizerViews = value
			request.RetryOptimizerViewsSet = true
		}
	}
	if !request.NoopRetryBudgetSet {
		if value, ok := metadataInt(metadata, "noop_retry_budget"); ok {
			request.NoopRetryBudget = value
			request.NoopRetryBudgetSet = true
		}
	}
	if !request.GateRejectRetryBudgetSet {
		if value, ok := metadataInt(metadata, "gate_reject_retry_budget"); ok {
			request.GateRejectRetryBudget = value
			request.GateRejectRetryBudgetSet = true
		}
	}
	if !request.WrongArtifactRetryBudgetSet {
		if value, ok := metadataInt(metadata, "wrong_artifact_retry_budget"); ok {
			request.WrongArtifactRetryBudget = value
			request.WrongArtifactRetryBudgetSet = true
		}
	}
	if !request.TargetArtifactRetryBudgetSet {
		if value, ok := metadataInt(metadata, "target_artifact_retry_budget"); ok {
			request.TargetArtifactRetryBudget = value
			request.TargetArtifactRetryBudgetSet = true
		}
	}
	if !request.HardFailureRetryBudgetSet {
		if value, ok := metadataInt(metadata, "hard_failure_retry_budget"); ok {
			request.HardFailureRetryBudget = value
			request.HardFailureRetryBudgetSet = true
		}
	}
	if !request.FinalEvalSet {
		request.FinalEval = metadataBool(metadata, "final_eval")
	}
}

func validateSkillOptTrainOptimizerRequestAfterDefaults(request *skillOptTrainOptimizerRequest) error {
	if request == nil {
		return nil
	}
	if request.OptimizerViewsSet && request.OptimizerViews <= 0 {
		return errors.New("--optimizer-views must be greater than zero")
	}
	if request.RetryOptimizerViewsSet {
		normalized, err := normalizeSkillOptRetryOptimizerViews(request.RetryOptimizerViews)
		if err != nil {
			return err
		}
		request.RetryOptimizerViews = normalized
		if request.OptimizerViewsSet {
			if retryViews, ok := parseSkillOptRetryOptimizerViewsNumber(normalized); ok && retryViews > request.OptimizerViews {
				return errors.New("--retry-optimizer-views cannot exceed --optimizer-views")
			}
		}
	}
	return nil
}

func metadataInt(metadata map[string]any, key string) (int, bool) {
	value := metadataFloatPtr(metadata, key)
	if value == nil {
		return 0, false
	}
	return int(*value), true
}

func skillOptMetadataString(metadataJSON string, path ...string) string {
	var current any
	if err := json.Unmarshal([]byte(metadataJSON), &current); err != nil {
		return ""
	}
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	if value, ok := current.(string); ok {
		return value
	}
	return ""
}

func skillOptTrainDecisionMetadata(existing string, reason string) string {
	var metadata map[string]any
	if strings.TrimSpace(existing) != "" {
		_ = json.Unmarshal([]byte(existing), &metadata)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["decision_reason"] = strings.TrimSpace(reason)
	metadata["decision"] = skillopt.TrainStateRunAbandoned
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return existing
	}
	return string(encoded)
}

func recordSkillOptTrainGenerationFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest, failure error) error {
	metadata := map[string]any{
		"status":       "failed",
		"agent":        strings.TrimSpace(request.GeneratorAgent),
		"agent_type":   strings.TrimSpace(request.GeneratorType),
		"error":        failure.Error(),
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "generation", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "generation", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func resolveSkillOptTrainBackendRequest(request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerRequest, skillOptTrainBackendResolution, error) {
	preset := strings.TrimSpace(strings.ToLower(request.Backend))
	switch preset {
	case "":
		optimizerBackend := strings.TrimSpace(request.OptimizerBackend)
		if optimizerBackend == "" {
			optimizerBackend = "openai_chat"
		}
		internalTargetAdapter := strings.TrimSpace(request.TargetBackend)
		if internalTargetAdapter == "" {
			internalTargetAdapter = "openai_chat"
		}
		evaluatorBackend := strings.TrimSpace(request.EvaluatorBackend)
		if evaluatorBackend == "" {
			evaluatorBackend = optimizerBackend
		}
		resolution := skillOptTrainBackendResolution{
			Backend:               "custom",
			OptimizerBackend:      optimizerBackend,
			TargetBackend:         skillOptTrainDisplayTargetBackend(internalTargetAdapter),
			InternalTargetAdapter: internalTargetAdapter,
			EvaluatorBackend:      evaluatorBackend,
		}
		resolution.ConfigStatus = skillOptTrainBackendConfigStatus(resolution)
		return request, resolution, nil
	case "codex":
		if err := skillOptTrainBackendPresetConflict("--optimizer-backend", request.OptimizerBackend, "codex"); err != nil {
			return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, err
		}
		if err := skillOptTrainCodexTargetBackendConflict(request.TargetBackend); err != nil {
			return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, err
		}
		if err := skillOptTrainBackendPresetConflict("--evaluator-backend", request.EvaluatorBackend, "codex"); err != nil {
			return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, err
		}
		request.OptimizerBackend = "codex"
		request.TargetBackend = "codex_exec"
		request.EvaluatorBackend = "codex"
		resolution := skillOptTrainBackendResolution{
			Backend:               "codex",
			OptimizerBackend:      "codex",
			TargetBackend:         "codex",
			InternalTargetAdapter: "codex_exec",
			EvaluatorBackend:      "codex",
			ConfigStatus:          "codex_no_azure_or_openai_required",
		}
		return request, resolution, nil
	default:
		return skillOptTrainOptimizerRequest{}, skillOptTrainBackendResolution{}, fmt.Errorf("backend preset %q is not supported; use codex or explicit backend flags", preset)
	}
}

func skillOptTrainBackendPresetConflict(flagName string, value string, expected string) error {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == expected {
		return nil
	}
	return fmt.Errorf("--backend codex conflicts with %s %q; omit %s or set it to %s", flagName, value, flagName, expected)
}

func skillOptTrainCodexTargetBackendConflict(value string) error {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "codex" || value == "codex_exec" {
		return nil
	}
	return fmt.Errorf("--backend codex conflicts with --target-backend %q; omit --target-backend or use codex/codex_exec", value)
}

func skillOptTrainDisplayTargetBackend(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "codex_exec") {
		return "codex"
	}
	return value
}

func skillOptTrainBackendConfigStatus(resolution skillOptTrainBackendResolution) string {
	backends := []string{
		resolution.OptimizerBackend,
		resolution.InternalTargetAdapter,
		resolution.EvaluatorBackend,
	}
	anyBackend := false
	externalCredentials := false
	for _, backend := range backends {
		backend = strings.TrimSpace(strings.ToLower(backend))
		if backend == "" {
			continue
		}
		anyBackend = true
		if backend == "openai_chat" || backend == "azure_openai" || strings.Contains(backend, "openai") || strings.Contains(backend, "azure") {
			externalCredentials = true
		}
	}
	if !anyBackend {
		return "using_optimizer_defaults"
	}
	if externalCredentials {
		return "external_credentials_may_be_required"
	}
	return "no_azure_or_openai_required"
}

func resolveSkillOptTrainOptimizerPaths(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (skillOptTrainOptimizerPaths, error) {
	outRoot := strings.TrimSpace(request.OutRoot)
	optimizerMetadata := decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["optimizer"])
	persistedOptimizerRoot := metadataString(optimizerMetadata, "optimizer_root")
	persistedAttempt := metadataString(optimizerMetadata, "optimizer_attempt")
	persistedAttemptPath := metadataString(optimizerMetadata, "optimizer_attempt_path")
	persistedOutRoot := metadataString(optimizerMetadata, "out_root")
	persistedTrainingPackage := metadataString(optimizerMetadata, "training_package")
	persistedCandidateOutput := metadataString(optimizerMetadata, "candidate_output")
	persistedArtifactDir := metadataString(optimizerMetadata, "artifact_dir")
	persistedBaseRoot := persistedOptimizerRoot
	if persistedBaseRoot == "" {
		persistedBaseRoot = inferSkillOptTrainOptimizerRoot(persistedOutRoot, persistedAttempt)
	}
	if persistedTrainingPackage != "" && skillopt.NormalizeTrainState(iteration.State) != skillopt.TrainStateFeedbackSynced {
		if outRoot != "" {
			absRequestedOutRoot, err := filepath.Abs(outRoot)
			if err != nil {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer out-root: %w", err)
			}
			absPersistedRoot := persistedBaseRoot
			if absPersistedRoot == "" {
				absPersistedRoot = persistedOutRoot
			}
			if absPersistedRoot == "" {
				absPersistedRoot = filepath.Dir(persistedTrainingPackage)
			}
			if absPersistedRoot, err = filepath.Abs(absPersistedRoot); err != nil {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve persisted optimizer out-root: %w", err)
			}
			absPersistedOutRoot := ""
			if persistedOutRoot != "" {
				if absPersistedOutRoot, err = filepath.Abs(persistedOutRoot); err != nil {
					return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve persisted optimizer attempt out-root: %w", err)
				}
			}
			if absRequestedOutRoot != absPersistedRoot && absRequestedOutRoot != absPersistedOutRoot {
				return skillOptTrainOptimizerPaths{}, fmt.Errorf("optimizer package already exported at %s; retry with the same --out-root or omit --out-root", persistedTrainingPackage)
			}
		}
		outRoot = persistedBaseRoot
		if outRoot == "" {
			outRoot = persistedOutRoot
		}
		if outRoot == "" {
			outRoot = filepath.Dir(filepath.Dir(filepath.Dir(persistedTrainingPackage)))
		}
	}
	if outRoot == "" {
		outRoot = persistedBaseRoot
	}
	if outRoot == "" {
		outRoot = inferSkillOptTrainOptimizerRoot(persistedOutRoot, persistedAttempt)
	}
	if outRoot == "" {
		outRoot = filepath.Join(paths.Evals, "train", session.ID, iteration.ID, "optimizer")
	}
	absOptimizerRoot, err := filepath.Abs(outRoot)
	if err != nil {
		return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer out-root: %w", err)
	}
	state := skillopt.NormalizeTrainState(iteration.State)
	attempt := persistedAttempt
	attemptPath := persistedAttemptPath
	if state == skillopt.TrainStateFeedbackSynced || persistedTrainingPackage == "" {
		attempt = "attempt-001"
		attemptPath = filepath.Join(absOptimizerRoot, "attempts", attempt)
	} else if request.RerunOptimizer {
		nextAttempt, err := nextSkillOptTrainOptimizerAttempt(absOptimizerRoot, attempt)
		if err != nil {
			return skillOptTrainOptimizerPaths{}, err
		}
		attempt = nextAttempt
		attemptPath = filepath.Join(absOptimizerRoot, "attempts", attempt)
	} else if attemptPath == "" {
		attemptPath = persistedOutRoot
	}
	if attemptPath == "" {
		attemptPath = filepath.Join(absOptimizerRoot, "attempts", firstNonEmpty(attempt, "attempt-001"))
	}
	absAttemptPath, err := filepath.Abs(attemptPath)
	if err != nil {
		return skillOptTrainOptimizerPaths{}, fmt.Errorf("resolve optimizer attempt path: %w", err)
	}
	if attempt == "" {
		attempt = filepath.Base(absAttemptPath)
	}
	trainingPackagePath := filepath.Join(absAttemptPath, "training.json")
	candidatePackagePath := filepath.Join(absAttemptPath, "candidate.json")
	artifactDir := filepath.Join(absAttemptPath, "artifacts")
	if persistedTrainingPackage != "" && state != skillopt.TrainStateFeedbackSynced {
		if !request.RerunOptimizer {
			trainingPackagePath = persistedTrainingPackage
			if persistedCandidateOutput != "" {
				candidatePackagePath = persistedCandidateOutput
			}
			if persistedArtifactDir != "" {
				artifactDir = persistedArtifactDir
			}
		}
	}
	return skillOptTrainOptimizerPaths{
		OutRoot:              absAttemptPath,
		OptimizerRoot:        absOptimizerRoot,
		OptimizerAttempt:     attempt,
		OptimizerAttemptPath: absAttemptPath,
		ArtifactRoot:         paths.ArtifactBlobs,
		TrainingPackagePath:  trainingPackagePath,
		CandidatePackagePath: candidatePackagePath,
		ArtifactDir:          artifactDir,
	}, nil
}

func inferSkillOptTrainOptimizerRoot(outRoot string, attempt string) string {
	outRoot = strings.TrimSpace(outRoot)
	attempt = strings.TrimSpace(attempt)
	if outRoot == "" || attempt == "" {
		return outRoot
	}
	clean := filepath.Clean(outRoot)
	if filepath.Base(clean) != attempt {
		return outRoot
	}
	attemptsDir := filepath.Dir(clean)
	if filepath.Base(attemptsDir) != "attempts" {
		return outRoot
	}
	return filepath.Dir(attemptsDir)
}

func nextSkillOptTrainOptimizerAttempt(root string, currentAttempt string) (string, error) {
	maxAttempt := skillOptTrainOptimizerAttemptNumber(currentAttempt)
	entries, err := os.ReadDir(filepath.Join(root, "attempts"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read optimizer attempts: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if number := skillOptTrainOptimizerAttemptNumber(entry.Name()); number > maxAttempt {
			maxAttempt = number
		}
	}
	return fmt.Sprintf("attempt-%03d", maxAttempt+1), nil
}

func skillOptTrainOptimizerAttemptNumber(attempt string) int {
	attempt = strings.TrimSpace(attempt)
	if !strings.HasPrefix(attempt, "attempt-") {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimPrefix(attempt, "attempt-"))
	if err != nil || number < 0 {
		return 0
	}
	return number
}

func exportSkillOptTrainPackage(ctx context.Context, store *db.Store, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, request skillOptTrainOptimizerRequest) (map[string]any, error) {
	pkg, err := skillopt.ExportTrainingPackage(ctx, store, iteration.EvalRunID)
	if err != nil {
		return nil, fmt.Errorf("export training package: %w", err)
	}
	if profile := skillopt.BuildEvaluatorProfile(request.EvaluatorID, request.EvaluatorModel, pkg.EvaluatorConfig); profile != nil {
		pkg.EvaluatorProfile = profile
	}
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode training package: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeSkillOptFile(paths.TrainingPackagePath, encoded); err != nil {
		return nil, fmt.Errorf("write training package: %w", err)
	}
	return map[string]any{
		"status":                 "package_created",
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"artifact_root":          paths.ArtifactRoot,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"created_at":             time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}, nil
}

func skillOptTrainOptimizerRecoveryAvailable(paths skillOptTrainOptimizerPaths) bool {
	for _, path := range []string{
		paths.CandidatePackagePath,
		filepath.Join(paths.OutRoot, "summary.json"),
		filepath.Join(paths.OutRoot, "runtime_state.json"),
		filepath.Join(paths.OutRoot, "history.json"),
		filepath.Join(paths.OutRoot, "best_skill.md"),
	} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func recoverSkillOptTrainOptimizerArtifacts(ctx context.Context, paths config.Paths, store *db.Store, sessionID string, outRoot string) (skillOptTrainRecoverResult, error) {
	session, iteration, _, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainRecoverResult{}, err
	}
	if iteration == nil {
		return skillOptTrainRecoverResult{SessionID: strings.TrimSpace(session.ID), Classification: "unrecoverable"}, errors.New("train session has no iteration to recover")
	}
	optimizerPaths, err := resolveSkillOptTrainOptimizerPaths(paths, session, *iteration, skillOptTrainOptimizerRequest{OutRoot: outRoot})
	if err != nil {
		return skillOptTrainRecoverResult{}, err
	}
	result := skillOptTrainRecoverResult{
		SessionID:            strings.TrimSpace(session.ID),
		IterationID:          strings.TrimSpace(iteration.ID),
		CurrentPhase:         skillopt.NormalizeTrainState(iteration.State),
		RecoveryAvailable:    skillOptTrainOptimizerRecoveryAvailable(optimizerPaths),
		OutRoot:              optimizerPaths.OutRoot,
		OptimizerRoot:        optimizerPaths.OptimizerRoot,
		OptimizerAttempt:     optimizerPaths.OptimizerAttempt,
		OptimizerAttemptPath: optimizerPaths.OptimizerAttemptPath,
		CandidatePackagePath: optimizerPaths.CandidatePackagePath,
		ArtifactDir:          optimizerPaths.ArtifactDir,
		Artifacts:            existingSkillOptTrainOptimizerArtifacts(optimizerPaths),
	}
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		result.Classification = "already_completed_candidate"
		result.CandidateVersionID = strings.TrimSpace(iteration.CandidateVersionID)
		result.NextAction = "candidate already exists; continue with candidate review or decision"
		return result, nil
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		result.Classification = "already_completed_no_candidate"
		result.NoCandidateReason = skillOptMetadataString(iteration.MetadataJSON, "candidate_import", "no_candidate_reason")
		result.NextAction = skillOptNoCandidateNextAction()
		return result, nil
	}
	releaseOptimizerLock, _, err := acquireSkillOptTrainOptimizerLock(ctx, store, session.ID, iteration.ID, skillOptTrainOptimizerLockTTL, skillOptTrainOptimizerRequest{OutRoot: optimizerPaths.OutRoot})
	if err != nil {
		result.Classification = "optimizer_active"
		result.NextAction = "wait for the active optimizer run to finish before recovering artifacts"
		return result, err
	}
	defer func() {
		_ = releaseOptimizerLock(context.Background())
	}()
	session, iteration, _, err = loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	if iteration == nil {
		result.CurrentPhase = ""
		result.Classification = "unrecoverable"
		return result, errors.New("train session has no iteration to recover")
	}
	optimizerPaths, err = resolveSkillOptTrainOptimizerPaths(paths, session, *iteration, skillOptTrainOptimizerRequest{OutRoot: outRoot})
	if err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	result.CurrentPhase = skillopt.NormalizeTrainState(iteration.State)
	result.RecoveryAvailable = skillOptTrainOptimizerRecoveryAvailable(optimizerPaths)
	result.OutRoot = optimizerPaths.OutRoot
	result.OptimizerRoot = optimizerPaths.OptimizerRoot
	result.OptimizerAttempt = optimizerPaths.OptimizerAttempt
	result.OptimizerAttemptPath = optimizerPaths.OptimizerAttemptPath
	result.CandidatePackagePath = optimizerPaths.CandidatePackagePath
	result.ArtifactDir = optimizerPaths.ArtifactDir
	result.Artifacts = existingSkillOptTrainOptimizerArtifacts(optimizerPaths)
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished, skillopt.TrainStateCandidatePromoted, skillopt.TrainStateCandidateRejected:
		result.Classification = "already_completed_candidate"
		result.CandidateVersionID = strings.TrimSpace(iteration.CandidateVersionID)
		result.NextAction = "candidate already exists; continue with candidate review or decision"
		return result, nil
	case skillopt.TrainStateOptimizerCompletedNoCandidate:
		result.Classification = "already_completed_no_candidate"
		result.NoCandidateReason = skillOptMetadataString(iteration.MetadataJSON, "candidate_import", "no_candidate_reason")
		result.NextAction = skillOptNoCandidateNextAction()
		return result, nil
	}
	candidateContent, err := os.ReadFile(optimizerPaths.CandidatePackagePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			result.Classification = "corrupted_unrecoverable"
			return result, fmt.Errorf("read optimizer candidate package: %w", err)
		}
		if reason := noCandidateReasonFromSkillOptTrainOptimizerArtifacts(optimizerPaths); reason != "" {
			session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
			if err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			if err := recordSkillOptTrainNoCandidate(ctx, store, session, *iteration, optimizerPaths, reason); err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			result.Classification = "completed_no_candidate"
			result.CurrentPhase = skillopt.TrainStateOptimizerCompletedNoCandidate
			result.NoCandidateReason = reason
			result.NextAction = skillOptNoCandidateNextAction()
			return result, nil
		}
		if result.RecoveryAvailable {
			result.Classification = "incomplete_resumable"
			result.NextAction = "candidate package is missing; rerun the optimizer with --rerun-optimizer or inspect the artifact directory"
			return result, errors.New("optimizer artifacts are present but candidate.json is missing")
		}
		result.Classification = "unavailable"
		result.NextAction = "run the optimizer before attempting recovery"
		return result, errors.New("no recoverable optimizer artifacts found")
	}
	var candidate skillopt.CandidatePackage
	if err := json.Unmarshal(candidateContent, &candidate); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, fmt.Errorf("decode optimizer candidate package: %w", err)
	}
	if err := validateSkillOptTrainCandidatePackage(ctx, store, session, *iteration, candidate); err != nil {
		if errors.Is(err, skillopt.ErrNoCandidate) {
			reason, nextAction := skillOptNoCandidateReasonAndNextAction(err, optimizerPaths.CandidatePackagePath)
			session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
			if err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			if err := recordSkillOptTrainNoCandidate(ctx, store, session, *iteration, optimizerPaths, reason); err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			result.Classification = "completed_no_candidate"
			result.CurrentPhase = skillopt.TrainStateOptimizerCompletedNoCandidate
			result.NoCandidateReason = reason
			result.NextAction = nextAction
			return result, nil
		}
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	if err := validateSkillOptTrainOptimizerRecoverableCompleteState(*iteration); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	version, err := importSkillOptTrainCandidate(ctx, paths, store, session, *iteration, optimizerPaths)
	if err != nil {
		if errors.Is(err, skillopt.ErrNoCandidate) {
			reason, nextAction := skillOptNoCandidateReasonAndNextAction(err, optimizerPaths.CandidatePackagePath)
			session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
			if err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			if err := recordSkillOptTrainNoCandidate(ctx, store, session, *iteration, optimizerPaths, reason); err != nil {
				result.Classification = "corrupted_unrecoverable"
				return result, err
			}
			result.Classification = "completed_no_candidate"
			result.CurrentPhase = skillopt.TrainStateOptimizerCompletedNoCandidate
			result.NoCandidateReason = reason
			result.NextAction = nextAction
			return result, nil
		}
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	session, iteration, err = markSkillOptTrainOptimizerRecoveredComplete(ctx, store, session, *iteration, optimizerPaths)
	if err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	metadata := map[string]any{
		"status":                 "recovered",
		"candidate_version":      version.ID,
		"candidate_package":      optimizerPaths.CandidatePackagePath,
		"artifact_dir":           optimizerPaths.ArtifactDir,
		"optimizer_root":         optimizerPaths.OptimizerRoot,
		"optimizer_attempt":      optimizerPaths.OptimizerAttempt,
		"optimizer_attempt_path": optimizerPaths.OptimizerAttemptPath,
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train recover",
	}
	session.State = skillopt.TrainStateCandidateCreated
	iteration.State = skillopt.TrainStateCandidateCreated
	iteration.CandidateVersionID = version.ID
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, *iteration); err != nil {
		result.Classification = "corrupted_unrecoverable"
		return result, err
	}
	result.Classification = "completed_candidate"
	result.CurrentPhase = skillopt.TrainStateCandidateCreated
	result.CandidateVersionID = version.ID
	result.NextAction = "publish candidate diff and preview review with train continue"
	return result, nil
}

func validateSkillOptTrainOptimizerRecoverableCompleteState(iteration db.SkillOptTrainIteration) error {
	state := skillopt.NormalizeTrainState(iteration.State)
	switch state {
	case skillopt.TrainStateOptimizerCompleted, skillopt.TrainStateTrainingPackageCreated:
		return nil
	default:
		return fmt.Errorf("cannot recover optimizer artifacts while iteration is in state %s", state)
	}
}

func markSkillOptTrainOptimizerRecoveredComplete(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths) (db.SkillOptTrainSession, *db.SkillOptTrainIteration, error) {
	if err := validateSkillOptTrainOptimizerRecoverableCompleteState(iteration); err != nil {
		return db.SkillOptTrainSession{}, nil, err
	}
	if skillopt.NormalizeTrainState(iteration.State) == skillopt.TrainStateOptimizerCompleted {
		return session, &iteration, nil
	}
	metadata := map[string]any{
		"status":                 "recovered",
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"artifact_root":          paths.ArtifactRoot,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train recover",
	}
	session.State = skillopt.TrainStateOptimizerCompleted
	iteration.State = skillopt.TrainStateOptimizerCompleted
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return db.SkillOptTrainSession{}, nil, err
	}
	if err := store.UpsertSkillOptTrainIteration(ctx, iteration); err != nil {
		return db.SkillOptTrainSession{}, nil, err
	}
	return session, &iteration, nil
}

func existingSkillOptTrainOptimizerArtifacts(paths skillOptTrainOptimizerPaths) []string {
	candidates := []string{
		paths.CandidatePackagePath,
		filepath.Join(paths.OutRoot, "summary.json"),
		filepath.Join(paths.OutRoot, "runtime_state.json"),
		filepath.Join(paths.OutRoot, "history.json"),
		filepath.Join(paths.OutRoot, "best_skill.md"),
	}
	existing := []string{}
	for _, path := range candidates {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	return existing
}

func noCandidateReasonFromSkillOptTrainOptimizerArtifacts(paths skillOptTrainOptimizerPaths) string {
	for _, path := range []string{
		filepath.Join(paths.OutRoot, "summary.json"),
		filepath.Join(paths.OutRoot, "runtime_state.json"),
		filepath.Join(paths.OutRoot, "history.json"),
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data any
		if err := json.Unmarshal(content, &data); err != nil {
			continue
		}
		if reason := noCandidateReasonFromValue(data); reason != "" {
			return reason
		}
	}
	return ""
}

func noCandidateReasonFromValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if raw, ok := typed["no_candidate_reason"]; ok {
			if text, ok := raw.(string); ok {
				reason := strings.TrimSpace(text)
				if reason != "" {
					return reason
				}
			}
		}
		for _, nested := range typed {
			if reason := noCandidateReasonFromValue(nested); reason != "" {
				return reason
			}
		}
	case []any:
		for _, nested := range typed {
			if reason := noCandidateReasonFromValue(nested); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func printSkillOptTrainRecoverResult(stdout io.Writer, result skillOptTrainRecoverResult) {
	writeLine(stdout, "session: %s", result.SessionID)
	writeLine(stdout, "iteration: %s", emptyText(result.IterationID))
	writeLine(stdout, "recovery_state: %s", emptyText(result.Classification))
	writeLine(stdout, "current_phase: %s", emptyText(result.CurrentPhase))
	writeLine(stdout, "recovery_available: %t", result.RecoveryAvailable)
	writeLine(stdout, "optimizer_out_root: %s", emptyText(result.OutRoot))
	writeLine(stdout, "optimizer_root: %s", emptyText(result.OptimizerRoot))
	writeLine(stdout, "optimizer_attempt: %s", emptyText(result.OptimizerAttempt))
	writeLine(stdout, "optimizer_attempt_path: %s", emptyText(result.OptimizerAttemptPath))
	writeLine(stdout, "candidate_package: %s", emptyText(result.CandidatePackagePath))
	writeLine(stdout, "artifact_dir: %s", emptyText(result.ArtifactDir))
	writeLine(stdout, "candidate: %s", emptyText(result.CandidateVersionID))
	if result.NoCandidateReason != "" {
		writeLine(stdout, "no_candidate_reason: %s", result.NoCandidateReason)
	}
	if len(result.Artifacts) > 0 {
		writeLine(stdout, "artifacts: %s", strings.Join(result.Artifacts, ","))
	}
	writeLine(stdout, "next: %s", emptyText(result.NextAction))
}

// isCodexFamilyBackend reports whether a gitmoot-skillopt backend runs through
// the codex CLI and would otherwise default to gpt-4o: the "codex" chat backend
// (optimizer/evaluator) and the "codex_exec" target backend.
func isCodexFamilyBackend(backend string) bool {
	switch strings.TrimSpace(backend) {
	case runtime.CodexRuntime, "codex_exec":
		return true
	default:
		return false
	}
}

func skillOptTrainOptimizerExecutable(request skillOptTrainOptimizerRequest) string {
	executable := strings.TrimSpace(request.SkillOptBin)
	if executable == "" {
		executable = "gitmoot-skillopt"
	}
	return executable
}

func resolveSkillOptTrainOptimizerExecutable(request skillOptTrainOptimizerRequest) (string, error) {
	executable := skillOptTrainOptimizerExecutable(request)
	resolved, err := skillOptTrainOptimizerRunner.LookPath(executable)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", fmt.Errorf("resolve gitmoot-skillopt executable %q: %w", resolved, err)
		}
	}
	return resolved, nil
}

func preflightSkillOptTrainOptimizerExecutable(ctx context.Context, request skillOptTrainOptimizerRequest) (string, subprocess.Result, error) {
	requested := skillOptTrainOptimizerExecutable(request)
	resolved, err := resolveSkillOptTrainOptimizerExecutable(request)
	if err != nil {
		return requested, subprocess.Result{}, skillOptTrainOptimizerPreflightError{
			Executable: requested,
			Step:       "find executable",
			Err:        err,
		}
	}
	result, err := skillOptTrainOptimizerRunner.Run(ctx, "", resolved, "--version")
	if err != nil {
		return resolved, result, skillOptTrainOptimizerPreflightError{
			Executable: resolved,
			Step:       "version check",
			Result:     result,
			Err:        err,
		}
	}
	result, err = skillOptTrainOptimizerRunner.Run(ctx, "", resolved, "optimize", "--help")
	if err != nil {
		return resolved, result, skillOptTrainOptimizerPreflightError{
			Executable: resolved,
			Step:       "optimize help check",
			Result:     result,
			Err:        err,
		}
	}
	return resolved, result, nil
}

func buildSkillOptTrainOptimizerCommand(iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths) (string, []string, error) {
	resolvedRequest, _, err := resolveSkillOptTrainBackendRequest(request)
	if err != nil {
		return "", nil, err
	}
	request = resolvedRequest
	resolved, err := resolveSkillOptTrainOptimizerExecutable(request)
	if err != nil {
		return "", nil, fmt.Errorf("find gitmoot-skillopt executable %q: %w", skillOptTrainOptimizerExecutable(request), err)
	}
	gate, err := skillOptTrainOptimizerGate(iteration, request)
	if err != nil {
		return "", nil, err
	}
	args := []string{
		"optimize",
		"--training-package", paths.TrainingPackagePath,
		"--artifact-root", paths.ArtifactRoot,
		"--out-root", paths.OutRoot,
		"--candidate-output", paths.CandidatePackagePath,
		"--artifact-dir", paths.ArtifactDir,
		"--gate-metric", gate,
	}
	optimizerModel := strings.TrimSpace(request.OptimizerModel)
	targetModel := strings.TrimSpace(request.TargetModel)
	if model := strings.TrimSpace(request.Model); model != "" {
		if optimizerModel == "" {
			optimizerModel = model
		}
		if targetModel == "" {
			targetModel = model
		}
	}
	// When a role runs on a codex-family backend with no model set, default to the
	// model the codex CLI is configured with. gitmoot-skillopt would otherwise
	// fall back to gpt-4o (its OPTIMIZER/TARGET_DEPLOYMENT default), which a
	// ChatGPT-account codex login rejects. The codex preset resolves the optimizer
	// to "codex" and the target to "codex_exec" — both pass an explicit model to
	// codex, so both need the override. The evaluator is left alone: it inherits
	// the optimizer model in gitmoot-skillopt when --evaluator-model is omitted.
	optimizerBackend := firstNonEmpty(strings.TrimSpace(request.OptimizerBackend), strings.TrimSpace(request.Backend))
	targetBackend := firstNonEmpty(strings.TrimSpace(request.TargetBackend), strings.TrimSpace(request.Backend))
	wantOptimizerCodexModel := optimizerModel == "" && isCodexFamilyBackend(optimizerBackend)
	wantTargetCodexModel := targetModel == "" && isCodexFamilyBackend(targetBackend)
	if wantOptimizerCodexModel || wantTargetCodexModel {
		if codexModel, _ := runtime.ConfiguredCodexModel(); codexModel != "" {
			if wantOptimizerCodexModel {
				optimizerModel = codexModel
			}
			if wantTargetCodexModel {
				targetModel = codexModel
			}
		}
	}
	if optimizerModel != "" {
		args = append(args, "--optimizer-model", optimizerModel)
	}
	if targetModel != "" {
		args = append(args, "--target-model", targetModel)
	}
	if optimizerBackend := strings.TrimSpace(request.OptimizerBackend); optimizerBackend != "" {
		args = append(args, "--optimizer-backend", optimizerBackend)
	}
	if targetBackend := strings.TrimSpace(request.TargetBackend); targetBackend != "" {
		args = append(args, "--target-backend", targetBackend)
	}
	if evaluatorID := strings.TrimSpace(request.EvaluatorID); evaluatorID != "" {
		args = append(args, "--evaluator-id", evaluatorID)
	}
	if evaluatorModel := strings.TrimSpace(request.EvaluatorModel); evaluatorModel != "" {
		args = append(args, "--evaluator-model", evaluatorModel)
	}
	if evaluatorBackend := strings.TrimSpace(request.EvaluatorBackend); evaluatorBackend != "" {
		args = append(args, "--evaluator-backend", evaluatorBackend)
	}
	if skillUpdateMode := strings.TrimSpace(request.SkillUpdateMode); skillUpdateMode != "" {
		args = append(args, "--skill-update-mode", skillUpdateMode)
	}
	if request.NumEpochs > 0 {
		args = append(args, "--num-epochs", strconv.Itoa(request.NumEpochs))
	}
	if request.BatchSize > 0 {
		args = append(args, "--batch-size", strconv.Itoa(request.BatchSize))
	}
	if request.OptimizerViewsSet {
		args = append(args, "--optimizer-views", strconv.Itoa(request.OptimizerViews))
	}
	if request.RetryOptimizerViewsSet {
		args = append(args, "--retry-optimizer-views", strings.TrimSpace(request.RetryOptimizerViews))
	}
	if request.NoopRetryBudgetSet {
		args = append(args, "--noop-retry-budget", strconv.Itoa(request.NoopRetryBudget))
	}
	if request.GateRejectRetryBudgetSet {
		args = append(args, "--gate-reject-retry-budget", strconv.Itoa(request.GateRejectRetryBudget))
	}
	if request.WrongArtifactRetryBudgetSet {
		args = append(args, "--wrong-artifact-retry-budget", strconv.Itoa(request.WrongArtifactRetryBudget))
	}
	if request.TargetArtifactRetryBudgetSet {
		args = append(args, "--target-artifact-retry-budget", strconv.Itoa(request.TargetArtifactRetryBudget))
	}
	if request.HardFailureRetryBudgetSet {
		args = append(args, "--hard-failure-retry-budget", strconv.Itoa(request.HardFailureRetryBudget))
	}
	if feedbackDirectMode := strings.TrimSpace(request.FeedbackDirectMode); feedbackDirectMode != "" {
		args = append(args, "--feedback-direct-mode", feedbackDirectMode)
	}
	if request.FinalEval {
		args = append(args, "--eval-test")
	}
	if request.DryRun {
		args = append(args, "--dry-run")
	}
	return resolved, args, nil
}

func skillOptTrainOptimizerGate(iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest) (string, error) {
	gate := strings.TrimSpace(strings.ToLower(request.Gate))
	if gate == "" {
		gate = skillOptMetadataString(iteration.MetadataJSON, "evaluation", "preferred_gate")
	}
	gate = strings.TrimSpace(strings.ToLower(gate))
	if gate == "hard_then_soft" {
		gate = "mixed"
	}
	if gate == "" {
		gate = "hard"
	}
	switch gate {
	case "hard", "soft", "mixed":
		return gate, nil
	default:
		return "", fmt.Errorf("optimizer gate %q is not supported; use hard, soft, or mixed", gate)
	}
}

// announceSkillOptTrainOptimizerLaunch notifies the operator that a long-lived
// optimizer run is about to start, since the run blocks with no streamed output
// until it completes. It is a notice only and never blocks for confirmation, so
// automated continue flows are unaffected. progress is nil for callers without a
// terminal.
func announceSkillOptTrainOptimizerLaunch(progress io.Writer, request skillOptTrainOptimizerRequest) {
	if progress == nil {
		return
	}
	if request.DryRun {
		fmt.Fprintln(progress, "skillopt train continue: launching optimizer dry run; this skips model calls but may still take a while")
		return
	}
	fmt.Fprintln(progress, "skillopt train continue: launching optimizer; this runs long-lived model calls and will not stream output until it finishes")
}

// skillOptTrainOptimizerProgressInterval is how often the optimizer heartbeat
// reports elapsed time; it is a var so tests can shorten it.
var skillOptTrainOptimizerProgressInterval = 30 * time.Second

// startSkillOptTrainOptimizerHeartbeat prints an elapsed-time line every
// interval while the (output-buffered, long-lived) optimizer subprocess runs,
// so the operator can tell it is alive. It returns a stop func; a nil progress
// writer makes it a no-op.
func startSkillOptTrainOptimizerHeartbeat(progress io.Writer) func() {
	if progress == nil {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(skillOptTrainOptimizerProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(progress, "optimizer running - %s\n", formatShortDuration(time.Since(start)))
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

func runSkillOptTrainOptimizer(ctx context.Context, progress io.Writer, paths skillOptTrainOptimizerPaths, request skillOptTrainOptimizerRequest, command string, args []string) (subprocess.Result, error) {
	timeout := strings.TrimSpace(request.Timeout)
	if timeout != "" {
		duration, err := time.ParseDuration(timeout)
		if err != nil {
			return subprocess.Result{}, fmt.Errorf("parse optimizer timeout: %w", err)
		}
		if duration <= 0 {
			return subprocess.Result{}, errors.New("optimizer timeout must be greater than zero")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	if err := os.MkdirAll(paths.OutRoot, 0o755); err != nil {
		return subprocess.Result{}, fmt.Errorf("create optimizer out-root: %w", err)
	}
	// Stream the optimizer's progress lines live when the runner supports it:
	// in the detached phase-view child, progress IS the tailed session log, so
	// the minibatch/analyst/accept lines appear as they happen instead of only
	// after exit. The buffered Result is unchanged either way.
	var result subprocess.Result
	var err error
	if streamer, ok := skillOptTrainOptimizerRunner.(subprocess.StreamRunner); ok && progress != nil {
		result, err = streamer.RunStream(ctx, paths.OutRoot, progress, command, args...)
	} else {
		result, err = skillOptTrainOptimizerRunner.Run(ctx, paths.OutRoot, command, args...)
	}
	if err != nil {
		return result, fmt.Errorf("optimizer failed: %w%s", err, subprocessDiagnostics(result))
	}
	return result, nil
}

func subprocessDiagnostics(result subprocess.Result) string {
	stderr := strings.TrimSpace(result.Stderr)
	stdout := strings.TrimSpace(result.Stdout)
	switch {
	case stderr != "" && stdout != "":
		return fmt.Sprintf(" (stderr: %s; stdout: %s)", truncateForMetadata(stderr), truncateForMetadata(stdout))
	case stderr != "":
		return fmt.Sprintf(" (stderr: %s)", truncateForMetadata(stderr))
	case stdout != "":
		return fmt.Sprintf(" (stdout: %s)", truncateForMetadata(stdout))
	default:
		return ""
	}
}

func skillOptTrainOptimizerMetadata(request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string, result subprocess.Result, status string, failure error) map[string]any {
	metadata := map[string]any{
		"status":                 status,
		"command":                command,
		"args":                   args,
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"dry_run":                request.DryRun,
		"stdout":                 truncateForMetadata(result.Stdout),
		"stderr":                 truncateForMetadata(result.Stderr),
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	if failure != nil {
		metadata["error"] = failure.Error()
		if nextAction := skillOptTrainOptimizerFailureNextAction(failure); nextAction != "" {
			metadata["next_action"] = nextAction
		}
	}
	addSkillOptTrainOptimizerConfigMetadata(metadata, request)
	return metadata
}

func skillOptTrainOptimizerFailureNextAction(failure error) string {
	var preflightErr skillOptTrainOptimizerPreflightError
	if errors.As(failure, &preflightErr) {
		return skillOptTrainSkillOptInstallNextAction()
	}
	return ""
}

func addSkillOptTrainOptimizerConfigMetadata(metadata map[string]any, request skillOptTrainOptimizerRequest) {
	if metadata == nil {
		return
	}
	if mode := strings.TrimSpace(request.FeedbackDirectMode); mode != "" {
		metadata["feedback_direct_mode"] = mode
	}
	// Resolved backend/model identify WHAT is running; the phase view shows
	// them in its header.
	if value := firstNonEmpty(strings.TrimSpace(request.OptimizerBackend), strings.TrimSpace(request.Backend)); value != "" {
		metadata["run_optimizer_backend"] = value
	}
	if value := firstNonEmpty(strings.TrimSpace(request.OptimizerModel), strings.TrimSpace(request.Model)); value != "" {
		metadata["run_optimizer_model"] = value
	}
	if request.FinalEvalSet {
		metadata["final_eval"] = request.FinalEval
	} else if request.FinalEval {
		metadata["final_eval"] = true
	}
	if request.OptimizerViewsSet {
		metadata["optimizer_views"] = request.OptimizerViews
	}
	if request.RetryOptimizerViewsSet {
		metadata["retry_optimizer_views"] = strings.TrimSpace(request.RetryOptimizerViews)
	}
	if request.TargetArtifactRetryBudgetSet {
		metadata["target_artifact_retry_budget"] = request.TargetArtifactRetryBudget
	}
	if request.HardFailureRetryBudgetSet {
		metadata["hard_failure_retry_budget"] = request.HardFailureRetryBudget
	}
	if request.NoopRetryBudgetSet {
		metadata["noop_retry_budget"] = request.NoopRetryBudget
	}
	if request.GateRejectRetryBudgetSet {
		metadata["gate_reject_retry_budget"] = request.GateRejectRetryBudget
	}
	if request.WrongArtifactRetryBudgetSet {
		metadata["wrong_artifact_retry_budget"] = request.WrongArtifactRetryBudget
	}
}

func recordSkillOptTrainOptimizerStarted(ctx context.Context, store *db.Store, session *db.SkillOptTrainSession, iteration *db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string) error {
	metadata := map[string]any{
		"status":                 "running",
		"command":                command,
		"args":                   args,
		"training_package":       paths.TrainingPackagePath,
		"out_root":               paths.OutRoot,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"candidate_output":       paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"dry_run":                request.DryRun,
		"started_at":             time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	addSkillOptTrainOptimizerConfigMetadata(metadata, request)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, *session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, *iteration)
}

func recordSkillOptTrainOptimizerFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainOptimizerRequest, paths skillOptTrainOptimizerPaths, command string, args []string, result subprocess.Result, failure error) error {
	metadata := skillOptTrainOptimizerMetadata(request, paths, command, args, result, "failed", failure)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "optimizer", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "optimizer", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func recordSkillOptTrainCandidateImportFailure(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, failure error) error {
	metadata := map[string]any{
		"status":                 "failed",
		"candidate_package":      paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"error":                  failure.Error(),
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	session.State = skillopt.TrainStateOptimizerCompleted
	iteration.State = skillopt.TrainStateOptimizerCompleted
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func recordSkillOptTrainNoCandidate(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, paths skillOptTrainOptimizerPaths, reason string) error {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateOptimizerCompletedNoCandidate); err != nil {
		return err
	}
	packageReason, packageNextAction, packageDetails := skillOptNoCandidatePackageMetadata(paths.CandidatePackagePath)
	if strings.TrimSpace(packageReason) != "" {
		reason = packageReason
	}
	nextAction := skillOptNoCandidateNextAction()
	if strings.TrimSpace(packageNextAction) != "" {
		nextAction = packageNextAction
	}
	metadata := map[string]any{
		"status":                 "no_candidate",
		"candidate_package":      paths.CandidatePackagePath,
		"artifact_dir":           paths.ArtifactDir,
		"optimizer_root":         paths.OptimizerRoot,
		"optimizer_attempt":      paths.OptimizerAttempt,
		"optimizer_attempt_path": paths.OptimizerAttemptPath,
		"no_candidate_reason":    reason,
		"next_action":            nextAction,
		"completed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"source":                 "gitmoot skillopt train continue",
	}
	if len(packageDetails) > 0 {
		metadata["no_candidate_details"] = packageDetails
	}
	session.State = skillopt.TrainStateOptimizerCompletedNoCandidate
	iteration.State = skillopt.TrainStateOptimizerCompletedNoCandidate
	iteration.CandidateVersionID = ""
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_import", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_import", metadata)
	if err := store.UpsertSkillOptTrainSession(ctx, session); err != nil {
		return err
	}
	return store.UpsertSkillOptTrainIteration(ctx, iteration)
}

func skillOptNoCandidateReason(err error) string {
	text := strings.TrimSpace(err.Error())
	marker := skillopt.ErrNoCandidate.Error() + ":"
	if index := strings.LastIndex(text, marker); index >= 0 {
		return strings.TrimSpace(text[index+len(marker):])
	}
	return text
}

func skillOptNoCandidateReasonAndNextAction(err error, candidatePackagePath string) (string, string) {
	reason := skillOptNoCandidateReason(err)
	packageReason, packageNextAction, _ := skillOptNoCandidatePackageMetadata(candidatePackagePath)
	if strings.TrimSpace(packageReason) != "" {
		reason = packageReason
	}
	nextAction := skillOptNoCandidateNextAction()
	if strings.TrimSpace(packageNextAction) != "" {
		nextAction = packageNextAction
	}
	return reason, nextAction
}

func skillOptNoCandidateNextAction() string {
	return "do not publish a candidate review; revise feedback and start another iteration, rerun the optimizer with --rerun-optimizer, or stop"
}

func skillOptNoCandidatePackageMetadata(candidatePackagePath string) (string, string, map[string]any) {
	candidatePackagePath = strings.TrimSpace(candidatePackagePath)
	if candidatePackagePath == "" {
		return "", "", nil
	}
	content, err := os.ReadFile(candidatePackagePath)
	if err != nil {
		return "", "", nil
	}
	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		return "", "", nil
	}
	summary := decodedSkillOptMetadataValue(raw["summary"])
	metadata := decodedSkillOptMetadataValue(summary["metadata"])
	evalReport := decodedSkillOptMetadataValue(raw["eval_report"])
	reason := ""
	nextAction := ""
	var details map[string]any
	for _, source := range []map[string]any{evalReport, metadata} {
		if skillOptCandidateReviewExplicitPromotable(source) {
			continue
		}
		if reason == "" {
			reason = metadataString(source, "no_candidate_reason")
		}
		if nextAction == "" {
			nextAction = metadataString(source, "next_action")
		}
		if len(details) == 0 {
			details = decodedSkillOptMetadataValue(source["no_candidate_details"])
		}
		details = skillOptNoCandidateDetailsWithDiagnostics(details, source)
	}
	for _, gateRejection := range []map[string]any{
		decodedSkillOptMetadataValue(summary["gate_rejection"]),
		decodedSkillOptMetadataValue(decodedSkillOptMetadataValue(summary["evaluator_score"])["gate_rejection"]),
		decodedSkillOptMetadataValue(evalReport["gate_rejection"]),
	} {
		if len(gateRejection) == 0 {
			continue
		}
		if reason == "" {
			reason = metadataString(gateRejection, "rejection_type")
		}
		if reason == "" {
			reason = metadataString(gateRejection, "primary_reason")
		}
		if nextAction == "" {
			nextAction = metadataString(gateRejection, "next_action")
		}
		if len(details) == 0 {
			details = skillOptNoCandidateDetailsFromGateRejection(gateRejection)
		}
		details = skillOptNoCandidateDetailsWithDiagnostics(details, gateRejection)
	}
	if reason == "" {
		return "", "", nil
	}
	return reason, nextAction, details
}

func skillOptNoCandidateDetailsWithDiagnostics(details map[string]any, source map[string]any) map[string]any {
	if len(source) == 0 {
		return skillOptNoCandidateDetailsWithFeedbackContext(details, nil)
	}
	diagnostics := decodedSkillOptMetadataValue(source["no_candidate_diagnostics"])
	if len(diagnostics) == 0 {
		diagnostics = decodedSkillOptMetadataValue(source["diagnostics"])
	}
	if len(diagnostics) == 0 {
		diagnostics = map[string]any{}
	}
	if categories := metadataStringSlice(source, "diagnostic_categories"); len(categories) > 0 && len(metadataStringSlice(diagnostics, "categories")) == 0 {
		diagnostics["categories"] = categories
	}
	for _, key := range []string{"selection_gate_relation", "stop_reason"} {
		if value := metadataString(source, key); value != "" && metadataString(diagnostics, key) == "" {
			diagnostics[key] = value
		}
	}
	if value := metadataBoolPtr(source, "retry_budget_exhausted"); value != nil && metadataBoolPtr(diagnostics, "retry_budget_exhausted") == nil {
		diagnostics["retry_budget_exhausted"] = *value
	}
	if stopReasons := metadataStringSlice(source, "retry_stop_reasons"); len(stopReasons) > 0 && len(metadataStringSlice(diagnostics, "retry_stop_reasons")) == 0 {
		diagnostics["retry_stop_reasons"] = stopReasons
	}
	if len(diagnostics) == 0 && len(metadataStringSlice(source, "feedback_themes")) == 0 {
		return skillOptNoCandidateDetailsWithFeedbackContext(details, source)
	}
	if details == nil {
		details = map[string]any{}
	}
	if len(diagnostics) > 0 {
		details["diagnostics"] = diagnostics
		if categories := metadataStringSlice(diagnostics, "categories"); len(categories) > 0 {
			details["diagnostic_categories"] = categories
		}
		for _, key := range []string{"selection_gate_relation", "stop_reason"} {
			if value := metadataString(diagnostics, key); value != "" {
				details[key] = value
			}
		}
		if value := metadataBoolPtr(diagnostics, "retry_budget_exhausted"); value != nil {
			details["retry_budget_exhausted"] = *value
		}
		if stopReasons := metadataStringSlice(diagnostics, "retry_stop_reasons"); len(stopReasons) > 0 {
			details["retry_stop_reasons"] = stopReasons
		}
	}
	if themes := metadataStringSlice(source, "feedback_themes"); len(themes) > 0 {
		details["feedback_themes"] = themes
	}
	if retryBudget := skillOptRetryBudgetFromAttempts(metadataString(details, "retry_attempts")); retryBudget != "" && metadataString(details, "retry_budget") == "" {
		details["retry_budget"] = retryBudget
	}
	return skillOptNoCandidateDetailsWithFeedbackContext(details, source)
}

func skillOptNoCandidateDetailsFromGateRejection(gateRejection map[string]any) map[string]any {
	if len(gateRejection) == 0 {
		return nil
	}
	details := map[string]any{}
	for _, key := range []string{"attempted_patch", "retry_attempts"} {
		if value := metadataString(gateRejection, key); value != "" {
			details[key] = value
		}
	}
	nextActions := metadataStringSlice(gateRejection, "next_actions")
	if len(nextActions) == 0 {
		nextActions = metadataStringSlice(gateRejection, "next_action")
	}
	if len(nextActions) > 0 {
		details["next_action"] = nextActions[0]
		details["next_actions"] = nextActions
	}
	if retryBudget := skillOptRetryBudgetFromAttempts(metadataString(gateRejection, "retry_attempts")); retryBudget != "" {
		details["retry_budget"] = retryBudget
	}
	if value := metadataBoolPtr(gateRejection, "duplicate_retry_detected"); value != nil {
		details["duplicate_retry_detected"] = *value
	}
	rejection := map[string]any{}
	for _, key := range []string{"baseline", "candidate"} {
		if value := decodedSkillOptMetadataValue(gateRejection[key]); len(value) > 0 {
			rejection[key] = value
			if gateScore := metadataString(value, "gate_score"); gateScore != "" {
				details[key+"_gate"] = gateScore
			}
			if hard := metadataString(value, "hard"); hard != "" {
				details[key+"_hard"] = hard
			}
			if soft := metadataString(value, "soft"); soft != "" {
				details[key+"_soft"] = soft
			}
		}
	}
	if evaluatorReason := skillOptNoCandidateEvaluatorReason(gateRejection, rejection); evaluatorReason != "" {
		details["evaluator_reason"] = evaluatorReason
	}
	for _, key := range []string{"primary_reason", "human_reason", "optimizer_hint"} {
		if value := metadataString(gateRejection, key); value != "" {
			rejection[key] = value
			if key == "optimizer_hint" {
				details["optimizer_hint"] = value
			}
		}
	}
	for _, key := range []string{"failed_dimensions", "evidence"} {
		if value := metadataStringSlice(gateRejection, key); len(value) > 0 {
			rejection[key] = value
			if key == "failed_dimensions" {
				details["failed_dimensions"] = value
			}
		}
	}
	if humanFeedbackContext := decodedSkillOptMetadataValue(gateRejection["human_feedback_context"]); len(humanFeedbackContext) > 0 {
		rejection["human_feedback_context"] = humanFeedbackContext
	}
	if len(rejection) > 0 {
		details["rejection"] = rejection
	}
	return skillOptNoCandidateDetailsWithFeedbackContext(details, gateRejection)
}

func skillOptRetryBudgetFromAttempts(retryAttempts string) string {
	retryAttempts = strings.TrimSpace(retryAttempts)
	if retryAttempts == "" {
		return ""
	}
	parts := strings.Split(retryAttempts, "/")
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func skillOptNoCandidateDetailsWithFeedbackContext(details map[string]any, source map[string]any) map[string]any {
	context := decodedSkillOptMetadataValue(nil)
	if len(details) > 0 {
		context = decodedSkillOptMetadataValue(details["human_feedback_context"])
	}
	if len(context) == 0 && len(source) > 0 {
		context = decodedSkillOptMetadataValue(source["human_feedback_context"])
	}
	if len(context) == 0 {
		return details
	}
	if details == nil {
		details = map[string]any{}
	}
	details["human_feedback_context"] = context
	for _, key := range []string{"feedback_source", "feedback_target", "review_issue", "review_run_id", "reviewed_skill_version"} {
		if value := metadataString(context, key); value != "" && metadataString(details, key) == "" {
			details[key] = value
		}
	}
	if metadataString(details, "score_basis") == "" && strings.EqualFold(metadataString(context, "feedback_target"), "baseline_review_outputs") {
		details["score_basis"] = "feedback_resolution"
	}
	if len(metadataStringSlice(details, "feedback_themes")) == 0 {
		if themes := metadataStringSlice(context, "themes"); len(themes) > 0 {
			details["feedback_themes"] = themes
		}
	}
	return details
}

func skillOptNoCandidateEvaluatorReason(gateRejection map[string]any, rejection map[string]any) string {
	for _, source := range []map[string]any{
		decodedSkillOptMetadataValue(rejection["candidate"]),
		decodedSkillOptMetadataValue(rejection["baseline"]),
		gateRejection,
	} {
		for _, key := range []string{"evaluator_reason", "evaluator_reasoning", "reasoning", "human_reason", "optimizer_hint", "primary_reason"} {
			if value := metadataString(source, key); value != "" {
				return value
			}
		}
	}
	return ""
}

func skillOptTrainOptimizerReportLines(result skillOptTrainOptimizerResult) []string {
	lines := []string{
		fmt.Sprintf("training_package: %s", result.TrainingPackagePath),
		fmt.Sprintf("optimizer_out_root: %s", result.OutRoot),
		fmt.Sprintf("optimizer_root: %s", result.OptimizerRoot),
		fmt.Sprintf("optimizer_attempt: %s", result.OptimizerAttempt),
		fmt.Sprintf("optimizer_attempt_path: %s", result.OptimizerAttemptPath),
		fmt.Sprintf("candidate_package: %s", result.CandidatePackagePath),
		fmt.Sprintf("artifact_dir: %s", result.ArtifactDir),
		fmt.Sprintf("backend: %s", result.BackendResolution.Backend),
		fmt.Sprintf("optimizer_backend: %s", emptyText(result.BackendResolution.OptimizerBackend)),
		fmt.Sprintf("target_backend: %s", emptyText(result.BackendResolution.TargetBackend)),
		fmt.Sprintf("internal_target_adapter: %s", emptyText(result.BackendResolution.InternalTargetAdapter)),
		fmt.Sprintf("evaluator_backend: %s", emptyText(result.BackendResolution.EvaluatorBackend)),
		fmt.Sprintf("backend_config_status: %s", result.BackendResolution.ConfigStatus),
		fmt.Sprintf("optimizer_lock: %s", result.OptimizerLockState),
		fmt.Sprintf("recovery_available: %t", result.RecoveryAvailable),
	}
	if strings.TrimSpace(result.Command) != "" {
		lines = append(lines, fmt.Sprintf("optimizer_command: %s", shellArgs(append([]string{result.Command}, result.Args...))))
	} else {
		lines = append(lines, "optimizer_command: -")
	}
	if mode := strings.TrimSpace(result.Request.FeedbackDirectMode); mode != "" {
		lines = append(lines, fmt.Sprintf("feedback_direct_mode: %s", mode))
	}
	if result.Request.OptimizerViewsSet {
		lines = append(lines, fmt.Sprintf("optimizer_views: %d", result.Request.OptimizerViews))
	}
	if result.Request.RetryOptimizerViewsSet {
		lines = append(lines, fmt.Sprintf("retry_optimizer_views: %s", strings.TrimSpace(result.Request.RetryOptimizerViews)))
	}
	if result.Request.FinalEval {
		lines = append(lines, "final_eval: true")
	}
	if result.Request.TargetArtifactRetryBudgetSet {
		lines = append(lines, fmt.Sprintf("target_artifact_retry_budget: %d", result.Request.TargetArtifactRetryBudget))
	}
	if result.Request.HardFailureRetryBudgetSet {
		lines = append(lines, fmt.Sprintf("hard_failure_retry_budget: %d", result.Request.HardFailureRetryBudget))
	}
	lines = append(lines, fmt.Sprintf("optimizer_dry_run: %t", result.DryRun))
	if next := strings.TrimSpace(result.NextAction); next != "" {
		lines = append(lines, fmt.Sprintf("next: %s", next))
	}
	return lines
}

func skillOptTrainOptimizerResultHasReport(result skillOptTrainOptimizerResult) bool {
	return strings.TrimSpace(result.TrainingPackagePath) != "" ||
		strings.TrimSpace(result.OutRoot) != "" ||
		strings.TrimSpace(result.CandidatePackagePath) != "" ||
		strings.TrimSpace(result.ArtifactDir) != "" ||
		strings.TrimSpace(result.BackendResolution.Backend) != ""
}

func importSkillOptTrainCandidate(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, optimizerPaths skillOptTrainOptimizerPaths) (db.AgentTemplateVersion, error) {
	content, err := os.ReadFile(optimizerPaths.CandidatePackagePath)
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("read optimizer candidate package: %w", err)
	}
	var candidate skillopt.CandidatePackage
	if err := json.Unmarshal(content, &candidate); err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("decode optimizer candidate package: %w", err)
	}
	if err := validateSkillOptTrainCandidatePackage(ctx, store, session, iteration, candidate); err != nil {
		return db.AgentTemplateVersion{}, err
	}
	version, err := skillopt.ImportCandidatePackageWithOptions(ctx, store, candidate, skillopt.CandidateImportOptions{
		SourcePath:  optimizerPaths.CandidatePackagePath,
		ArtifactDir: optimizerPaths.ArtifactDir,
		BlobStore:   artifact.NewStore(paths.ArtifactBlobs),
	})
	if err != nil {
		return db.AgentTemplateVersion{}, fmt.Errorf("import optimizer candidate package: %w", err)
	}
	return version, nil
}

func validateSkillOptTrainCandidatePackage(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidate skillopt.CandidatePackage) error {
	templateID := strings.TrimSpace(candidate.TemplateID)
	if templateID != strings.TrimSpace(session.TemplateID) {
		return fmt.Errorf("optimizer candidate template_id %q does not match train session template %q", templateID, strings.TrimSpace(session.TemplateID))
	}
	expectedBase := strings.TrimSpace(iteration.BaseTemplateVersionID)
	if expectedBase == "" {
		expectedBase = strings.TrimSpace(session.TemplateVersionID)
	}
	baseRef := strings.TrimSpace(candidate.BaseVersionID)
	if baseRef == "" {
		base, err := store.GetAgentTemplate(ctx, templateID)
		if err != nil {
			return fmt.Errorf("load optimizer candidate current base for %q: %w", templateID, err)
		}
		if base.VersionID != expectedBase {
			return fmt.Errorf("optimizer candidate omitted base_version_id and current base is %q, want active train base %q", base.VersionID, expectedBase)
		}
		return nil
	}
	base, err := store.GetAgentTemplateReference(ctx, baseRef)
	if err != nil {
		return fmt.Errorf("load optimizer candidate base version %q: %w", baseRef, err)
	}
	if base.ID != templateID {
		return fmt.Errorf("optimizer candidate base_version_id %q belongs to template %q, want %q", baseRef, base.ID, templateID)
	}
	if base.VersionID != expectedBase {
		return fmt.Errorf("optimizer candidate base_version_id %q resolved to %q, want active train base %q", baseRef, base.VersionID, expectedBase)
	}
	return nil
}

func truncateForMetadata(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 2000 {
		return value
	}
	return value[:2000] + "...<truncated>"
}

func skillOptTrainGenerationRepo(session db.SkillOptTrainSession) string {
	if repo := strings.TrimSpace(session.WorkspaceRepo); repo != "" {
		return repo
	}
	return strings.TrimSpace(session.TargetRepo)
}

func ensureSkillOptTrainGenerationRepoReady(ctx context.Context, store *db.Store, repoName string) error {
	repoName = strings.TrimSpace(repoName)
	if repoName == "" {
		return errors.New("skillopt train generation repo is required")
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return err
	}
	if existing, err := store.GetRepo(ctx, repo.FullName()); err == nil {
		if strings.TrimSpace(existing.CheckoutPath) == "" {
			return fmt.Errorf("generation repo %s has no checkout path; run `gitmoot repo add %s --path /path/to/checkout` before train continue", repo.FullName(), repo.FullName())
		}
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: existing.CheckoutPath})
		if err != nil {
			return fmt.Errorf("generation repo %s checkout is not ready: %w", repo.FullName(), err)
		}
		record.PollInterval = existing.PollInterval
		return store.UpsertRepo(ctx, record)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
	if err != nil {
		return fmt.Errorf("generation repo %s is not registered with a checkout path; run `gitmoot repo add %s --path /path/to/checkout` before train continue: %w", repo.FullName(), repo.FullName(), err)
	}
	return store.UpsertRepo(ctx, record)
}

const (
	skillOptTrainGenerationModeTargetSkill       = "target_skill"
	skillOptTrainGenerationModeSkillOptGenerator = "skillopt_generator"
	skillOptTrainGenerationModeCustomAgent       = "custom_agent"
)

type skillOptTrainGenerationDispatch struct {
	Mode              string
	Agent             string
	Type              string
	Runtime           string
	TemplateID        string
	TemplateVersionID string
}

func skillOptTrainGeneratorSelection(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, request skillOptTrainContinueRequest) (skillOptTrainGenerationDispatch, error) {
	agent := strings.TrimSpace(request.GeneratorAgent)
	agentType := strings.TrimSpace(request.GeneratorType)
	if agent != "" && agentType != "" {
		return skillOptTrainGenerationDispatch{}, errors.New("use only one of --generator-agent or --generator-type")
	}
	if agent != "" {
		return skillOptTrainGenerationDispatch{Mode: skillOptTrainGenerationModeCustomAgent, Agent: agent}, nil
	}
	if agentType != "" {
		return skillOptTrainGenerationDispatch{Mode: skillOptTrainGenerationModeSkillOptGenerator, Agent: agentType, Type: agentType}, nil
	}
	templateVersionID := skillOptTrainGenerationTemplateVersion(session, iteration, run)
	if templateVersionID == "" {
		return skillOptTrainFallbackGeneratorDispatch(request), nil
	}
	template, err := loadInstalledTemplate(ctx, store, templateVersionID)
	if err != nil {
		return skillOptTrainGenerationDispatch{}, err
	}
	return skillOptTrainGenerationDispatch{
		Mode:              skillOptTrainGenerationModeTargetSkill,
		Runtime:           skillOptTrainTargetSkillGenerationRuntime(request),
		TemplateID:        template.ID,
		TemplateVersionID: template.VersionID,
	}, nil
}

func skillOptTrainFallbackGeneratorDispatch(request skillOptTrainContinueRequest) skillOptTrainGenerationDispatch {
	agentType := strings.TrimSpace(request.GeneratorType)
	if agentType == "" {
		agentType = "skillopt-generator"
	}
	return skillOptTrainGenerationDispatch{Mode: skillOptTrainGenerationModeSkillOptGenerator, Agent: agentType, Type: agentType}
}

func skillOptTrainGenerationTemplateVersion(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun) string {
	return firstNonEmpty(
		strings.TrimSpace(iteration.BaseTemplateVersionID),
		strings.TrimSpace(run.TemplateVersionID),
		strings.TrimSpace(session.TemplateVersionID),
	)
}

func skillOptTrainTargetSkillGenerationRuntime(request skillOptTrainContinueRequest) string {
	for _, value := range []string{
		request.Optimizer.Backend,
		request.Optimizer.TargetBackend,
		request.Optimizer.OptimizerBackend,
	} {
		switch strings.TrimSpace(strings.ToLower(value)) {
		case runtime.ClaudeRuntime, "claude-code":
			return runtime.ClaudeRuntime
		case runtime.CodexRuntime, "codex_exec":
			return runtime.CodexRuntime
		}
	}
	return runtime.CodexRuntime
}

func skillOptTrainGenerationConcurrency(request skillOptTrainContinueRequest, dispatchType string) (int, error) {
	if strings.TrimSpace(dispatchType) == "" {
		return 1, nil
	}
	types, err := loadAgentTypeConfig(request.Home)
	if err != nil {
		return 0, err
	}
	agentType, ok := types[dispatchType]
	if !ok {
		return 0, fmt.Errorf("agent %q not found", dispatchType)
	}
	if agentType.MaxBackground <= 0 {
		return 1, nil
	}
	return agentType.MaxBackground, nil
}

func skillOptTrainOptionLabels(count int) []string {
	if count <= 0 {
		count = 2
	}
	labels := make([]string, 0, count)
	for index := 0; index < count; index++ {
		if index < 26 {
			labels = append(labels, string(rune('a'+index)))
			continue
		}
		labels = append(labels, fmt.Sprintf("option-%d", index+1))
	}
	return labels
}

func skillOptTrainGenerationRoles(run db.EvalRun) []string {
	if !skillOptRunUsesRankedOptions(run) {
		return []string{"baseline", "candidate"}
	}
	return skillOptTrainOptionLabels(run.OptionsCount)
}

func buildSkillOptTrainGenerationPrompt(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, run db.EvalRun, item db.EvalReviewItem, role string, rankedRun bool) string {
	itemMetadata := decodedSkillOptMetadata(item.MetadataJSON)
	sessionMetadata := decodedSkillOptMetadata(session.MetadataJSON)
	requestText := metadataString(sessionMetadata, "request")
	if requestText == "" {
		requestText = session.RequestSummary
	}
	var builder strings.Builder
	builder.WriteString("Generate one review option for a Gitmoot SkillOpt training run.\n")
	builder.WriteString("Return the generated artifact content in gitmoot_result.summary with decision implemented. Do not include commentary outside the artifact.\n\n")
	builder.WriteString("Training request:\n")
	builder.WriteString(requestText)
	builder.WriteString("\n\n")
	builder.WriteString("Session: ")
	builder.WriteString(session.ID)
	builder.WriteString("\nIteration: ")
	builder.WriteString(iteration.ID)
	builder.WriteString("\nEval run: ")
	builder.WriteString(run.ID)
	builder.WriteString("\nMode: ")
	builder.WriteString(run.Mode)
	builder.WriteString("\nExploration level: ")
	builder.WriteString(run.ExplorationLevel)
	if rankedRun {
		builder.WriteString("\nOption label: ")
		builder.WriteString(strings.ToUpper(role))
	} else {
		builder.WriteString("\nA/B artifact role: ")
		builder.WriteString(role)
	}
	builder.WriteString("\nItem id: ")
	builder.WriteString(item.ItemID)
	builder.WriteString("\nTitle: ")
	builder.WriteString(item.Title)
	if brief := metadataString(itemMetadata, "brief"); brief != "" {
		builder.WriteString("\nBrief: ")
		builder.WriteString(brief)
	}
	if audience := metadataString(itemMetadata, "target_audience"); audience != "" {
		builder.WriteString("\nTarget audience: ")
		builder.WriteString(audience)
	}
	if outputType := metadataString(itemMetadata, "output_type"); outputType != "" {
		builder.WriteString("\nOutput type: ")
		builder.WriteString(outputType)
	}
	if hints, ok := itemMetadata["artifact_hints"].([]any); ok && len(hints) > 0 {
		builder.WriteString("\nArtifact hints:")
		for _, hint := range hints {
			if text := strings.TrimSpace(fmt.Sprint(hint)); text != "" {
				builder.WriteString("\n- ")
				builder.WriteString(text)
			}
		}
	}
	builder.WriteString("\n\nGeneration rules:\n")
	if rankedRun {
		builder.WriteString("- Make this option meaningfully different from the other labels in layout, content strategy, and visual/interaction direction.\n")
	} else if role == "baseline" {
		builder.WriteString("- Generate the baseline artifact: a solid, conventional answer that satisfies the item brief.\n")
	} else {
		builder.WriteString("- Generate the candidate artifact: a meaningfully different improved answer intended to be compared against the baseline.\n")
	}
	switch run.ExplorationLevel {
	case db.ExplorationLevelHigh:
		builder.WriteString("- Use high exploration: vary the product explanation, proof/content structure, and visual direction substantially.\n")
	case db.ExplorationLevelMedium:
		builder.WriteString("- Use medium exploration: combine promising directions while keeping alternatives visibly different.\n")
	case db.ExplorationLevelLow:
		builder.WriteString("- Use low exploration: make narrow refinements and avoid broad direction changes.\n")
	}
	builder.WriteString("- Keep the artifact self-contained and directly reviewable.\n")
	builder.WriteString("- Preserve the requested output type.\n")
	if skillOptTrainWantsVuePreviewBundle(session) {
		requiresVuePreviewBundle := skillOptTrainRequiresVuePreviewBundle(session)
		builder.WriteString("\nPreview bundle contract:\n")
		if requiresVuePreviewBundle {
			builder.WriteString("- This train session requires a Vue/Vite preview bundle for every generated option.\n")
			builder.WriteString("- Keep gitmoot_result.summary as a string value. The string content must be exactly one serialized JSON object, with no markdown, code fences, or prose.\n")
			builder.WriteString("- Do not set gitmoot_result.summary to a nested object; encode the bundle JSON as the summary string.\n")
		} else {
			builder.WriteString("- This train session is configured for optional Vue/Vite previews. Prefer a Vue/Vite preview bundle so Gitmoot can publish preview URLs; plain text or markdown is accepted only as inline fallback.\n")
			builder.WriteString("- If you return a preview bundle, keep gitmoot_result.summary as a string containing exactly one serialized JSON object, with no markdown, code fences, or prose.\n")
			builder.WriteString("- If you return plain text or markdown fallback, use the normal summary string and do not include the bundle JSON shape.\n")
		}
		builder.WriteString("- Use renderer \"vue-vite\".\n")
		builder.WriteString("- Include build_command exactly \"npm run build\" and dist_dir \"dist\".\n")
		builder.WriteString("- Include files with these required relative paths: package.json, index.html, src/main.js, src/App.vue.\n")
		builder.WriteString("- package.json scripts must include only \"build\": \"vite build\". Do not include dependencies or devDependencies; Gitmoot supplies trusted build dependencies.\n")
		builder.WriteString("- index.html and src/main.js may use a simple Vue mount placeholder; Gitmoot canonicalizes and overwrites them with trusted scaffold files before build.\n")
		builder.WriteString("- src/App.vue must be scriptless template/style Vue only. Do not include script blocks, imports, require, import.meta, @import, or CSS url().\n")
		builder.WriteString("- Each file entry must have path and non-empty content. Use slash-separated relative paths only.\n")
		builder.WriteString("- Do not include local absolute paths, path traversal, secrets, .env files, node_modules, dependency caches, dist, built outputs, vite.config.js, or files outside the required paths.\n")
		builder.WriteString("- The JSON object shape is: renderer string, files array of {path, content}, build_command string, dist_dir string.\n")
	}
	return builder.String()
}

func buildSkillOptTrainGenerationRetryPrompt(basePrompt string, validationError map[string]any) string {
	var builder strings.Builder
	builder.WriteString(basePrompt)
	builder.WriteString("\n\nRetry instruction:\n")
	builder.WriteString("- Retry this same review option only; do not change the item id or option label.\n")
	builder.WriteString("- The previous generated artifact failed validation. Fix the concrete validation error below and return a fresh artifact.\n")
	builder.WriteString("- Do not repeat the same invalid output.\n")
	builder.WriteString("\nValidation error:\n")
	fmt.Fprintf(&builder, "- class: %s\n", validationError["class"])
	fmt.Fprintf(&builder, "- message: %s\n", validationError["message"])
	return builder.String()
}

func skillOptTrainOptionValidationError(itemID string, role string, attempt int, err error) map[string]any {
	return map[string]any{
		"class":   "preview_bundle",
		"item_id": itemID,
		"role":    role,
		"attempt": attempt,
		"message": strings.TrimSpace(err.Error()),
	}
}

func prepareReviewItemContentArtifact(blobStore artifact.Store, runID string, itemID string, role string, content []byte, mediaType string, driver string) (db.EvalArtifact, error) {
	if len(content) == 0 || strings.TrimSpace(string(content)) == "" {
		return db.EvalArtifact{}, fmt.Errorf("%s content is required", role)
	}
	blob, err := blobStore.Put(content)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("store %s artifact blob: %w", role, err)
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = "text/plain"
	}
	if strings.TrimSpace(driver) == "" {
		driver = "text"
	}
	return db.EvalArtifact{
		ID:        reviewItemArtifactID(runID, itemID, role),
		Hash:      blob.Hash,
		MediaType: mediaType,
		SizeBytes: blob.Size,
		Driver:    driver,
	}, nil
}

func skillOptTrainGeneratedOptionMetadata(output localAgentJobOutput, prompt string, generationMode string, templateID string, templateVersionID string, sampleLabel string, previewBundleMetadata *skillopt.PreviewBundleMetadata, retryAttempts int, validationErrors []map[string]any) string {
	metadata := map[string]any{
		"source":              "gitmoot skillopt train continue",
		"job_id":              output.JobID,
		"agent":               output.Agent,
		"prompt":              prompt,
		"raw_output_count":    output.RawOutputCount,
		"generation_mode":     generationMode,
		"template_id":         templateID,
		"template_version_id": templateVersionID,
		"sample_label":        sampleLabel,
	}
	if previewBundleMetadata != nil {
		metadata["preview_bundle"] = *previewBundleMetadata
	}
	if retryAttempts > 0 {
		metadata["retry_attempts"] = retryAttempts
		metadata["validation_errors"] = validationErrors
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func skillOptTrainGenerationStrategy(run db.EvalRun) map[string]any {
	return map[string]any{
		"mode":              run.Mode,
		"exploration_level": run.ExplorationLevel,
		"options_count":     run.OptionsCount,
	}
}

func mergeSkillOptTrainMetadata(existing string, key string, value map[string]any) string {
	metadata := decodedSkillOptMetadata(existing)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[key] = value
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return existing
	}
	return string(encoded)
}

func decodedSkillOptMetadata(value string) map[string]any {
	var metadata map[string]any
	if strings.TrimSpace(value) != "" {
		_ = json.Unmarshal([]byte(value), &metadata)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	return metadata
}

func decodedSkillOptMetadataValue(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func metadataSlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			values = append(values, item)
		}
		return values
	case []map[string]string:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			metadata := make(map[string]any, len(item))
			for key, value := range item {
				metadata[key] = value
			}
			values = append(values, metadata)
		}
		return values
	default:
		return nil
	}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func skillOptTrainConfirmCommand(args []string, sessionID string) string {
	filtered := make([]string, 0, len(args)+4)
	filtered = append(filtered, "gitmoot", "skillopt", "train", "start")
	hasSession := false
	for _, arg := range args {
		if arg == "--dry-run" || arg == "-dry-run" || strings.HasPrefix(arg, "--dry-run=") || strings.HasPrefix(arg, "-dry-run=") || arg == "--yes" || arg == "-yes" || strings.HasPrefix(arg, "--yes=") || strings.HasPrefix(arg, "-yes=") {
			continue
		}
		if arg == "--session" || arg == "-session" || strings.HasPrefix(arg, "--session=") || strings.HasPrefix(arg, "-session=") {
			hasSession = true
		}
		filtered = append(filtered, arg)
	}
	if !hasSession && strings.TrimSpace(sessionID) != "" {
		filtered = append(filtered, "--session", strings.TrimSpace(sessionID))
	}
	filtered = append(filtered, "--yes")
	return shellArgs(filtered)
}

func runSkillOptReview(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "create":
		return runSkillOptReviewCreate(args[1:], stdout, stderr)
	case "item":
		return runSkillOptReviewItem(args[1:], stdout, stderr)
	case "status":
		return runSkillOptReviewStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt review command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptReviewCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "agent template id or version to review")
	repoFlag := fs.String("repo", "", "target repository in owner/repo form")
	runID := fs.String("run", "", "review run id")
	mode := fs.String("mode", "", "review mode: validate, explore, refine, or distill")
	explorationLevel := fs.String("exploration-level", "", "exploration level: high, medium, or low")
	optionsCount := fs.Int("options", 0, "expected number of review options")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review create does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*templateID) == "" || strings.TrimSpace(*repoFlag) == "" || strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt review create requires --template, --repo, and --run")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review create: %v\n", err)
		return 2
	}
	var run db.EvalRun
	if err := withStore(*home, func(store *db.Store) error {
		template, err := loadInstalledTemplate(context.Background(), store, *templateID)
		if err != nil {
			return err
		}
		run = db.EvalRun{
			ID:                strings.TrimSpace(*runID),
			TemplateID:        template.ID,
			TemplateVersionID: template.VersionID,
			TargetRepo:        repo.FullName(),
			State:             "review",
			Mode:              strings.TrimSpace(*mode),
			ExplorationLevel:  strings.TrimSpace(*explorationLevel),
			OptionsCount:      *optionsCount,
			MetadataJSON:      `{"driver":"manual-review"}`,
		}
		return store.UpsertEvalRun(context.Background(), run)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review create: %v\n", err)
		return 1
	}
	writeLine(stdout, "created review %s for %s", run.ID, run.TemplateVersionID)
	return 0
}

func runSkillOptReviewItem(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runSkillOptReviewItemAdd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt review item command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type skillOptOptionSpec struct {
	Label string
	Path  string
}

type preparedSkillOptOption struct {
	Spec     skillOptOptionSpec
	Artifact db.EvalArtifact
	Metadata string
}

func parseSkillOptOptionFlags(values []string) ([]skillOptOptionSpec, error) {
	specs := make([]skillOptOptionSpec, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		label, path, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("--option must use label=path form")
		}
		label = strings.ToLower(strings.TrimSpace(label))
		path = strings.TrimSpace(path)
		if err := validateSkillOptOptionLabel(label); err != nil {
			return nil, err
		}
		if path == "" {
			return nil, fmt.Errorf("option %s path is required", label)
		}
		if _, ok := seen[label]; ok {
			return nil, fmt.Errorf("duplicate option label %q", label)
		}
		seen[label] = struct{}{}
		specs = append(specs, skillOptOptionSpec{Label: label, Path: path})
	}
	return specs, nil
}

func validateSkillOptOptionLabel(label string) error {
	if label == "" {
		return errors.New("option label is required")
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("option label %q must use only letters, digits, dots, dashes, or underscores", label)
		}
	}
	return nil
}

func runSkillOptReviewItemAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review item add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "review run id")
	itemID := fs.String("item", "", "review item id")
	title := fs.String("title", "", "review item title")
	baselinePath := fs.String("baseline", "", "baseline output file")
	candidatePath := fs.String("candidate", "", "candidate output file")
	metadataJSON := fs.String("metadata-json", "", "JSON metadata to attach to the review item")
	mediaType := fs.String("media-type", "", "media type override for stored artifacts")
	driver := fs.String("driver", "text", "artifact driver")
	var optionFlags repeatedStringFlag
	fs.Var(&optionFlags, "option", "N-way option in label=path form; repeat once per option")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review item add does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*itemID) == "" {
		fmt.Fprintln(stderr, "skillopt review item add requires --run and --item")
		return 2
	}
	hasAB := strings.TrimSpace(*baselinePath) != "" || strings.TrimSpace(*candidatePath) != ""
	hasOptions := len(optionFlags) > 0
	if hasAB && hasOptions {
		fmt.Fprintln(stderr, "skillopt review item add accepts either --baseline/--candidate or repeated --option flags, not both")
		return 2
	}
	if !hasOptions && (strings.TrimSpace(*baselinePath) == "" || strings.TrimSpace(*candidatePath) == "") {
		fmt.Fprintln(stderr, "skillopt review item add requires --baseline and --candidate, or repeated --option label=path flags")
		return 2
	}
	optionSpecs, err := parseSkillOptOptionFlags(optionFlags)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 2
	}
	metadata, err := normalizeSkillOptMetadataJSON(*metadataJSON)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 2
	}
	var item db.EvalReviewItem
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		run, err := store.GetEvalRun(ctx, strings.TrimSpace(*runID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("review run %s not found", strings.TrimSpace(*runID))
			}
			return err
		}
		blobStore := artifact.NewStore(paths.ArtifactBlobs)
		rankedRun := skillOptRunUsesRankedOptions(run)
		if hasOptions && !rankedRun {
			return fmt.Errorf("review run %s is validate/A/B mode; use --baseline and --candidate", run.ID)
		}
		if !hasOptions && rankedRun {
			return fmt.Errorf("review run %s is ranked mode; use repeated --option label=path flags", run.ID)
		}
		if hasOptions {
			if run.OptionsCount > 0 && len(optionSpecs) != run.OptionsCount {
				return fmt.Errorf("review run %s expects %d options, got %d", run.ID, run.OptionsCount, len(optionSpecs))
			}
			preparedOptions := make([]preparedSkillOptOption, 0, len(optionSpecs))
			for _, spec := range optionSpecs {
				optionArtifact, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "option-"+spec.Label, spec.Path, *mediaType, *driver)
				if err != nil {
					return err
				}
				optionMetadata, err := reviewOptionMetadataJSON(spec.Path)
				if err != nil {
					return err
				}
				preparedOptions = append(preparedOptions, preparedSkillOptOption{
					Spec:     spec,
					Artifact: optionArtifact,
					Metadata: optionMetadata,
				})
			}
			item = db.EvalReviewItem{
				RunID:        run.ID,
				ItemID:       strings.TrimSpace(*itemID),
				Title:        strings.TrimSpace(*title),
				MetadataJSON: metadata,
			}
			if err := preserveExistingSkillOptReviewItemDetails(ctx, store, &item); err != nil {
				return err
			}
			if err := store.UpsertEvalReviewItem(ctx, item); err != nil {
				return err
			}
			replacementOptions := make([]db.EvalReviewOption, 0, len(preparedOptions))
			for _, prepared := range preparedOptions {
				if err := store.UpsertEvalArtifact(ctx, prepared.Artifact); err != nil {
					return fmt.Errorf("register option %s artifact: %w", prepared.Spec.Label, err)
				}
				replacementOptions = append(replacementOptions, db.EvalReviewOption{
					RunID:        run.ID,
					ItemID:       strings.TrimSpace(*itemID),
					Label:        prepared.Spec.Label,
					ArtifactID:   prepared.Artifact.ID,
					Role:         "option",
					MetadataJSON: prepared.Metadata,
				})
			}
			if err := store.ReplaceEvalReviewOptions(ctx, run.ID, strings.TrimSpace(*itemID), replacementOptions); err != nil {
				return err
			}
			return nil
		}
		baseline, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "baseline", *baselinePath, *mediaType, *driver)
		if err != nil {
			return err
		}
		candidate, err := prepareReviewItemArtifact(blobStore, run.ID, *itemID, "candidate", *candidatePath, *mediaType, *driver)
		if err != nil {
			return err
		}
		if baseline.ID == candidate.ID {
			return errors.New("baseline and candidate artifact ids must be different")
		}
		if err := store.UpsertEvalArtifact(ctx, baseline); err != nil {
			return fmt.Errorf("register baseline artifact: %w", err)
		}
		if err := store.UpsertEvalArtifact(ctx, candidate); err != nil {
			return fmt.Errorf("register candidate artifact: %w", err)
		}
		item = db.EvalReviewItem{
			RunID:               run.ID,
			ItemID:              strings.TrimSpace(*itemID),
			Title:               strings.TrimSpace(*title),
			BaselineArtifactID:  baseline.ID,
			CandidateArtifactID: candidate.ID,
			MetadataJSON:        metadata,
		}
		if err := preserveExistingSkillOptReviewItemDetails(ctx, store, &item); err != nil {
			return err
		}
		return store.UpsertEvalReviewItem(ctx, item)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review item add: %v\n", err)
		return 1
	}
	writeLine(stdout, "added review item %s to %s", item.ItemID, item.RunID)
	return 0
}

func runSkillOptReviewStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt review status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "review run id")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt review status does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt review status requires --run")
		return 2
	}
	var status skillOptReviewStatus
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		status, err = loadSkillOptReviewStatus(context.Background(), store, artifact.NewStore(paths.ArtifactBlobs), *runID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt review status: %v\n", err)
		return 1
	}
	itemCount := len(status.Items)
	feedbackCount := len(status.Feedback) + len(status.RankedFeedback)
	fmt.Fprintf(stdout, "run: %s\n", status.Run.ID)
	fmt.Fprintf(stdout, "template: %s\n", status.Run.TemplateID)
	fmt.Fprintf(stdout, "template_version: %s\n", status.Run.TemplateVersionID)
	fmt.Fprintf(stdout, "repo: %s\n", status.Run.TargetRepo)
	fmt.Fprintf(stdout, "state: %s\n", status.Run.State)
	fmt.Fprintf(stdout, "mode: %s\n", status.Recommendation.CurrentMode)
	fmt.Fprintf(stdout, "exploration_level: %s\n", status.Recommendation.ExplorationLevel)
	fmt.Fprintf(stdout, "items: %d\n", itemCount)
	fmt.Fprintf(stdout, "feedback: %d\n", feedbackCount)
	fmt.Fprintf(stdout, "pairwise_preferences: %d\n", len(status.PairwisePreferences))
	fmt.Fprintf(stdout, "ranking_stability: %s\n", status.Recommendation.RankingStability)
	fmt.Fprintf(stdout, "recommended_next_mode: %s\n", status.Recommendation.RecommendedMode)
	fmt.Fprintf(stdout, "recommendation: %s\n", status.Recommendation.Summary())
	fmt.Fprintf(stdout, "packet_blockers: %d\n", len(status.PacketBlockers))
	fmt.Fprintf(stdout, "training_blockers: %d\n", len(status.TrainingBlockers))
	fmt.Fprintf(stdout, "ready_for_packet: %t\n", status.PacketReady)
	fmt.Fprintf(stdout, "ready_for_training: %t\n", status.TrainingReady)
	for _, blocker := range status.PacketBlockers {
		fmt.Fprintf(stdout, "packet_blocker: %s\n", blocker)
	}
	for _, blocker := range status.TrainingBlockers {
		fmt.Fprintf(stdout, "training_blocker: %s\n", blocker)
	}
	return 0
}

func preserveExistingSkillOptReviewItemDetails(ctx context.Context, store *db.Store, item *db.EvalReviewItem) error {
	if store == nil || item == nil {
		return nil
	}
	if strings.TrimSpace(item.Title) != "" && strings.TrimSpace(item.MetadataJSON) != "" {
		return nil
	}
	items, err := store.ListEvalReviewItems(ctx, item.RunID)
	if err != nil {
		return err
	}
	for _, existing := range items {
		if strings.TrimSpace(existing.ItemID) != strings.TrimSpace(item.ItemID) {
			continue
		}
		if strings.TrimSpace(item.Title) == "" {
			item.Title = existing.Title
		}
		if strings.TrimSpace(item.MetadataJSON) == "" {
			item.MetadataJSON = existing.MetadataJSON
		}
		return nil
	}
	return nil
}

type skillOptReviewStatus struct {
	Run                 db.EvalRun
	Items               []db.EvalReviewItem
	Feedback            []db.FeedbackEvent
	RankedFeedback      []db.RankedFeedbackEvent
	PairwisePreferences []db.PairwisePreference
	Recommendation      skillopt.PhaseRecommendation
	PacketBlockers      []string
	TrainingBlockers    []string
	PacketReady         bool
	TrainingReady       bool
}

func loadSkillOptReviewStatus(ctx context.Context, store *db.Store, blobStore artifact.Store, runID string) (skillOptReviewStatus, error) {
	run, err := store.GetEvalRun(ctx, strings.TrimSpace(runID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillOptReviewStatus{}, fmt.Errorf("review run %s not found", strings.TrimSpace(runID))
		}
		return skillOptReviewStatus{}, err
	}
	items, err := store.ListEvalReviewItems(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	events, err := store.ListFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	rankedEvents, err := store.ListRankedFeedbackEvents(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, run.ID)
	if err != nil {
		return skillOptReviewStatus{}, err
	}
	packetBlockers := reviewPacketBlockers(ctx, store, blobStore, run, items)
	trainingBlockers := reviewTrainingBlockers(ctx, store, run, items, events, rankedEvents)
	recommendation := skillopt.RecommendPhaseForItems(run, items, events, rankedEvents, pairwisePreferences)
	return skillOptReviewStatus{
		Run:                 run,
		Items:               items,
		Feedback:            events,
		RankedFeedback:      rankedEvents,
		PairwisePreferences: pairwisePreferences,
		Recommendation:      recommendation,
		PacketBlockers:      packetBlockers,
		TrainingBlockers:    trainingBlockers,
		PacketReady:         len(packetBlockers) == 0,
		TrainingReady:       len(packetBlockers) == 0 && len(trainingBlockers) == 0,
	}, nil
}

func reviewPacketBlockers(ctx context.Context, store *db.Store, blobStore artifact.Store, run db.EvalRun, items []db.EvalReviewItem) []string {
	if len(items) == 0 {
		return []string{"run has no review items"}
	}
	var blockers []string
	validated := map[string]struct{}{}
	for _, item := range items {
		itemID := strings.TrimSpace(item.ItemID)
		if itemID == "" {
			itemID = item.ID
		}
		if skillOptRunUsesRankedOptions(run) {
			options, err := store.ListEvalReviewOptions(ctx, run.ID, item.ItemID)
			if err != nil {
				blockers = append(blockers, fmt.Sprintf("item %s options are not readable: %v", itemID, err))
				continue
			}
			if len(options) == 0 {
				blockers = append(blockers, fmt.Sprintf("item %s has no registered options", itemID))
				continue
			}
			if run.OptionsCount > 0 && len(options) != run.OptionsCount {
				blockers = append(blockers, fmt.Sprintf("item %s has %d options, want %d", itemID, len(options), run.OptionsCount))
				continue
			}
			for _, option := range options {
				blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "option "+option.Label, option.ArtifactID, validated)...)
			}
			continue
		}
		baseline := strings.TrimSpace(item.BaselineArtifactID)
		candidate := strings.TrimSpace(item.CandidateArtifactID)
		if baseline == "" || candidate == "" {
			blockers = append(blockers, fmt.Sprintf("item %s is missing a baseline or candidate artifact", itemID))
			continue
		}
		if baseline == candidate {
			blockers = append(blockers, fmt.Sprintf("item %s uses the same artifact for baseline and candidate", itemID))
			continue
		}
		blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "baseline", baseline, validated)...)
		blockers = append(blockers, validateReviewArtifactBlob(ctx, store, blobStore, itemID, "candidate", candidate, validated)...)
	}
	return blockers
}

func skillOptRunUsesRankedOptions(run db.EvalRun) bool {
	return run.Mode != db.EvalRunModeValidate || run.OptionsCount > 2
}

func reviewTrainingBlockers(ctx context.Context, store *db.Store, run db.EvalRun, items []db.EvalReviewItem, events []db.FeedbackEvent, rankedEvents []db.RankedFeedbackEvent) []string {
	if len(items) == 0 {
		return []string{"run has no review items"}
	}
	var blockers []string
	feedbackByItem := map[string]int{}
	for _, event := range events {
		feedbackByItem[strings.TrimSpace(event.ItemID)]++
	}
	for _, event := range rankedEvents {
		feedbackByItem[strings.TrimSpace(event.ItemID)]++
	}
	for _, item := range items {
		itemID := strings.TrimSpace(item.ItemID)
		if itemID == "" {
			itemID = item.ID
		}
		if feedbackByItem[itemID] == 0 {
			blockers = append(blockers, fmt.Sprintf("item %s has no imported feedback", itemID))
		}
	}
	if _, err := skillopt.ExportTrainingPackage(ctx, store, run.ID); err != nil {
		blockers = append(blockers, fmt.Sprintf("training export failed: %v", err))
	}
	return blockers
}

func validateReviewArtifactBlob(ctx context.Context, store *db.Store, blobStore artifact.Store, itemID string, role string, artifactID string, validated map[string]struct{}) []string {
	if _, ok := validated[artifactID]; ok {
		return nil
	}
	validated[artifactID] = struct{}{}
	record, err := store.GetEvalArtifact(ctx, artifactID)
	if err != nil {
		return []string{fmt.Sprintf("item %s %s artifact %s is not registered: %v", itemID, role, artifactID, err)}
	}
	if _, err := blobStore.Read(record.Hash); err != nil {
		return []string{fmt.Sprintf("item %s %s artifact %s blob is not readable: %v", itemID, role, artifactID, err)}
	}
	return nil
}

func prepareReviewItemArtifact(blobStore artifact.Store, runID string, itemID string, role string, path string, mediaTypeOverride string, driver string) (db.EvalArtifact, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return db.EvalArtifact{}, fmt.Errorf("%s path is required", role)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("read %s file: %w", role, err)
	}
	mediaType, err := reviewArtifactMediaType(path, content, mediaTypeOverride)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("%s file: %w", role, err)
	}
	blob, err := blobStore.Put(content)
	if err != nil {
		return db.EvalArtifact{}, fmt.Errorf("store %s artifact blob: %w", role, err)
	}
	artifactRecord := db.EvalArtifact{
		ID:        reviewItemArtifactID(runID, itemID, role),
		Hash:      blob.Hash,
		MediaType: mediaType,
		SizeBytes: blob.Size,
		Driver:    strings.TrimSpace(driver),
	}
	if artifactRecord.Driver == "" {
		artifactRecord.Driver = "text"
	}
	return artifactRecord, nil
}

func reviewItemArtifactID(runID string, itemID string, role string) string {
	return strings.TrimSpace(runID) + "/" + strings.TrimSpace(itemID) + "/" + strings.TrimSpace(role)
}

func reviewArtifactMediaType(path string, content []byte, override string) (string, error) {
	if mediaType := strings.TrimSpace(override); mediaType != "" {
		return mediaType, nil
	}
	if !utf8.Valid(content) {
		return "", errors.New("binary content requires --media-type")
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown", nil
	case ".txt", ".text", ".diff", ".patch":
		return "text/plain", nil
	case ".csv":
		return "text/csv", nil
	case ".json":
		return "application/json", nil
	}
	return "text/plain", nil
}

func normalizeSkillOptMetadataJSON(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return "", fmt.Errorf("metadata-json: %w", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("metadata-json: %w", err)
	}
	return string(encoded), nil
}

func reviewOptionMetadataJSON(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	encoded, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		return "", fmt.Errorf("option metadata: %w", err)
	}
	return string(encoded), nil
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
	artifactDir := fs.String("artifact-dir", "", "directory containing candidate package artifacts")
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
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		version, err := skillopt.ImportCandidatePackageWithOptions(context.Background(), store, pkg, skillopt.CandidateImportOptions{
			SourcePath:  *file,
			ArtifactDir: *artifactDir,
			BlobStore:   artifact.NewStore(paths.ArtifactBlobs),
		})
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

func runSkillOptCandidate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runSkillOptCandidateList(args[1:], stdout, stderr)
	case "show":
		return runSkillOptCandidateShow(args[1:], stdout, stderr)
	case "promote":
		return runSkillOptCandidatePromote(args[1:], stdout, stderr)
	case "reject":
		return runSkillOptCandidateReject(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt candidate command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptCandidateList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id to filter")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt candidate list does not accept positional arguments")
		return 2
	}
	var versions []db.AgentTemplateVersion
	var reviews map[string]db.AgentTemplateCandidateReview
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		versions, err = store.ListPendingAgentTemplateVersions(context.Background(), *templateID)
		if err != nil {
			return err
		}
		reviews = make(map[string]db.AgentTemplateCandidateReview, len(versions))
		for _, version := range versions {
			review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
			if err == nil {
				reviews[version.ID] = review
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate list: %v\n", err)
		return 1
	}
	if len(versions) == 0 {
		writeLine(stdout, "no pending candidates")
		return 0
	}
	fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", "VERSION", "TEMPLATE", "STATE", "SCORE", "SUMMARY")
	for _, version := range versions {
		review := reviews[version.ID]
		fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", version.ID, version.TemplateID, version.State, scoreText(review.Score), firstLine(review.PreferenceSummary))
	}
	return 0
}

// runSkillOptJudgeReport renders an offline calibration report comparing the
// LLM judge's accept/reject signal against the human promote/reject decision
// captured at decision time (#345). It is read-only: it lists the stored
// judge-outcome rows and computes a confusion matrix, overall agreement,
// Cohen's kappa, soft-score calibration bands, and per-dimension disagreement.
func runSkillOptJudgeReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt judge-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id to filter")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt judge-report does not accept positional arguments")
		return 2
	}
	var outcomes []db.SkillOptJudgeOutcome
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		outcomes, err = store.ListSkillOptJudgeOutcomes(context.Background(), *templateID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt judge-report: %v\n", err)
		return 1
	}
	renderSkillOptJudgeReport(stdout, outcomes)
	return 0
}

func renderSkillOptJudgeReport(stdout io.Writer, outcomes []db.SkillOptJudgeOutcome) {
	if len(outcomes) == 0 {
		writeLine(stdout, "no judge outcomes captured")
		return
	}
	// Confusion matrix: count each of the four direction buckets.
	counts := map[string]int{
		db.SkillOptJudgeDirectionAgreeAccept:            0,
		db.SkillOptJudgeDirectionAgreeReject:            0,
		db.SkillOptJudgeDirectionJudgeAcceptHumanReject: 0,
		db.SkillOptJudgeDirectionJudgeRejectHumanAccept: 0,
	}
	for _, outcome := range outcomes {
		counts[outcome.Direction]++
	}
	total := len(outcomes)
	agreeAccept := counts[db.SkillOptJudgeDirectionAgreeAccept]
	agreeReject := counts[db.SkillOptJudgeDirectionAgreeReject]
	judgeAcceptHumanReject := counts[db.SkillOptJudgeDirectionJudgeAcceptHumanReject]
	judgeRejectHumanAccept := counts[db.SkillOptJudgeDirectionJudgeRejectHumanAccept]

	fmt.Fprintf(stdout, "judge outcomes: %d\n", total)
	writeLine(stdout, "")
	writeLine(stdout, "confusion matrix (judge vs human)")
	fmt.Fprintf(stdout, "  %-22s %-14s %-14s\n", "", "human promote", "human reject")
	fmt.Fprintf(stdout, "  %-22s %-14d %-14d\n", "judge accept", agreeAccept, judgeAcceptHumanReject)
	fmt.Fprintf(stdout, "  %-22s %-14d %-14d\n", "judge reject", judgeRejectHumanAccept, agreeReject)
	writeLine(stdout, "")

	// Overall agreement = (agree_accept + agree_reject) / total.
	agreements := agreeAccept + agreeReject
	fmt.Fprintf(stdout, "agreement rate: %.3f (%d/%d)\n", float64(agreements)/float64(total), agreements, total)

	// Cohen's kappa for the 2x2 judge-vs-human table:
	//   po = observed agreement; pe = chance agreement from the marginals.
	//   kappa = (po - pe) / (1 - pe). When pe == 1 (a rater used a single label
	//   for every row) kappa is undefined: report 1.000 only when observed
	//   agreement is also perfect, otherwise "n/a" rather than a misleading 1.0.
	po := float64(agreements) / float64(total)
	judgeAcceptTotal := agreeAccept + judgeAcceptHumanReject
	humanAcceptTotal := agreeAccept + judgeRejectHumanAccept
	judgeRejectTotal := total - judgeAcceptTotal
	humanRejectTotal := total - humanAcceptTotal
	pe := (float64(judgeAcceptTotal)*float64(humanAcceptTotal) + float64(judgeRejectTotal)*float64(humanRejectTotal)) / (float64(total) * float64(total))
	switch {
	case pe < 1:
		fmt.Fprintf(stdout, "cohen's kappa: %.3f\n", (po-pe)/(1-pe))
	case po >= 1:
		writeLine(stdout, "cohen's kappa: 1.000")
	default:
		writeLine(stdout, "cohen's kappa: n/a (degenerate: a rater used a single label)")
	}
	writeLine(stdout, "")

	renderSkillOptJudgeCalibration(stdout, outcomes)
	renderSkillOptJudgeDimensionDisagreement(stdout, outcomes)
}

// skillOptJudgeBands are the soft-score calibration bands, low edge inclusive.
var skillOptJudgeBands = []struct {
	label string
	low   float64
	high  float64
}{
	{"[0.00,0.25)", 0.00, 0.25},
	{"[0.25,0.50)", 0.25, 0.50},
	{"[0.50,0.75)", 0.50, 0.75},
	{"[0.75,1.00]", 0.75, 1.01},
}

// renderSkillOptJudgeCalibration buckets outcomes by the judge's soft score and
// reports the human promote rate within each band — a calibration curve showing
// whether higher judge confidence tracks more human promotions.
func renderSkillOptJudgeCalibration(stdout io.Writer, outcomes []db.SkillOptJudgeOutcome) {
	type bandStat struct {
		count    int
		promoted int
	}
	stats := make([]bandStat, len(skillOptJudgeBands))
	unscored := 0
	for _, outcome := range outcomes {
		soft, ok := skillOptJudgeSoftScore(outcome.JudgeScoreJSON)
		if !ok {
			unscored++
			continue
		}
		humanPromoted := outcome.HumanDecision == "promoted"
		// Clamp out-of-range soft scores into [0,1] so a malformed value lands in
		// an edge band rather than being silently dropped from the calibration.
		if soft < 0 {
			soft = 0
		}
		if soft > 1 {
			soft = 1
		}
		for index, band := range skillOptJudgeBands {
			if soft >= band.low && soft < band.high {
				stats[index].count++
				if humanPromoted {
					stats[index].promoted++
				}
				break
			}
		}
	}
	writeLine(stdout, "calibration (judge soft-score band vs human promote rate)")
	fmt.Fprintf(stdout, "  %-14s %-8s %s\n", "BAND", "N", "HUMAN PROMOTE RATE")
	for index, band := range skillOptJudgeBands {
		stat := stats[index]
		rate := "-"
		if stat.count > 0 {
			rate = fmt.Sprintf("%.3f (%d/%d)", float64(stat.promoted)/float64(stat.count), stat.promoted, stat.count)
		}
		fmt.Fprintf(stdout, "  %-14s %-8d %s\n", band.label, stat.count, rate)
	}
	if unscored > 0 {
		fmt.Fprintf(stdout, "  %-14s %-8d %s\n", "no soft score", unscored, "-")
	}
	writeLine(stdout, "")
}

// renderSkillOptJudgeDimensionDisagreement, when dimension_scores are present in
// the captured eval reports, reports each dimension's mean score split by human
// promote vs reject, plus the gap between them — surfacing dimensions where the
// judge's per-dimension score diverges from the human decision.
func renderSkillOptJudgeDimensionDisagreement(stdout io.Writer, outcomes []db.SkillOptJudgeOutcome) {
	type dimStat struct {
		promoteSum   float64
		promoteCount int
		rejectSum    float64
		rejectCount  int
	}
	stats := map[string]*dimStat{}
	for _, outcome := range outcomes {
		dimensions := skillOptJudgeDimensionScores(outcome.JudgeScoreJSON)
		humanPromoted := outcome.HumanDecision == "promoted"
		for name, score := range dimensions {
			stat := stats[name]
			if stat == nil {
				stat = &dimStat{}
				stats[name] = stat
			}
			if humanPromoted {
				stat.promoteSum += score
				stat.promoteCount++
			} else {
				stat.rejectSum += score
				stat.rejectCount++
			}
		}
	}
	if len(stats) == 0 {
		return
	}
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	writeLine(stdout, "per-dimension disagreement (mean judge dimension score by human decision)")
	fmt.Fprintf(stdout, "  %-22s %-16s %-16s %s\n", "DIMENSION", "PROMOTE MEAN", "REJECT MEAN", "GAP")
	for _, name := range names {
		stat := stats[name]
		promoteMean, promoteOK := skillOptJudgeMean(stat.promoteSum, stat.promoteCount)
		rejectMean, rejectOK := skillOptJudgeMean(stat.rejectSum, stat.rejectCount)
		gap := "-"
		if promoteOK && rejectOK {
			gap = fmt.Sprintf("%+.3f", promoteMean-rejectMean)
		}
		fmt.Fprintf(stdout, "  %-22s %-16s %-16s %s\n", name, skillOptJudgeMeanText(promoteMean, promoteOK, stat.promoteCount), skillOptJudgeMeanText(rejectMean, rejectOK, stat.rejectCount), gap)
	}
	writeLine(stdout, "")
}

func skillOptJudgeMean(sum float64, count int) (float64, bool) {
	if count == 0 {
		return 0, false
	}
	return sum / float64(count), true
}

func skillOptJudgeMeanText(mean float64, ok bool, count int) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.3f (n=%d)", mean, count)
}

// skillOptJudgeReportRoot decodes a captured judge eval report and returns the
// candidate sources to inspect: the report root plus any nested evaluator_score
// object (mirroring how the report is read at capture time).
func skillOptJudgeReportRoot(judgeScoreJSON string) []map[string]any {
	judgeScoreJSON = strings.TrimSpace(judgeScoreJSON)
	if judgeScoreJSON == "" {
		return nil
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(judgeScoreJSON), &report); err != nil {
		return nil
	}
	sources := []map[string]any{report}
	if nested := decodedSkillOptMetadataValue(report["evaluator_score"]); len(nested) > 0 {
		sources = append(sources, nested)
	}
	return sources
}

// skillOptJudgeSoftScore extracts the continuous judge score used for the
// calibration curve. It mirrors the soft-score fallback chain of the verdict
// heuristic (skillOptJudgeAcceptFromReport in internal/db/store.go): "soft",
// then the landing-page profile's "best_selection_soft"/"best_selection_hard".
// Keeping the two in lockstep means every report that produced an accept/reject
// verdict is also banded in calibration — otherwise landing-page reports (which
// carry best_selection_soft, not a top-level "soft") would be silently dropped
// into "no soft score", blinding calibration to the dominant real report shape.
func skillOptJudgeSoftScore(judgeScoreJSON string) (float64, bool) {
	for _, source := range skillOptJudgeReportRoot(judgeScoreJSON) {
		for _, key := range []string{"soft", "best_selection_soft", "best_selection_hard"} {
			if value, ok := skillOptJudgeFloatValue(source[key]); ok {
				return value, true
			}
		}
	}
	return 0, false
}

func skillOptJudgeDimensionScores(judgeScoreJSON string) map[string]float64 {
	for _, source := range skillOptJudgeReportRoot(judgeScoreJSON) {
		raw := decodedSkillOptMetadataValue(source["dimension_scores"])
		if len(raw) == 0 {
			continue
		}
		scores := make(map[string]float64, len(raw))
		for name, value := range raw {
			if score, ok := skillOptJudgeFloatValue(value); ok {
				scores[name] = score
			}
		}
		if len(scores) > 0 {
			return scores
		}
	}
	return nil
}

func skillOptJudgeFloatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func runSkillOptCandidateShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate show requires exactly one version id")
		return 2
	}
	versionID := fs.Arg(0)
	var version db.AgentTemplateVersion
	var review db.AgentTemplateCandidateReview
	var hasReview bool
	var base db.AgentTemplate
	var hasBase bool
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		version, err = store.GetAgentTemplateVersionByID(context.Background(), versionID)
		if err != nil {
			return err
		}
		review, err = store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
		if err == nil {
			hasReview = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		baseRef := strings.TrimSpace(review.BaseVersionID)
		if baseRef == "" {
			current, err := store.GetAgentTemplate(context.Background(), version.TemplateID)
			if err != nil {
				return err
			}
			baseRef = current.VersionID
		}
		if baseRef != "" && baseRef != version.ID {
			base, err = store.GetAgentTemplateReference(context.Background(), baseRef)
			if err == nil {
				hasBase = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate show: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "version: %s\n", version.ID)
	fmt.Fprintf(stdout, "template: %s\n", version.TemplateID)
	fmt.Fprintf(stdout, "state: %s\n", version.State)
	fmt.Fprintf(stdout, "source: %s@%s:%s\n", version.SourceRepo, version.SourceRef, version.SourcePath)
	fmt.Fprintf(stdout, "content_hash: %s\n", version.ContentHash)
	if hasReview {
		fmt.Fprintf(stdout, "base_version: %s\n", emptyText(review.BaseVersionID))
		fmt.Fprintf(stdout, "score: %s\n", scoreText(review.Score))
		fmt.Fprintf(stdout, "preference_summary: %s\n", emptyText(review.PreferenceSummary))
		fmt.Fprintf(stdout, "diff_artifact: %s\n", emptyText(review.DiffArtifactID))
		if strings.TrimSpace(review.EvalReportJSON) != "" {
			fmt.Fprintf(stdout, "eval_report:\n%s\n", indentJSON(review.EvalReportJSON))
		}
		if strings.TrimSpace(review.DecisionReason) != "" {
			fmt.Fprintf(stdout, "decision_reason: %s\n", review.DecisionReason)
		}
	}
	if hasBase {
		diff := artifact.TextDriver{}.Diff(base.VersionID+".md", version.ID+".md", []byte(base.Content), []byte(version.Content))
		fmt.Fprintf(stdout, "content_diff:\n%s", diff)
	}
	return 0
}

func runSkillOptCandidatePromote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate promote requires exactly one version id")
		return 2
	}
	var promoted db.AgentTemplateVersion
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		promoted, err = store.PromoteAgentTemplateVersion(context.Background(), fs.Arg(0))
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate promote: %v\n", err)
		return 1
	}
	writeLine(stdout, "promoted candidate %s", promoted.ID)
	return 0
}

func runSkillOptJudge(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptJudgeUsage(stdout)
		return 0
	}
	switch args[0] {
	case "promote":
		return runSkillOptJudgePromote(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt judge command %q\n\n", args[0])
		printSkillOptJudgeUsage(stderr)
		return 2
	}
}

func printSkillOptJudgeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot skillopt judge promote --template <id> --task-kind <kind> --file <pkg.json> [--home <h>] [--yes] [--json]")
}

// skillOptJudgePromoteResult is the machine-readable preview/apply summary for
// `skillopt judge promote`. Applied is false in preview mode (no --yes).
type skillOptJudgePromoteResult struct {
	TemplateID            string  `json:"template_id"`
	TaskKind              string  `json:"task_kind"`
	Applied               bool    `json:"applied"`
	Accepted              bool    `json:"accepted"`
	BaselineAgreement     float64 `json:"baseline_agreement"`
	BestAgreement         float64 `json:"best_agreement"`
	AgreementDelta        float64 `json:"agreement_delta"`
	BestOrigin            string  `json:"best_origin,omitempty"`
	PreviousPromptVersion string  `json:"previous_judge_prompt_version,omitempty"`
	NewPromptVersion      string  `json:"new_judge_prompt_version,omitempty"`
	PromptBytes           int     `json:"prompt_bytes"`
	PromptPreview         string  `json:"prompt_preview,omitempty"`
}

const skillOptJudgePromptPreviewLimit = 800

func runSkillOptJudgePromote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt judge promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "agent template id to promote the judge prompt into")
	taskKind := fs.String("task-kind", "", "task kind variant to promote (use _global for the all-items pass)")
	file := fs.String("file", "", "judge candidate package JSON file")
	yes := fs.Bool("yes", false, "apply the promotion; without it the command previews and writes nothing")
	jsonOutput := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt judge promote does not accept positional arguments")
		return 2
	}
	templateRef := strings.TrimSpace(*templateID)
	if templateRef == "" {
		fmt.Fprintln(stderr, "skillopt judge promote requires --template")
		return 2
	}
	kind := strings.TrimSpace(*taskKind)
	if kind == "" {
		fmt.Fprintln(stderr, "skillopt judge promote requires --task-kind")
		return 2
	}
	packagePath := strings.TrimSpace(*file)
	if packagePath == "" {
		fmt.Fprintln(stderr, "skillopt judge promote requires --file")
		return 2
	}
	data, err := os.ReadFile(packagePath)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt judge promote: read package: %v\n", err)
		return 2
	}
	pkg, err := skillopt.ParseJudgeCandidatePackage(data)
	if err != nil {
		fmt.Fprintf(stderr, "skillopt judge promote: %v\n", err)
		return 2
	}
	variant, ok := pkg.Variants[kind]
	if !ok {
		fmt.Fprintf(stderr, "skillopt judge promote: task-kind %q not found in package variants\n", kind)
		return 2
	}
	if !variant.Accepted {
		fmt.Fprintf(stderr, "skillopt judge promote: variant %q was not accepted by the judge optimizer (best_origin=%q); refusing to promote\n", kind, variant.BestOrigin)
		return 1
	}
	if strings.TrimSpace(variant.BestPrompt) == "" {
		fmt.Fprintf(stderr, "skillopt judge promote: variant %q has an empty best_prompt; nothing to promote\n", kind)
		return 1
	}

	result := skillOptJudgePromoteResult{
		TemplateID:        templateRef,
		TaskKind:          kind,
		Accepted:          variant.Accepted,
		BaselineAgreement: variant.BaselineAgreement,
		BestAgreement:     variant.BestAgreement,
		AgreementDelta:    variant.BestAgreement - variant.BaselineAgreement,
		BestOrigin:        variant.BestOrigin,
		NewPromptVersion:  strings.TrimSpace(variant.JudgePromptVersion),
		PromptBytes:       len(variant.BestPrompt),
		PromptPreview:     truncateSkillOptJudgePrompt(variant.BestPrompt),
	}

	// Preview by default; only --yes writes.
	openStore := withReadOnlyStore
	if *yes {
		openStore = withStore
	}
	if err := openStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		template, err := loadInstalledTemplate(ctx, store, templateRef)
		if err != nil {
			return err
		}
		metadata, err := agenttemplate.UnmarshalMetadata(template.MetadataJSON)
		if err != nil {
			return fmt.Errorf("decode template metadata: %w", err)
		}
		result.PreviousPromptVersion = strings.TrimSpace(metadata.Evaluation["judge_prompt_version"])
		updatedMetadata, err := applyJudgePromptToMetadata(metadata, kind, variant)
		if err != nil {
			return err
		}
		if !*yes {
			return nil
		}
		metadataJSON, err := agenttemplate.MarshalMetadata(updatedMetadata)
		if err != nil {
			return fmt.Errorf("encode template metadata: %w", err)
		}
		if _, err := store.UpdateAgentTemplateMetadata(ctx, template.ID, metadataJSON); err != nil {
			return err
		}
		result.Applied = true
		reason := fmt.Sprintf("judge prompt promoted for task_kind=%s: agreement %.3f→%.3f (delta %+.3f), origin=%s, version %s→%s",
			kind, variant.BaselineAgreement, variant.BestAgreement, result.AgreementDelta, variant.BestOrigin,
			emptyToDash(result.PreviousPromptVersion), emptyToDash(result.NewPromptVersion))
		outcome := db.SkillOptJudgeOutcome{
			CandidateVersionID: judgeOutcomeCandidateVersionID(template),
			TemplateID:         template.ID,
			JudgePromptVersion: result.NewPromptVersion,
			HumanDecision:      "promoted",
			// The human promoted and the judge optimizer already accepted this
			// variant (the accepted-gate above), so the decision agrees with the
			// judge's signal.
			Direction: db.SkillOptJudgeDirectionAgreeAccept,
			Reason:    reason,
		}
		if err := store.InsertSkillOptJudgeOutcome(ctx, outcome); err != nil {
			return fmt.Errorf("record judge outcome: %w", err)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt judge promote: %v\n", err)
		return 1
	}

	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "skillopt judge promote: %v\n", err)
			return 1
		}
		return 0
	}
	printSkillOptJudgePromoteResult(stdout, result)
	return 0
}

// applyJudgePromptToMetadata folds an accepted judge variant into a template's
// flat Evaluation map: judge_prompt_templates is stored as a JSON-encoded
// map[task_kind]string (merging so sibling task kinds are preserved), and
// judge_prompt_version records the variant's version. The encoding round-trips
// through EvaluationConfigForReader → judgePromptConfigFromConfig.
func applyJudgePromptToMetadata(metadata agenttemplate.Metadata, taskKind string, variant skillopt.JudgeCandidateVariant) (agenttemplate.Metadata, error) {
	templates := map[string]string{}
	if existing := strings.TrimSpace(metadata.Evaluation["judge_prompt_templates"]); existing != "" {
		if err := json.Unmarshal([]byte(existing), &templates); err != nil {
			// Existing value is not a JSON object map; start fresh rather than
			// silently dropping the new prompt.
			templates = map[string]string{}
		}
	}
	templates[taskKind] = variant.BestPrompt
	encoded, err := json.Marshal(templates)
	if err != nil {
		return agenttemplate.Metadata{}, fmt.Errorf("encode judge prompt templates: %w", err)
	}
	if metadata.Evaluation == nil {
		metadata.Evaluation = map[string]string{}
	}
	metadata.Evaluation["judge_prompt_templates"] = string(encoded)
	if version := strings.TrimSpace(variant.JudgePromptVersion); version != "" {
		metadata.Evaluation["judge_prompt_version"] = version
	}
	return metadata, nil
}

// judgeOutcomeCandidateVersionID picks a non-empty candidate_version_id for the
// audit row (the column is NOT NULL): the template's current version when known,
// otherwise the template id itself, since judge-prompt promotion targets the
// template rather than a specific candidate version.
func judgeOutcomeCandidateVersionID(template db.AgentTemplate) string {
	if id := strings.TrimSpace(template.VersionID); id != "" {
		return id
	}
	return template.ID
}

func truncateSkillOptJudgePrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if utf8.RuneCountInString(prompt) <= skillOptJudgePromptPreviewLimit {
		return prompt
	}
	runes := []rune(prompt)
	return string(runes[:skillOptJudgePromptPreviewLimit]) + "…"
}

func emptyToDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func printSkillOptJudgePromoteResult(w io.Writer, result skillOptJudgePromoteResult) {
	writeLine(w, "template: %s", result.TemplateID)
	writeLine(w, "task_kind: %s", result.TaskKind)
	writeLine(w, "accepted: %t", result.Accepted)
	writeLine(w, "agreement: %.3f → %.3f (delta %+.3f)", result.BaselineAgreement, result.BestAgreement, result.AgreementDelta)
	if result.BestOrigin != "" {
		writeLine(w, "best_origin: %s", result.BestOrigin)
	}
	writeLine(w, "judge_prompt_version: %s → %s", emptyToDash(result.PreviousPromptVersion), emptyToDash(result.NewPromptVersion))
	writeLine(w, "prompt_bytes: %d", result.PromptBytes)
	if result.PromptPreview != "" {
		writeLine(w, "prompt_preview:")
		writeLine(w, "%s", result.PromptPreview)
	}
	if result.Applied {
		writeLine(w, "applied: wrote judge prompt into template %s", result.TemplateID)
	} else {
		writeLine(w, "preview only: nothing was written. Re-run with --yes to apply.")
	}
}

func runSkillOptCandidateReject(args []string, stdout, stderr io.Writer) int {
	parsed, help, ok := parseSkillOptCandidateRejectArgs(args, stderr)
	if help {
		printSkillOptUsage(stdout)
		return 0
	}
	if !ok {
		return 2
	}
	if parsed.versionID == "" {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	if parsed.extraVersion {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	var rejected db.AgentTemplateVersion
	if err := withStore(parsed.home, func(store *db.Store) error {
		var err error
		rejected, err = store.RejectAgentTemplateVersion(context.Background(), parsed.versionID, parsed.reason)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate reject: %v\n", err)
		return 1
	}
	writeLine(stdout, "rejected candidate %s", rejected.ID)
	return 0
}

type skillOptCandidateRejectArgs struct {
	home         string
	reason       string
	versionID    string
	extraVersion bool
}

func parseSkillOptCandidateRejectArgs(args []string, stderr io.Writer) (skillOptCandidateRejectArgs, bool, bool) {
	var parsed skillOptCandidateRejectArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			return parsed, true, true
		case arg == "--home" || arg == "--reason":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "skillopt candidate reject: %s requires a value\n", arg)
				return parsed, false, false
			}
			i++
			if arg == "--home" {
				parsed.home = args[i]
			} else {
				parsed.reason = args[i]
			}
		case strings.HasPrefix(arg, "--home="):
			parsed.home = strings.TrimPrefix(arg, "--home=")
		case strings.HasPrefix(arg, "--reason="):
			parsed.reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "skillopt candidate reject: unknown flag %s\n", arg)
			return parsed, false, false
		case parsed.versionID == "":
			parsed.versionID = arg
		default:
			parsed.extraVersion = true
		}
	}
	return parsed, false, true
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

func runSkillOptFeedback(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	if args[0] != "markdown" && args[0] != "github" {
		fmt.Fprintf(stderr, "unknown skillopt feedback collector %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
	if len(args) < 2 {
		fmt.Fprintf(stderr, "skillopt feedback %s requires a subcommand\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
	if args[0] == "markdown" {
		switch args[1] {
		case "export":
			return runSkillOptFeedbackMarkdownExport(args[2:], stdout, stderr)
		case "import":
			return runSkillOptFeedbackMarkdownImport(args[2:], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "unknown skillopt feedback markdown command %q\n\n", args[1])
			printSkillOptUsage(stderr)
			return 2
		}
	}
	switch args[1] {
	case "publish":
		return runSkillOptFeedbackGitHubPublish(args[2:], stdout, stderr)
	case "sync":
		return runSkillOptFeedbackGitHubSync(args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt feedback github command %q\n\n", args[1])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptFeedbackMarkdownExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback markdown export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	output := fs.String("output", "", "packet output directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback markdown export does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" || strings.TrimSpace(*output) == "" {
		fmt.Fprintln(stderr, "skillopt feedback markdown export requires --run and --output")
		return 2
	}
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		collector := feedback.MarkdownCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
		return collector.WritePacket(context.Background(), store, *runID, *output)
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback markdown export: %v\n", err)
		return 1
	}
	writeLine(stdout, "wrote markdown feedback packet for %s to %s", *runID, *output)
	return 0
}

func runSkillOptFeedbackMarkdownImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback markdown import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	packet := fs.String("packet", "", "packet directory containing feedback.yml")
	reviewer := fs.String("reviewer", "", "reviewer name override")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback markdown import does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*packet) == "" {
		fmt.Fprintln(stderr, "skillopt feedback markdown import requires --packet")
		return 2
	}
	var count int
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		collector := feedback.MarkdownCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
		result, err := collector.ImportPacket(context.Background(), store, *packet, *reviewer)
		if err != nil {
			return err
		}
		count = result.Count()
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback markdown import: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported %d feedback events", count)
	return 0
}

func runSkillOptFeedbackGitHubPublish(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback github publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	repoFlag := fs.String("repo", "", "GitHub repository owner/repo")
	pullRequest := fs.Int64("pr", 0, "existing pull request number to comment on instead of creating an issue")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback github publish does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt feedback github publish requires --run")
		return 2
	}
	var result feedback.GitHubPublishResult
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		run, err := store.GetEvalRun(context.Background(), strings.TrimSpace(*runID))
		if err != nil {
			return err
		}
		repo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, *repoFlag)
		if err != nil {
			return err
		}
		client := newSkillOptGitHubClient()
		if err := client.Preflight(context.Background(), repo); err != nil {
			return err
		}
		collector := feedback.GitHubCollector{
			BlobStore: artifact.NewStore(paths.ArtifactBlobs),
			GitHub:    client,
		}
		result, err = collector.Publish(context.Background(), store, run.ID, feedback.GitHubPublishTarget{
			Repo:        repo,
			PullRequest: *pullRequest,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback github publish: %v\n", err)
		return 1
	}
	writeLine(stdout, "published github feedback %s for %s to %s#%d: %s", result.Mode, strings.TrimSpace(*runID), result.Repo.FullName(), result.IssueNumber, result.URL)
	return 0
}

func runSkillOptFeedbackGitHubSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt feedback github sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runID := fs.String("run", "", "eval run id")
	repoFlag := fs.String("repo", "", "GitHub repository owner/repo")
	issueNumber := fs.Int64("issue", 0, "GitHub issue number containing feedback comments")
	pullRequest := fs.Int64("pr", 0, "GitHub pull request number containing feedback comments")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "skillopt feedback github sync requires --run")
		return 2
	}
	if *issueNumber > 0 && *pullRequest > 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync accepts only one of --issue or --pr")
		return 2
	}
	targetNumber := *issueNumber
	if targetNumber == 0 {
		targetNumber = *pullRequest
	}
	if targetNumber <= 0 {
		fmt.Fprintln(stderr, "skillopt feedback github sync requires --issue or --pr")
		return 2
	}
	var result feedback.ImportResult
	if err := withSkillOptStore(*home, func(paths config.Paths, store *db.Store) error {
		run, err := store.GetEvalRun(context.Background(), strings.TrimSpace(*runID))
		if err != nil {
			return err
		}
		repo, err := resolveSkillOptFeedbackRepo(context.Background(), paths, store, run, *repoFlag)
		if err != nil {
			return err
		}
		client := newSkillOptGitHubClient()
		if err := client.Preflight(context.Background(), repo); err != nil {
			return err
		}
		collector := feedback.GitHubCollector{
			BlobStore: artifact.NewStore(paths.ArtifactBlobs),
			GitHub:    client,
		}
		result, err = collector.Sync(context.Background(), store, run.ID, repo, targetNumber)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt feedback github sync: %v\n", err)
		return 1
	}
	writeLine(stdout, "imported %d github feedback events", result.Count())
	for _, diagnostic := range result.Diagnostics {
		writeLine(stdout, "github_feedback_diagnostic: %s", diagnostic)
	}
	return 0
}

func resolveSkillOptFeedbackRepo(ctx context.Context, paths config.Paths, store *db.Store, run db.EvalRun, repoFlag string) (github.Repository, error) {
	if strings.TrimSpace(repoFlag) != "" {
		requested, err := daemon.ParseRepository(repoFlag)
		if err != nil {
			return github.Repository{}, err
		}
		if expected, ok, err := resolveSkillOptTrainFeedbackRepo(ctx, store, run); err != nil {
			return github.Repository{}, err
		} else if ok && expected.FullName() != "" && !strings.EqualFold(requested.FullName(), expected.FullName()) {
			return github.Repository{}, fmt.Errorf("train run %s expects github feedback repo %s; got %s", run.ID, expected.FullName(), requested.FullName())
		}
		return requested, nil
	}
	if expected, ok, err := resolveSkillOptTrainFeedbackRepo(ctx, store, run); err != nil {
		return github.Repository{}, err
	} else if ok && expected.FullName() != "" {
		return expected, nil
	}
	if expectedRepo := skillOptMetadataString(run.MetadataJSON, "review", "expected_repo"); expectedRepo != "" {
		if repo, err := daemon.ParseRepository(expectedRepo); err == nil {
			return repo, nil
		}
	}
	if strings.TrimSpace(run.TargetRepo) != "" {
		if repo, err := daemon.ParseRepository(run.TargetRepo); err == nil {
			return repo, nil
		}
	}
	templateRef := strings.TrimSpace(run.TemplateVersionID)
	if templateRef == "" {
		templateRef = strings.TrimSpace(run.TemplateID)
	}
	if templateRef != "" {
		template, err := store.GetAgentTemplateReference(ctx, templateRef)
		if err == nil && strings.TrimSpace(template.SourceRepo) != "" {
			if repo, err := daemon.ParseRepository(template.SourceRepo); err == nil {
				return repo, nil
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return github.Repository{}, err
		}
	}
	defaultRepo, err := config.LoadDefaultFeedbackRepo(paths)
	if err != nil {
		return github.Repository{}, err
	}
	if strings.TrimSpace(defaultRepo) != "" {
		return daemon.ParseRepository(defaultRepo)
	}
	return github.Repository{}, errors.New("skillopt feedback github requires --repo because no target repo, template source repo, or [feedback].repo default is configured")
}

func resolveSkillOptTrainFeedbackRepo(ctx context.Context, store *db.Store, run db.EvalRun) (github.Repository, bool, error) {
	iteration, err := store.GetSkillOptTrainIterationByEvalRun(ctx, run.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return github.Repository{}, false, nil
	}
	if err != nil {
		return github.Repository{}, true, err
	}
	session, err := store.GetSkillOptTrainSession(ctx, iteration.SessionID)
	if err != nil {
		return github.Repository{}, true, err
	}
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	expectedRepo := strings.TrimSpace(policy.ExpectedReviewRepo)
	if expectedRepo == "" {
		return github.Repository{}, true, nil
	}
	repo, err := daemon.ParseRepository(expectedRepo)
	if err != nil {
		return github.Repository{}, true, fmt.Errorf("train expected review repo: %w", err)
	}
	return repo, true, nil
}

func scoreText(score *float64) string {
	if score == nil {
		return "-"
	}
	return fmt.Sprintf("%.4g", *score)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if before, _, ok := strings.Cut(value, "\n"); ok {
		return strings.TrimSpace(before)
	}
	return value
}

func emptyText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func indentJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return value
	}
	encoded, err := json.MarshalIndent(decoded, "  ", "  ")
	if err != nil {
		return value
	}
	return string(encoded)
}

func withSkillOptStore(home string, fn func(config.Paths, *db.Store) error) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	if err := config.Initialize(paths); err != nil {
		return err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	return fn(paths, store)
}
