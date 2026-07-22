package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// orgPolicyResolver accepts a raw --home, while the Root variant below accepts
// the already resolved <home>/.gitmoot path used by daemon engines.
func orgPolicyResolver(home string) func(string) workflow.OrgEnforcement {
	if strings.TrimSpace(home) == "" {
		paths, err := pathsFromFlag("")
		if err != nil {
			return func(string) workflow.OrgEnforcement { return workflow.OrgEnforcement{LoadErr: err} }
		}
		return orgPolicyResolverPaths(paths)
	}
	return orgPolicyResolverPaths(config.PathsForHome(home))
}

func orgPolicyResolverRoot(root string) func(string) workflow.OrgEnforcement {
	return orgPolicyResolverPaths(config.Paths{ConfigFile: configFileAtRoot(root)})
}

func orgPolicyResolverPaths(paths config.Paths) func(string) workflow.OrgEnforcement {
	return func(string) workflow.OrgEnforcement {
		cfg, err := config.LoadOrg(paths)
		if err != nil {
			return workflow.OrgEnforcement{LoadErr: err}
		}
		return workflow.OrgEnforcement{
			Enabled: cfg.Enabled(), Enforce: cfg.Enforce(),
			Role: func(name string) (workflow.OrgRole, bool) {
				r, ok := cfg.Role(name)
				return workflow.OrgRole{Name: r.Name, Parent: r.Parent, Scope: append([]string(nil), r.Scope...), MergeRule: r.MergeRule}, ok
			},
			ScopeMatches: config.ScopeMatches,
		}
	}
}

// preflightOrgScope prevents an org-blocked implement/task-run request from
// allocating a task worktree or branch lock before Mailbox.Enqueue can reject.
// Warn mode intentionally leaves the durable event to the enqueue chokepoint.
func preflightOrgScope(policy workflow.OrgEnforcement, repo, actingRole string, operatorOrigin bool) error {
	if !operatorOrigin {
		return nil
	}
	_, err := workflow.OrgScopeDecision(policy, actingRole, repo)
	return err
}

func fixedOrgPolicy(policy workflow.OrgEnforcement) func(string) workflow.OrgEnforcement {
	return func(string) workflow.OrgEnforcement { return policy }
}

var newOrgProvider = func(roles []string) org.Provider { return cockpit.NewHerdrOrgProvider(roles) }

var orgDoctorRunner subprocess.Runner = subprocess.ExecRunner{}

func runOrg(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrgUsage(stdout)
		return 0
	}
	switch args[0] {
	case "validate", "show":
		return runOrgValidateOrShow(args, stdout, stderr)
	case "init":
		return runOrgInit(args[1:], stdout, stderr)
	case "brief":
		return runOrgBrief(args[1:], stdout, stderr)
	case "chart":
		return runOrgChart(args[1:], stdout, stderr)
	case "status":
		return runOrgStatus(args[1:], stdout, stderr)
	case "escalate":
		return runOrgEscalate(args[1:], stdout, stderr)
	case "events":
		return runOrgEvents(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown org command %q\n\n", args[0])
		printOrgUsage(stderr)
		return 2
	}
}

func printOrgUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org validate [--home PATH]")
	fmt.Fprintln(w, "  gitmoot org show [--home PATH]")
	fmt.Fprintln(w, "  gitmoot org init [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org brief --role NAME [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org chart [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org status [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org escalate --to ROLE --workflow LABEL [--org-role ROLE] [--repo OWNER/REPO] [--json] [--home DIR] \"QUESTION\"")
	fmt.Fprintln(w, "  gitmoot org events rule add --on KIND [--match FILTER] --wake ROLE [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org events rule list [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org events rule rm [--home DIR] ID")
}

func runOrgValidateOrShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "org %s does not accept positional arguments\n", args[0])
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: resolve paths: %v\n", args[0], err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: %v\n", args[0], err)
		return 1
	}
	if args[0] == "validate" {
		fmt.Fprintf(stdout, "ok %d roles\n", len(cfg.Roles()))
		return 0
	}
	for _, role := range cfg.Roles() {
		fmt.Fprintf(stdout, "%s\tparent=%s\tscope=%s\tmerge_rule=%s\n", role.Name, role.Parent, strings.Join(role.Scope, ","), firstNonEmpty(role.MergeRule, "owner"))
	}
	return 0
}

const starterOrgConfig = `

# Organization registry (#1042). enforce = "warn" logs a durable event without
# rejecting; set "block" to reject out-of-scope dispatches (see gitmoot org validate).
[org]
enforce = "warn"

[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"
`

func runOrgInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org init accepts no positional arguments")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org init: resolve paths: %v\n", err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "org init: %v\n", err)
		return 1
	}
	registry, err := config.LoadOrgRegistry(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org init: %v\n", err)
		return 1
	}
	if !registry.Enabled() {
		file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "org init: open config: %v\n", err)
			return 1
		}
		_, writeErr := io.WriteString(file, starterOrgConfig)
		closeErr := file.Close()
		if writeErr != nil {
			fmt.Fprintf(stderr, "org init: write config: %v\n", writeErr)
			return 1
		}
		if closeErr != nil {
			fmt.Fprintf(stderr, "org init: close config: %v\n", closeErr)
			return 1
		}
		registry, err = config.LoadOrgRegistry(paths)
		if err != nil {
			fmt.Fprintf(stderr, "org init: validate scaffold: %v\n", err)
			return 1
		}
	}
	check := doctor.CheckHerdrVersion(context.Background(), orgDoctorRunner, doctor.OrgMinimumHerdrVersion)
	if !check.OK {
		fmt.Fprintf(stderr, "org init: %s\n", check.Detail)
		return 1
	}
	if _, err := orgProviderSnapshot(context.Background(), registry); err != nil {
		fmt.Fprintf(stderr, "org init: Herdr snapshot unavailable: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "initialized organization registry at %s\n", paths.ConfigFile)
	fmt.Fprintf(stdout, "herdr: %s\n", check.Detail)
	return 0
}

type orgBriefOutput struct {
	Role            string             `json:"role"`
	Parent          string             `json:"parent,omitempty"`
	Children        []string           `json:"children"`
	Path            []string           `json:"path"`
	Scope           []string           `json:"scope"`
	MergeRule       string             `json:"merge_rule"`
	LastSeenAt      string             `json:"last_seen_at,omitempty"`
	LastCommand     string             `json:"last_command,omitempty"`
	ProviderState   org.LifecycleState `json:"provider_state"`
	ProviderDetail  string             `json:"provider_detail,omitempty"`
	ObservedAt      time.Time          `json:"observed_at,omitempty"`
	ProviderVersion string             `json:"provider_version,omitempty"`
	// TODO(#1058): add bounded open escalations only after creation, resolution,
	// and correlation identifiers have a frozen store contract.
}

type orgStatusOutput struct {
	Role            string             `json:"role"`
	Parent          string             `json:"parent,omitempty"`
	Depth           int                `json:"depth,omitempty"`
	Scope           []string           `json:"scope"`
	MergeRule       string             `json:"merge_rule"`
	LastSeenAt      string             `json:"last_seen_at,omitempty"`
	LastSeenAge     string             `json:"last_seen_age,omitempty"`
	LastCommand     string             `json:"last_command,omitempty"`
	ProviderState   org.LifecycleState `json:"provider_state"`
	ProviderDetail  string             `json:"provider_detail,omitempty"`
	ObservedAt      time.Time          `json:"observed_at,omitempty"`
	ProviderVersion string             `json:"provider_version,omitempty"`
}

func runOrgBrief(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org brief", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roleName := fs.String("role", "", "organization role to brief")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*roleName) == "" {
		fmt.Fprintln(stderr, "org brief requires --role NAME")
		return 2
	}
	ctx := context.Background()
	registry, presence, store, err := loadOrgCommandState(ctx, *home)
	if err != nil {
		fmt.Fprintf(stderr, "org brief: %v\n", err)
		return 1
	}
	defer store.Close()
	role, ok := registry.Role(*roleName)
	if !ok {
		fmt.Fprintf(stderr, "org brief: unknown org role %q\n", strings.TrimSpace(*roleName))
		return 1
	}
	if err := store.TouchOrgRolePresence(ctx, role.Name, "org brief"); err != nil {
		fmt.Fprintf(stderr, "org brief: touch presence: %v\n", err)
		return 1
	}
	updatedPresence, err := store.ListOrgRolePresence(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "org brief: reload presence: %v\n", err)
		return 1
	}
	for _, row := range updatedPresence {
		presence[row.Role] = row
	}
	snapshot, snapshotErr := orgProviderSnapshot(ctx, registry)
	live := org.RoleLiveState{State: org.StateUnknown}
	if snapshotErr != nil {
		live.Detail = snapshotErr.Error()
	} else if value, exists := snapshot.States[role.Name]; exists {
		live = value
	} else {
		live.Detail = "provider snapshot omitted this role"
	}
	children := registry.Children(role.Name)
	childNames := make([]string, 0, len(children))
	for _, child := range children {
		childNames = append(childNames, child.Name)
	}
	row := presence[role.Name]
	out := orgBriefOutput{
		Role: role.Name, Parent: role.Parent, Children: childNames, Path: registry.Path(role.Name), Scope: role.Scope,
		MergeRule: role.MergeRule, LastSeenAt: row.LastSeenAt, LastCommand: row.LastCommand,
		ProviderState: live.State, ProviderDetail: live.Detail, ObservedAt: snapshot.ObservedAt, ProviderVersion: snapshot.ProviderVersion,
	}
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "org brief: %v\n", err)
			return 1
		}
		return 0
	}
	printOrgBrief(stdout, out)
	return 0
}

