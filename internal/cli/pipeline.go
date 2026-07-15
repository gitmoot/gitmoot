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
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// runPipeline is the CLI for the #681 pipeline registry: define, inspect, and
// toggle declarative pipelines. run/resume/cancel and the per-run funnel view
// arrive in later steps and are deliberately not registered here (only what
// already works is exposed). The mux mirrors runAgentHeartbeat's shape.
func runPipeline(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runPipelineAdd(args[1:], stdout, stderr)
	case "export":
		return runPipelineExport(args[1:], stdout, stderr)
	case "import":
		return runPipelineImport(args[1:], stdout, stderr)
	case "install-defaults":
		return runPipelineInstallDefaults(args[1:], stdout, stderr)
	case "list":
		return runPipelineList(args[1:], stdout, stderr)
	case "run":
		return runPipelineRunCmd(args[1:], stdout, stderr)
	case "show":
		return runPipelineShow(args[1:], stdout, stderr)
	case "bind-trigger":
		return runPipelineBindTrigger(args[1:], stdout, stderr)
	case "resume":
		return runPipelineResume(args[1:], stdout, stderr)
	case "cancel":
		return runPipelineCancel(args[1:], stdout, stderr)
	case "enable":
		return runPipelineSetEnabled(args[1:], true, stdout, stderr)
	case "disable":
		return runPipelineSetEnabled(args[1:], false, stdout, stderr)
	case "remove":
		return runPipelineRemove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown pipeline command %q\n\n", args[0])
		printPipelineUsage(stderr)
		return 2
	}
}

func printPipelineUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot pipeline add <spec.yaml> [--enable]")
	fmt.Fprintln(w, "  gitmoot pipeline export <name> --output <dir>")
	fmt.Fprintln(w, "  gitmoot pipeline import <dir> --repo owner/name [--name <newname>] [--agent-map exported=local ...] [--force] [--enable]")
	fmt.Fprintln(w, "  gitmoot pipeline install-defaults")
	fmt.Fprintln(w, "  gitmoot pipeline list [--json]")
	fmt.Fprintln(w, "  gitmoot pipeline run <name>")
	fmt.Fprintln(w, "  gitmoot pipeline show <name|run-id> [--json]")
	fmt.Fprintln(w, "  gitmoot pipeline bind-trigger <name>")
	fmt.Fprintln(w, "  gitmoot pipeline resume <run-id> [--from <stage>]")
	fmt.Fprintln(w, "  gitmoot pipeline cancel <run-id>")
	fmt.Fprintln(w, "  gitmoot pipeline enable <name>")
	fmt.Fprintln(w, "  gitmoot pipeline disable <name>")
	fmt.Fprintln(w, "  gitmoot pipeline remove <name>")
}

func runPipelineInstallDefaults(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline install-defaults", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline install-defaults does not accept positional arguments")
		return 2
	}
	var result defaultPipelineInstallResult
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		result, err = installDefaultMemoryPipelines(context.Background(), store, paths, *home)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline install-defaults: %v\n", err)
		return 1
	}
	for _, name := range result.Installed {
		status := "manual-only"
		if rec, ok, err := loadPipelineForInstallStatus(*home, name); err == nil && ok {
			status = installDefaultsEnabledLabel(rec.Enabled)
		}
		writeLine(stdout, "installed default pipeline %s (%s)", name, status)
	}
	for _, name := range result.Skipped {
		writeLine(stdout, "skipped existing default pipeline %s", name)
	}
	return 0
}

func loadPipelineForInstallStatus(home string, name string) (db.Pipeline, bool, error) {
	var rec db.Pipeline
	var ok bool
	err := withStore(home, func(store *db.Store) error {
		var err error
		rec, ok, err = store.GetPipeline(context.Background(), name)
		return err
	})
	return rec, ok, err
}

// pipelineRunnerAgentName derives the hidden shell runner agent name for a
// pipeline. It carries the recognizable "pipeline-...-runner" marker so
// isPipelineRunnerAgentName can hide it from `agent list`, mirroring the
// ephemeral-agent naming precedent in workflow/result.go. The pipeline name is a
// validated name-safe token, so no slugging is needed.
func pipelineRunnerAgentName(pipelineName string) string {
	return "pipeline-" + strings.TrimSpace(pipelineName) + "-runner"
}

// isPipelineRunnerAgentName reports whether an agent name is a pipeline's hidden
// shell runner (#681). Such agents are an implementation detail of the pipeline
// executor and are filtered out of the default `agent list` output, mirroring the
// isEphemeralAgentName filter.
func isPipelineRunnerAgentName(name string) bool {
	return strings.HasPrefix(name, "pipeline-") && strings.HasSuffix(name, "-runner") &&
		len(name) > len("pipeline--runner")
}

func runPipelineAdd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline add requires a spec file")
			return 2
		}
		return 0
	}
	specFile := strings.TrimSpace(args[0])
	fs := flag.NewFlagSet("pipeline add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	enable := fs.Bool("enable", false, "enable the pipeline immediately (default disabled)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline add accepts exactly one spec file")
		return 2
	}
	raw, err := os.ReadFile(specFile)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline add: read spec: %v\n", err)
		return 1
	}
	spec, err := pipeline.Load(raw)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline add: %v\n", err)
		return 2
	}
	repo := ""
	if strings.TrimSpace(spec.Repo) != "" {
		parsed, err := daemon.ParseRepository(spec.Repo)
		if err != nil {
			fmt.Fprintf(stderr, "pipeline add: invalid repo %q: %v\n", spec.Repo, err)
			return 2
		}
		repo = parsed.FullName()
	}
	var finalEnabled bool
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		finalEnabled, err = addPipelineCore(context.Background(), store, spec, raw, repo, pipelineAddCoreOptions{
			Home: *home, Enable: *enable, Stdout: stdout, Stderr: stderr,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline add: %v\n", err)
		return 1
	}
	writeLine(stdout, "added pipeline %s (%s, %d stages)", spec.Name, enabledLabel(finalEnabled), len(spec.Stages))
	return 0
}

// pipelineAddCoreOptions controls the reusable registry mutation shared by
// `pipeline add` and bundle import. ForceEnabled is deliberately separate from
// Enable: ordinary add preserves an existing row's enabled bit unless --enable
// is supplied, while import must land disabled unless its own --enable flag was
// explicitly supplied.
type pipelineAddCoreOptions struct {
	Home         string
	Enable       bool
	ForceEnabled bool
	Stdout       io.Writer
	Stderr       io.Writer
}

