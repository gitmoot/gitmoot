package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
	yaml "gopkg.in/yaml.v3"
)

const (
	pipelineBundleVersion                   = 1
	pipelineBundleRepoParameter             = "__GITMOOT_REPO__"
	pipelineBundleDevelopmentMinimumVersion = "0.9.2-dev"
)

var (
	pipelineBundleLookPath = exec.LookPath
	pipelineBundleToken    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	pipelineBundlePlain    = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
	pipelineAbsolutePath   = regexp.MustCompile(`(?:^|[\s"'=:;(])(/(?:root|home|Users|tmp)(?:/[^\s"'` + "`" + `;|&<>),\]}]*)?)`)
)

type pipelineBundleManifest struct {
	BundleVersion     int                        `yaml:"bundle_version"`
	GitmootVersionMin string                     `yaml:"gitmoot_version_min"`
	Pipeline          string                     `yaml:"pipeline"`
	Description       string                     `yaml:"description,omitempty"`
	Repo              string                     `yaml:"repo"`
	Requirements      pipelineBundleRequirements `yaml:"requirements"`
	Warnings          []string                   `yaml:"warnings"`
	WriteAuthority    []string                   `yaml:"write_authority"`
	Agents            []pipelineBundleAgent      `yaml:"agents"`
	SpecSHA256        string                     `yaml:"spec_sha256"`
}

type pipelineBundleRequirements struct {
	Runtimes          []string                   `yaml:"runtimes"`
	Connections       []pipelineBundleConnection `yaml:"connections"`
	UpstreamPipelines []string                   `yaml:"upstream_pipelines"`
}

type pipelineBundleConnection struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type pipelineBundleAgent struct {
	Name        string `yaml:"name"`
	Runtime     string `yaml:"runtime"`
	TemplateRef string `yaml:"template_ref,omitempty"`
}

func runPipelineExport(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline export requires a pipeline name")
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	fs := flag.NewFlagSet("pipeline export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	output := fs.String("output", "", "directory to write the bundle into")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || name == "" {
		fmt.Fprintln(stderr, "pipeline export accepts exactly one pipeline name")
		return 2
	}
	if strings.TrimSpace(*output) == "" {
		fmt.Fprintln(stderr, "pipeline export requires --output <dir>")
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		return exportPipelineBundle(context.Background(), store, name, *output, stdout, stderr)
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline export: %v\n", err)
		return 1
	}
	return 0
}

func exportPipelineBundle(ctx context.Context, store *db.Store, name, output string, stdout, stderr io.Writer) error {
	record, found, err := store.GetPipeline(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("unknown pipeline %q", name)
	}
	raw := []byte(record.SpecYAML)
	spec, err := pipeline.Load(raw)
	if err != nil {
		return fmt.Errorf("stored spec is invalid: %w", err)
	}
	parameterized, err := rewritePipelineBundleSpec(raw, pipelineBundleRepoParameter, "", nil)
	if err != nil {
		return fmt.Errorf("parameterize repo: %w", err)
	}

	agents, templates, err := collectPipelineBundleAgents(ctx, store, spec)
	if err != nil {
		return err
	}
	warnings := detectPipelineAbsolutePathWarnings(spec)
	manifest := pipelineBundleManifest{
		BundleVersion:     pipelineBundleVersion,
		GitmootVersionMin: pipelineBundleExportMinimumVersion(),
		Pipeline:          spec.Name,
		Description:       pipelineBundleDescription(raw),
		Repo:              pipelineBundleRepoParameter,
		Warnings:          warnings,
		WriteAuthority:    pipelineBundleWriteAuthority(spec),
		Agents:            agents,
		SpecSHA256:        pipeline.Hash(parameterized),
	}
	manifest.Requirements = derivePipelineBundleRequirements(spec, agents)
	encoded, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode bundle manifest: %w", err)
	}

	output = filepath.Clean(output)
	if err := preparePipelineBundleOutput(output); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "spec.yaml"), parameterized, 0o644); err != nil {
		return fmt.Errorf("write spec.yaml: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(output, "templates"), 0o755); err != nil {
		return fmt.Errorf("create templates directory: %w", err)
	}
	for id, content := range templates {
		if err := os.WriteFile(filepath.Join(output, "templates", id+".md"), content, 0o644); err != nil {
			return fmt.Errorf("write template %s: %w", id, err)
		}
	}
	if err := os.WriteFile(filepath.Join(output, "bundle.yaml"), encoded, 0o644); err != nil {
		return fmt.Errorf("write bundle.yaml: %w", err)
	}
	writeLine(stdout, "exported pipeline %s -> %s", spec.Name, output)
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "WARNING: %s\n", warning)
	}
	if len(templates) > 0 {
		fmt.Fprintf(stderr, "note: publishing %d template(s) to %s; prompts are pushed verbatim — only publish private prompts to a private repo\n", len(templates), output)
	}
	return nil
}