func runOrgChart(args []string, stdout, stderr io.Writer) int {
	return runOrgOverview("chart", args, stdout, stderr)
}

func runOrgStatus(args []string, stdout, stderr io.Writer) int {
	return runOrgOverview("status", args, stdout, stderr)
}

func runOrgOverview(command string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "org %s accepts no positional arguments\n", command)
		return 2
	}
	ctx := context.Background()
	registry, presence, store, err := loadOrgCommandState(ctx, *home)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: %v\n", command, err)
		return 1
	}
	defer store.Close()
	snapshot, err := orgProviderSnapshot(ctx, registry)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: Herdr snapshot unavailable: %v\n", command, err)
		return 1
	}
	rows := make([]orgStatusOutput, 0, len(registry.Roles))
	observedNow := time.Now().UTC()
	for _, role := range registry.SortedRoles() {
		live, ok := snapshot.States[role.Name]
		if !ok {
			live = org.RoleLiveState{State: org.StateUnknown, Detail: "provider snapshot omitted this role"}
		}
		seen := presence[role.Name]
		rows = append(rows, orgStatusOutput{
			Role: role.Name, Parent: role.Parent, Depth: len(registry.Path(role.Name)) - 1,
			Scope: role.Scope, MergeRule: role.MergeRule, LastSeenAt: seen.LastSeenAt, LastSeenAge: orgPresenceAge(seen.LastSeenAt, observedNow), LastCommand: seen.LastCommand,
			ProviderState: live.State, ProviderDetail: live.Detail, ObservedAt: snapshot.ObservedAt, ProviderVersion: snapshot.ProviderVersion,
		})
	}
	if command == "chart" {
		sort.Slice(rows, func(i, j int) bool {
			left, right := strings.Join(registry.Path(rows[i].Role), "/"), strings.Join(registry.Path(rows[j].Role), "/")
			return left < right
		})
	}
	if *jsonOutput {
		if err := writeJSON(stdout, rows); err != nil {
			fmt.Fprintf(stderr, "org %s: %v\n", command, err)
			return 1
		}
		return 0
	}
	if command == "chart" {
		for _, row := range rows {
			fmt.Fprintf(stdout, "%s%s · %s · scope=%s · merge=%s · seen=%s\n", strings.Repeat("  ", row.Depth), row.Role, row.ProviderState, strings.Join(row.Scope, ","), dash(row.MergeRule), dash(row.LastSeenAge))
		}
		return 0
	}
	fmt.Fprintln(stdout, "ROLE\tSTATE\tLAST SEEN\tAGE\tLAST COMMAND\tDETAIL")
	for _, row := range rows {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Role, row.ProviderState, dash(row.LastSeenAt), dash(row.LastSeenAge), dash(row.LastCommand), dash(row.ProviderDetail))
	}
	return 0
}

