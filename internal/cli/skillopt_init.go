package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cli/style"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

var skillOptTrainInitInteractive = func() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// skillOptTrainInitStdin is the reader the interactive train-init wizard reads
// answers from. It is a function so tests can substitute a scripted stdin.
var skillOptTrainInitStdin = func() io.Reader { return os.Stdin }

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