// addPipelineCore is the behavior-preserving write core behind runPipelineAdd.
// It validates local write targets and trigger cycles, warns about dormant
// upstreams/missing agents, persists the verbatim bytes + hash, arms trigger
// state, provisions the hidden runner, and performs the existing trigger bind /
// cleanup behavior. Callers are responsible for parsing the YAML and validating
// the repo string before entering this core.
func addPipelineCore(ctx context.Context, store *db.Store, spec pipeline.Spec, raw []byte, repo string, opts pipelineAddCoreOptions) (bool, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	interval, jitter := "", ""
	if spec.Schedule != nil {
		interval = spec.Schedule.Interval
		jitter = spec.Schedule.Jitter
	}
	record := db.Pipeline{
		Name:     spec.Name,
		Repo:     repo,
		SpecYAML: string(raw),
		SpecHash: pipeline.Hash(raw),
		Interval: interval,
		Jitter:   jitter,
	}
	runnerName := pipelineRunnerAgentName(spec.Name)
	if err := validatePipelineProducePaths(ctx, store, opts.Home, spec); err != nil {
		return false, err
	}
	registered, err := store.ListPipelines(ctx)
	if err != nil {
		return false, err
	}
	if err := validatePipelineTriggerCycle(registered, spec); err != nil {
		return false, err
	}
	if spec.Trigger != nil && spec.Trigger.Kind == "pipeline" {
		if _, found, err := store.GetPipeline(ctx, spec.Trigger.Pipeline); err != nil {
			return false, err
		} else if !found {
			fmt.Fprintf(stderr, "warning: pipeline %q references upstream pipeline %q which does not exist yet; it will remain dormant until the upstream is added\n", spec.Name, spec.Trigger.Pipeline)
		}
	}
	// Refuse to clobber a real managed agent that happens to occupy the runner
	// name. A pre-existing shell runner from an earlier add is idempotent.
	if existing, err := store.GetAgent(ctx, runnerName); err == nil && existing.Runtime != runtime.ShellRuntime {
		return false, fmt.Errorf("runner agent name %q collides with an existing %s agent", runnerName, existing.Runtime)
	}
	checkedAgents := make(map[string]struct{}, len(spec.Stages))
	for _, stage := range spec.Stages {
		if stage.Agent == "" {
			continue
		}
		if _, ok := checkedAgents[stage.Agent]; ok {
			continue
		}
		checkedAgents[stage.Agent] = struct{}{}
		if _, err := store.GetAgent(ctx, stage.Agent); err != nil {
			fmt.Fprintf(stderr, "warning: stage %q references agent %q which does not exist yet; create it before the pipeline runs (gitmoot agent ...)\n", stage.ID, stage.Agent)
		}
	}
	previous, previousFound, err := store.GetPipeline(ctx, spec.Name)
	if err != nil {
		return false, err
	}
	if err := store.CreateOrUpdatePipeline(ctx, record); err != nil {
		return false, err
	}
	if spec.Trigger != nil && spec.Trigger.Kind == "pipeline" {
		if err := store.ArmPipelineTrigger(ctx, spec.Name, spec.Trigger.Pipeline, time.Now().UTC()); err != nil {
			return false, err
		}
	} else if err := store.DeletePipelineTriggerState(ctx, spec.Name); err != nil {
		return false, err
	}
	if err := store.UpsertAgent(ctx, pipelineRunnerAgent(runnerName, repo)); err != nil {
		return false, err
	}
	if opts.ForceEnabled || opts.Enable {
		if err := store.SetPipelineEnabled(ctx, spec.Name, opts.Enable); err != nil {
			return false, err
		}
	}
	saved, _, err := store.GetPipeline(ctx, spec.Name)
	if err != nil {
		return false, err
	}
	finalEnabled := saved.Enabled
	if finalEnabled && spec.Trigger != nil && spec.Trigger.Kind == "email" {
		if _, bindErr := bindPipelineTrigger(ctx, store, saved, activepiecesAuthOptions{Home: opts.Home}, triggerBindingPending); bindErr != nil {
			fmt.Fprintf(stderr, "warning: pipeline %s was registered but its trigger is pending: %v; retry with `gitmoot pipeline bind-trigger %s`\n", spec.Name, bindErr, spec.Name)
		}
	}
	if spec.Trigger == nil && previousFound && strings.TrimSpace(previous.TriggerBinding) != "" {
		if cleanupErr := cleanupPipelineTrigger(ctx, store, previous, activepiecesAuthOptions{Home: opts.Home}); cleanupErr != nil {
			fmt.Fprintf(stderr, "warning: pipeline %s no longer declares a trigger, but its stale Activepieces flow could not be removed: %v; retry cleanup with `gitmoot pipeline bind-trigger %s`\n", spec.Name, cleanupErr, spec.Name)
		} else {
			writeLine(stdout, "cleaned up stale trigger flow for pipeline %s", spec.Name)
		}
	}
	return finalEnabled, nil
}

// validatePipelineProducePaths resolves every declared produce write target at
// add time and rejects overlap in either direction with Gitmoot-owned state or a
// managed checkout. Symlink resolution is applied to the deepest existing
// ancestor so a not-yet-created final directory is still checked safely.
func validatePipelineProducePaths(ctx context.Context, store *db.Store, homeFlag string, spec pipeline.Spec) error {
	for _, stage := range spec.Stages {
		if stage.Kind() != pipeline.StageKindAgentProduce {
			continue
		}
		if _, err := canonicalizePipelineProducePaths(ctx, store, homeFlag, fmt.Sprintf("stage %q", stage.ID), stage.Writes); err != nil {
			return err
		}
	}
	return nil
}