func preparePipelineBundleOutput(output string) error {
	info, err := os.Stat(output)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(output, 0o755)
	}
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("output %s exists and is not a directory", output)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return fmt.Errorf("read output directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("output directory %s is not empty", output)
	}
	return nil
}

func collectPipelineBundleAgents(ctx context.Context, store *db.Store, spec pipeline.Spec) ([]pipelineBundleAgent, map[string][]byte, error) {
	names := make(map[string]struct{})
	for _, stage := range spec.Stages {
		if stage.Agent != "" {
			names[stage.Agent] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	agents := make([]pipelineBundleAgent, 0, len(ordered))
	templates := make(map[string][]byte)
	for _, name := range ordered {
		agent, err := store.GetAgent(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, fmt.Errorf("pipeline agent %q is not registered", name)
			}
			return nil, nil, err
		}
		templateID, _ := db.SplitAgentTemplateReference(agent.TemplateID)
		agents = append(agents, pipelineBundleAgent{Name: name, Runtime: agent.Runtime, TemplateRef: templateID})
		if templateID == "" {
			continue
		}
		cached, err := store.GetAgentTemplateReference(ctx, agent.TemplateID)
		if err != nil {
			return nil, nil, fmt.Errorf("export template %q for agent %q: %w", agent.TemplateID, name, err)
		}
		content, err := agenttemplate.Export(cached)
		if err != nil {
			return nil, nil, err
		}
		if prior, ok := templates[templateID]; ok && !bytes.Equal(prior, []byte(content)) {
			return nil, nil, fmt.Errorf("agents reference different snapshots for template %q", templateID)
		}
		templates[templateID] = []byte(content)
	}
	return agents, templates, nil
}

func derivePipelineBundleRequirements(spec pipeline.Spec, agents []pipelineBundleAgent) pipelineBundleRequirements {
	runtimeSet := make(map[string]struct{})
	for _, stage := range spec.Stages {
		if stage.Kind() == pipeline.StageKindShell {
			runtimeSet[runtime.ShellRuntime] = struct{}{}
		}
	}
	for _, agent := range agents {
		if agent.Runtime != "" {
			runtimeSet[agent.Runtime] = struct{}{}
		}
	}
	runtimes := make([]string, 0, len(runtimeSet))
	for name := range runtimeSet {
		runtimes = append(runtimes, name)
	}
	sort.Strings(runtimes)
	requirements := pipelineBundleRequirements{Runtimes: runtimes, Connections: []pipelineBundleConnection{}, UpstreamPipelines: []string{}}
	if spec.Trigger != nil {
		switch spec.Trigger.Kind {
		case "email":
			requirements.Connections = append(requirements.Connections, pipelineBundleConnection{Kind: "email", Name: spec.Trigger.Connection})
		case "pipeline":
			requirements.UpstreamPipelines = append(requirements.UpstreamPipelines, spec.Trigger.Pipeline)
		}
	}
	return requirements
}

func pipelineBundleWriteAuthority(spec pipeline.Spec) []string {
	flags := make([]string, 0, 3)
	if spec.AllowScheduledWrites {
		flags = append(flags, "allow_scheduled_writes")
	}
	if spec.AllowTriggeredWrites {
		flags = append(flags, "allow_triggered_writes")
	}
	if spec.AllowAutoMerge {
		flags = append(flags, "allow_auto_merge")
	}
	return flags
}

func detectPipelineAbsolutePathWarnings(spec pipeline.Spec) []string {
	warnings := []string{}
	seen := make(map[string]struct{})
	for _, stage := range spec.Stages {
		if stage.Kind() != pipeline.StageKindShell {
			continue
		}
		for _, match := range pipelineAbsolutePath.FindAllStringSubmatch(stage.Cmd, -1) {
			path := strings.TrimRight(match[1], ".:")
			warning := fmt.Sprintf("stage %q command contains host-specific absolute path %q", stage.ID, path)
			if _, ok := seen[warning]; ok {
				continue
			}
			seen[warning] = struct{}{}
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

func runPipelineImport(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline import requires a bundle directory")
			return 2
		}
		return 0
	}
	bundleDir := filepath.Clean(args[0])
	fs := flag.NewFlagSet("pipeline import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "target repository (owner/name)")
	nameFlag := fs.String("name", "", "import under a different pipeline name")
	force := fs.Bool("force", false, "replace conflicting templates, agents, or pipeline")
	enable := fs.Bool("enable", false, "enable after import (also re-consents declared write authority)")
	var mapFlags repeatedFlag
	fs.Var(&mapFlags, "agent-map", "map an exported agent to a registered local agent (exported=local; repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline import accepts exactly one bundle directory")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline import: --repo is required and must be owner/name: %v\n", err)
		return 2
	}
	agentMap, err := parsePipelineAgentMappings(mapFlags)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline import: %v\n", err)
		return 2
	}
	manifest, raw, err := readPipelineBundleFiles(bundleDir)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline import: %v\n", err)
		return 1
	}
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		reportManifest := pipelineBundleReportManifest(manifest, raw)
		report := inspectPipelineBundleRequirements(ctx, store, paths, *home, reportManifest, agentMap)
		printPipelineBundleRequirements(stdout, report, reportManifest)
		if err := validatePipelineBundle(manifest, raw, buildinfo.Current().Version); err != nil {
			return err
		}
		return importPipelineBundle(ctx, store, *home, bundleDir, repo.FullName(), strings.TrimSpace(*nameFlag), agentMap, *force, *enable, manifest, raw, stdout, stderr, report)
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline import: %v\n", err)
		return 1
	}
	return 0
}