func printOrgBrief(w io.Writer, brief orgBriefOutput) {
	fmt.Fprintf(w, "role: %s\n", brief.Role)
	fmt.Fprintf(w, "parent: %s\n", dash(brief.Parent))
	fmt.Fprintf(w, "children: %s\n", dash(strings.Join(brief.Children, ", ")))
	fmt.Fprintf(w, "path: %s\n", dash(strings.Join(brief.Path, " > ")))
	fmt.Fprintf(w, "scope: %s\n", dash(strings.Join(brief.Scope, ", ")))
	fmt.Fprintf(w, "merge_rule: %s\n", dash(brief.MergeRule))
	fmt.Fprintf(w, "last_seen: %s\n", dash(brief.LastSeenAt))
	fmt.Fprintf(w, "last_command: %s\n", dash(brief.LastCommand))
	fmt.Fprintf(w, "provider: %s\n", brief.ProviderState)
	fmt.Fprintf(w, "provider_detail: %s\n", dash(brief.ProviderDetail))
}

func loadOrgCommandState(ctx context.Context, home string) (org.Registry, map[string]db.OrgRolePresence, *db.Store, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return org.Registry{}, nil, nil, err
	}
	registry, err := config.LoadOrgRegistry(paths)
	if err != nil {
		return org.Registry{}, nil, nil, err
	}
	if !registry.Enabled() {
		return org.Registry{}, nil, nil, errors.New("organization registry is disabled; run `gitmoot org init`")
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return org.Registry{}, nil, nil, err
	}
	rows, err := store.ListOrgRolePresence(ctx)
	if err != nil {
		store.Close()
		return org.Registry{}, nil, nil, err
	}
	presence := make(map[string]db.OrgRolePresence, len(rows))
	for _, row := range rows {
		presence[row.Role] = row
	}
	return registry, presence, store, nil
}

func orgProviderSnapshot(ctx context.Context, registry org.Registry) (org.Snapshot, error) {
	roles := registry.SortedRoles()
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	provider := newOrgProvider(names)
	if provider == nil {
		return org.Snapshot{}, errors.New("organization live-state provider is not configured")
	}
	return provider.Snapshot(ctx)
}

// validateAndTouchActingOrgRole is the shared local job ingress for --org-role.
// Validation happens before dispatch mutation; invalid/disabled config creates
// neither a presence row nor a job.
func validateAndTouchActingOrgRole(ctx context.Context, store *db.Store, home, role, command string) error {
	role = strings.TrimSpace(role)
	if role == "" {
		return nil
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	registry, err := config.LoadOrgRegistry(paths)
	if err != nil {
		return err
	}
	if !registry.Enabled() {
		return errors.New("--org-role requires an enabled organization registry; run `gitmoot org init`")
	}
	if _, ok := registry.Role(role); !ok {
		return fmt.Errorf("unknown org role %q", role)
	}
	return store.TouchOrgRolePresence(ctx, role, command)
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func orgPresenceAge(value string, now time.Time) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var observed time.Time
	var err error
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		observed, err = time.Parse(layout, value)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "unknown"
	}
	age := now.Sub(observed.UTC())
	if age < 0 {
		age = 0
	}
	return age.Round(time.Second).String()
}