// validatePipelineTriggerCycle overlays candidate on the stored trigger graph
// and walks downstream->upstream edges from the candidate. Missing upstreams are
// leaves (the add path warns separately); only a closed chain is rejected.
func validatePipelineTriggerCycle(records []db.Pipeline, candidate pipeline.Spec) error {
	edges := make(map[string]string, len(records)+1)
	for _, rec := range records {
		spec, err := pipeline.Load([]byte(rec.SpecYAML))
		if err != nil || spec.Trigger == nil || spec.Trigger.Kind != "pipeline" {
			continue
		}
		edges[spec.Name] = spec.Trigger.Pipeline
	}
	delete(edges, candidate.Name)
	if candidate.Trigger != nil && candidate.Trigger.Kind == "pipeline" {
		edges[candidate.Name] = candidate.Trigger.Pipeline
	} else {
		return nil
	}

	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(edges))
	stack := make([]string, 0, len(edges)+1)
	var visit func(string) []string
	visit = func(name string) []string {
		state[name] = visiting
		stack = append(stack, name)
		upstream := edges[name]
		if upstream != "" {
			switch state[upstream] {
			case visiting:
				start := 0
				for i, item := range stack {
					if item == upstream {
						start = i
						break
					}
				}
				cycle := append([]string(nil), stack[start:]...)
				return append(cycle, upstream)
			case unvisited:
				if cycle := visit(upstream); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		return nil
	}
	if cycle := visit(candidate.Name); cycle != nil {
		return fmt.Errorf("pipeline trigger cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

// canonicalizePipelineProducePaths is the single filesystem safety check used
// both at pipeline-add time and immediately before a produce delivery. It returns
// resolved canonical targets so the runtime grant cannot follow a symlink that
// changed after validation.
func canonicalizePipelineProducePaths(ctx context.Context, store *db.Store, homeFlag, subject string, writes []string) ([]string, error) {
	if store == nil {
		return nil, errors.New("produce path validation requires a store")
	}
	paths, err := pathsFromFlag(homeFlag)
	if err != nil {
		return nil, err
	}
	protected := []struct {
		label string
		path  string
	}{{label: "gitmoot home", path: paths.Home}}
	repos, err := store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if strings.TrimSpace(repo.CheckoutPath) != "" {
			protected = append(protected, struct {
				label string
				path  string
			}{label: "managed checkout " + repo.Owner + "/" + repo.Name, path: repo.CheckoutPath})
		}
	}
	resolvedWrites := make([]string, 0, len(writes))
	for _, declared := range writes {
		resolved, err := resolveProduceSafetyPath(declared)
		if err != nil {
			return nil, fmt.Errorf("%s writes path %q: %w", subject, declared, err)
		}
		if resolved == string(filepath.Separator) {
			return nil, fmt.Errorf("%s writes path %q resolves to filesystem root, which is not allowed", subject, declared)
		}
		for _, item := range protected {
			protectedPath, err := resolveProduceSafetyPath(item.path)
			if err != nil {
				return nil, fmt.Errorf("resolve %s %q: %w", item.label, item.path, err)
			}
			if pathsOverlap(resolved, protectedPath) {
				return nil, fmt.Errorf("%s writes path %q resolves to %q and overlaps %s %q", subject, declared, resolved, item.label, protectedPath)
			}
		}
		resolvedWrites = append(resolvedWrites, resolved)
	}
	return resolvedWrites, nil
}

func resolveProduceSafetyPath(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	probe := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func pathsOverlap(left, right string) bool {
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return contains(left, right) || contains(right, left)
}

// pipelineRunnerAgent builds the hidden shell agent that owns a pipeline's stage
// jobs. The stage command travels per-job (via the stage job's runtime-override
// ref), NOT on this agent's runtime_ref, so one runner serves every stage. It is
// least-privilege read-only (the shell adapter runs the command verbatim; the
// policy is nominal for shell) and holds only the "ask" capability, matching the
// stage jobs' Action.
func pipelineRunnerAgent(name, repo string) db.Agent {
	return db.Agent{
		Name:           name,
		Role:           "pipeline-runner",
		Runtime:        runtime.ShellRuntime,
		RepoScope:      repo,
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
		HealthStatus:   "unknown",
	}
}

type pipelineStageJSON struct {
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	Cmd              string   `json:"cmd,omitempty"`
	CmdPreview       string   `json:"cmd_preview,omitempty"`
	Agent            string   `json:"agent,omitempty"`
	AgentRuntime     string   `json:"agent_runtime,omitempty"`
	Prompt           string   `json:"prompt,omitempty"`
	PromptPreview    string   `json:"prompt_preview,omitempty"`
	Action           string   `json:"action,omitempty"`
	Needs            []string `json:"needs,omitempty"`
	Timeout          string   `json:"timeout,omitempty"`
	Retry            int      `json:"retry,omitempty"`
	SuccessDecisions []string `json:"success_decisions,omitempty"`
	Write            bool     `json:"write,omitempty"`
	Writes           []string `json:"writes,omitempty"`
	Network          bool     `json:"network,omitempty"`
	Check            string   `json:"check,omitempty"`
	CheckRetries     *int     `json:"check_retries,omitempty"`
}

type pipelineJSON struct {
	Name                string              `json:"name"`
	Repo                string              `json:"repo,omitempty"`
	Group               string              `json:"group"`
	Enabled             bool                `json:"enabled"`
	Mode                string              `json:"mode"`
	Interval            string              `json:"interval,omitempty"`
	Jitter              string              `json:"jitter,omitempty"`
	SpecHash            string              `json:"spec_hash"`
	Stages              []pipelineStageJSON `json:"stages,omitempty"`
	LastRunAt           string              `json:"last_run_at,omitempty"`
	NextDueAt           string              `json:"next_due_at,omitempty"`
	LastRunID           string              `json:"last_run_id,omitempty"`
	LastStatus          string              `json:"last_status,omitempty"`
	TriggerBindingState string              `json:"trigger_binding_state,omitempty"`
}

func runPipelineList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print the pipelines as a JSON array")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline list does not accept positional arguments")
		return 2
	}
	var pipelines []db.Pipeline
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		pipelines, err = store.ListPipelines(context.Background())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline list: %v\n", err)
		return 1
	}
	knownPipelines := pipelineNameSet(pipelines)
	if *jsonOut {
		out := make([]pipelineJSON, 0, len(pipelines))
		for _, p := range pipelines {
			out = append(out, pipelineToJSON(p, false, nil, pipelineUpstreamMissing(p, knownPipelines)))
		}
		return encodePipelineJSON(stdout, stderr, out)
	}
	for _, p := range pipelines {
		group, _ := resolvedPipelineGroup(p)
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, enabledLabel(p.Enabled), pipelineListInterval(p, pipelineUpstreamMissing(p, knownPipelines)), firstNonEmpty(p.Repo, "-"), firstNonEmpty(group, "-"), firstNonEmpty(p.LastStatus, "-"), firstNonEmpty(triggerBindingState(p.TriggerBinding), "-"))
	}
	return 0
}

func runPipelineShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print the pipeline as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline show requires a name")
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline show accepts exactly one name")
		return 2
	}
	var (
		record          db.Pipeline
		agents          map[string]db.Agent
		found           bool
		runView         pipelineRunView
		runFound        bool
		upstreamMissing bool
	)
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		var err error
		// `show <name|run-id>`: a pipeline name resolves to the registry view; a run
		// id (its distinct "prun-" marker never collides with a name) resolves to the
		// run funnel. Try the pipeline first — a name is the common case.
		record, found, err = store.GetPipeline(ctx, name)
		if err != nil {
			return err
		}
		if found {
			agents = pipelineStageAgents(ctx, store, record)
			rows, listErr := store.ListPipelines(ctx)
			if listErr != nil {
				return listErr
			}
			upstreamMissing = pipelineUpstreamMissing(record, pipelineNameSet(rows))
			return nil
		}
		runView, runFound, err = loadPipelineRunView(ctx, store, name)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline show: %v\n", err)
		return 1
	}
	if found {
		if *jsonOut {
			return encodePipelineJSON(stdout, stderr, pipelineToJSON(record, true, agents, upstreamMissing))
		}
		printPipeline(stdout, record, agents, upstreamMissing)
		return 0
	}
	if runFound {
		if *jsonOut {
			return encodePipelineJSON(stdout, stderr, pipelineRunToJSON(runView))
		}
		printPipelineRunFunnel(stdout, runView)
		return 0
	}
	fmt.Fprintf(stderr, "pipeline or run %s not found\n", name)
	return 1
}