func readPipelineBundleFiles(dir string) (pipelineBundleManifest, []byte, error) {
	manifestRaw, err := os.ReadFile(filepath.Join(dir, "bundle.yaml"))
	if err != nil {
		return pipelineBundleManifest{}, nil, fmt.Errorf("read bundle.yaml: %w", err)
	}
	var manifest pipelineBundleManifest
	dec := yaml.NewDecoder(bytes.NewReader(manifestRaw))
	dec.KnownFields(true)
	if err := dec.Decode(&manifest); err != nil {
		return pipelineBundleManifest{}, nil, fmt.Errorf("parse bundle.yaml: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return pipelineBundleManifest{}, nil, errors.New("parse bundle.yaml: multiple YAML documents are not supported")
		}
		return pipelineBundleManifest{}, nil, fmt.Errorf("parse bundle.yaml: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "spec.yaml"))
	if err != nil {
		return pipelineBundleManifest{}, nil, fmt.Errorf("read spec.yaml: %w", err)
	}
	return manifest, raw, nil
}

func validatePipelineBundle(manifest pipelineBundleManifest, raw []byte, currentVersion string) error {
	if manifest.BundleVersion != pipelineBundleVersion {
		return fmt.Errorf("unsupported bundle_version %d (supported: %d)", manifest.BundleVersion, pipelineBundleVersion)
	}
	if strings.TrimSpace(manifest.GitmootVersionMin) == "" {
		return errors.New("bundle manifest is missing gitmoot_version_min")
	}
	if err := requirePipelineBundleVersion(manifest.GitmootVersionMin, currentVersion); err != nil {
		return err
	}
	if manifest.Pipeline == "" || !pipelineBundleToken.MatchString(manifest.Pipeline) {
		return fmt.Errorf("bundle manifest pipeline %q is not a name-safe token", manifest.Pipeline)
	}
	if manifest.Repo != pipelineBundleRepoParameter {
		return fmt.Errorf("bundle manifest repo must be the parameter token %q", pipelineBundleRepoParameter)
	}
	if got := pipeline.Hash(raw); got != strings.TrimSpace(manifest.SpecSHA256) {
		return fmt.Errorf("spec_sha256 mismatch: manifest has %s, spec.yaml is %s", manifest.SpecSHA256, got)
	}
	spec, err := pipeline.Load(raw)
	if err != nil {
		return fmt.Errorf("invalid bundled spec: %w", err)
	}
	if spec.Name != manifest.Pipeline {
		return fmt.Errorf("bundle manifest pipeline %q does not match spec name %q", manifest.Pipeline, spec.Name)
	}
	if spec.Repo != pipelineBundleRepoParameter {
		return fmt.Errorf("bundled spec repo must be the parameter token %q", pipelineBundleRepoParameter)
	}
	manifestAgents := make(map[string]pipelineBundleAgent, len(manifest.Agents))
	for _, agent := range manifest.Agents {
		if !pipelineBundleToken.MatchString(agent.Name) {
			return fmt.Errorf("bundle agent name %q is not a name-safe token", agent.Name)
		}
		if strings.TrimSpace(agent.Runtime) == "" {
			return fmt.Errorf("bundle agent %q is missing runtime", agent.Name)
		}
		if _, duplicate := manifestAgents[agent.Name]; duplicate {
			return fmt.Errorf("bundle manifest repeats agent %q", agent.Name)
		}
		if agent.TemplateRef != "" {
			if err := agenttemplate.ValidateID(agent.TemplateRef); err != nil {
				return fmt.Errorf("bundle agent %q template_ref: %w", agent.Name, err)
			}
		}
		manifestAgents[agent.Name] = agent
	}
	for _, stage := range spec.Stages {
		if stage.Agent == "" {
			continue
		}
		if _, ok := manifestAgents[stage.Agent]; !ok {
			return fmt.Errorf("bundle spec references agent %q but the manifest has no agent entry", stage.Agent)
		}
	}
	if len(manifestAgents) != countPipelineBundleAgents(spec) {
		return errors.New("bundle manifest agents do not exactly match the agents referenced by spec.yaml")
	}
	derivedRequirements := derivePipelineBundleRequirements(spec, manifest.Agents)
	if !equalPipelineBundleRequirements(manifest.Requirements, derivedRequirements) {
		return errors.New("bundle manifest requirements do not match spec.yaml and its agent declarations")
	}
	if !stringSlicesEqual(manifest.WriteAuthority, pipelineBundleWriteAuthority(spec)) {
		return errors.New("bundle manifest write_authority does not match spec.yaml")
	}
	if !stringSlicesEqual(manifest.Warnings, detectPipelineAbsolutePathWarnings(spec)) {
		return errors.New("bundle manifest warnings do not match spec.yaml")
	}
	return nil
}

// pipelineBundleReportManifest derives security-relevant report fields from the
// spec bytes instead of trusting the manifest's copies. The integrity and exact
// equality checks still run immediately after the report; this derivation means
// even a corrupt/tampered bundle cannot hide write authority or path warnings in
// the report that is intentionally printed before validation errors.
func pipelineBundleReportManifest(manifest pipelineBundleManifest, raw []byte) pipelineBundleManifest {
	spec, err := pipeline.Load(raw)
	if err != nil {
		return manifest
	}
	manifest.Requirements = derivePipelineBundleRequirements(spec, manifest.Agents)
	manifest.WriteAuthority = pipelineBundleWriteAuthority(spec)
	manifest.Warnings = detectPipelineAbsolutePathWarnings(spec)
	return manifest
}

func countPipelineBundleAgents(spec pipeline.Spec) int {
	names := make(map[string]struct{})
	for _, stage := range spec.Stages {
		if stage.Agent != "" {
			names[stage.Agent] = struct{}{}
		}
	}
	return len(names)
}

func equalPipelineBundleRequirements(left, right pipelineBundleRequirements) bool {
	if !stringSlicesEqual(left.Runtimes, right.Runtimes) || !stringSlicesEqual(left.UpstreamPipelines, right.UpstreamPipelines) || len(left.Connections) != len(right.Connections) {
		return false
	}
	for i := range left.Connections {
		if left.Connections[i] != right.Connections[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type pipelineBundleRequirementReport struct {
	Runtimes    map[string]string
	Connections map[string]string
	Upstreams   map[string]string
	AgentErrors []error
	MapErrors   []error
}

func inspectPipelineBundleRequirements(ctx context.Context, store *db.Store, paths config.Paths, home string, manifest pipelineBundleManifest, agentMap map[string]string) pipelineBundleRequirementReport {
	report := pipelineBundleRequirementReport{Runtimes: map[string]string{}, Connections: map[string]string{}, Upstreams: map[string]string{}}
	for _, name := range manifest.Requirements.Runtimes {
		if pipelineBundleRuntimeAvailable(name) {
			report.Runtimes[name] = "present"
		} else {
			report.Runtimes[name] = "missing"
		}
	}
	for _, requirement := range manifest.Requirements.UpstreamPipelines {
		_, found, err := store.GetPipeline(ctx, requirement)
		if err != nil {
			report.Upstreams[requirement] = "unchecked (" + err.Error() + ")"
		} else if found {
			report.Upstreams[requirement] = "present"
		} else {
			report.Upstreams[requirement] = "missing (pipeline will remain dormant)"
		}
	}
	for _, requirement := range manifest.Requirements.Connections {
		key := requirement.Kind + "/" + requirement.Name
		report.Connections[key] = inspectPipelineBundleConnection(ctx, paths, home, requirement)
	}
	agentByName := make(map[string]pipelineBundleAgent, len(manifest.Agents))
	for _, agent := range manifest.Agents {
		agentByName[agent.Name] = agent
		if local, mapped := agentMap[agent.Name]; mapped {
			if _, err := store.GetAgent(ctx, local); err != nil {
				report.MapErrors = append(report.MapErrors, fmt.Errorf("agent map %s=%s names local agent %q which is not registered", agent.Name, local, local))
			}
			continue
		}
		if !pipelineBundleRuntimeAvailable(agent.Runtime) {
			report.AgentErrors = append(report.AgentErrors, fmt.Errorf("agent %q requires missing runtime %q (install it or use --agent-map %s=<local>)", agent.Name, agent.Runtime, agent.Name))
		}
	}
	for exported := range agentMap {
		if _, ok := agentByName[exported]; !ok {
			report.MapErrors = append(report.MapErrors, fmt.Errorf("--agent-map references exported agent %q which is not in the bundle", exported))
		}
	}
	return report
}

func inspectPipelineBundleConnection(ctx context.Context, paths config.Paths, home string, requirement pipelineBundleConnection) string {
	if requirement.Name == "" {
		return "missing (empty name)"
	}
	if _, err := os.Stat(activepiecesCredentialsPath(paths.Home)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "unchecked (Activepieces is not configured)"
		}
		return "unchecked (cannot inspect Activepieces credentials file)"
	}
	session, err := openActivepiecesSession(ctx, activepiecesAuthOptions{Home: home})
	if err != nil {
		return "unchecked (" + err.Error() + ")"
	}
	present, err := session.Client.HasConnection(ctx, session.Token, session.ProjectID, requirement.Name)
	if err != nil {
		return "unchecked (" + err.Error() + ")"
	}
	if present {
		return "present"
	}
	return "missing"
}

func printPipelineBundleRequirements(w io.Writer, report pipelineBundleRequirementReport, manifest pipelineBundleManifest) {
	fmt.Fprintln(w, "Requirements report:")
	printRequirementMap(w, "runtime", report.Runtimes)
	printRequirementMap(w, "connection", report.Connections)
	printRequirementMap(w, "upstream pipeline", report.Upstreams)
	if len(manifest.WriteAuthority) == 0 {
		fmt.Fprintln(w, "  write authority: none")
	} else {
		for _, authority := range manifest.WriteAuthority {
			fmt.Fprintf(w, "  write authority: %s (requires explicit re-consent before enabling)\n", authority)
		}
	}
	if len(manifest.Warnings) == 0 {
		fmt.Fprintln(w, "  warning: none")
	} else {
		for _, warning := range manifest.Warnings {
			fmt.Fprintf(w, "  warning: %s\n", warning)
		}
	}
}

func printRequirementMap(w io.Writer, label string, values map[string]string) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Fprintf(w, "  %s: none\n", label)
		return
	}
	for _, key := range keys {
		fmt.Fprintf(w, "  %s %s: %s\n", label, key, values[key])
	}
}

func importPipelineBundle(ctx context.Context, store *db.Store, home, bundleDir, repo, overrideName string, agentMap map[string]string, force, enable bool, manifest pipelineBundleManifest, raw []byte, stdout, stderr io.Writer, report pipelineBundleRequirementReport) error {
	if len(report.MapErrors) > 0 {
		return report.MapErrors[0]
	}
	if len(report.AgentErrors) > 0 {
		return report.AgentErrors[0]
	}
	name := manifest.Pipeline
	if overrideName != "" {
		if !pipelineBundleToken.MatchString(overrideName) {
			return fmt.Errorf("--name %q is not a name-safe token", overrideName)
		}
		name = overrideName
	}
	if _, found, err := store.GetPipeline(ctx, name); err != nil {
		return err
	} else if found && !force {
		return fmt.Errorf("pipeline %q already exists; use --name or --force", name)
	}

	for _, bundled := range manifest.Agents {
		if _, mapped := agentMap[bundled.Name]; mapped {
			continue
		}
		if err := installPipelineBundleTemplate(ctx, store, bundleDir, bundled, force); err != nil {
			return err
		}
		if err := installPipelineBundleAgent(ctx, store, bundled, repo, force); err != nil {
			return err
		}
	}
	rewritten, err := rewritePipelineBundleSpec(raw, repo, name, agentMap)
	if err != nil {
		return fmt.Errorf("inject import parameters: %w", err)
	}
	spec, err := pipeline.Load(rewritten)
	if err != nil {
		return fmt.Errorf("validate imported spec: %w", err)
	}
	parsedRepo, err := daemon.ParseRepository(spec.Repo)
	if err != nil {
		return fmt.Errorf("invalid imported repo %q: %w", spec.Repo, err)
	}
	finalEnabled, err := addPipelineCore(ctx, store, spec, rewritten, parsedRepo.FullName(), pipelineAddCoreOptions{
		Home: home, Enable: enable, ForceEnabled: true, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		return err
	}
	writeLine(stdout, "imported pipeline %s (%s, %d stages)", spec.Name, enabledLabel(finalEnabled), len(spec.Stages))
	return nil
}

func installPipelineBundleTemplate(ctx context.Context, store *db.Store, bundleDir string, bundled pipelineBundleAgent, force bool) error {
	if bundled.TemplateRef == "" {
		return nil
	}
	path := filepath.Join(bundleDir, "templates", bundled.TemplateRef+".md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read embedded template %q: %w", bundled.TemplateRef, err)
	}
	if _, err := agenttemplate.ParseTemplateContent(string(raw)); err != nil {
		return fmt.Errorf("invalid embedded template %q: %w", bundled.TemplateRef, err)
	}
	existing, err := store.GetAgentTemplate(ctx, bundled.TemplateRef)
	if err == nil {
		exported, exportErr := agenttemplate.Export(existing)
		if exportErr != nil {
			return exportErr
		}
		if exported == string(raw) {
			return nil
		}
		if !force {
			return fmt.Errorf("agent template %q already exists with different content; use --force", bundled.TemplateRef)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := agenttemplate.AddLocal(ctx, store, bundled.TemplateRef, path, "", ""); err != nil {
		return fmt.Errorf("install embedded template %q: %w", bundled.TemplateRef, err)
	}
	return nil
}

func installPipelineBundleAgent(ctx context.Context, store *db.Store, bundled pipelineBundleAgent, repo string, force bool) error {
	existing, err := store.GetAgent(ctx, bundled.Name)
	if err == nil {
		same := existing.Runtime == bundled.Runtime && existing.TemplateID == bundled.TemplateRef && existing.RepoScope == repo
		if same {
			return nil
		}
		if !force {
			return fmt.Errorf("agent %q already exists with different runtime, template, or repo scope; use --force or --agent-map", bundled.Name)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := registerAgentOnly(ctx, store, bundled.Name, bundled.Runtime, bundled.TemplateRef, repo); err != nil {
		return fmt.Errorf("register agent %q: %w", bundled.Name, err)
	}
	// registerAgentOnly intentionally accepts the dashboard's minimal shape, but
	// runtime delivery still requires a non-empty role. Bundles do not transport
	// machine policy or roles, so give newly materialized agents the neutral local
	// role used for generic pipeline workers without widening capabilities or
	// autonomy.
	registered, err := store.GetAgent(ctx, bundled.Name)
	if err != nil {
		return err
	}
	registered.Role = "worker"
	if err := store.UpsertAgent(ctx, registered); err != nil {
		return fmt.Errorf("finish registering agent %q: %w", bundled.Name, err)
	}
	return nil
}

func parsePipelineAgentMappings(values []string) (map[string]string, error) {
	mappings := make(map[string]string, len(values))
	for _, raw := range values {
		parts := strings.Split(raw, "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --agent-map %q; use exported=local", raw)
		}
		exported, local := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !pipelineBundleToken.MatchString(exported) || !pipelineBundleToken.MatchString(local) {
			return nil, fmt.Errorf("invalid --agent-map %q; both names must be name-safe tokens", raw)
		}
		if prior, duplicate := mappings[exported]; duplicate && prior != local {
			return nil, fmt.Errorf("exported agent %q is mapped more than once", exported)
		}
		mappings[exported] = local
	}
	return mappings, nil
}

func pipelineBundleRuntimeAvailable(name string) bool {
	name = strings.TrimSpace(name)
	if name == runtime.ShellRuntime {
		return true
	}
	binary := name
	if name == runtime.KimiRuntime || name == runtime.KimiCLIRuntime {
		binary = "kimi"
	}
	supported := false
	for _, candidate := range runtime.SupportedRuntimes() {
		if name == candidate {
			supported = true
			break
		}
	}
	if !supported {
		return false
	}
	_, err := pipelineBundleLookPath(binary)
	return err == nil
}

func requirePipelineBundleVersion(minimum, current string) error {
	minimum = strings.TrimSpace(strings.TrimPrefix(minimum, "v"))
	current = strings.TrimSpace(strings.TrimPrefix(current, "v"))
	// Commit-stamped development builds are not semantically orderable. Treat
	// this feature branch as its declared compatibility floor rather than as
	// infinitely new, so a bundle that requires a future release is still
	// rejected clearly.
	if current == "dev" || strings.HasPrefix(current, "dev-") {
		current = pipelineBundleDevelopmentMinimumVersion
	}
	if minimum == current {
		return nil
	}
	minVersion, err := parsePipelineBundleSemver(minimum)
	if err != nil {
		return fmt.Errorf("invalid gitmoot_version_min %q: %w", minimum, err)
	}
	curVersion, err := parsePipelineBundleSemver(current)
	if err != nil {
		return fmt.Errorf("cannot compare current Gitmoot version %q with bundle minimum %q", current, minimum)
	}
	if comparePipelineBundleSemver(curVersion, minVersion) < 0 {
		return fmt.Errorf("bundle requires Gitmoot >= %s; current version is %s", minimum, current)
	}
	return nil
}

func pipelineBundleExportMinimumVersion() string {
	current := strings.TrimSpace(strings.TrimPrefix(buildinfo.Current().Version, "v"))
	if _, err := parsePipelineBundleSemver(current); err == nil {
		return current
	}
	return pipelineBundleDevelopmentMinimumVersion
}

type pipelineBundleSemver struct {
	major, minor, patch int
	pre                 string
}

func parsePipelineBundleSemver(value string) (pipelineBundleSemver, error) {
	core := value
	if plus := strings.IndexByte(core, '+'); plus >= 0 {
		core = core[:plus]
	}
	pre := ""
	if dash := strings.IndexByte(core, '-'); dash >= 0 {
		pre = core[dash+1:]
		core = core[:dash]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return pipelineBundleSemver{}, errors.New("expected major.minor.patch")
	}
	numbers := [3]int{}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return pipelineBundleSemver{}, errors.New("expected numeric major.minor.patch")
		}
		numbers[i] = n
	}
	return pipelineBundleSemver{major: numbers[0], minor: numbers[1], patch: numbers[2], pre: pre}, nil
}

func comparePipelineBundleSemver(left, right pipelineBundleSemver) int {
	for _, pair := range [][2]int{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if left.pre == right.pre {
		return 0
	}
	if left.pre == "" {
		return 1
	}
	if right.pre == "" {
		return -1
	}
	return comparePipelineBundlePrerelease(left.pre, right.pre)
}

func comparePipelineBundlePrerelease(left, right string) int {
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for i := 0; i < len(leftParts) && i < len(rightParts); i++ {
		if leftParts[i] == rightParts[i] {
			continue
		}
		leftNumber, leftErr := strconv.Atoi(leftParts[i])
		rightNumber, rightErr := strconv.Atoi(rightParts[i])
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		default:
			return strings.Compare(leftParts[i], rightParts[i])
		}
	}
	if len(leftParts) < len(rightParts) {
		return -1
	}
	if len(leftParts) > len(rightParts) {
		return 1
	}
	return 0
}

// rewritePipelineBundleSpec performs scalar-only YAML node surgery. yaml.Node
// identifies the exact name/repo/stage-agent scalars, then the corresponding
// source spans are replaced from the end of the byte slice backward. The rest
// of the spec is never marshaled, so comments, key order, block scalars, quoting,
// blank lines, and trailing newlines stay byte-identical and output is stable.
func rewritePipelineBundleSpec(raw []byte, repo, name string, agentMap map[string]string) ([]byte, error) {
	doc, root, err := decodePipelineBundleYAML(raw)
	if err != nil {
		return nil, err
	}
	_ = doc
	replacements := []pipelineBundleReplacement{}
	repoNode := mappingScalarValue(root, "repo")
	if repoNode != nil {
		replacement, err := scalarNodeReplacement(raw, repoNode, repo)
		if err != nil {
			return nil, err
		}
		replacements = append(replacements, replacement)
	} else {
		nameNode := mappingScalarValue(root, "name")
		if nameNode == nil {
			return nil, errors.New("pipeline spec is missing top-level name")
		}
		offset, err := lineEndOffset(raw, nameNode.Line)
		if err != nil {
			return nil, err
		}
		newline := "\n"
		if offset >= 2 && raw[offset-2] == '\r' {
			newline = "\r\n"
		}
		replacements = append(replacements, pipelineBundleReplacement{start: offset, end: offset, value: []byte("repo: " + yamlPlainScalar(repo) + newline)})
	}
	if name != "" {
		nameNode := mappingScalarValue(root, "name")
		if nameNode == nil {
			return nil, errors.New("pipeline spec is missing top-level name")
		}
		replacement, err := scalarNodeReplacement(raw, nameNode, name)
		if err != nil {
			return nil, err
		}
		replacements = append(replacements, replacement)
	}
	if len(agentMap) > 0 {
		stages := mappingValue(root, "stages")
		if stages == nil || stages.Kind != yaml.SequenceNode {
			return nil, errors.New("pipeline spec stages must be a sequence")
		}
		for _, stage := range stages.Content {
			agentNode := mappingScalarValue(stage, "agent")
			if agentNode == nil {
				continue
			}
			if mapped, ok := agentMap[agentNode.Value]; ok {
				replacement, err := scalarNodeReplacement(raw, agentNode, mapped)
				if err != nil {
					return nil, err
				}
				replacements = append(replacements, replacement)
			}
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
	out := append([]byte(nil), raw...)
	for _, replacement := range replacements {
		if replacement.start < 0 || replacement.end < replacement.start || replacement.end > len(out) {
			return nil, errors.New("invalid YAML scalar source span")
		}
		next := make([]byte, 0, len(out)-(replacement.end-replacement.start)+len(replacement.value))
		next = append(next, out[:replacement.start]...)
		next = append(next, replacement.value...)
		next = append(next, out[replacement.end:]...)
		out = next
	}
	return out, nil
}

type pipelineBundleReplacement struct {
	start int
	end   int
	value []byte
}

func decodePipelineBundleYAML(raw []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("parse pipeline YAML: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("pipeline YAML root must be a mapping")
	}
	return &doc, doc.Content[0], nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func mappingScalarValue(mapping *yaml.Node, key string) *yaml.Node {
	node := mappingValue(mapping, key)
	if node == nil || node.Kind != yaml.ScalarNode {
		return nil
	}
	return node
}

func scalarNodeReplacement(raw []byte, node *yaml.Node, value string) (pipelineBundleReplacement, error) {
	if node.Style == yaml.LiteralStyle || node.Style == yaml.FoldedStyle {
		return pipelineBundleReplacement{}, fmt.Errorf("cannot parameterize block-style scalar at line %d", node.Line)
	}
	lineStart, lineEnd, err := lineBounds(raw, node.Line)
	if err != nil {
		return pipelineBundleReplacement{}, err
	}
	start := lineStart + node.Column - 1
	if start < lineStart || start >= lineEnd {
		return pipelineBundleReplacement{}, fmt.Errorf("invalid YAML scalar column at line %d", node.Line)
	}
	end, err := yamlScalarEnd(raw, start, lineEnd)
	if err != nil {
		return pipelineBundleReplacement{}, err
	}
	return pipelineBundleReplacement{start: start, end: end, value: []byte(yamlPlainScalar(value))}, nil
}

func yamlScalarEnd(raw []byte, start, lineEnd int) (int, error) {
	if raw[start] == '\'' {
		for i := start + 1; i < lineEnd; i++ {
			if raw[i] != '\'' {
				continue
			}
			if i+1 < lineEnd && raw[i+1] == '\'' {
				i++
				continue
			}
			return i + 1, nil
		}
		return 0, errors.New("unterminated single-quoted YAML scalar")
	}
	if raw[start] == '"' {
		escaped := false
		for i := start + 1; i < lineEnd; i++ {
			if escaped {
				escaped = false
				continue
			}
			if raw[i] == '\\' {
				escaped = true
				continue
			}
			if raw[i] == '"' {
				return i + 1, nil
			}
		}
		return 0, errors.New("unterminated double-quoted YAML scalar")
	}
	end := lineEnd
	for i := start; i < lineEnd; i++ {
		if raw[i] == '#' && (i == start || raw[i-1] == ' ' || raw[i-1] == '\t') {
			end = i
			break
		}
		if raw[i] == ',' || raw[i] == ']' || raw[i] == '}' {
			end = i
			break
		}
	}
	for end > start && (raw[end-1] == ' ' || raw[end-1] == '\t' || raw[end-1] == '\r') {
		end--
	}
	return end, nil
}

func yamlPlainScalar(value string) string {
	if pipelineBundlePlain.MatchString(value) {
		return value
	}
	return strconv.Quote(value)
}

func lineBounds(raw []byte, line int) (int, int, error) {
	if line < 1 {
		return 0, 0, errors.New("invalid YAML line")
	}
	start := 0
	for current := 1; current < line; current++ {
		index := bytes.IndexByte(raw[start:], '\n')
		if index < 0 {
			return 0, 0, fmt.Errorf("YAML line %d is outside source", line)
		}
		start += index + 1
	}
	end := len(raw)
	if index := bytes.IndexByte(raw[start:], '\n'); index >= 0 {
		end = start + index
	}
	return start, end, nil
}

func lineEndOffset(raw []byte, line int) (int, error) {
	_, end, err := lineBounds(raw, line)
	if err != nil {
		return 0, err
	}
	if end < len(raw) && raw[end] == '\n' {
		return end + 1, nil
	}
	return end, nil
}

func pipelineBundleDescription(raw []byte) string {
	_, root, err := decodePipelineBundleYAML(raw)
	if err != nil {
		return ""
	}
	if node := mappingScalarValue(root, "description"); node != nil {
		return node.Value
	}
	return ""
}