type orgEscalateOutput struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Workflow string `json:"workflow"`
	Question string `json:"question"`
}

func runOrgEscalate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org escalate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	toFlag := fs.String("to", "", "ancestor role to escalate to")
	workflowID := fs.String("workflow", "", "workflow label for the escalation note")
	fromFlag := fs.String("org-role", "", "acting organization role")
	repo := fs.String("repo", "", "repository binding for the escalation note")
	jsonOutput := fs.Bool("json", false, "print the escalation as JSON")
	question, flagArgs, ok := orgEscalateQuestionAndFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "org escalate requires exactly one question")
		return 2
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org escalate requires exactly one question")
		return 2
	}
	if question == "" {
		fmt.Fprintln(stderr, "org escalate question must be non-empty")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org escalate: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org escalate: %v\n", err)
		return 1
	}
	if !cfg.Enabled() {
		fmt.Fprintln(stderr, "org escalate requires an [org] registry")
		return 2
	}
	from := strings.ToLower(strings.TrimSpace(*fromFlag))
	if from == "" {
		from = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	if _, ok := cfg.Role(from); !ok {
		fmt.Fprintf(stderr, "unknown org role %q\n", from)
		return 2
	}
	to := strings.ToLower(strings.TrimSpace(*toFlag))
	if to == "" {
		fmt.Fprintln(stderr, "org escalate requires --to")
		return 2
	}
	if _, ok := cfg.Role(to); !ok {
		fmt.Fprintf(stderr, "unknown org role %q\n", to)
		return 2
	}
	if !slices.Contains(cfg.Ancestors(from), to) {
		fmt.Fprintf(stderr, "--to %q must be an ancestor of %q in the org hierarchy\n", to, from)
		return 2
	}
	label := strings.TrimSpace(*workflowID)
	if label == "" {
		fmt.Fprintln(stderr, "org escalate requires --workflow")
		return 2
	}
	if err := workflow.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "org escalate: %v\n", err)
		return 2
	}
	body := workflow.FormatOrgEscalateNote(from, to, label, question)
	if body == "" || len(body) > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "org escalate question must produce a note of at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		count, err := store.CountJobsByWorkflow(context.Background(), label)
		if err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("workflow %q has no jobs; refusing note to guard against a typo", label)
		}
		_, err = store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID: label, Author: from, Body: body, Repo: strings.TrimSpace(*repo),
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "org escalate: %v\n", err)
		return 1
	}
	out := orgEscalateOutput{From: from, To: to, Workflow: label, Question: question}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			fmt.Fprintf(stderr, "org escalate: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "escalated from %s to %s in workflow %s\n", from, to, label)
	return 0
}

func orgEscalateQuestionAndFlags(args []string) (string, []string, bool) {
	needsValue := map[string]bool{"--home": true, "--to": true, "--workflow": true, "--org-role": true, "--repo": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if needsValue[arg] {
			i++
			if i >= len(args) {
				return "", nil, false
			}
			continue
		}
		if arg == "--json" || strings.HasPrefix(arg, "--home=") || strings.HasPrefix(arg, "--to=") || strings.HasPrefix(arg, "--workflow=") || strings.HasPrefix(arg, "--org-role=") || strings.HasPrefix(arg, "--repo=") {
			continue
		}
		if i != len(args)-1 {
			return "", nil, false
		}
		return strings.TrimSpace(arg), args[:i], strings.TrimSpace(arg) != ""
	}
	return "", nil, false
}

var eventRuleKinds = map[string]struct{}{
	"escalation":   {},
	"attention":    {},
	"guard":        {},
	"job-terminal": {},
	"blocked":      {},
}