func runPipelineSetEnabled(args []string, enabled bool, stdout, stderr io.Writer) int {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	fs := flag.NewFlagSet("pipeline "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	apURL := fs.String("url", "", "Activepieces URL")
	port := fs.Int("port", defaultActivepiecesPort, "local Activepieces port")
	email := fs.String("email", defaultActivepiecesEmail, "Activepieces admin email")
	password := fs.String("password", "", "Activepieces admin password (uses saved credentials when omitted)")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintf(stderr, "pipeline %s requires a name\n", verb)
			return 2
		}
		return 0
	}
	parsed, reorderErr := reorderFlagArgs(args, map[string]struct{}{"home": {}, "url": {}, "port": {}, "email": {}, "password": {}}, nil)
	if reorderErr != nil {
		fmt.Fprintf(stderr, "pipeline %s: %v\n", verb, reorderErr)
		return 2
	}
	if err := fs.Parse(parsed); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "pipeline %s accepts exactly one name\n", verb)
		return 2
	}
	name := strings.TrimSpace(fs.Arg(0))
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		rec, ok, err := store.GetPipeline(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("pipeline %s not found", name)
		}
		auth := activepiecesAuthOptions{Home: *home, URL: *apURL, Port: *port, Email: *email, Password: *password}
		if !enabled {
			// Fail closed: the local bridge rejects runs immediately even if AP is
			// unreachable and cannot be switched off.
			if err := store.SetPipelineEnabled(ctx, name, false); err != nil {
				return err
			}
			shouldDisableBinding := true
			if spec, loadErr := pipeline.Load([]byte(rec.SpecYAML)); loadErr == nil && spec.Trigger != nil && spec.Trigger.Kind == "pipeline" {
				shouldDisableBinding = false
			}
			if shouldDisableBinding && strings.TrimSpace(rec.TriggerBinding) != "" {
				if err := disablePipelineTrigger(ctx, store, rec, auth); err != nil {
					fmt.Fprintf(stderr, "warning: pipeline %s is disabled locally, but Activepieces flow disable failed: %v\n", name, err)
				}
			}
			return nil
		}
		spec, loadErr := pipeline.Load([]byte(rec.SpecYAML))
		if loadErr != nil {
			if strings.TrimSpace(rec.TriggerBinding) == "" {
				return store.SetPipelineEnabled(ctx, name, true)
			}
			return loadErr
		}
		if spec.Trigger != nil && spec.Trigger.Kind == "pipeline" {
			if err := store.ArmPipelineTrigger(ctx, name, spec.Trigger.Pipeline, time.Now().UTC()); err != nil {
				return err
			}
		} else if spec.Trigger != nil && spec.Trigger.Kind == "email" {
			if _, err := bindPipelineTrigger(ctx, store, rec, auth, triggerBindingError); err != nil {
				return err
			}
		}
		return store.SetPipelineEnabled(ctx, name, true)
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline %s: %v\n", verb, err)
		return 1
	}
	writeLine(stdout, "%sd pipeline %s", verb, name)
	return 0
}

func runPipelineRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	apURL := fs.String("url", "", "Activepieces URL")
	port := fs.Int("port", defaultActivepiecesPort, "local Activepieces port")
	email := fs.String("email", defaultActivepiecesEmail, "Activepieces admin email")
	password := fs.String("password", "", "Activepieces admin password (uses saved credentials when omitted)")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline remove requires a name")
			return 2
		}
		return 0
	}
	parsed, reorderErr := reorderFlagArgs(args, map[string]struct{}{"home": {}, "url": {}, "port": {}, "email": {}, "password": {}}, nil)
	if reorderErr != nil {
		fmt.Fprintf(stderr, "pipeline remove: %v\n", reorderErr)
		return 2
	}
	if err := fs.Parse(parsed); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "pipeline remove accepts exactly one name")
		return 2
	}
	name := strings.TrimSpace(fs.Arg(0))
	var removed bool
	var removedRecord db.Pipeline
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		removedRecord, _, err = store.GetPipeline(context.Background(), name)
		if err != nil {
			return err
		}
		removed, err = store.DeletePipeline(context.Background(), name)
		if err != nil {
			return err
		}
		// Best-effort: dispose the hidden runner agent so removing a pipeline does not
		// leak it. Ignore the outcome — the pipeline row is what `remove` is about, and
		// leaving an orphan runner is harmless (run/job cleanup lands in the run step).
		_, _ = store.RemoveAgent(context.Background(), pipelineRunnerAgentName(name))
		shouldDeleteBinding := true
		if spec, loadErr := pipeline.Load([]byte(removedRecord.SpecYAML)); loadErr == nil && spec.Trigger != nil && spec.Trigger.Kind == "pipeline" {
			shouldDeleteBinding = false
		}
		if removed {
			_ = store.DeletePipelineTriggerState(context.Background(), name)
		}
		if removed && shouldDeleteBinding && strings.TrimSpace(removedRecord.TriggerBinding) != "" {
			auth := activepiecesAuthOptions{Home: *home, URL: *apURL, Port: *port, Email: *email, Password: *password}
			if cleanupErr := deletePipelineTrigger(context.Background(), removedRecord, auth); cleanupErr != nil {
				binding, _ := decodeTriggerBinding(removedRecord.TriggerBinding)
				fmt.Fprintf(stderr, "warning: removed pipeline %s locally, but Activepieces flow %s needs manual cleanup: %v\n", name, binding.FlowID, cleanupErr)
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline remove: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stderr, "pipeline %s not found\n", name)
		return 1
	}
	writeLine(stdout, "removed pipeline %s", name)
	return 0
}

// pipelineToJSON projects a stored pipeline row into its script-stable JSON
// shape. When withStages is set it parses the stored (already-validated) spec to
// enumerate the stage DAG; a parse failure degrades to no stages rather than
// failing the command.
func pipelineToJSON(record db.Pipeline, withStages bool, agents map[string]db.Agent, upstreamMissing ...bool) pipelineJSON {
	group, _ := resolvedPipelineGroup(record)
	out := pipelineJSON{
		Name:                record.Name,
		Repo:                record.Repo,
		Group:               group,
		Enabled:             record.Enabled,
		Mode:                pipelineDisplayMode(record, upstreamMissing...),
		Interval:            record.Interval,
		Jitter:              record.Jitter,
		SpecHash:            record.SpecHash,
		LastRunAt:           heartbeatTimeForStatus(record.LastRunAt),
		NextDueAt:           heartbeatTimeForStatus(record.NextDueAt),
		LastRunID:           record.LastRunID,
		LastStatus:          record.LastStatus,
		TriggerBindingState: triggerBindingState(record.TriggerBinding),
	}
	if out.LastRunAt == "-" {
		out.LastRunAt = ""
	}
	if out.NextDueAt == "-" {
		out.NextDueAt = ""
	}
	if withStages {
		if spec, err := pipeline.Load([]byte(record.SpecYAML)); err == nil {
			for _, stage := range spec.Stages {
				agentRuntime := ""
				if agent, ok := agents[stage.Agent]; ok {
					agentRuntime = strings.TrimSpace(agent.Runtime)
				}
				out.Stages = append(out.Stages, pipelineStageJSON{
					ID:               stage.ID,
					Kind:             pipelineStageKindName(stage),
					Cmd:              stage.Cmd,
					CmdPreview:       pipelineCmdPreview(stage.Cmd),
					Agent:            stage.Agent,
					AgentRuntime:     agentRuntime,
					Prompt:           stage.Prompt,
					PromptPreview:    pipelinePromptPreview(stage.Prompt),
					Action:           stage.Action,
					Needs:            stage.Needs,
					Timeout:          stage.Timeout,
					Retry:            stage.Retry,
					SuccessDecisions: stage.SuccessDecisions,
					Write:            stage.Write,
					Writes:           stage.Writes,
					Network:          stage.Network,
					Check:            stage.Check,
					CheckRetries:     stage.CheckRetries,
				})
			}
		}
	}
	return out
}

// resolvedPipelineGroup is the single display-resolution rule for pipeline
// groups. An explicit spec group wins; existing specs fall back to their repo.
// The bool reports that the repo fallback was used so text display can label it.
func resolvedPipelineGroup(record db.Pipeline) (string, bool) {
	if spec, err := pipeline.Load([]byte(record.SpecYAML)); err == nil && spec.Group != "" {
		return spec.Group, false
	}
	return strings.TrimSpace(record.Repo), true
}

func pipelineDisplayMode(record db.Pipeline, upstreamMissing ...bool) string {
	spec, err := pipeline.Load([]byte(record.SpecYAML))
	if err == nil && spec.Trigger != nil {
		if spec.Trigger.Kind == "pipeline" {
			mode := "after: " + spec.Trigger.Pipeline
			if len(upstreamMissing) > 0 && upstreamMissing[0] {
				mode += " (upstream missing)"
			}
			return mode
		}
		state := triggerBindingState(record.TriggerBinding)
		if state == "" {
			// No binding record exists yet (never bound, e.g. added disabled):
			// say so instead of asserting a lifecycle that has not started.
			state = "unbound"
		}
		mode := fmt.Sprintf("email-triggered (%s)", state)
		// A trigger+schedule hybrid is legal and the scheduler still fires it;
		// show both so neither start source is hidden.
		if spec.Schedule != nil {
			mode += ", scheduled " + spec.Schedule.Interval
		}
		return mode
	}
	if err == nil && spec.Schedule != nil {
		return "scheduled " + spec.Schedule.Interval
	}
	if strings.TrimSpace(record.Interval) != "" {
		return "scheduled " + strings.TrimSpace(record.Interval)
	}
	return "manual"
}

func pipelineListInterval(record db.Pipeline, upstreamMissing ...bool) string {
	spec, err := pipeline.Load([]byte(record.SpecYAML))
	if err == nil && spec.Trigger != nil {
		if spec.Trigger.Kind == "pipeline" {
			mode := "after: " + spec.Trigger.Pipeline
			if len(upstreamMissing) > 0 && upstreamMissing[0] {
				mode += " (upstream missing)"
			}
			return mode
		}
		// Hybrids keep their live interval visible: the scheduler fires them too.
		if interval := strings.TrimSpace(record.Interval); interval != "" {
			return "email+" + interval
		}
		return "email"
	}
	return firstNonEmpty(record.Interval, "-")
}

func pipelineNameSet(records []db.Pipeline) map[string]struct{} {
	known := make(map[string]struct{}, len(records))
	for _, rec := range records {
		known[rec.Name] = struct{}{}
	}
	return known
}

func pipelineUpstreamMissing(record db.Pipeline, known map[string]struct{}) bool {
	spec, err := pipeline.Load([]byte(record.SpecYAML))
	if err != nil || spec.Trigger == nil || spec.Trigger.Kind != "pipeline" {
		return false
	}
	_, found := known[spec.Trigger.Pipeline]
	return !found
}

func pipelineStageAgents(ctx context.Context, store *db.Store, record db.Pipeline) map[string]db.Agent {
	agents := make(map[string]db.Agent)
	spec, err := pipeline.Load([]byte(record.SpecYAML))
	if err != nil {
		return agents
	}
	for _, stage := range spec.Stages {
		name := strings.TrimSpace(stage.Agent)
		if name == "" {
			continue
		}
		agent, err := store.GetAgent(ctx, name)
		if err == nil {
			agents[name] = agent
		}
	}
	return agents
}

func pipelineStageKindName(stage pipeline.Stage) string {
	switch stage.Kind() {
	case pipeline.StageKindShell:
		return "shell"
	case pipeline.StageKindAgentAsk:
		return "agent_ask"
	case pipeline.StageKindAgentReview:
		return "agent_review"
	case pipeline.StageKindAgentImplement:
		return "agent_implement"
	case pipeline.StageKindAgentProduce:
		return "produce"
	case pipeline.StageKindGate:
		return "gate"
	case pipeline.StageKindOrchestrate:
		return "orchestrate"
	default:
		return "unknown"
	}
}

func pipelineStageBadge(stage pipeline.Stage) string {
	switch stage.Kind() {
	case pipeline.StageKindShell:
		return "[SHELL]"
	case pipeline.StageKindAgentAsk:
		return "[AGENT ask]"
	case pipeline.StageKindAgentReview:
		return "[AGENT review]"
	case pipeline.StageKindAgentImplement:
		return "[AGENT implement]"
	case pipeline.StageKindAgentProduce:
		return "[PRODUCE]"
	case pipeline.StageKindGate:
		return fmt.Sprintf("[GATE %s]", stage.Gate)
	case pipeline.StageKindOrchestrate:
		return "[ORCHESTRATE]"
	default:
		return "[UNKNOWN]"
	}
}

func pipelineAgentLabel(name string, agents map[string]db.Agent) string {
	agent, ok := agents[name]
	if !ok || strings.TrimSpace(agent.Runtime) == "" {
		return fmt.Sprintf("%s (unregistered)", name)
	}
	identity := strings.TrimSpace(agent.Runtime)
	if model := strings.TrimSpace(agent.Model); model != "" {
		identity += "/" + model
	}
	if effort := strings.TrimSpace(agent.Effort); effort != "" {
		identity += " effort=" + effort
	}
	return fmt.Sprintf("%s (%s)", name, identity)
}