func runOrgEvents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage:\n  gitmoot org events rule add --on <kind> [--match <filter>] --wake <role> [--home path]\n  gitmoot org events rule list [--home path]\n  gitmoot org events rule rm [--home path] <id>")
		return 0
	}
	if args[0] != "rule" {
		fmt.Fprintf(stderr, "unknown org events command %q\n", args[0])
		return 2
	}
	if len(args) == 1 || args[1] == "-h" || args[1] == "--help" {
		fmt.Fprintln(stdout, "Usage:\n  gitmoot org events rule add --on <kind> [--match <filter>] --wake <role> [--home path]\n  gitmoot org events rule list [--home path]\n  gitmoot org events rule rm [--home path] <id>")
		return 0
	}
	switch args[1] {
	case "add":
		return runOrgEventRuleAdd(args[2:], stdout, stderr)
	case "list":
		return runOrgEventRuleList(args[2:], stdout, stderr)
	case "rm":
		return runOrgEventRuleRemove(args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown org events rule command %q\n", args[1])
		return 2
	}
}

func runOrgEventRuleAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org events rule add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	onKind := fs.String("on", "", "event kind: escalation, attention, guard, job-terminal, or blocked")
	// V1 intentionally keeps matching simple and inspectable: one
	// case-insensitive substring tested independently against repo and job id;
	// an empty filter matches every event of the selected kind.
	match := fs.String("match", "", "case-insensitive substring matched against event repo or job id; empty matches all")
	wake := fs.String("wake", "", "organization role to wake")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org events rule add does not accept positional arguments")
		return 2
	}
	kind := strings.ToLower(strings.TrimSpace(*onKind))
	if _, ok := eventRuleKinds[kind]; !ok {
		fmt.Fprintf(stderr, "unknown event rule kind %q; want escalation, attention, guard, job-terminal, or blocked\n", kind)
		return 2
	}
	roleName := strings.ToLower(strings.TrimSpace(*wake))
	if roleName == "" {
		fmt.Fprintln(stderr, "org events rule add requires --wake")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org events rule add: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org events rule add: %v\n", err)
		return 1
	}
	role, ok := cfg.Role(roleName)
	if !ok {
		fmt.Fprintf(stderr, "unknown org role %q\n", roleName)
		return 2
	}
	id, err := newEventRuleID()
	if err != nil {
		fmt.Fprintf(stderr, "org events rule add: generate id: %v\n", err)
		return 1
	}
	rule := db.EventRule{ID: id, OnKind: kind, MatchFilter: strings.TrimSpace(*match), WakeRole: role.Name, Enabled: true}
	if err := withStore(*home, func(store *db.Store) error {
		return store.AddEventRule(context.Background(), rule)
	}); err != nil {
		fmt.Fprintf(stderr, "org events rule add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "added %s\n", id)
	return 0
}

func runOrgEventRuleList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org events rule list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org events rule list does not accept positional arguments")
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		rules, err := store.ListEventRules(context.Background())
		if err != nil {
			return err
		}
		for _, rule := range rules {
			fmt.Fprintf(stdout, "%s\ton=%s\tmatch=%s\twake=%s\tenabled=%t\n", rule.ID, rule.OnKind, rule.MatchFilter, rule.WakeRole, rule.Enabled)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "org events rule list: %v\n", err)
		return 1
	}
	return 0
}

func runOrgEventRuleRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org events rule rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "org events rule rm requires exactly one rule id; place --home before the id")
		return 2
	}
	id := strings.TrimSpace(fs.Arg(0))
	if err := withStore(*home, func(store *db.Store) error {
		return store.DeleteEventRule(context.Background(), id)
	}); err != nil {
		fmt.Fprintf(stderr, "org events rule rm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %s\n", id)
	return 0
}

func newEventRuleID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "event-rule-" + hex.EncodeToString(raw[:]), nil
}