func pipelinePreview(value, newlineReplacement string, limit int) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\n", newlineReplacement)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func pipelineCmdPreview(cmd string) string {
	if strings.TrimSpace(cmd) == "" {
		return ""
	}
	return pipelinePreview(cmd, "; ", 80)
}

func pipelinePromptPreview(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return ""
	}
	return pipelinePreview(prompt, " ", 100)
}

func printPipeline(stdout io.Writer, record db.Pipeline, agents map[string]db.Agent, upstreamMissing ...bool) {
	spec, specErr := pipeline.Load([]byte(record.SpecYAML))
	writeLine(stdout, "name: %s", record.Name)
	writeLine(stdout, "repo: %s", firstNonEmpty(record.Repo, "-"))
	group, defaulted := resolvedPipelineGroup(record)
	group = firstNonEmpty(group, "-")
	if defaulted {
		group += " (default)"
	}
	writeLine(stdout, "group: %s", group)
	if specErr == nil && spec.Description != "" {
		writeLine(stdout, "description:")
		for _, line := range strings.Split(spec.Description, "\n") {
			writeLine(stdout, "  %s", line)
		}
	}
	writeLine(stdout, "enabled: %t", record.Enabled)
	writeLine(stdout, "mode: %s", pipelineDisplayMode(record, upstreamMissing...))
	writeLine(stdout, "interval: %s", firstNonEmpty(record.Interval, "-"))
	writeLine(stdout, "jitter: %s", firstNonEmpty(record.Jitter, "-"))
	writeLine(stdout, "spec_hash: %s", record.SpecHash)
	writeLine(stdout, "last_run: %s", heartbeatTimeForStatus(record.LastRunAt))
	writeLine(stdout, "next_due: %s", heartbeatTimeForStatus(record.NextDueAt))
	writeLine(stdout, "last_status: %s", firstNonEmpty(record.LastStatus, "-"))
	writeLine(stdout, "last_run_id: %s", firstNonEmpty(record.LastRunID, "-"))
	writeLine(stdout, "trigger_binding: %s", firstNonEmpty(triggerBindingState(record.TriggerBinding), "-"))
	writeLine(stdout, "stages:")
	if specErr != nil {
		writeLine(stdout, "  (unparseable stored spec: %v)", specErr)
		return
	}
	for _, stage := range spec.Stages {
		needs := "-"
		if len(stage.Needs) > 0 {
			needs = strings.Join(stage.Needs, ",")
		}
		fields := []string{"  " + stage.ID, pipelineStageBadge(stage)}
		switch stage.Kind() {
		case pipeline.StageKindShell:
			fields = append(fields, "cmd: "+pipelineCmdPreview(stage.Cmd))
		case pipeline.StageKindAgentAsk, pipeline.StageKindAgentReview, pipeline.StageKindAgentImplement, pipeline.StageKindAgentProduce, pipeline.StageKindOrchestrate:
			fields = append(fields, pipelineAgentLabel(stage.Agent, agents))
		case pipeline.StageKindGate:
			if stage.Source != "" {
				fields = append(fields, "source="+stage.Source)
			}
		}
		if stage.Timeout != "" {
			fields = append(fields, "timeout="+stage.Timeout)
		}
		if stage.Retry > 0 {
			fields = append(fields, fmt.Sprintf("retry=%d", stage.Retry))
		}
		fields = append(fields, "needs="+needs)
		writeLine(stdout, "%s", strings.Join(fields, "\t"))
		if preview := pipelinePromptPreview(stage.Prompt); preview != "" {
			writeLine(stdout, "          prompt: %s", strconv.Quote(preview))
		}
	}
}

func encodePipelineJSON(stdout, stderr io.Writer, value any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fmt.Fprintf(stderr, "pipeline: %v\n", err)
		return 1
	}
	return 0
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

// runPipelineRunCmd is `gitmoot pipeline run <name>`: it creates a manual run of a
// registered pipeline, enqueues its ready root stages (via one advance pass), and
// prints the run id (script-stable, so `RUN=$(gitmoot pipeline run foo)` works).
// It enforces the two run preconditions: the pipeline must carry a repo (stage
// jobs need a managed repo for the worker to claim them) and it must have no
// in-flight run (one active run per pipeline). Manual runs ignore the schedule's
// enabled flag — a disabled pipeline can still be run by hand.
func runPipelineRunCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline run requires a name")
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline run accepts exactly one name")
		return 2
	}
	var runID string
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		rec, ok, err := store.GetPipeline(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("pipeline %s not found", name)
		}
		if strings.TrimSpace(rec.Repo) == "" {
			return fmt.Errorf("pipeline %s has no repo; stages need a managed repo to run", name)
		}
		if active, ok, err := store.ActivePipelineRun(ctx, name); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("pipeline %s already has an active run %s", name, active.ID)
		}
		spec, err := pipeline.Load([]byte(rec.SpecYAML))
		if err != nil {
			return fmt.Errorf("stored spec is invalid: %w", err)
		}
		now := time.Now().UTC()
		run, err := createPipelineRun(ctx, store, rec, spec, "manual", "{}", now)
		if err != nil {
			return err
		}
		enqueue := newPipelineStageEnqueuer(store, *home)
		if _, err := advancePipelineRun(ctx, store, enqueue, rec, spec, run, now); err != nil {
			return err
		}
		runID = run.ID
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline run: %v\n", err)
		return 1
	}
	writeLine(stdout, "%s", runID)
	return 0
}

// pipelineRunView is the resolved data behind `pipeline show <run-id>`: the run
// row plus its stage rows ordered into spec/DAG order for the funnel.
type pipelineRunView struct {
	run         db.PipelineRun
	stages      []db.PipelineRunStage
	progress    map[string]db.JobEvent
	orchestrate map[string]bool
	tokens      int
	stageTokens map[string]pipelineStageTokens
}

type pipelineStageTokens struct {
	input  int
	output int
}

// loadPipelineRunView loads a run and its stages, ordered for display. A missing
// run returns ok=false (so `show` falls through to "not found").
func loadPipelineRunView(ctx context.Context, store *db.Store, id string) (pipelineRunView, bool, error) {
	run, ok, err := store.GetPipelineRun(ctx, id)
	if err != nil || !ok {
		return pipelineRunView{}, ok, err
	}
	stages, err := store.ListPipelineRunStages(ctx, run.ID)
	if err != nil {
		return pipelineRunView{}, false, err
	}
	stages = orderPipelineRunStages(ctx, store, run, stages)
	jobIDs := make([]string, 0, len(stages))
	for _, stage := range stages {
		if stage.JobID != "" && (stage.State == pipeline.StageQueued || stage.State == pipeline.StageRunning) {
			jobIDs = append(jobIDs, stage.JobID)
		}
	}
	progress, err := store.GetLatestJobEventsByKind(ctx, jobIDs, "progress")
	if err != nil {
		return pipelineRunView{}, false, err
	}
	orchestrate := map[string]bool{}
	if rec, found, getErr := store.GetPipeline(ctx, run.Pipeline); getErr == nil && found && strings.TrimSpace(rec.SpecHash) == strings.TrimSpace(run.SpecHash) {
		if spec, loadErr := pipeline.Load([]byte(rec.SpecYAML)); loadErr == nil {
			for _, stage := range spec.Stages {
				orchestrate[stage.ID] = stage.Kind() == pipeline.StageKindOrchestrate
			}
		}
	}
	stageTokens := make(map[string]pipelineStageTokens, len(stages))
	for _, stage := range stages {
		if strings.TrimSpace(stage.JobID) == "" {
			continue
		}
		if job, getErr := store.GetJob(ctx, stage.JobID); getErr == nil {
			stageTokens[stage.StageID] = pipelineStageTokens{input: job.InputTokens, output: job.OutputTokens}
		}
	}
	tokens, err := sumPipelineRunTokens(ctx, store, run.ID, stages, orchestrate)
	if err != nil {
		return pipelineRunView{}, false, err
	}
	return pipelineRunView{run: run, stages: stages, progress: progress, orchestrate: orchestrate, tokens: tokens, stageTokens: stageTokens}, true, nil
}

// sumPipelineRunTokens combines the run-rooted jobs (every ordinary stage) with
// each orchestrate attempt's self-rooted coordination tree. The sets are disjoint:
// pipelineStageJobRequest deliberately self-roots orchestrate stages, while every
// other stage uses runID, so this cannot double-count.
func sumPipelineRunTokens(ctx context.Context, store *db.Store, runID string, stages []db.PipelineRunStage, orchestrate map[string]bool) (int, error) {
	total, err := store.SumJobTokensByRoot(ctx, runID)
	if err != nil {
		return 0, err
	}
	for _, stage := range stages {
		if !orchestrate[stage.StageID] || strings.TrimSpace(stage.JobID) == "" {
			continue
		}
		for attempt := 0; attempt <= stage.Attempt; attempt++ {
			stageTotal, err := store.SumJobTokensByRoot(ctx, pipelineStageJobID(runID, stage.StageID, attempt))
			if err != nil {
				return 0, err
			}
			total += stageTotal
		}
	}
	return total, nil
}

// orderPipelineRunStages reorders stage rows into spec (topological) order when the
// run's pipeline + matching spec snapshot are available; otherwise it keeps the
// store's stable stage_id order. Any stage rows not present in the spec (should not
// happen) are appended so no data is dropped.
func orderPipelineRunStages(ctx context.Context, store *db.Store, run db.PipelineRun, stages []db.PipelineRunStage) []db.PipelineRunStage {
	rec, ok, err := store.GetPipeline(ctx, run.Pipeline)
	if err != nil || !ok {
		return stages
	}
	spec, err := pipeline.Load([]byte(rec.SpecYAML))
	if err != nil || strings.TrimSpace(rec.SpecHash) != strings.TrimSpace(run.SpecHash) {
		return stages
	}
	byID := make(map[string]db.PipelineRunStage, len(stages))
	for _, stage := range stages {
		byID[stage.StageID] = stage
	}
	ordered := make([]db.PipelineRunStage, 0, len(stages))
	seen := make(map[string]struct{}, len(stages))
	for _, stage := range spec.Stages {
		if row, ok := byID[stage.ID]; ok {
			ordered = append(ordered, row)
			seen[stage.ID] = struct{}{}
		}
	}
	for _, stage := range stages {
		if _, ok := seen[stage.StageID]; !ok {
			ordered = append(ordered, stage)
		}
	}
	return ordered
}

// printPipelineRunFunnel renders a run as the TEXT FUNNEL (decision 9), e.g.
// `source OK -> score OK -> deploy BLOCKED (needs: R2 token)`, preceded by the run
// header. A failed run surfaces the exact `gitmoot report bug --job <stage-job>`
// command for the halted stage (NO auto-filing).
func printPipelineRunFunnel(stdout io.Writer, view pipelineRunView) {
	printPipelineRunFunnelAt(stdout, view, time.Now().UTC())
}

func printPipelineRunFunnelAt(stdout io.Writer, view pipelineRunView, now time.Time) {
	run := view.run
	writeLine(stdout, "run: %s", run.ID)
	writeLine(stdout, "pipeline: %s", run.Pipeline)
	writeLine(stdout, "trigger: %s", firstNonEmpty(run.Trigger, "-"))
	if payload := strings.TrimSpace(run.PayloadJSON); payload != "" && payload != "{}" {
		writeLine(stdout, "payload_json: %s", payload)
	}
	writeLine(stdout, "state: %s", run.State)
	writeLine(stdout, "started: %s", heartbeatTimeForStatus(run.StartedAt))
	writeLine(stdout, "finished: %s", heartbeatTimeForStatus(run.FinishedAt))
	writeLine(stdout, "tokens: %d (best-effort)", view.tokens)
	if strings.TrimSpace(run.HaltStage) != "" {
		writeLine(stdout, "halt_stage: %s", run.HaltStage)
	}
	if strings.TrimSpace(run.HaltReason) != "" {
		writeLine(stdout, "halt_reason: %s", run.HaltReason)
	}
	if needs := decodePipelineNeeds(run.NeedsJSON); len(needs) > 0 {
		writeLine(stdout, "needs: %s", strings.Join(needs, ", "))
	}
	writeLine(stdout, "")
	writeLine(stdout, "%s", pipelineFunnelLine(view.stages))
	for _, stage := range view.stages {
		if stage.State != pipeline.StageQueued && stage.State != pipeline.StageRunning {
			continue
		}
		writeLine(stdout, "  %s: %s; enqueued %s ago", stage.StageID, strings.ToUpper(stage.State), pipelineElapsed(now, stage.StartedAt))
		event, hasProgress := view.progress[stage.JobID]
		progress, validProgress := decodePipelineProgress(event.Message)
		if stage.State == pipeline.StageRunning && hasProgress && validProgress {
			age := pipelineEventAge(now, event.CreatedAt)
			if progress.Activity != "" {
				writeLine(stdout, "    last activity %s ago: %s", age, progress.Activity)
			} else {
				writeLine(stdout, "    last activity %s ago: elapsed %s", age, progress.Elapsed)
			}
		}
		if stage.State == pipeline.StageRunning && view.orchestrate[stage.StageID] && (!hasProgress || !validProgress || !pipelineProgressFresh(now, event.CreatedAt)) {
			writeLine(stdout, "    (sub-tree running; no per-stage progress)")
		}
	}
	if run.State == pipeline.RunFailed {
		if jobID := haltStageJobID(view); jobID != "" {
			writeLine(stdout, "")
			writeLine(stdout, "stage failed; report it with:")
			writeLine(stdout, "  gitmoot report bug --job %s", jobID)
		}
	}
}

func pipelineElapsed(now, started time.Time) string {
	if started.IsZero() {
		return "?"
	}
	d := now.Sub(started)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

func pipelineEventTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func pipelineEventAge(now time.Time, createdAt string) string {
	return pipelineElapsed(now, pipelineEventTime(createdAt))
}

func pipelineProgressFresh(now time.Time, createdAt string) bool {
	created := pipelineEventTime(createdAt)
	if created.IsZero() {
		return false
	}
	age := now.Sub(created)
	return age >= 0 && age <= 2*pipelineProgressInterval
}

func decodePipelineProgress(message string) (pipelineProgressEventPayload, bool) {
	var payload pipelineProgressEventPayload
	if err := json.Unmarshal([]byte(message), &payload); err != nil {
		return pipelineProgressEventPayload{}, false
	}
	payload.Activity = sanitizePipelineProgressLine(payload.Activity)
	return payload, strings.TrimSpace(payload.Elapsed) != "" || payload.Activity != ""
}

// pipelineFunnelLine joins each stage's `<id> <LABEL>` into the funnel line.
func pipelineFunnelLine(stages []db.PipelineRunStage) string {
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		parts = append(parts, stage.StageID+" "+pipelineStageFunnelLabel(stage))
	}
	return strings.Join(parts, " -> ")
}

// pipelineStageFunnelLabel is the funnel status token for a stage: OK for a
// succeeded stage, BLOCKED (needs: ...) for a parked stage, else the uppercased
// state (PENDING/QUEUED/RUNNING/FAILED/SKIPPED/CANCELLED).
func pipelineStageFunnelLabel(stage db.PipelineRunStage) string {
	switch stage.State {
	case pipeline.StageSucceeded:
		return "OK"
	case pipeline.StageBlocked:
		if needs := decodePipelineNeeds(stage.NeedsJSON); len(needs) > 0 {
			return fmt.Sprintf("BLOCKED (needs: %s)", strings.Join(needs, ", "))
		}
		return "BLOCKED"
	default:
		return strings.ToUpper(stage.State)
	}
}

// haltStageJobID returns the job id of the run's halt stage (for the bug-report
// command).
func haltStageJobID(view pipelineRunView) string {
	for _, stage := range view.stages {
		if stage.StageID == view.run.HaltStage {
			return strings.TrimSpace(stage.JobID)
		}
	}
	return ""
}

type pipelineRunStageJSON struct {
	ID           string                        `json:"id"`
	State        string                        `json:"state"`
	JobID        string                        `json:"job_id,omitempty"`
	Attempt      int                           `json:"attempt,omitempty"`
	Needs        []string                      `json:"needs,omitempty"`
	Summary      string                        `json:"summary,omitempty"`
	StartedAt    string                        `json:"started_at,omitempty"`
	FinishedAt   string                        `json:"finished_at,omitempty"`
	Progress     *pipelineRunStageProgressJSON `json:"progress,omitempty"`
	InputTokens  *int                          `json:"input_tokens,omitempty"`
	OutputTokens *int                          `json:"output_tokens,omitempty"`
}

// The external gitmoot-dashboard repo can already derive elapsed time from
// started_at. Teaching it the optional progress object is Phase 2 of #816; keep
// this CLI JSON addition backward-compatible until that separate consumer moves.

type pipelineRunStageProgressJSON struct {
	Elapsed   string `json:"elapsed"`
	Activity  string `json:"activity,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

type pipelineRunJSON struct {
	ID          string                 `json:"id"`
	Pipeline    string                 `json:"pipeline"`
	Trigger     string                 `json:"trigger"`
	PayloadJSON string                 `json:"payload_json,omitempty"`
	State       string                 `json:"state"`
	HaltStage   string                 `json:"halt_stage,omitempty"`
	HaltReason  string                 `json:"halt_reason,omitempty"`
	Needs       []string               `json:"needs,omitempty"`
	SpecHash    string                 `json:"spec_hash"`
	StartedAt   string                 `json:"started_at,omitempty"`
	FinishedAt  string                 `json:"finished_at,omitempty"`
	Funnel      string                 `json:"funnel"`
	Stages      []pipelineRunStageJSON `json:"stages,omitempty"`
	Tokens      int                    `json:"tokens"`
}

// pipelineRunToJSON projects a run view into its script-stable JSON shape.
func pipelineRunToJSON(view pipelineRunView) pipelineRunJSON {
	run := view.run
	out := pipelineRunJSON{
		ID:          run.ID,
		Pipeline:    run.Pipeline,
		Trigger:     run.Trigger,
		PayloadJSON: strings.TrimSpace(run.PayloadJSON),
		State:       run.State,
		HaltStage:   run.HaltStage,
		HaltReason:  run.HaltReason,
		Needs:       decodePipelineNeeds(run.NeedsJSON),
		SpecHash:    run.SpecHash,
		StartedAt:   pipelineRunTimeJSON(run.StartedAt),
		FinishedAt:  pipelineRunTimeJSON(run.FinishedAt),
		Funnel:      pipelineFunnelLine(view.stages),
		Tokens:      view.tokens,
	}
	if out.PayloadJSON == "{}" {
		out.PayloadJSON = ""
	}
	for _, stage := range view.stages {
		stageJSON := pipelineRunStageJSON{
			ID:         stage.StageID,
			State:      stage.State,
			JobID:      stage.JobID,
			Attempt:    stage.Attempt,
			Needs:      decodePipelineNeeds(stage.NeedsJSON),
			Summary:    stage.Summary,
			StartedAt:  pipelineRunTimeJSON(stage.StartedAt),
			FinishedAt: pipelineRunTimeJSON(stage.FinishedAt),
		}
		if usage, ok := view.stageTokens[stage.StageID]; ok {
			stageJSON.InputTokens = &usage.input
			stageJSON.OutputTokens = &usage.output
		}
		if event, ok := view.progress[stage.JobID]; ok {
			if progress, valid := decodePipelineProgress(event.Message); valid {
				stageJSON.Progress = &pipelineRunStageProgressJSON{
					Elapsed: progress.Elapsed, Activity: progress.Activity,
					UpdatedAt: pipelineRunTimeJSON(pipelineEventTime(event.CreatedAt)),
				}
			}
		}
		out.Stages = append(out.Stages, stageJSON)
	}
	return out
}

func pipelineRunTimeJSON(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
